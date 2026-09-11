package asr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeShim drops a bash script at path whose sole job is to (1) dump
// its own argv one-per-line into <argsFile>, (2) emit the fixed
// strings on stdout/stderr, (3) synthesise a stub `<prefix>.srt` at
// the path passed via `-of <prefix>` when exiting 0 (unless
// skipSrtWrite is true, used to exercise the post-exit "srt missing"
// guard), and (4) exit with <exitCode>. This lets whisper_test.go
// verify [WhisperRecognizer]'s argv assembly against plan §5.5 and
// its post-exit stat guard against contracts.md §二.3 without
// shelling out to a real whisper-cli binary.
//
// The script is bash 3.2 compatible (matches fakebin/) and uses
// printf, so it stays deterministic across macOS default shells.
func writeShim(t *testing.T, path, argsFile, stdoutStr, stderrStr string, exitCode int) {
	t.Helper()
	writeShimEx(t, path, argsFile, stdoutStr, stderrStr, exitCode, false)
}

// writeShimEx is writeShim with an explicit skipSrtWrite knob for the
// narrow tests that need to simulate "exit 0 but no srt produced".
func writeShimEx(t *testing.T, path, argsFile, stdoutStr, stderrStr string, exitCode int, skipSrtWrite bool) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell shim requires POSIX bash; whisper.go is macOS/Linux-only in scope")
	}
	srtBlock := `
of=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-of" ]; then
    of="$a"
  fi
  prev="$a"
done
if [ -n "$of" ]; then
  printf '1\n00:00:00,000 --> 00:00:01,000\nshim\n' > "${of}.srt"
fi
`
	if skipSrtWrite {
		srtBlock = "\n"
	}
	body := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
: > %q
for a in "$@"; do
  printf '%%s\n' "$a" >> %q
done
if [ -n %q ]; then
  printf '%%s' %q
fi
if [ -n %q ]; then
  printf '%%s' %q >&2
