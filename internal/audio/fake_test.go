package audio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFake_HappyPath_CreatesEmptyFile locks the zero-value contract:
// a bare [FakeTranscoder] must materialise a zero-byte file at `out`
// (matching contracts.md §五 "wav produced" post-condition) and record
// exactly one entry in Calls with the caller's paths untouched.
func TestFake_HappyPath_CreatesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.wav")
	f := &FakeTranscoder{}
	if err := f.ToWav16kMono(context.Background(), "src.m4a", out); err != nil {
		t.Fatalf("ToWav16kMono: %v", err)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat out: %v", err)
	}
	if st.Size() != 0 {
		t.Errorf("empty fake should produce 0-byte file, got %d bytes", st.Size())
	}
	if len(f.Calls) != 1 || f.Calls[0].In != "src.m4a" || f.Calls[0].Out != out {
		t.Errorf("Calls = %+v; want single {In:src.m4a Out:%s}", f.Calls, out)
	}
}

// TestFake_Payload_WritesBytesToOut proves Payload is written verbatim
// into `out`. The bytes are arbitrary — we're not asserting the fake
// emits a valid RIFF header; per fake.go's docstring it is "whatever
// you asked for, in a file".
func TestFake_Payload_WritesBytesToOut(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.wav")
	payload := []byte("RIFF-fake-riff-header")
	f := &FakeTranscoder{Payload: payload}
	if err := f.ToWav16kMono(context.Background(), "src.m4a", out); err != nil {
		t.Fatalf("ToWav16kMono: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("payload mismatch: got %q want %q", got, payload)
	}
}

// TestFake_InjectedErr_WrappedWithEnvelope_NoOutputFile verifies the
// error-injection branch: when Err is set, the fake must (a) return
// that error via errors.Is (unwrapping-safe), (b) wrap it in the
// same "audio: fake:" envelope the real impl uses for its own
// errors, so pipeline log lines look identical against both, (c)
// leave the filesystem untouched (no partial `out`), and (d) still
// record the attempt in Calls so tests can assert "was invoked with
// these paths" even on failure.
func TestFake_InjectedErr_WrappedWithEnvelope_NoOutputFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "should-not-exist.wav")
	sentinel := errors.New("ffmpeg blew up")
	f := &FakeTranscoder{Err: sentinel}
	err := f.ToWav16kMono(context.Background(), "src.m4a", out)
	if !errors.Is(err, sentinel) {
		t.Errorf("expected errors.Is(err, sentinel); got %v", err)
	}
	if !strings.Contains(err.Error(), "audio: fake") {
		t.Errorf("injected Err should be wrapped in 'audio: fake' envelope; got %q", err.Error())
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Errorf("Err injection should skip filesystem work; found file at %s (err=%v)", out, statErr)
	}
	if len(f.Calls) != 1 {
		t.Errorf("Calls should have 1 entry even when Err set, got %d", len(f.Calls))
	}
}

// TestFake_CtxCancelledBefore_ReturnsWrappedCtxErr locks the ctx-first
// ordering: cancellation trumps every other branch and must NOT touch
// disk or Calls. Envelope prefix "audio: fake:" is also asserted so a
// refactor that drops the wrapper triggers a red test.
func TestFake_CtxCancelledBefore_ReturnsWrappedCtxErr(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.wav")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &FakeTranscoder{}
	err := f.ToWav16kMono(ctx, "src.m4a", out)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled); got %v", err)
	}
	if !strings.Contains(err.Error(), "audio: fake") {
		t.Errorf("error should carry 'audio: fake' envelope, got %q", err.Error())
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Errorf("cancelled call must not touch disk; file exists (err=%v)", statErr)
	}
	if len(f.Calls) != 0 {
		t.Errorf("ctx check runs BEFORE Calls append; got %+v", f.Calls)
	}
}

// TestFake_CtxCancellationWinsOverInjectedErr locks a subtle ordering
// property in [FakeTranscoder.ToWav16kMono]: ctx.Err() is checked
// FIRST, so a cancelled ctx must shadow any injected Err. Guards
// against a refactor that reorders the branches.
func TestFake_CtxCancellationWinsOverInjectedErr(t *testing.T) {
	sentinel := errors.New("would-be-ffmpeg-error")
	f := &FakeTranscoder{Err: sentinel}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := f.ToWav16kMono(ctx, "in", "out")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("ctx should win over Err; got %v", err)
	}
	if errors.Is(err, sentinel) {
		t.Errorf("injected Err should be shadowed by ctx; got %v", err)
	}
}

