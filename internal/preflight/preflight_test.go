package preflight

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"bilibili-txt/internal/config"
)

// fakeFileInfo lets us hand-craft os.Stat return values without touching
// the real filesystem. Zero value ⇒ regular file, mode 0755, 200 MiB —
// i.e. a "healthy" whisper model. Individual tests override the fields
// they care about.
type fakeFileInfo struct {
	name  string
	size  int64
	mode  fs.FileMode
	isDir bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.isDir }
func (f fakeFileInfo) Sys() any           { return nil }

// makeExecutable writes a stub file at path with the 0755 mode bit set
// so real os.Stat + preflight's `0111` check will pass. Body is
// deliberately non-empty so callers can spot-check file size if they
// care. t.TempDir cleanup handles removal.
func makeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// makeNonExecutable writes a plain file (mode 0644) so preflight's
// executable-bit check rejects it. Used to exercise the "yaml points
// at a non-executable file" branch.
func makeNonExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// resultByName picks the first CheckResult with matching Name. Fails
// the test if no such result exists — every plan §4.2 check must
// always appear, even when preceding checks fail.
func resultByName(t *testing.T, rs []CheckResult, name string) CheckResult {
	t.Helper()
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result named %q in %+v", name, rs)
	return CheckResult{}
}

// baseCfg builds a config that "looks reasonable" — model + output dir
// point at temp locations that the test controls, binaries left empty
// so PATH resolution kicks in. Individual tests override only what
// they care about, keeping the noise low.
func baseCfg(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "ggml-large-v3-turbo.bin")
	// 200 MiB stub — well above the 100 MiB floor so healthy-path
	// tests don't need to think about it.
	if err := os.WriteFile(modelPath, make([]byte, 200*1024*1024), 0o644); err != nil {
		t.Fatalf("write model: %v", err)
	}
	return &config.Config{
		Format:    "txt",
		Model:     modelPath,
		OutputDir: filepath.Join(dir, "out"),
		Naming:    config.Naming{OnConflict: "ask"},
	}
}

func TestRun_AllChecksPresentEvenWhenSomeFail(t *testing.T) {
	// Plan §4.2 explicitly requires that Run collects *all* failures.
	// Give it a config where everything is broken and assert the
	// slice length is exactly the six documented checks.
	cfg := &config.Config{
		Format:    "txt",
		Model:     "/does/not/exist",
		OutputDir: "/definitely/not/writable/xxxx",
	}
	r := &Runner{
		LookPath:  func(name string) (string, error) { return "", errors.New("not found") },
		VersionOf: func(path string) (string, error) { return "", errors.New("unused") },
		StatFile:  func(name string) (fs.FileInfo, error) { return nil, fs.ErrNotExist },
	}
	got := r.Run(cfg)
	wantNames := []string{
		"yt-dlp",
		"yt-dlp version",
		"ffmpeg",
		"whisper-cli",
		"whisper model",
		"output dir",
	}
	if len(got) != len(wantNames) {
		t.Fatalf("expected %d checks, got %d: %+v", len(wantNames), len(got), got)
	}
	for i, r := range got {
		if r.Name != wantNames[i] {
			t.Fatalf("result[%d].Name = %q, want %q", i, r.Name, wantNames[i])
		}
	}
}

func TestRun_HappyPath_PATHResolution(t *testing.T) {
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}

	dir := t.TempDir()
	ytdlpPath := filepath.Join(dir, "yt-dlp")
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	whisperPath := filepath.Join(dir, "whisper-cli")
	makeExecutable(t, ytdlpPath)
	makeExecutable(t, ffmpegPath)
	makeExecutable(t, whisperPath)

	r := &Runner{
		LookPath: func(name string) (string, error) {
			switch name {
			case "yt-dlp":
				return ytdlpPath, nil
			case "ffmpeg":
				return ffmpegPath, nil
			case "whisper-cli":
				return whisperPath, nil
			}
			return "", errors.New("unexpected LookPath: " + name)
		},
		VersionOf: func(path string) (string, error) {
			if path == ytdlpPath {
				return "2025.10.22", nil
			}
			return "", errors.New("unexpected VersionOf: " + path)
		},
	}
	got := r.Run(cfg)
	if !AllOK(got) {
		t.Fatalf("expected all OK, got %+v", got)
	}
	// Source suffix should be "(PATH)" for all three binaries when
	// they came from PATH resolution.
	for _, name := range []string{"yt-dlp", "ffmpeg", "whisper-cli"} {
		res := resultByName(t, got, name)
		if !strings.Contains(res.Message, "(PATH)") {
			t.Errorf("%s message should note source (PATH), got %q", name, res.Message)
		}
	}
	// Version check message should include the resolved version.
	verMsg := resultByName(t, got, "yt-dlp version").Message
	if !strings.Contains(verMsg, "2025.10.22") {
		t.Errorf("version check message should mention 2025.10.22, got %q", verMsg)
	}
}

