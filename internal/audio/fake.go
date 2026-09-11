package audio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// FakeTranscoder is the unit-test [Transcoder]. It creates an empty
// file at `out` (matching the "produces a wav file" post-condition)
// without spawning ffmpeg. Downstream code that only cares that the
// file exists — e.g. pipeline plumbing that pipes the wav path into a
// FakeRecognizer — is fully exercisable through this.
//
// The zero value works: no error injection, honours the caller's ctx.
// All fields exist purely to steer tests:
//
//   - Err: return this error wrapped in `audio: fake: %w` (after
//     checking ctx.Err()) and do not touch the filesystem. Emulates
//     "ffmpeg exited non-zero". Callers rely on `errors.Is(err, Err)`
//     rather than string prefix matching, and the wrapper keeps the
//     envelope symmetric with [FfmpegTranscoder]'s error format.
//   - Payload: bytes to write into `out` instead of leaving it empty.
//     Useful for tests that eyeball wav content, though the fake makes
//     no claim about producing a valid RIFF header — it is literally
//     "whatever you asked for, in a file".
//   - Calls: append (in, out) for each invocation so tests can assert
//     "was called exactly N times with these paths" without wrapping
//     the fake in another mock.
//
// Caveats:
//
//   - Unlike the real [FfmpegTranscoder], the fake does NOT open `in`
//     — it only writes `out`. Pipeline code that depends on the
//     transcoder validating the source file's existence must add its
//     own pre-check; the [Transcoder] contract itself only promises
//     "produces a wav file at out; parent dir must exist".
//   - Not safe for concurrent use across goroutines: `Calls` is
//     appended without a mutex. v1 pipeline is strictly sequential
//     (contracts.md §五), so this is a deliberate simplification. If a
//     future milestone parallelises transcode calls, wrap the fake in
//     a sync.Mutex or upgrade this struct.
type FakeTranscoder struct {
	Err     error
	Payload []byte
	Calls   []FakeCall
}

// FakeCall records a single ToWav16kMono invocation. Both fields are
// the values the caller actually passed — no path normalisation is
// applied so tests see exactly what the pipeline handed us.
type FakeCall struct {
	In  string
	Out string
}

// ToWav16kMono implements [Transcoder] for tests.
//
// Semantics — deliberately narrower than the real [FfmpegTranscoder]
// so tests are easy to reason about, but envelope-symmetric with it so
// pipeline code sees identical error text prefixes:
//
//   - ctx cancellation is honoured *before* any filesystem work: if
//     ctx.Done() has already fired we return ctx.Err() wrapped in the
//     same "audio: fake: %w" envelope the real implementation uses
//     for ctx errors. This keeps `errors.Is(err, context.Canceled)`
//     assertions portable between fake and real.
//   - Empty in/out are rejected up front, mirroring the real
//     implementation so pipeline code that violates the contract
//     fails identically against both.
//   - Err (when non-nil) is wrapped in `audio: fake: %w` (still
//     `errors.Is`-detectable) without touching disk — no partial
//     `out` file is left behind, matching the "failed run must not
//     produce output" convention the fakebin scripts also follow.
//   - On success, [os.Create] truncates + writes `out` (matching
//     ffmpeg -y). If Payload is non-nil its bytes are written before
//     the file is closed. The parent directory MUST exist (contract
//     clause) — os.Create's ENOENT is surfaced inside the
//     `audio: fake <in> -> <out>: %w` envelope, which mirrors the
//     real implementation's `audio: ffmpeg <in> -> <out>: %w` shape.
func (f *FakeTranscoder) ToWav16kMono(ctx context.Context, in, out string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("audio: fake: %w", err)
	}
	if strings.TrimSpace(in) == "" {
		return errors.New("audio: fake: empty input path")
	}
	if strings.TrimSpace(out) == "" {
		return errors.New("audio: fake: empty output path")
	}

	// Record BEFORE injected error so tests can still verify the call
	// happened when Err is set.
	f.Calls = append(f.Calls, FakeCall{In: in, Out: out})

	if f.Err != nil {
		// Wrap so error text is symmetric with real's "audio: ffmpeg:"
		// envelope; errors.Is(err, f.Err) still holds via %w.
		return fmt.Errorf("audio: fake: %w", f.Err)
	}

	// os.Create ⇒ O_RDWR|O_CREATE|O_TRUNC, matching ffmpeg -y's
	// overwrite-if-exists semantics that contracts.md §五 documents
	// as an idempotency property of the interface.
	file, err := os.Create(out)
	if err != nil {
		return fmt.Errorf("audio: fake %s -> %s: %w", in, out, err)
	}
	if len(f.Payload) > 0 {
		if _, err := file.Write(f.Payload); err != nil {
			// Best-effort close so the fd doesn't leak even in the
			// error path; the write error is what the caller cares
			// about.
			_ = file.Close()
			return fmt.Errorf("audio: fake %s -> %s: %w", in, out, err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("audio: fake %s -> %s: %w", in, out, err)
	}
	return nil
}

// Compile-time check: FakeTranscoder satisfies [Transcoder].
var _ Transcoder = (*FakeTranscoder)(nil)
