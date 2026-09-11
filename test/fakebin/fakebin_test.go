//go:build !windows

package fakebin_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// projectTempDir returns a fresh temp dir that lives *inside* the repo
// under .tmp/. The system /var/folders/... TMPDIR is off-limits to the
// sandboxed child processes we spawn (bash + cp), so t.TempDir() cannot
// be used here. The directory is registered for cleanup on test end.
func projectTempDir(t *testing.T) string {
	t.Helper()
	base := filepath.Join(repoRoot(t), ".tmp", "fakebin-test")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir base tmp: %v", err)
	}
	dir, err := os.MkdirTemp(base, "run-*")
	if err != nil {
		t.Fatalf("mktemp under repo: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func fakebinDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "testdata", "fakebin")
}

func testdataDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "testdata")
}

func mustExecutable(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if st.IsDir() {
		t.Fatalf("%s is directory, expected file", path)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("%s is not executable (mode=%v); did you chmod +x?", path, st.Mode().Perm())
	}
}

func runScript(t *testing.T, script string, env []string, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	cmd := exec.Command(script, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var so, se strings.Builder
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return so.String(), se.String(), ee.ExitCode()
		}
		t.Fatalf("run %s %v: %v", script, args, err)
	}
	return so.String(), se.String(), 0
}

func TestFakebin_ScriptsExistAndExecutable(t *testing.T) {
	dir := fakebinDir(t)
	for _, name := range []string{"yt-dlp", "ffmpeg", "whisper-cli"} {
		mustExecutable(t, filepath.Join(dir, name))
	}
}

func TestFakebin_SampleFixturesExist(t *testing.T) {
	root := testdataDir(t)
	for _, rel := range []string{
		"metadata.json",
		"metadata-nosub.json",
		"sample.srt",
		"sample.m4a",
		"sample-asr.srt",
	} {
		p := filepath.Join(root, rel)
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("missing sample fixture %s: %v", rel, err)
		}
		if st.Size() == 0 {
			t.Fatalf("sample fixture %s is empty", rel)
		}
	}
}

func TestYtdlp_Version_PrintsRecentDate(t *testing.T) {
	stdout, _, exit := runScript(t, filepath.Join(fakebinDir(t), "yt-dlp"), nil, "--version")
	if exit != 0 {
		t.Fatalf("--version exit=%d", exit)
	}
	got := strings.TrimSpace(stdout)
	if !strings.HasPrefix(got, "20") || len(got) < len("2025.01.01") {
		t.Errorf("version should look like YYYY.MM.DD (>= 2025.01.01), got %q", got)
	}
	if strings.Compare(got, "2025.01.01") < 0 {
		t.Errorf("version %q < preflight minimum 2025.01.01", got)
	}
}

func TestYtdlp_DumpJson_HappyPath_HasSubtitles(t *testing.T) {
	stdout, _, exit := runScript(t, filepath.Join(fakebinDir(t), "yt-dlp"), nil,
		"--no-warnings", "-J", "--skip-download",
		"https://www.bilibili.com/video/BVfake123",
	)
	if exit != 0 {
		t.Fatalf("-J exit=%d stdout=%q", exit, stdout)
	}
	for _, kw := range []string{`"id"`, `"title"`, `"duration"`, `"subtitles"`, `BVfake123`} {
		if !strings.Contains(stdout, kw) {
			t.Errorf("metadata json missing %q; got: %s", kw, stdout)
		}
	}
}

func TestYtdlp_DumpJson_Nosub_WhenBVIDMatchesNosubFixture(t *testing.T) {
	stdout, _, exit := runScript(t, filepath.Join(fakebinDir(t), "yt-dlp"), nil,
		"--no-warnings", "-J", "--skip-download",
		"https://www.bilibili.com/video/BVnosub999",
	)
	if exit != 0 {
		t.Fatalf("-J nosub exit=%d", exit)
	}
	if !strings.Contains(stdout, "BVnosub999") {
		t.Errorf("expected BVnosub999 in metadata, got %s", stdout)
	}
	if !strings.Contains(stdout, `"subtitles": {}`) && !strings.Contains(stdout, `"subtitles":{}`) {
		t.Errorf("nosub metadata should have empty subtitles map, got %s", stdout)
	}
}

