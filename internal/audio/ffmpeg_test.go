//go:build !windows

package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// projectTempDir returns a fresh sandbox-friendly temp dir living
// inside the repo under .tmp/. Trae's sandbox refuses /var/folders/
// paths whenever we spawn a child (ffmpeg stub via `exec.CommandContext`),
// so we mirror the pattern already established in test/fakebin/
// fakebin_test.go. The directory is auto-cleaned at test end.
func projectTempDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(1)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// audio package sits at internal/audio → .. .. resolves to repo root.
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	base := filepath.Join(repoRoot, ".tmp", "audio-test")
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

// writeArgvCaptureScript drops a POSIX shell stub at scriptPath that
// prints each argv token on its own line to captureFile (so we can
// assert argv ordering with plain string compares) and then exits with
// exitCode. If stderrLine is non-empty it is echoed to stderr before
// exit, so tests can verify the real transcoder's stderr plumbing
// (Stderr → io.Writer) is wired correctly.
//
// The script is created 0755 so exec.CommandContext can spawn it.
// t.TempDir cleanup removes it at the end of the test.
//
// We deliberately avoid `exec` in the shebang path — /bin/sh is
// available on macOS and Linux and the fakebin scripts under
// testdata/fakebin/ already assume the same baseline.
func writeArgvCaptureScript(t *testing.T, scriptPath, captureFile, stderrLine string, exitCode int) {
	t.Helper()
	body := fmt.Sprintf(`#!/bin/sh
: > %[1]q
for arg in "$@"; do
  printf '%%s\n' "$arg" >> %[1]q
done
if [ -n %[2]q ]; then
  printf '%%s\n' %[2]q >&2
fi
exit %[3]d
`, captureFile, stderrLine, exitCode)
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
}

// readLines slurps the argv capture file and returns one token per
// line. Empty file ⇒ nil slice (avoids the "one empty string" foot-gun
// that strings.Split would introduce).
func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// TestFfmpeg_ArgvMatchesPlan_5_4 is the primary contract test for
// [FfmpegTranscoder]: the argv sent to the child MUST equal, in
// order, the recipe frozen in [docs/plan.md §5.4]:
//
//	ffmpeg -y -i <in> -vn -ar 16000 -ac 1 -c:a pcm_s16le <out>
//
// Any reordering, added flag, or dropped flag will trip this test.
// The whisper.cpp model expects 16 kHz mono s16le PCM — silently
// changing any of those tokens produces a wav whisper-cli will refuse.
func TestFfmpeg_ArgvMatchesPlan_5_4(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "ffmpeg-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureScript(t, script, capture, "", 0)

	in := filepath.Join(dir, "src.m4a")
	out := filepath.Join(dir, "out.wav")

	tr := &FfmpegTranscoder{Binary: script}
	if err := tr.ToWav16kMono(context.Background(), in, out); err != nil {
		t.Fatalf("ToWav16kMono: %v", err)
	}

	want := []string{
		"-y",
		"-i", in,
		"-vn",
		"-ar", "16000",
		"-ac", "1",
		"-c:a", "pcm_s16le",
		out,
	}
	got := readLines(t, capture)
	if len(got) != len(want) {
		t.Fatalf("argv length = %d, want %d; got=%q want=%q", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q; full got=%q", i, got[i], want[i], got)
		}
	}
}

// TestFfmpeg_BinaryDefaultsToPATHName exercises the zero-value contract
// documented in [FfmpegTranscoder] doc block: leaving Binary empty must
// resolve to the string "ffmpeg" (which then flows through $PATH via
// exec.LookPath). We assert by setting $PATH so *only* our stub is
// reachable under the literal name "ffmpeg" and verifying the argv
// capture fires.
//
// This is critical because preflight-resolved absolute paths are
// injected into Binary in production; the fallback is the default
// path. If someone changes the empty-string handling, tests must
// scream.
func TestFfmpeg_BinaryDefaultsToPATHName(t *testing.T) {
	dir := projectTempDir(t)
	stub := filepath.Join(dir, "ffmpeg")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureScript(t, stub, capture, "", 0)

	t.Setenv("PATH", dir)

	tr := &FfmpegTranscoder{} // Binary left empty on purpose.
	err := tr.ToWav16kMono(context.Background(), "in.m4a", "out.wav")
	if err != nil {
		t.Fatalf("ToWav16kMono with default binary: %v", err)
	}
	if lines := readLines(t, capture); len(lines) == 0 {
		t.Errorf("expected stub argv capture; file empty")
	}
}