// TestFake_EmptyInputPathRejected covers the "reject early, don't
// touch disk" guarantee for the empty input case. Same contract the
// real [FfmpegTranscoder] enforces so pipeline code that violates it
// fails identically against both implementations.
func TestFake_EmptyInputPathRejected(t *testing.T) {
	f := &FakeTranscoder{}
	err := f.ToWav16kMono(context.Background(), "", "out.wav")
	if err == nil || !strings.Contains(err.Error(), "empty input path") {
		t.Errorf("expected 'empty input path'; got %v", err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("no Calls should be recorded on validation failure; got %+v", f.Calls)
	}
}

// TestFake_EmptyOutputPathRejected — the output-path twin of the
// above. Kept as a separate test so an assertion failure names the
// exact branch that regressed.
func TestFake_EmptyOutputPathRejected(t *testing.T) {
	f := &FakeTranscoder{}
	err := f.ToWav16kMono(context.Background(), "in.m4a", "")
	if err == nil || !strings.Contains(err.Error(), "empty output path") {
		t.Errorf("expected 'empty output path'; got %v", err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("no Calls should be recorded; got %+v", f.Calls)
	}
}

// TestFake_WhitespacePathsRejected extends the empty-path checks to
// cover TrimSpace-adjacent inputs (spaces, tabs, newlines). The fake
// treats "just whitespace" as "unspecified" — mirrors the real
// ffmpeg impl, and matches the config package's tilde/whitespace
// handling convention documented in [docs/plan.md §4.2].
func TestFake_WhitespacePathsRejected(t *testing.T) {
	cases := []struct {
		name string
		in   string
		out  string
		want string
	}{
		{"spaces-in", "   ", "out.wav", "empty input path"},
		{"tab-in", "\t", "out.wav", "empty input path"},
		{"nl-in", "\n\n", "out.wav", "empty input path"},
		{"spaces-out", "in.m4a", "   ", "empty output path"},
		{"tab-out", "in.m4a", "\t\t", "empty output path"},
	}
	f := &FakeTranscoder{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := f.ToWav16kMono(context.Background(), tc.in, tc.out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("in=%q out=%q -> err=%v, want %q", tc.in, tc.out, err, tc.want)
			}
		})
	}
	if len(f.Calls) != 0 {
		t.Errorf("no Calls should accumulate on validation errors; got %d", len(f.Calls))
	}
}

// TestFake_MultipleCalls_AppendsAll confirms Calls acts as a full
// append-only log: three successful invocations produce three entries
// in original order. Pipeline tests will lean on this to assert
// "transcode was invoked N times with these exact paths".
func TestFake_MultipleCalls_AppendsAll(t *testing.T) {
	dir := t.TempDir()
	f := &FakeTranscoder{}
	names := []string{"a.wav", "b.wav", "c.wav"}
	for i, name := range names {
		out := filepath.Join(dir, name)
		if err := f.ToWav16kMono(context.Background(), "in.m4a", out); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := len(f.Calls); got != len(names) {
		t.Fatalf("Calls has %d entries, want %d", got, len(names))
	}
	for i, name := range names {
		if !strings.HasSuffix(f.Calls[i].Out, name) {
			t.Errorf("Calls[%d].Out = %q, want suffix %q", i, f.Calls[i].Out, name)
		}
	}
}

// TestFake_OverwriteExistingFile_TruncatesToPayload proves ffmpeg -y
// semantics: os.Create with O_TRUNC drops any pre-existing content.
// Guards contracts.md §五's idempotency clause ("repeated calls
// produce the same output").
func TestFake_OverwriteExistingFile_TruncatesToPayload(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "reused.wav")
	if err := os.WriteFile(out, []byte("old-and-longer-than-payload"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	f := &FakeTranscoder{Payload: []byte("new")}
	if err := f.ToWav16kMono(context.Background(), "in.m4a", out); err != nil {
		t.Fatalf("call: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("file was not truncated to payload; got %q want %q", got, "new")
	}
}

// TestFake_ParentDirMissing_SurfacesCreateError enforces the "the
// implementation does not MkdirAll" contract clause (contracts.md
// §二.2). A missing parent dir must surface os.Create's ENOENT with
// our envelope prefix intact, and the error chain must remain
// os.IsNotExist-detectable for pipeline code that wants to
// differentiate.
func TestFake_ParentDirMissing_SurfacesCreateError(t *testing.T) {
	f := &FakeTranscoder{}
	err := f.ToWav16kMono(context.Background(),
		"in.m4a",
		filepath.Join(t.TempDir(), "does", "not", "exist", "out.wav"))
	if err == nil {
		t.Fatal("expected error for missing parent dir; got nil")
	}
	if !strings.Contains(err.Error(), "audio: fake ") || !strings.Contains(err.Error(), " -> ") {
		t.Errorf("error should carry symmetric 'audio: fake <in> -> <out>' envelope, got %q", err.Error())
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("errors.Is(err, os.ErrNotExist) should hold; got %v", err)
	}
	// Call is still recorded — the attempt happened even though disk
	// I/O failed. Matches the "record before Err" convention.
	if len(f.Calls) != 1 {
		t.Errorf("Calls should have 1 entry (attempt was made), got %d", len(f.Calls))
	}
}