func TestRun_HappyPath_YAMLResolution(t *testing.T) {
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}

	binDir := t.TempDir()
	ytdlpPath := filepath.Join(binDir, "yt-dlp")
	ffmpegPath := filepath.Join(binDir, "ffmpeg")
	whisperPath := filepath.Join(binDir, "whisper-cli")
	makeExecutable(t, ytdlpPath)
	makeExecutable(t, ffmpegPath)
	makeExecutable(t, whisperPath)

	cfg.Binaries = config.Binaries{
		Ytdlp:      ytdlpPath,
		Ffmpeg:     ffmpegPath,
		WhisperCLI: whisperPath,
	}
	r := &Runner{
		LookPath: func(name string) (string, error) {
			t.Fatalf("LookPath must NOT be called when yaml supplies path; got %q", name)
			return "", nil
		},
		VersionOf: func(path string) (string, error) {
			if path == ytdlpPath {
				return "2025.11.01", nil
			}
			return "", errors.New("unexpected VersionOf: " + path)
		},
	}
	got := r.Run(cfg)
	if !AllOK(got) {
		t.Fatalf("expected all OK, got %+v", got)
	}
	for _, name := range []string{"yt-dlp", "ffmpeg", "whisper-cli"} {
		res := resultByName(t, got, name)
		if !strings.Contains(res.Message, "(yaml)") {
			t.Errorf("%s message should note source (yaml), got %q", name, res.Message)
		}
	}
}

func TestRun_YAMLPointsAtMissingFile_NoFallback(t *testing.T) {
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "not-there")
	cfg.Binaries.Ytdlp = missing
	r := &Runner{
		LookPath: func(name string) (string, error) {
			if name == "yt-dlp" {
				t.Fatalf("must not fall back to PATH when yaml value was set-but-invalid")
			}
			// ffmpeg + whisper-cli left unspecified → PATH lookup allowed
			return "/opt/homebrew/bin/" + name, nil
		},
		StatFile: func(name string) (fs.FileInfo, error) {
			// The path we care about must NOT exist; everything else
			// (fake ffmpeg/whisper-cli under /opt/homebrew, the model,
			// the output-dir probe file) gets a healthy stub so the
			// test focuses on the yaml-yt-dlp branch.
			if name == missing {
				return nil, fs.ErrNotExist
			}
			return fakeFileInfo{name: name, size: 200 * 1024 * 1024, mode: 0o755}, nil
		},
		VersionOf: func(path string) (string, error) {
			return "2025.10.22", nil
		},
	}
	got := r.Run(cfg)
	ytres := resultByName(t, got, "yt-dlp")
	if ytres.OK {
		t.Fatalf("expected yt-dlp check to fail with missing yaml path; got %+v", ytres)
	}
	if !strings.Contains(ytres.Message, "binaries.ytdlp") {
		t.Errorf("failure message should mention binaries.ytdlp for locality; got %q", ytres.Message)
	}
	if !strings.Contains(ytres.Message, missing) {
		t.Errorf("failure message should echo the offending path; got %q", ytres.Message)
	}
}

func TestRun_YAMLPointsAtDirectory_NoFallback(t *testing.T) {
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dirAsBinary := t.TempDir()
	cfg.Binaries.Ffmpeg = dirAsBinary
	r := &Runner{
		LookPath: func(name string) (string, error) {
			if name == "ffmpeg" {
				t.Fatalf("must not fall back to PATH when yaml value was set-but-invalid")
			}
			return "/opt/homebrew/bin/" + name, nil
		},
		StatFile: func(name string) (fs.FileInfo, error) {
			if name == dirAsBinary {
				return fakeFileInfo{name: name, isDir: true, mode: fs.ModeDir | 0o755}, nil
			}
			return fakeFileInfo{name: name, size: 200 * 1024 * 1024, mode: 0o755}, nil
		},
		VersionOf: func(string) (string, error) { return "2025.10.22", nil },
	}
	got := r.Run(cfg)
	res := resultByName(t, got, "ffmpeg")
	if res.OK {
		t.Fatalf("directory-as-binary must fail; got %+v", res)
	}
	// Message is in Chinese; assert the semantic keyword rather than an
	// English fragment that only happens to appear in the temp-dir path.
	if !strings.Contains(res.Message, "目录") {
		t.Errorf("failure message should hint it's a directory; got %q", res.Message)
	}
}