// TestFfmpeg_StderrIsPipedToWriter verifies contracts.md §四.4 (log
// contract): child stderr must reach the caller-supplied io.Writer.
// The stub prints a fixed line to stderr; we assert it lands in our
// buffer. In production this writer is
// [logger.StderrSink]("ffmpeg") — we don't exercise the sink here,
// just the plumbing.
func TestFfmpeg_StderrIsPipedToWriter(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "ffmpeg-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureScript(t, script, capture, "fake-ffmpeg-progress-line", 0)

	var buf bytes.Buffer
	tr := &FfmpegTranscoder{Binary: script, Stderr: &buf}
	if err := tr.ToWav16kMono(context.Background(), "in.m4a", "out.wav"); err != nil {
		t.Fatalf("ToWav16kMono: %v", err)
	}
	if !strings.Contains(buf.String(), "fake-ffmpeg-progress-line") {
		t.Errorf("stderr should be piped to Writer; got %q", buf.String())
	}
}

// TestFfmpeg_StderrNilFallsBackToDiscard proves the nil-Stderr branch
// does not panic and does not attach the parent's stderr fd (which
// would leak child chatter into `go test` output).
//
// Direct nil-check: after a successful run against a stub that writes
// to stderr, nothing surfaces in os.Stderr from the child. We can't
// easily observe io.Discard directly but we CAN confirm the call did
// not crash and the argv capture happened, which is enough coverage
// combined with the doc comment.
func TestFfmpeg_StderrNilFallsBackToDiscard(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "ffmpeg-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureScript(t, script, capture, "should-vanish", 0)

	tr := &FfmpegTranscoder{Binary: script} // Stderr left nil
	if err := tr.ToWav16kMono(context.Background(), "in.m4a", "out.wav"); err != nil {
		t.Fatalf("ToWav16kMono: %v", err)
	}
	if lines := readLines(t, capture); len(lines) == 0 {
		t.Errorf("stub should have run; argv capture is empty")
	}
}

// TestFfmpeg_NonZeroExit_WrapsErrorWithPaths asserts the failure
// envelope: non-zero exit produces an error whose text contains both
// input and output paths (so `pipeline` logs pinpoint which pair
// blew up), and unwrapping yields an *exec.ExitError chain caller
// code can inspect. Sentinel-free path — the real ffmpeg impl
// deliberately does not define its own sentinels for exec failures.
func TestFfmpeg_NonZeroExit_WrapsErrorWithPaths(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "ffmpeg-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureScript(t, script, capture, "boom", 1)

	tr := &FfmpegTranscoder{Binary: script}
	err := tr.ToWav16kMono(context.Background(), "/tmp/in.m4a", "/tmp/out.wav")
	if err == nil {
		t.Fatal("expected error for exit 1; got nil")
	}
	if !strings.Contains(err.Error(), "audio: ffmpeg /tmp/in.m4a -> /tmp/out.wav") {
		t.Errorf("error must include both paths for triage; got %q", err.Error())
	}
	// Argv was still captured — the failure is post-spawn, not a
	// synchronous validation error.
	if lines := readLines(t, capture); len(lines) == 0 {
		t.Errorf("stub should have run before failing; argv capture is empty")
	}
}