func TestYtdlp_DumpJson_FailInjection(t *testing.T) {
	_, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "yt-dlp"),
		[]string{"FAKEBIN_FAIL_YTDLP=metadata"},
		"--no-warnings", "-J", "--skip-download", "https://x/BVfake123",
	)
	if exit == 0 {
		t.Fatalf("FAKEBIN_FAIL_YTDLP=metadata should fail, exit=0")
	}
	if !strings.Contains(strings.ToLower(stderr), "metadata") {
		t.Errorf("stderr should mention metadata failure, got %q", stderr)
	}
}

func TestYtdlp_WriteSubs_CopiesSrtToPaths(t *testing.T) {
	tmp := projectTempDir(t)
	_, _, exit := runScript(t, filepath.Join(fakebinDir(t), "yt-dlp"), nil,
		"--no-warnings",
		"--write-subs", "--write-auto-subs",
		"--sub-langs", "zh-CN,zh-Hans,ai-zh",
		"--sub-format", "srt/best",
		"--convert-subs", "srt",
		"--skip-download",
		"-o", "%(id)s.%(ext)s",
		"--paths", tmp,
		"https://www.bilibili.com/video/BVfake123",
	)
	if exit != 0 {
		t.Fatalf("write-subs exit=%d", exit)
	}
	want := filepath.Join(tmp, "BVfake123.zh-CN.srt")
	st, err := os.Stat(want)
	if err != nil {
		t.Fatalf("expected srt at %s, err=%v; dir contents=%s", want, err, listDir(t, tmp))
	}
	if st.Size() == 0 {
		t.Errorf("copied srt is empty")
	}
}

func TestYtdlp_WriteSubs_FailInjection(t *testing.T) {
	tmp := projectTempDir(t)
	_, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "yt-dlp"),
		[]string{"FAKEBIN_FAIL_YTDLP=subtitle"},
		"--write-subs", "--skip-download",
		"-o", "%(id)s.%(ext)s",
		"--paths", tmp,
		"https://x/BVfake123",
	)
	if exit == 0 {
		t.Fatalf("FAKEBIN_FAIL_YTDLP=subtitle should fail")
	}
	if !strings.Contains(strings.ToLower(stderr), "subtitle") {
		t.Errorf("stderr should mention subtitle failure, got %q", stderr)
	}
}

func TestYtdlp_ExtractAudio_CopiesM4a(t *testing.T) {
	tmp := projectTempDir(t)
	_, _, exit := runScript(t, filepath.Join(fakebinDir(t), "yt-dlp"), nil,
		"--no-warnings",
		"-f", "bestaudio[ext=m4a]/bestaudio",
		"-x", "--audio-format", "m4a", "--audio-quality", "5",
		"-o", "%(id)s.%(ext)s",
		"--paths", tmp,
		"https://www.bilibili.com/video/BVfake123",
	)
	if exit != 0 {
		t.Fatalf("extract audio exit=%d", exit)
	}
	want := filepath.Join(tmp, "BVfake123.m4a")
	st, err := os.Stat(want)
	if err != nil {
		t.Fatalf("expected m4a at %s, err=%v; dir contents=%s", want, err, listDir(t, tmp))
	}
	if st.Size() == 0 {
		t.Errorf("copied m4a is empty")
	}
}

func TestYtdlp_ExtractAudio_FailInjection(t *testing.T) {
	tmp := projectTempDir(t)
	_, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "yt-dlp"),
		[]string{"FAKEBIN_FAIL_YTDLP=audio"},
		"-x", "--audio-format", "m4a",
		"-o", "%(id)s.%(ext)s",
		"--paths", tmp,
		"https://x/BVfake123",
	)
	if exit == 0 {
		t.Fatalf("FAKEBIN_FAIL_YTDLP=audio should fail")
	}
	if !strings.Contains(strings.ToLower(stderr), "audio") {
		t.Errorf("stderr should mention audio failure, got %q", stderr)
	}
}

func TestYtdlp_UnknownMode_ReturnsError(t *testing.T) {
	_, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "yt-dlp"), nil,
		"--no-such-flag-only",
	)
	if exit == 0 {
		t.Fatalf("unknown-only invocation should fail")
	}
	if stderr == "" {
		t.Errorf("expected non-empty stderr on unknown mode")
	}
}