fi%sexit %d
`, argsFile, argsFile,
		stdoutStr, stdoutStr,
		stderrStr, stderrStr,
		srtBlock,
		exitCode)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write shim %s: %v", path, err)
	}
}

// readArgs slurps the arg-dump file the shim wrote and splits on
// newlines. Empty file ⇒ nil slice (matches shim behaviour when no
// argv was seen). Trailing empty entry from the final newline is
// trimmed.
func readArgs(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// ============================================================
// FakeRecognizer tests
// ============================================================

func TestFakeRecognizer_ZeroValue_WritesDefaultSRT(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "out")
	f := &FakeRecognizer{}

	got, err := f.Transcribe(context.Background(), "in.wav", "model.bin", prefix)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	want := prefix + ".srt"
	if got != want {
		t.Errorf("srtPath = %q, want %q", got, want)
	}
	b, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(b) != defaultFakeSRT {
		t.Errorf("default payload mismatch\n got:  %q\n want: %q", string(b), defaultFakeSRT)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("Calls length = %d, want 1", len(f.Calls))
	}
	if f.Calls[0] != (FakeCall{Wav: "in.wav", Model: "model.bin", Prefix: prefix}) {
		t.Errorf("Calls[0] = %+v", f.Calls[0])
	}
}

func TestFakeRecognizer_PayloadOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "out")
	f := &FakeRecognizer{Payload: []byte("1\n00:00:00,000 --> 00:00:01,000\ncustom\n")}

	if _, err := f.Transcribe(context.Background(), "in.wav", "m.bin", prefix); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	b, err := os.ReadFile(prefix + ".srt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), "custom") {
		t.Errorf("expected custom payload to win; got %q", string(b))
	}
}

func TestFakeRecognizer_FixturePath_ReadsSampleAsr(t *testing.T) {
	// Locate testdata/sample-asr.srt via a relative walk from this
	// test file's directory. Using runtime.Caller lets us stay
	// robust against `go test` from either repo root or the package
	// directory (both are common).
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	fixture := filepath.Join(repoRoot, "testdata", "sample-asr.srt")
	if _, err := os.Stat(fixture); err != nil {
		t.Skipf("sample-asr.srt not present at %s: %v", fixture, err)
	}

	dir := t.TempDir()
	prefix := filepath.Join(dir, "out")
	f := &FakeRecognizer{FixturePath: fixture}

	if _, err := f.Transcribe(context.Background(), "in.wav", "m.bin", prefix); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	got, err := os.ReadFile(prefix + ".srt")
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("fixture copy mismatch\n got:  %q\n want: %q", string(got), string(want))
	}
}

func TestFakeRecognizer_PayloadBeatsFixturePath(t *testing.T) {
	// Precedence: Payload > FixturePath > defaultFakeSRT.
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "fixture.srt")
	if err := os.WriteFile(fixturePath, []byte("FIXTURE"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	prefix := filepath.Join(dir, "out")
	f := &FakeRecognizer{
		Payload:     []byte("PAYLOAD"),
		FixturePath: fixturePath,
	}
	if _, err := f.Transcribe(context.Background(), "in.wav", "m.bin", prefix); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	b, _ := os.ReadFile(prefix + ".srt")
	if string(b) != "PAYLOAD" {
		t.Errorf("Payload should win over FixturePath; got %q", string(b))
	}
}

func TestFakeRecognizer_FixturePath_Missing_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "out")
	f := &FakeRecognizer{FixturePath: filepath.Join(dir, "does-not-exist.srt")}
	got, err := f.Transcribe(context.Background(), "in.wav", "m.bin", prefix)
	if err == nil {
		t.Fatalf("expected error when fixture is missing; got srtPath=%q", got)
	}
	if got != "" {
		t.Errorf("srtPath must be empty on error, got %q", got)
	}
	if !strings.Contains(err.Error(), "asr: fake:") {
		t.Errorf("error should carry asr: fake: envelope, got %v", err)
	}
}

func TestFakeRecognizer_ErrInjected_ReturnsErrAndNoFile(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "out")
	sentinel := errors.New("simulated whisper failure")
	f := &FakeRecognizer{Err: sentinel}

	got, err := f.Transcribe(context.Background(), "in.wav", "m.bin", prefix)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected injected sentinel; got %v", err)
	}
	if got != "" {
		t.Errorf("srtPath must be empty on injected error; got %q", got)
	}
	if _, statErr := os.Stat(prefix + ".srt"); statErr == nil {
		t.Errorf("injected error must not leave an .srt file behind")
	}
	if len(f.Calls) != 1 {
		t.Fatalf("Calls should record invocation even on error; got %d", len(f.Calls))
	}
}

func TestFakeRecognizer_CtxCancelledBeforeCall_ReturnsCtxErr(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "out")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f := &FakeRecognizer{}
	got, err := f.Transcribe(ctx, "in.wav", "m.bin", prefix)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled; got %v", err)
	}
	if got != "" {
		t.Errorf("srtPath must be empty on ctx cancel; got %q", got)
	}
	if _, statErr := os.Stat(prefix + ".srt"); statErr == nil {
		t.Errorf("cancelled ctx must not create output file")
	}
	if len(f.Calls) != 0 {
		t.Errorf("Calls must not record cancelled invocation; got %d", len(f.Calls))
	}
}

func TestFakeRecognizer_EmptyInputs_Rejected(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "out")
	cases := []struct {
		name             string
		wav, model, pref string
		wantIn           string
	}{
		{"empty wav", "", "m.bin", prefix, "wav"},
		{"whitespace wav", "   \t\n", "m.bin", prefix, "wav"},
		{"empty model", "in.wav", "", prefix, "model"},
		{"whitespace model", "in.wav", "  ", prefix, "model"},
		{"empty prefix", "in.wav", "m.bin", "", "prefix"},
		{"whitespace prefix", "in.wav", "m.bin", "   ", "prefix"},
		{"srt-suffixed prefix", "in.wav", "m.bin", filepath.Join(dir, "out.srt"), ".srt"},
		{"srt-suffixed prefix upper", "in.wav", "m.bin", filepath.Join(dir, "OUT.SRT"), ".srt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &FakeRecognizer{}
			got, err := f.Transcribe(context.Background(), tc.wav, tc.model, tc.pref)
			if err == nil {
				t.Fatalf("expected validation error; got srtPath=%q", got)
			}
			if got != "" {
				t.Errorf("srtPath must be empty; got %q", got)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error should mention %q; got %v", tc.wantIn, err)
			}
			if len(f.Calls) != 0 {
				t.Errorf("validation failure must not record a call")
			}
		})
	}
}

func TestFakeRecognizer_ParentDirMissing_SurfacesENOENT(t *testing.T) {
	// Contract: parent dir MUST exist; fake surfaces the underlying
	// ENOENT untouched (well, wrapped in "asr: fake: write ...").
	dir := t.TempDir()
	prefix := filepath.Join(dir, "no-such-subdir", "out")
	f := &FakeRecognizer{}
	got, err := f.Transcribe(context.Background(), "in.wav", "m.bin", prefix)
	if err == nil {
		t.Fatalf("expected ENOENT-flavoured error; got srtPath=%q", got)
	}
	if got != "" {
		t.Errorf("srtPath must be empty on write error; got %q", got)
	}
	if !strings.Contains(err.Error(), "asr: fake:") {
		t.Errorf("error should carry asr: fake: envelope; got %v", err)
	}
}

func TestFakeRecognizer_SatisfiesInterface(t *testing.T) {
	var _ Recognizer = (*FakeRecognizer)(nil)
}

// ============================================================
// WhisperRecognizer tests (argv-shim based, no real whisper-cli)
// ============================================================

func TestWhisperRecognizer_SatisfiesInterface(t *testing.T) {
	var _ Recognizer = (*WhisperRecognizer)(nil)
}

// TestWhisperRecognizer_Argv_MatchesPlanSection5_5 pins the exact argv
// sequence plan §5.5 mandates. Any drift in flag order, omission, or
// value transformation flips this test red — including the
// implicit-default cases (Language, Threads, --print-progress).
func TestWhisperRecognizer_Argv_MatchesPlanSection5_5(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "whisper-cli-shim.sh")
	argsFile := filepath.Join(dir, "args.txt")
	writeShim(t, shim, argsFile, "", "", 0)

	prefix := filepath.Join(dir, "run", "result")
	if err := os.MkdirAll(filepath.Dir(prefix), 0o755); err != nil {
		t.Fatalf("mkdir prefix parent: %v", err)
	}

	w := &WhisperRecognizer{Binary: shim, Threads: 4} // Language defaults to zh
	got, err := w.Transcribe(context.Background(), "/tmp/in.wav", "/tmp/model.bin", prefix)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	// srtPath is normalised to abs; since prefix was already abs the
	// value equals prefix+".srt" byte-for-byte on POSIX.
	wantSrt, _ := filepath.Abs(prefix + ".srt")
	if got != wantSrt {
		t.Errorf("srtPath = %q, want %q", got, wantSrt)
	}

	want := []string{
		"-m", "/tmp/model.bin",
		"-f", "/tmp/in.wav",
		"-l", "zh",
		"-osrt",
		"-of", prefix,
		"--threads", "4",
		"--print-progress",
	}
	gotArgs := readArgs(t, argsFile)
	if !equalStringSlices(gotArgs, want) {
		t.Errorf("argv mismatch\n got:  %v\n want: %v", gotArgs, want)
	}
}

func TestWhisperRecognizer_ThreadsDefault_UsesRuntimeNumCPU(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "w.sh")
	argsFile := filepath.Join(dir, "args.txt")
	writeShim(t, shim, argsFile, "", "", 0)

	w := &WhisperRecognizer{Binary: shim} // Threads=0 ⇒ NumCPU()
	if _, err := w.Transcribe(context.Background(), "/tmp/in.wav", "/tmp/m.bin", filepath.Join(dir, "p")); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	args := readArgs(t, argsFile)
	// Locate --threads and inspect the value.
	found := false
	for i, a := range args {
		if a == "--threads" && i+1 < len(args) {
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				t.Fatalf("--threads value not int: %q", args[i+1])
			}
			if n != runtime.NumCPU() {
				t.Errorf("--threads = %d, want runtime.NumCPU() = %d", n, runtime.NumCPU())
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("--threads flag missing from argv %v", args)
	}
}

func TestWhisperRecognizer_LanguageOverride_PropagatesToArgv(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "w.sh")
	argsFile := filepath.Join(dir, "args.txt")
	writeShim(t, shim, argsFile, "", "", 0)

	w := &WhisperRecognizer{Binary: shim, Language: "en"}
	if _, err := w.Transcribe(context.Background(), "/tmp/in.wav", "/tmp/m.bin", filepath.Join(dir, "p")); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	args := readArgs(t, argsFile)
	found := false
	for i, a := range args {
		if a == "-l" && i+1 < len(args) && args[i+1] == "en" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("-l en not found in argv: %v", args)
	}
}

func TestWhisperRecognizer_EmptyInputs_RejectedSynchronously(t *testing.T) {
	// The shim MUST NOT run — validation errors happen before exec.
	dir := t.TempDir()
	shim := filepath.Join(dir, "w.sh")
	argsFile := filepath.Join(dir, "args.txt")
	writeShim(t, shim, argsFile, "", "", 0)

	cases := []struct {
		name             string
		wav, model, pref string
		wantSubstr       string
	}{
		{"empty wav", "", "/tmp/m.bin", "/tmp/p", "wav"},
		{"whitespace wav", "  ", "/tmp/m.bin", "/tmp/p", "wav"},
		{"empty model", "/tmp/in.wav", "", "/tmp/p", "model"},
		{"whitespace model", "/tmp/in.wav", "\t", "/tmp/p", "model"},
		{"empty prefix", "/tmp/in.wav", "/tmp/m.bin", "", "prefix"},
		{"whitespace prefix", "/tmp/in.wav", "/tmp/m.bin", "  ", "prefix"},
		{"srt-suffixed prefix", "/tmp/in.wav", "/tmp/m.bin", "/tmp/p.srt", ".srt"},
		{"srt-suffixed prefix upper", "/tmp/in.wav", "/tmp/m.bin", "/tmp/P.SRT", ".srt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &WhisperRecognizer{Binary: shim}
			got, err := w.Transcribe(context.Background(), tc.wav, tc.model, tc.pref)
			if err == nil {
				t.Fatalf("expected validation error; got srtPath=%q", got)
			}
			if got != "" {
				t.Errorf("srtPath must be empty; got %q", got)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error should mention %q; got %v", tc.wantSubstr, err)
			}
		})
	}
	// Sanity: shim should not have been invoked (arg file stays empty).
	if b, _ := os.ReadFile(argsFile); len(b) != 0 {
		t.Errorf("shim ran despite validation error; argsFile contents: %q", string(b))
	}
}

// TestWhisperRecognizer_CtxCancelledBeforeCall_SkipsExec pins the
// contract clause "If ctx is already cancelled at call time the
// implementation returns immediately without any side-effect". The
// shim MUST NOT run — validation happens before exec.CommandContext.
func TestWhisperRecognizer_CtxCancelledBeforeCall_SkipsExec(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "w.sh")
	argsFile := filepath.Join(dir, "args.txt")
	writeShim(t, shim, argsFile, "", "", 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w := &WhisperRecognizer{Binary: shim}
	got, err := w.Transcribe(ctx, "/tmp/in.wav", "/tmp/m.bin", filepath.Join(dir, "p"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled; got %v", err)
	}
	if got != "" {
		t.Errorf("srtPath must be empty on ctx cancel; got %q", got)
	}
	if b, _ := os.ReadFile(argsFile); len(b) != 0 {
		t.Errorf("shim ran despite pre-cancelled ctx; argsFile contents: %q", string(b))
	}
}

func TestWhisperRecognizer_NonZeroExit_ReturnsWrappedError(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "w.sh")
	argsFile := filepath.Join(dir, "args.txt")
	writeShim(t, shim, argsFile, "", "boom: model failed\n", 1)

	w := &WhisperRecognizer{Binary: shim}
	got, err := w.Transcribe(context.Background(), "/tmp/in.wav", "/tmp/m.bin", filepath.Join(dir, "p"))
	if err == nil {
		t.Fatalf("expected non-zero exit error; got srtPath=%q", got)
	}
	if got != "" {
		t.Errorf("srtPath must be empty on non-zero exit; got %q", got)
	}
	if !strings.Contains(err.Error(), "asr: whisper") {
		t.Errorf("error should carry asr: whisper envelope; got %v", err)
	}
	if !strings.Contains(err.Error(), "/tmp/in.wav") || !strings.Contains(err.Error(), ".srt") {
		t.Errorf("error should mention in/out paths for debuggability; got %v", err)
	}
}

func TestWhisperRecognizer_BinaryMissing_ReturnsWrappedError(t *testing.T) {
	dir := t.TempDir()
	w := &WhisperRecognizer{Binary: filepath.Join(dir, "no-such-shim")}
	got, err := w.Transcribe(context.Background(), "/tmp/in.wav", "/tmp/m.bin", filepath.Join(dir, "p"))
	if err == nil {
		t.Fatalf("expected spawn error; got srtPath=%q", got)
	}
	if got != "" {
		t.Errorf("srtPath must be empty; got %q", got)
	}
	if !strings.Contains(err.Error(), "asr: whisper") {
		t.Errorf("error should carry asr: whisper envelope; got %v", err)
	}
}

func TestWhisperRecognizer_CtxCancelled_ReturnsCtxErr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash sleep shim is POSIX-only")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "slow.sh")
	// Custom shim: `exec sleep` lets bash *replace* its own process
	// image with sleep, so exec.CommandContext's SIGKILL lands on
	// sleep directly. Without `exec`, bash would fork sleep and then
	// wait on it — SIGKILL would only reap bash while sleep kept
	// running, causing Transcribe to block on Wait() for the full
	// 5s despite ctx being cancelled.
	body := "#!/usr/bin/env bash\nexec sleep 5\n"
	if err := os.WriteFile(shim, []byte(body), 0o755); err != nil {
		t.Fatalf("write slow shim: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	w := &WhisperRecognizer{Binary: shim}
	start := time.Now()
	got, err := w.Transcribe(ctx, "/tmp/in.wav", "/tmp/m.bin", filepath.Join(dir, "p"))
	dur := time.Since(start)
	if err == nil {
		t.Fatalf("expected ctx error; got srtPath=%q", got)
	}
	if got != "" {
		t.Errorf("srtPath must be empty on cancel; got %q", got)
	}
	// Should surface via ctx.Err() wrapping so callers can use errors.Is.
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap ctx.Err(); got %v", err)
	}
	// Must have been killed shortly after timeout — well under sleep 5.
	if dur > 3*time.Second {
		t.Errorf("Transcribe took %v; expected sub-3s kill on ctx cancel", dur)
	}
}

func TestWhisperRecognizer_StderrSink_ReceivesChildStderr(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "w.sh")
	argsFile := filepath.Join(dir, "args.txt")
	writeShim(t, shim, argsFile, "", "hello-stderr\n", 0)

	var sink strings.Builder
	w := &WhisperRecognizer{Binary: shim, Stderr: &sink}
	if _, err := w.Transcribe(context.Background(), "/tmp/in.wav", "/tmp/m.bin", filepath.Join(dir, "p")); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if !strings.Contains(sink.String(), "hello-stderr") {
		t.Errorf("stderr sink did not receive child stderr; got %q", sink.String())
	}
}

func TestWhisperRecognizer_StdoutSink_ReceivesChildStdout(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "w.sh")
	argsFile := filepath.Join(dir, "args.txt")
	writeShim(t, shim, argsFile, "progress = 25%\n", "", 0)

	var sink strings.Builder
	w := &WhisperRecognizer{Binary: shim, Stdout: &sink}
	if _, err := w.Transcribe(context.Background(), "/tmp/in.wav", "/tmp/m.bin", filepath.Join(dir, "p")); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if !strings.Contains(sink.String(), "progress") {
		t.Errorf("stdout sink did not receive child stdout; got %q", sink.String())
	}
}

// TestWhisperRecognizer_NilStderrDefault_DiscardsChildOutput guards
// that a nil Stderr/Stdout does not blow up on child processes that
// scream loudly — the defaults must swallow the bytes cleanly.
func TestWhisperRecognizer_NilStderrDefault_DiscardsChildOutput(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "w.sh")
	argsFile := filepath.Join(dir, "args.txt")
	writeShim(t, shim, argsFile, "chatty stdout\n", "chatty stderr\n", 0)

	w := &WhisperRecognizer{Binary: shim} // Stdout=Stderr=nil ⇒ io.Discard
	if _, err := w.Transcribe(context.Background(), "/tmp/in.wav", "/tmp/m.bin", filepath.Join(dir, "p")); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	// If we reach here without panic and without hanging, the io.Discard
	// wiring worked. Nothing else to assert — the sinks are unobservable
	// by design.
	_ = io.Discard
}

// TestWhisperRecognizer_ExitZeroButNoSrt_ReturnsError pins the
// post-exit stat guard: contracts.md §二.3 promises srtPath exists
// on disk after a successful call, so a zero-exit tool that skipped
// the write must fail loudly at the ASR boundary rather than deferring
// the confusion to subtitle.Parse.
func TestWhisperRecognizer_ExitZeroButNoSrt_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "w.sh")
	argsFile := filepath.Join(dir, "args.txt")
	writeShimEx(t, shim, argsFile, "", "", 0, true /* skipSrtWrite */)

	prefix := filepath.Join(dir, "p")
	w := &WhisperRecognizer{Binary: shim}
	got, err := w.Transcribe(context.Background(), "/tmp/in.wav", "/tmp/m.bin", prefix)
	if err == nil {
		t.Fatalf("expected error when srt not produced; got srtPath=%q", got)
	}
	if got != "" {
		t.Errorf("srtPath must be empty when srt missing; got %q", got)
	}
	if !strings.Contains(err.Error(), "exit 0 but") || !strings.Contains(err.Error(), "missing") {
		t.Errorf("error should mention exit-0-but-missing; got %v", err)
	}
	if _, statErr := os.Stat(prefix + ".srt"); statErr == nil {
		t.Fatalf("shim leaked a stub srt despite skipSrtWrite")
	}
}

// TestWhisperRecognizer_ReturnsAbsSrtPath_WhenPrefixRelative pins the
// contract clause "srtPath is normalised to an absolute path via
// filepath.Abs". Passing a CWD-relative prefix must still yield an
// absolute return value.
func TestWhisperRecognizer_ReturnsAbsSrtPath_WhenPrefixRelative(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "w.sh")
	argsFile := filepath.Join(dir, "args.txt")
	writeShim(t, shim, argsFile, "", "", 0)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir tmp: %v", err)
	}

	w := &WhisperRecognizer{Binary: shim}
	got, err := w.Transcribe(context.Background(), "/tmp/in.wav", "/tmp/m.bin", "relative-p")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("srtPath must be absolute; got %q", got)
	}
	if !strings.HasSuffix(got, string(filepath.Separator)+"relative-p.srt") {
		t.Errorf("srtPath basename mismatch; got %q", got)
	}
}

// TestFakeRecognizer_ReturnsAbsSrtPath_WhenPrefixRelative mirrors the
// whisper-side check so pipeline code that relies on the abs-path
// invariant works identically against both implementations.
func TestFakeRecognizer_ReturnsAbsSrtPath_WhenPrefixRelative(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir tmp: %v", err)
	}
	f := &FakeRecognizer{}
	got, err := f.Transcribe(context.Background(), "in.wav", "m.bin", "rel-p")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("srtPath must be absolute; got %q", got)
	}
	if !strings.HasSuffix(got, string(filepath.Separator)+"rel-p.srt") {
		t.Errorf("srtPath basename mismatch; got %q", got)
	}
}

// TestFakeRecognizer_Idempotent_OverwritesExisting pins the contract's
// idempotency clause: repeated calls with the same wav/model/prefix
// produce the current payload's output, overwriting whatever was there.
func TestFakeRecognizer_Idempotent_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "out")
	f := &FakeRecognizer{Payload: []byte("PAYLOAD_A")}
	if _, err := f.Transcribe(context.Background(), "in.wav", "m.bin", prefix); err != nil {
		t.Fatalf("first Transcribe: %v", err)
	}
	f.Payload = []byte("PAYLOAD_B")
	if _, err := f.Transcribe(context.Background(), "in.wav", "m.bin", prefix); err != nil {
		t.Fatalf("second Transcribe: %v", err)
	}
	b, err := os.ReadFile(prefix + ".srt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "PAYLOAD_B" {
		t.Errorf("expected second payload to overwrite; got %q", string(b))
	}
	if len(f.Calls) != 2 {
		t.Errorf("expected 2 recorded calls; got %d", len(f.Calls))
	}
}

// TestFakeRecognizer_EmptyPayload_FallsThroughToDefault documents the
// precedence quirk: len(Payload)==0 (non-nil empty slice) is treated
// as "no payload set" and falls through to FixturePath / default, so
// tests can't accidentally emit an empty srt and confuse downstream
// subtitle.ErrEmpty diagnostics with a genuine parse failure.
func TestFakeRecognizer_EmptyPayload_FallsThroughToDefault(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "out")
	f := &FakeRecognizer{Payload: []byte{}}
	if _, err := f.Transcribe(context.Background(), "in.wav", "m.bin", prefix); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	b, err := os.ReadFile(prefix + ".srt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != defaultFakeSRT {
		t.Errorf("empty Payload should fall through to defaultFakeSRT; got %q", string(b))
	}
}

// TestFakeRecognizer_DefaultPayload_MatchesTestdataSampleAsr is a
// drift-guard: [defaultFakeSRT] is documented to mirror the on-disk
// fixture [testdata/sample-asr.srt] so pipeline callers can assert
// against either source interchangeably. If someone edits the fixture
// without syncing the embedded constant (or vice versa) this test
// flips red instead of silently diverging in downstream expectations.
func TestFakeRecognizer_DefaultPayload_MatchesTestdataSampleAsr(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	fixture := filepath.Join(repoRoot, "testdata", "sample-asr.srt")
	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	if string(want) != defaultFakeSRT {
		t.Errorf("defaultFakeSRT drift from %s\n embedded: %q\n fixture:  %q", fixture, defaultFakeSRT, string(want))
	}
}

// TestWhisperRecognizer_TrimsWhitespaceInputs pins that the input
// paths / prefix are TrimSpace-normalised *before* validation and
// argv assembly. Without normalisation, a caller passing "foo   "
// would produce a real `foo   .srt` file with a trailing-space
// basename — pathological but survivable — and worse, the same
// whitespace-only value would pass the empty check via a stale
// pre-trim reference. This test locks both invariants at once.
func TestWhisperRecognizer_TrimsWhitespaceInputs(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "w.sh")
	argsFile := filepath.Join(dir, "args.txt")
	writeShim(t, shim, argsFile, "", "", 0)

	prefix := filepath.Join(dir, "p")
	w := &WhisperRecognizer{Binary: shim}
	got, err := w.Transcribe(context.Background(),
		"  /tmp/in.wav\t", "\t/tmp/m.bin ", prefix+"  ")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	wantSrt, _ := filepath.Abs(prefix + ".srt")
	if got != wantSrt {
		t.Errorf("srtPath = %q, want %q (trailing whitespace must be trimmed)", got, wantSrt)
	}
	args := readArgs(t, argsFile)
	for _, a := range args {
		if a != strings.TrimSpace(a) {
			t.Errorf("argv leaked untrimmed value %q; full argv: %v", a, args)
		}
	}
}

// TestFakeRecognizer_TrimsWhitespaceInputs mirrors the whisper-side
// invariant so downstream tests that swap in FakeRecognizer see the
// same normalisation semantics.
func TestFakeRecognizer_TrimsWhitespaceInputs(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "out")
	f := &FakeRecognizer{}
	got, err := f.Transcribe(context.Background(),
		" in.wav ", "\tm.bin\n", "  "+prefix+"\t")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	wantSrt, _ := filepath.Abs(prefix + ".srt")
	if got != wantSrt {
		t.Errorf("srtPath = %q, want %q (trailing whitespace must be trimmed)", got, wantSrt)
	}
	if _, err := os.Stat(prefix + ".srt"); err != nil {
		t.Errorf("expected trimmed prefix .srt to exist; stat err = %v", err)
	}
	// Sanity: no whitespace-padded sibling was written.
	if _, err := os.Stat(prefix + "\t.srt"); err == nil {
		t.Errorf("untrimmed sibling was created")
	}
	// Call recording should also carry trimmed values, so pipeline
	// tests can assert on canonical paths without duplicating the
	// TrimSpace dance.
	if len(f.Calls) != 1 || f.Calls[0].Wav != "in.wav" || f.Calls[0].Model != "m.bin" || f.Calls[0].Prefix != prefix {
		t.Errorf("Calls did not record trimmed values; got %+v", f.Calls)
	}
}

// equalStringSlices is the local helper so tests don't pull in
// reflect.DeepEqual just for this. Nil and empty slices are treated
// as equal (matches the readArgs empty-file → nil convention).
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