// TestFfmpeg_CtxCancelledMidRun_WrapsCtxErr proves the ctx-error
// preference: [FfmpegTranscoder.ToWav16kMono] must return a wrapped
// [context.Canceled] (not the raw *exec.ExitError from the SIGKILL)
// when ctx fires during the child's run. Callers doing
// `errors.Is(err, context.Canceled)` rely on this to map to exit 130.
func TestFfmpeg_CtxCancelledMidRun_WrapsCtxErr(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "ffmpeg-slow")
	// `exec sleep` replaces the shell with the sleep process so
	// SIGKILL from exec.CommandContext hits sleep directly. Without
	// exec the shell keeps running while sleep is its child, and
	// killing the shell doesn't reap the child on macOS, which lets
	// the test hang for the full sleep duration.
	body := `#!/bin/sh
exec sleep 5
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write slow stub: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	tr := &FfmpegTranscoder{Binary: script}
	start := time.Now()
	err := tr.ToWav16kMono(ctx, "in.m4a", "out.wav")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected ctx error; got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected errors.Is(err, context.DeadlineExceeded); got %v", err)
	}
	if !strings.Contains(err.Error(), "audio: ffmpeg") {
		t.Errorf("error should carry 'audio: ffmpeg' envelope; got %q", err.Error())
	}
	if elapsed > 2*time.Second {
		t.Errorf("ctx cancel should short-circuit fast; took %s", elapsed)
	}
}

// TestFfmpeg_CtxAlreadyCancelledBeforeSpawn covers the tighter case:
// ctx is already done when Run() is called. exec.CommandContext will
// refuse to spawn and cmd.Run() returns an error; the transcoder's
// post-Run ctx-check must convert it into a wrapped ctx.Err().
func TestFfmpeg_CtxAlreadyCancelledBeforeSpawn(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "ffmpeg-stub")
	writeArgvCaptureScript(t, script, filepath.Join(dir, "argv.txt"), "", 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tr := &FfmpegTranscoder{Binary: script}
	err := tr.ToWav16kMono(ctx, "in.m4a", "out.wav")
	if err == nil {
		t.Fatal("expected ctx error; got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled); got %v", err)
	}
}

// TestFfmpeg_EmptyInputPath_RejectedWithoutSpawn confirms the early
// validation path: empty `in` returns a synchronous error and never
// spawns the child. We prove "no spawn" by pointing Binary at a stub
// whose *side effect* is creating the argv-capture file; the file
// must not appear.
func TestFfmpeg_EmptyInputPath_RejectedWithoutSpawn(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "ffmpeg-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureScript(t, script, capture, "", 0)

	tr := &FfmpegTranscoder{Binary: script}
	err := tr.ToWav16kMono(context.Background(), "", "out.wav")
	if err == nil || !strings.Contains(err.Error(), "empty input path") {
		t.Errorf("expected 'empty input path', got %v", err)
	}
	if _, statErr := os.Stat(capture); statErr == nil {
		t.Errorf("empty input must reject BEFORE spawn; capture file was created")
	} else if !os.IsNotExist(statErr) {
		t.Errorf("unexpected stat error: %v", statErr)
	}
}

// TestFfmpeg_EmptyOutputPath_RejectedWithoutSpawn — twin of the
// above for the output path. Same rationale: an empty `out` would
// make ffmpeg treat `-` as stdout and silently corrupt behaviour.
func TestFfmpeg_EmptyOutputPath_RejectedWithoutSpawn(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "ffmpeg-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureScript(t, script, capture, "", 0)

	tr := &FfmpegTranscoder{Binary: script}
	err := tr.ToWav16kMono(context.Background(), "in.m4a", "")
	if err == nil || !strings.Contains(err.Error(), "empty output path") {
		t.Errorf("expected 'empty output path', got %v", err)
	}
	if _, statErr := os.Stat(capture); statErr == nil {
		t.Errorf("empty output must reject BEFORE spawn; capture file was created")
	}
}

// TestFfmpeg_WhitespacePathsRejected mirrors the fake's whitespace
// coverage on the real impl. Prevents a caller that hands us "  "
// (from mis-quoted CLI flags, say) from ever reaching exec.
func TestFfmpeg_WhitespacePathsRejected(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "ffmpeg-stub")
	writeArgvCaptureScript(t, script, filepath.Join(dir, "argv.txt"), "", 0)

	tr := &FfmpegTranscoder{Binary: script}
	cases := []struct {
		name string
		in   string
		out  string
		want string
	}{
		{"tab-in", "\t", "out.wav", "empty input path"},
		{"spaces-out", "in.m4a", "   ", "empty output path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tr.ToWav16kMono(context.Background(), tc.in, tc.out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want contains %q", err, tc.want)
			}
		})
	}
}

// TestFfmpeg_SpawnFailure_SurfacesExecError proves the "binary missing
// / not executable" branch: exec.LookPath / CommandContext fails
// before the child ever runs, and the returned error still carries
// the "audio: ffmpeg" envelope and both paths for triage.
func TestFfmpeg_SpawnFailure_SurfacesExecError(t *testing.T) {
	tr := &FfmpegTranscoder{Binary: "/nonexistent/definitely-not-here-xyz"}
	err := tr.ToWav16kMono(context.Background(), "in.m4a", "out.wav")
	if err == nil {
		t.Fatal("expected spawn failure; got nil")
	}
	if !strings.Contains(err.Error(), "audio: ffmpeg in.m4a -> out.wav") {
		t.Errorf("error must include both paths; got %q", err.Error())
	}
}

// TestFfmpeg_StdoutIsDiscardedNotLeaked closes the loop on the
// "stdout ⇒ io.Discard" contract line: verify that a stub which
// prints to stdout does NOT leak its bytes into the caller (there's
// no Stdout knob to intercept, so we assert indirectly by checking
// Stderr writer stays untouched — i.e. no accidental duplication).
func TestFfmpeg_StdoutIsDiscardedNotLeaked(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "ffmpeg-stub")
	body := `#!/bin/sh