func TestFfmpeg_Version_Prints(t *testing.T) {
	stdout, _, exit := runScript(t, filepath.Join(fakebinDir(t), "ffmpeg"), nil, "-version")
	if exit != 0 {
		t.Fatalf("-version exit=%d", exit)
	}
	if !strings.Contains(strings.ToLower(stdout), "ffmpeg") {
		t.Errorf("version output should mention ffmpeg, got %q", stdout)
	}
}

func TestFfmpeg_Transcode_TouchesOutputWav(t *testing.T) {
	tmp := projectTempDir(t)
	in := filepath.Join(tmp, "in.m4a")
	if err := os.WriteFile(in, []byte("fake m4a payload"), 0o644); err != nil {
		t.Fatalf("prep input: %v", err)
	}
	out := filepath.Join(tmp, "out.wav")
	_, _, exit := runScript(t, filepath.Join(fakebinDir(t), "ffmpeg"), nil,
		"-y", "-i", in, "-vn", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", out,
	)
	if exit != 0 {
		t.Fatalf("transcode exit=%d", exit)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("expected wav at %s, err=%v", out, err)
	}
}

func TestFfmpeg_FailInjection(t *testing.T) {
	tmp := projectTempDir(t)
	in := filepath.Join(tmp, "in.m4a")
	_ = os.WriteFile(in, []byte("x"), 0o644)
	out := filepath.Join(tmp, "out.wav")
	_, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "ffmpeg"),
		[]string{"FAKEBIN_FAIL_FFMPEG=1"},
		"-y", "-i", in, "-vn", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", out,
	)
	if exit == 0 {
		t.Fatalf("FAKEBIN_FAIL_FFMPEG=1 should fail")
	}
	if !strings.Contains(strings.ToLower(stderr), "ffmpeg") {
		t.Errorf("stderr should mention ffmpeg failure, got %q", stderr)
	}
	if _, err := os.Stat(out); err == nil {
		t.Errorf("failed run must not produce output wav")
	}
}

func TestFfmpeg_MissingRequiredFlags(t *testing.T) {
	_, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "ffmpeg"), nil,
		"-i", "nothing.m4a", "out.wav",
	)
	if exit == 0 {
		t.Fatalf("missing -ar/-ac should fail")
	}
	if stderr == "" {
		t.Errorf("expected non-empty stderr")
	}
}

func TestWhisperCli_Version_Prints(t *testing.T) {
	stdout, _, exit := runScript(t, filepath.Join(fakebinDir(t), "whisper-cli"), nil, "--version")
	if exit != 0 {
		t.Fatalf("--version exit=%d", exit)
	}
	if !strings.Contains(strings.ToLower(stdout), "whisper") {
		t.Errorf("version output should mention whisper, got %q", stdout)
	}
}

func TestWhisperCli_Transcribe_WritesSrtAtOfPrefix(t *testing.T) {
	tmp := projectTempDir(t)
	wav := filepath.Join(tmp, "in.wav")
	if err := os.WriteFile(wav, []byte("fake wav"), 0o644); err != nil {
		t.Fatalf("prep wav: %v", err)
	}
	prefix := filepath.Join(tmp, "result")
	stdout, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "whisper-cli"), nil,
		"-m", "/nonexistent/model.bin",
		"-f", wav,
		"-l", "zh",
		"-osrt",
		"-of", prefix,
		"--threads", "4",
		"--print-progress",
	)
	if exit != 0 {
		t.Fatalf("transcribe exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	want := prefix + ".srt"
	st, err := os.Stat(want)
	if err != nil {
		t.Fatalf("expected srt at %s, err=%v", want, err)
	}
	if st.Size() == 0 {
		t.Errorf("srt is empty")
	}
	if !strings.Contains(stdout, "progress") && !strings.Contains(stderr, "progress") {
		t.Errorf("--print-progress should produce a progress line")
	}
}

func TestWhisperCli_FailInjection(t *testing.T) {
	tmp := projectTempDir(t)
	wav := filepath.Join(tmp, "in.wav")
	_ = os.WriteFile(wav, []byte("x"), 0o644)
	prefix := filepath.Join(tmp, "result")
	_, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "whisper-cli"),
		[]string{"FAKEBIN_FAIL_WHISPER=1"},
		"-m", "/x/model.bin", "-f", wav, "-osrt", "-of", prefix,
	)
	if exit == 0 {
		t.Fatalf("FAKEBIN_FAIL_WHISPER=1 should fail")
	}
	if !strings.Contains(strings.ToLower(stderr), "whisper") {
		t.Errorf("stderr should mention whisper failure, got %q", stderr)
	}
	if _, err := os.Stat(prefix + ".srt"); err == nil {
		t.Errorf("failed run must not produce srt")
	}
}

