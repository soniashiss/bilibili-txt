// Package asr recognises spoken audio into srt subtitles by driving
// whisper.cpp's `whisper-cli` binary.
//
// The exported [Recognizer] interface is intentionally narrow — a
// single method that turns a 16 kHz mono wav (as produced by
// [internal/audio.Transcoder]) into an on-disk `.srt` file — so the
// pipeline layer can swap implementations without plumbing whisper's
// forty-odd flags across package boundaries:
//
//   - [WhisperRecognizer] shells out to real whisper-cli with the argv
//     fixed in plan §5.5 (`whisper-cli -m <model> -f <wav> -l zh
//     -osrt -of <prefix> --threads <n> --print-progress`).
//   - [FakeRecognizer] copies a canned srt fixture to
//     `<outPrefix>.srt`, satisfying the "produces an srt file at that
//     path" post-condition without spawning a subprocess. Intended for
//     unit tests of callers (pipeline, tmpdir plumbing) that must not
//     depend on a real whisper.cpp install.
//
// See [docs/contracts.md §二.3] for the frozen semantic contract this
// package implements; any change to the [Recognizer] signature or its
// side-effect surface must be gated on a contract update, not a local
// tweak.
package asr

import "context"

// Recognizer transcribes a wav file into an srt subtitle file using
// automatic speech recognition.
//
// Contract (from [docs/contracts.md §二.3]):
//
//   - Reads `wavPath`; writes `<outPrefix>.srt`. The output file is
//     overwritten if it already exists (whisper-cli behaviour; fakes
//     MUST match this).
//   - `modelPath` points to a whisper.cpp ggml model file. The
//     recognizer does NOT validate the file — preflight §4.2 has
//     already checked existence and the 100 MiB floor.
//   - The parent directory of `outPrefix` MUST exist; implementations
//     are free to surface the underlying "no such file or directory"
//     error — they do not attempt to `MkdirAll`.
//   - `outPrefix` MUST NOT end with `.srt` (case-insensitive); both
//     implementations reject such prefixes up front so callers do
//     not accidentally produce `<name>.srt.srt`.
//   - Returns on the first of: successful transcribe, non-zero tool
//     exit, or ctx.Done(). When ctx is cancelled the underlying
//     process is killed and the returned error wraps ctx.Err() so
//     callers can `errors.Is(err, context.Canceled)` /
//     `context.DeadlineExceeded`. If ctx is already cancelled at
//     call time the implementation returns immediately without any
//     side-effect.
//   - The returned `srtPath` is normalised to an absolute path via
//     [filepath.Abs]; it always points at `<outPrefix>.srt`
//     (whisper-cli's naming convention for `-osrt -of <prefix>`).
//   - Implementations verify that `<outPrefix>.srt` actually exists
//     on disk before returning success, so callers can rely on the
//     post-condition without a redundant [os.Stat].
//   - Idempotent by design: repeated calls with the same wav/model/
//     prefix produce the same output (subject to the wav bytes).
type Recognizer interface {
	Transcribe(ctx context.Context, wavPath, modelPath, outPrefix string) (srtPath string, err error)
}