printf 'unexpected-stdout-line\n'
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write stdout stub: %v", err)
	}
	var buf bytes.Buffer
	tr := &FfmpegTranscoder{Binary: script, Stderr: &buf}
	if err := tr.ToWav16kMono(context.Background(), "in.m4a", "out.wav"); err != nil {
		t.Fatalf("call: %v", err)
	}
	if strings.Contains(buf.String(), "unexpected-stdout-line") {
		t.Errorf("stdout must not leak into Stderr writer; got %q", buf.String())
	}
}

// TestFfmpeg_CtxCancellationWinsOverEmptyPath locks the ordering
// property introduced by the P2-1 code review: ctx.Err() is checked
// FIRST, so a cancelled ctx + empty input pair yields
// context.Canceled — not the empty-path validation error. Symmetric
// with [TestFake_CtxCancellationWinsOverInjectedErr]; both
// implementations must agree on "ctx wins over every other branch"
// so pipeline error mapping (`errors.Is(err, context.Canceled)` →
// exit 130) fires regardless of which transcoder is wired in.
func TestFfmpeg_CtxCancellationWinsOverEmptyPath(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "ffmpeg-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureScript(t, script, capture, "", 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tr := &FfmpegTranscoder{Binary: script}
	err := tr.ToWav16kMono(ctx, "", "") // both empty AND ctx done
	if !errors.Is(err, context.Canceled) {
		t.Errorf("ctx should win over empty-path; got %v", err)
	}
	if strings.Contains(err.Error(), "empty input path") ||
		strings.Contains(err.Error(), "empty output path") {
		t.Errorf("empty-path branch leaked through cancelled-ctx guard: %q", err.Error())
	}
	if _, statErr := os.Stat(capture); statErr == nil {
		t.Errorf("no child should have spawned; capture file appeared")
	}
}

// TestFfmpeg_CompileTimeSatisfiesInterface pins the compile-time
// invariant: *FfmpegTranscoder must implement [Transcoder]. If someone
// renames or drops the method the build breaks — but keeping the
// assertion here also documents the intent for reviewers who don't
// scroll to the bottom of ffmpeg.go.
func TestFfmpeg_CompileTimeSatisfiesInterface(t *testing.T) {
	var _ Transcoder = (*FfmpegTranscoder)(nil)
}