func TestWhisperCli_MissingRequiredFlags(t *testing.T) {
	_, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "whisper-cli"), nil,
		"-m", "model.bin",
	)
	if exit == 0 {
		t.Fatalf("missing -f/-of should fail")
	}
	if stderr == "" {
		t.Errorf("expected non-empty stderr")
	}
}

// TestYtdlp_MetadataMode_WithWriteSubsFlags reproduces plan §5.1: the real
// metadata invocation combines --dump-single-json with --write-subs on the
// same command. Regardless of flag order, metadata must win over subtitle.
func TestYtdlp_MetadataMode_WithWriteSubsFlags(t *testing.T) {
	cases := [][]string{
		{
			"--no-warnings",
			"--dump-single-json",
			"--write-subs", "--write-auto-subs",
			"--sub-langs", "zh-CN,zh-Hans,ai-zh",
			"--skip-download",
			"https://www.bilibili.com/video/BVfake123",
		},
		{
			"--no-warnings",
			"--write-subs", "--write-auto-subs",
			"--sub-langs", "zh-CN,zh-Hans,ai-zh",
			"--dump-single-json",
			"--skip-download",
			"https://www.bilibili.com/video/BVfake123",
		},
	}
	for i, args := range cases {
		stdout, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "yt-dlp"), nil, args...)
		if exit != 0 {
			t.Fatalf("case %d: exit=%d stderr=%q", i, exit, stderr)
		}
		if !strings.Contains(stdout, `"id"`) || !strings.Contains(stdout, "BVfake123") {
			t.Errorf("case %d: expected metadata JSON regardless of flag order, got: %s", i, stdout)
		}
	}
}

// TestYtdlp_FailInjection_InvalidValue guards the strict validation of the
// FAKEBIN_FAIL_YTDLP env var — unknown values must not silently pass.
func TestYtdlp_FailInjection_InvalidValue(t *testing.T) {
	_, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "yt-dlp"),
		[]string{"FAKEBIN_FAIL_YTDLP=bogus"},
		"--version",
	)
	if exit == 0 {
		t.Fatalf("unknown FAKEBIN_FAIL_YTDLP value should fail")
	}
	if !strings.Contains(strings.ToLower(stderr), "fakebin_fail_ytdlp") {
		t.Errorf("stderr should mention the offending env var, got %q", stderr)
	}
}

func TestFfmpeg_MissingArOnly(t *testing.T) {
	_, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "ffmpeg"), nil,
		"-y", "-i", "nothing.m4a", "-vn", "-ac", "1", "-c:a", "pcm_s16le", "out.wav",
	)
	if exit == 0 {
		t.Fatalf("missing -ar alone should still fail")
	}
	if stderr == "" {
		t.Errorf("expected non-empty stderr on missing -ar")
	}
}

func TestFfmpeg_MissingAcOnly(t *testing.T) {
	_, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "ffmpeg"), nil,
		"-y", "-i", "nothing.m4a", "-vn", "-ar", "16000", "-c:a", "pcm_s16le", "out.wav",
	)
	if exit == 0 {
		t.Fatalf("missing -ac alone should still fail")
	}
	if stderr == "" {
		t.Errorf("expected non-empty stderr on missing -ac")
	}
}

func TestWhisperCli_MissingOsrtFlag(t *testing.T) {
	tmp := projectTempDir(t)
	wav := filepath.Join(tmp, "in.wav")
	if err := os.WriteFile(wav, []byte("x"), 0o644); err != nil {
		t.Fatalf("prep wav: %v", err)
	}
	prefix := filepath.Join(tmp, "out")
	_, stderr, exit := runScript(t, filepath.Join(fakebinDir(t), "whisper-cli"), nil,
		"-m", "model.bin", "-f", wav, "-of", prefix,
	)
	if exit == 0 {
		t.Fatalf("missing -osrt should fail even when -f/-of present")
	}
	if stderr == "" {
		t.Errorf("expected non-empty stderr on missing -osrt")
	}
}

func listDir(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "<readdir err: " + err.Error() + ">"
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.Name())
		b.WriteString(" ")
	}
	return b.String()
}