func TestRun_YAMLPointsAtNonExecutable_NoFallback(t *testing.T) {
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir := t.TempDir()
	nonExec := filepath.Join(dir, "whisper-cli")
	makeNonExecutable(t, nonExec)
	cfg.Binaries.WhisperCLI = nonExec

	r := &Runner{
		LookPath: func(name string) (string, error) {
			if name == "whisper-cli" {
				t.Fatalf("must not fall back to PATH")
			}
			return "/opt/homebrew/bin/" + name, nil
		},
		VersionOf: func(string) (string, error) { return "2025.10.22", nil },
	}
	got := r.Run(cfg)
	res := resultByName(t, got, "whisper-cli")
	if res.OK {
		t.Fatalf("non-executable yaml path must fail; got %+v", res)
	}
	if !strings.Contains(res.Message, "不可执行") {
		t.Errorf("failure message should say '不可执行'; got %q", res.Message)
	}
}

func TestRun_PATHFallback_NothingFound(t *testing.T) {
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	r := &Runner{
		LookPath: func(name string) (string, error) { return "", errors.New("not in PATH") },
		VersionOf: func(string) (string, error) {
			t.Fatalf("must not run --version when binary was not resolved")
			return "", nil
		},
	}
	got := r.Run(cfg)
	for _, name := range []string{"yt-dlp", "ffmpeg", "whisper-cli"} {
		res := resultByName(t, got, name)
		if res.OK {
			t.Errorf("%s should have failed with no PATH match; got %+v", name, res)
		}
		if !strings.Contains(strings.ToLower(res.Message), "brew install") {
			t.Errorf("%s failure message should include install hint; got %q", name, res.Message)
		}
	}
	// Version check must be present but degraded (skipped or failed)
	// because yt-dlp itself failed to resolve.
	ver := resultByName(t, got, "yt-dlp version")
	if ver.OK {
		t.Errorf("version check should fail when yt-dlp is missing; got %+v", ver)
	}
}

func TestRun_YtdlpVersionTooOld(t *testing.T) {
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir := t.TempDir()
	ytdlpPath := filepath.Join(dir, "yt-dlp")
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	whisperPath := filepath.Join(dir, "whisper-cli")
	makeExecutable(t, ytdlpPath)
	makeExecutable(t, ffmpegPath)
	makeExecutable(t, whisperPath)
	r := &Runner{
		LookPath: func(name string) (string, error) {
			return filepath.Join(dir, name), nil
		},
		VersionOf: func(path string) (string, error) {
			return "2023.09.24", nil
		},
	}
	got := r.Run(cfg)
	// The other checks should still pass.
	if !resultByName(t, got, "yt-dlp").OK {
		t.Errorf("yt-dlp existence should still pass; got %+v", resultByName(t, got, "yt-dlp"))
	}
	ver := resultByName(t, got, "yt-dlp version")
	if ver.OK {
		t.Fatalf("version 2023.09.24 must be flagged too old; got %+v", ver)
	}
	if !strings.Contains(ver.Message, "2023.09.24") {
		t.Errorf("version message should echo actual version 2023.09.24; got %q", ver.Message)
	}
	if !strings.Contains(strings.ToLower(ver.Message), "brew upgrade") {
		t.Errorf("version message should suggest brew upgrade; got %q", ver.Message)
	}
}

func TestRun_YtdlpVersionUnparseable(t *testing.T) {
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir := t.TempDir()
	ytdlpPath := filepath.Join(dir, "yt-dlp")
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	whisperPath := filepath.Join(dir, "whisper-cli")
	makeExecutable(t, ytdlpPath)
	makeExecutable(t, ffmpegPath)
	makeExecutable(t, whisperPath)
	r := &Runner{
		LookPath: func(name string) (string, error) {
			return filepath.Join(dir, name), nil
		},
		// Unexpected shape — no digits, only text. Preflight must
		// treat this as a version-check failure but keep other results.
		VersionOf: func(string) (string, error) {
			return "yt-dlp version not-a-real-version", nil
		},
	}
	got := r.Run(cfg)
	ver := resultByName(t, got, "yt-dlp version")
	if ver.OK {
		t.Fatalf("unparseable version must fail; got %+v", ver)
	}
}

func TestRun_YtdlpVersionRunError(t *testing.T) {
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir := t.TempDir()
	ytdlpPath := filepath.Join(dir, "yt-dlp")
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	whisperPath := filepath.Join(dir, "whisper-cli")
	makeExecutable(t, ytdlpPath)
	makeExecutable(t, ffmpegPath)
	makeExecutable(t, whisperPath)
	r := &Runner{
		LookPath: func(name string) (string, error) {
			return filepath.Join(dir, name), nil
		},
		VersionOf: func(string) (string, error) {
			return "", errors.New("boom")
		},
	}
	got := r.Run(cfg)
	ver := resultByName(t, got, "yt-dlp version")
	if ver.OK {
		t.Fatalf("run error must fail version check; got %+v", ver)
	}
	if !strings.Contains(ver.Detail, "boom") {
		t.Errorf("version Detail should surface the run error; got %q", ver.Detail)
	}
}

func TestRun_ModelMissing(t *testing.T) {
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg.Model = filepath.Join(t.TempDir(), "no-such-model.bin")
	r := &Runner{
		LookPath:  func(string) (string, error) { return "", errors.New("na") },
		VersionOf: func(string) (string, error) { return "", errors.New("na") },
	}
	got := r.Run(cfg)
	res := resultByName(t, got, "whisper model")
	if res.OK {
		t.Fatalf("model missing must fail; got %+v", res)
	}
	if !strings.Contains(res.Message, cfg.Model) {
		t.Errorf("model failure should echo path; got %q", res.Message)
	}
	if !strings.Contains(res.Message, "docs/research.md") {
		t.Errorf("model failure should reference download instructions; got %q", res.Message)
	}
}

func TestRun_ModelTooSmall(t *testing.T) {
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tiny := filepath.Join(t.TempDir(), "ggml-tiny.bin")
	if err := os.WriteFile(tiny, []byte("truncated"), 0o644); err != nil {
		t.Fatalf("write tiny model: %v", err)
	}
	cfg.Model = tiny
	r := &Runner{
		LookPath:  func(string) (string, error) { return "", errors.New("na") },
		VersionOf: func(string) (string, error) { return "", errors.New("na") },
	}
	got := r.Run(cfg)
	res := resultByName(t, got, "whisper model")
	if res.OK {
		t.Fatalf("< 100MB model must fail; got %+v", res)
	}
	if !strings.Contains(res.Message, "过小") && !strings.Contains(res.Message, "size") {
		t.Errorf("failure message should hint at size problem; got %q", res.Message)
	}
}

func TestRun_ModelSize_ThresholdOverridable(t *testing.T) {
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A tiny 1 KiB file that would fail the default 100 MiB floor but
	// passes when the test lowers the threshold via Runner.MinModelSize.
	tiny := filepath.Join(t.TempDir(), "ggml-test.bin")
	if err := os.WriteFile(tiny, make([]byte, 1024), 0o644); err != nil {
		t.Fatalf("write tiny: %v", err)
	}
	cfg.Model = tiny
	r := &Runner{
		LookPath:     func(string) (string, error) { return "", errors.New("na") },
		VersionOf:    func(string) (string, error) { return "", errors.New("na") },
		MinModelSize: 512, // 512 bytes — tiny file exceeds this.
	}
	got := r.Run(cfg)
	res := resultByName(t, got, "whisper model")
	if !res.OK {
		t.Fatalf("with MinModelSize=512 the 1KiB model must pass; got %+v", res)
	}
}

func TestRun_OutputDirWritable_CreatesIfMissing(t *testing.T) {
	cfg := baseCfg(t)
	// Output dir does NOT exist yet — preflight should MkdirAll it.
	if _, err := os.Stat(cfg.OutputDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("output dir should not exist yet: %v", err)
	}
	r := &Runner{
		LookPath:  func(string) (string, error) { return "", errors.New("na") },
		VersionOf: func(string) (string, error) { return "", errors.New("na") },
	}
	got := r.Run(cfg)
	res := resultByName(t, got, "output dir")
	if !res.OK {
		t.Fatalf("expected preflight to create + accept output dir; got %+v", res)
	}
	if _, err := os.Stat(cfg.OutputDir); err != nil {
		t.Fatalf("output dir should exist after preflight; stat err=%v", err)
	}
}

func TestRun_OutputDirNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod semantics differ on Windows")
	}
	// Root user (containers/CI) can bypass 0555 write protection — skip
	// there to avoid a bogus "test failed" alarm when we're really just
	// running as an all-powerful UID.
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permission checks; skipping writability test")
	}
	cfg := baseCfg(t)
	parent := t.TempDir()
	readOnly := filepath.Join(parent, "ro")
	if err := os.Mkdir(readOnly, 0o555); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	// Restore write bit at cleanup so t.TempDir can rm it.
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) })
	cfg.OutputDir = filepath.Join(readOnly, "nested")

	r := &Runner{
		LookPath:  func(string) (string, error) { return "", errors.New("na") },
		VersionOf: func(string) (string, error) { return "", errors.New("na") },
	}
	got := r.Run(cfg)
	res := resultByName(t, got, "output dir")
	if res.OK {
		t.Fatalf("read-only parent must trip output-dir check; got %+v", res)
	}
	if !strings.Contains(res.Message, cfg.OutputDir) {
		t.Errorf("failure message should echo the offending path; got %q", res.Message)
	}
}

func TestAllOK(t *testing.T) {
	if !AllOK(nil) {
		t.Errorf("empty slice should be trivially OK")
	}
	if !AllOK([]CheckResult{{OK: true}, {OK: true}}) {
		t.Errorf("all true should be OK")
	}
	if AllOK([]CheckResult{{OK: true}, {OK: false}}) {
		t.Errorf("one failure should flip AllOK to false")
	}
}

func TestFormatFailures_EmptyWhenAllPass(t *testing.T) {
	got := FormatFailures([]CheckResult{{Name: "x", OK: true, Message: "yay"}})
	if got != "" {
		t.Errorf("no failures should render empty string; got %q", got)
	}
}

func TestFormatFailures_ListsAllFailures(t *testing.T) {
	rs := []CheckResult{
		{Name: "yt-dlp", OK: false, Message: "缺 yt-dlp; brew install yt-dlp"},
		{Name: "ffmpeg", OK: true, Message: "ffmpeg=6.1.1 (PATH)"},
		{Name: "whisper model", OK: false, Message: "模型文件不存在: /x/y.bin"},
	}
	got := FormatFailures(rs)
	if !strings.Contains(got, "yt-dlp") || !strings.Contains(got, "whisper model") {
		t.Errorf("output should mention both failing check names; got:\n%s", got)
	}
	if strings.Contains(got, "ffmpeg=6.1.1") {
		t.Errorf("output must not include passing results; got:\n%s", got)
	}
	if !strings.Contains(got, "缺 yt-dlp") || !strings.Contains(got, "/x/y.bin") {
		t.Errorf("output should surface Message content for each failure; got:\n%s", got)
	}
}

func TestRun_DebugModeSurfacesResolvedSourcesInMessage(t *testing.T) {
	// This locks the "yt-dlp=<version> (yaml)" / "(PATH)" phrasing
	// that main uses for its Info-level summary log.
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir := t.TempDir()
	yt := filepath.Join(dir, "yt-dlp")
	ff := filepath.Join(dir, "ffmpeg")
	wh := filepath.Join(dir, "whisper-cli")
	makeExecutable(t, yt)
	makeExecutable(t, ff)
	makeExecutable(t, wh)
	cfg.Binaries.Ytdlp = yt // yaml-supplied → expect (yaml)
	r := &Runner{
		LookPath: func(name string) (string, error) {
			// only ffmpeg + whisper-cli fall back to PATH
			return filepath.Join(dir, name), nil
		},
		VersionOf: func(string) (string, error) { return "2025.10.22", nil },
	}
	got := r.Run(cfg)
	yres := resultByName(t, got, "yt-dlp")
	if !strings.Contains(yres.Message, "(yaml)") {
		t.Errorf("yaml-supplied yt-dlp must render '(yaml)'; got %q", yres.Message)
	}
	fres := resultByName(t, got, "ffmpeg")
	if !strings.Contains(fres.Message, "(PATH)") {
		t.Errorf("PATH-resolved ffmpeg must render '(PATH)'; got %q", fres.Message)
	}
}

// TestRunFunctionExists is a tiny smoke test that the package-level
// Run(...) helper delegates to a zero-value Runner. Otherwise callers
// have no way to skip the DI layer.
func TestRun_PackageHelperDelegatesToZeroRunner(t *testing.T) {
	// Passing a nil-ish cfg + real-runtime Runner would touch the
	// developer machine's filesystem. Instead we assert Run(cfg)
	// yields the same shape as (&Runner{}).Run(cfg).
	//
	// We rely on the check names being stable (see TestRun_AllChecksPresentEvenWhenSomeFail).
	tmp := t.TempDir()
	cfg := &config.Config{
		Format:    "txt",
		Model:     filepath.Join(tmp, "no-model.bin"),
		OutputDir: filepath.Join(tmp, "out"),
	}
	got := Run(cfg)
	if len(got) != 6 {
		t.Fatalf("Run should return 6 checks; got %d: %+v", len(got), got)
	}
	// Since no fakes are wired, results depend on the host — we only
	// assert Names/order are the plan §4.2 sequence.
	wantNames := []string{"yt-dlp", "yt-dlp version", "ffmpeg", "whisper-cli", "whisper model", "output dir"}
	for i, r := range got {
		if r.Name != wantNames[i] {
			t.Fatalf("check[%d]=%q, want %q", i, r.Name, wantNames[i])
		}
	}
}

// TestRun_YAMLPointsAtBrokenSymlink_NoFallback covers plan §4.2's
// "typical invalid scenarios: file missing / directory / no +x bit /
// broken symlink" — os.Stat follows symlinks, so a symlink whose
// target no longer exists must be caught by the stat-error branch.
func TestRun_YAMLPointsAtBrokenSymlink_NoFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may need admin on Windows")
	}
	cfg := baseCfg(t)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "yt-dlp")
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg.Binaries.Ytdlp = link
	r := &Runner{
		LookPath: func(name string) (string, error) {
			if name == "yt-dlp" {
				t.Fatalf("must not fall back to PATH for set-but-broken yaml symlink")
			}
			return "/opt/homebrew/bin/" + name, nil
		},
		VersionOf: func(string) (string, error) { return "2025.10.22", nil },
	}
	got := r.Run(cfg)
	res := resultByName(t, got, "yt-dlp")
	if res.OK {
		t.Fatalf("broken symlink must fail; got %+v", res)
	}
	if !strings.Contains(res.Message, link) {
		t.Errorf("failure should echo the offending symlink path; got %q", res.Message)
	}
}

// TestExtractYtdlpVersion_IgnoresStderrNoise guards E-2: the version
// probe must not pick up date-shaped tokens from unrelated stderr
// warnings.
func TestExtractYtdlpVersion_IgnoresStderrNoise(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"single-line", "2025.10.22\n", "2025.10.22"},
		{"prefixed", "yt-dlp version 2025.10.22\n", "2025.10.22"},
		{"noise-then-version", "WARNING: pip 24.0 (2024.03.14) available\nyt-dlp 2025.10.22\n", "2025.10.22"},
		{"only-noise", "WARNING: something 2024.03.14 happened\n", "2024.03.14"},
		{"empty", "", ""},
		{"no-date", "yt-dlp: not a real version\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractYtdlpVersion(tc.raw); got != tc.want {
				t.Errorf("extractYtdlpVersion(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestRun_NilCfg_ReportsInternalError guards E-6: nil cfg is a caller
// bug, but should surface as a structured CheckResult rather than a
// bare nil-panic so main can log-and-exit deterministically.
func TestRun_NilCfg_ReportsInternalError(t *testing.T) {
	got := (&Runner{}).Run(nil)
	if len(got) != 1 {
		t.Fatalf("nil cfg should yield exactly 1 sentinel result; got %+v", got)
	}
	if got[0].OK {
		t.Fatalf("nil-cfg sentinel must not be OK; got %+v", got[0])
	}
	if AllOK(got) {
		t.Fatalf("AllOK must be false for nil-cfg path")
	}
	if !strings.Contains(got[0].Message, "nil") && !strings.Contains(got[0].Message, "config") {
		t.Errorf("sentinel message should identify the nil-config bug; got %q", got[0].Message)
	}
}
