// Package audio transcodes source audio files into the canonical wire
// format whisper-cli consumes: 16 kHz mono 16-bit little-endian PCM WAV.
//
// The exported [Transcoder] interface is intentionally narrow — a single
// method that turns any ffmpeg-readable container into a `.wav`. That
// lets the pipeline swap implementations without plumbing command-line
// arguments across package boundaries:
//
//   - [FfmpegTranscoder] shells out to real ffmpeg with the argv fixed
//     in plan §5.4 (`ffmpeg -y -i <in> -vn -ar 16000 -ac 1 -c:a
//     pcm_s16le <out>`).
//   - [FakeTranscoder] writes an empty file at the destination path,
//     satisfying the "produces a wav file at `out`" post-condition
//     without spawning a subprocess. Intended for unit tests of
//     callers (pipeline, tmpdir plumbing) that must not depend on a
//     real ffmpeg install.
//
// See [docs/contracts.md §二.2] for the frozen semantic contract this
// package implements; any change to the [Transcoder] signature or its
// side-effect surface must be gated on a contract update, not a local
// tweak.
package audio

import "context"

// Transcoder converts a source audio file into a canonical 16 kHz mono
// 16-bit PCM WAV suitable as whisper-cli input.
//
// Contract (from [docs/contracts.md §二.2]):
//
//   - Reads `in`; writes `out`. The output file is overwritten if it
//     already exists (ffmpeg's `-y`; fakes MUST match this).
//   - The parent directory of `out` MUST exist; implementations are
//     free to surface the underlying "no such file or directory" error
//     — they do not attempt to `MkdirAll`.
//   - Returns on the first of: successful transcode, non-zero tool
//     exit, or ctx.Done(). When ctx is cancelled the underlying
//     process is killed and the returned error wraps ctx.Err() so
//     callers can `errors.Is(err, context.Canceled)` /
//     `context.DeadlineExceeded`.
//   - Idempotent by design: repeated calls with the same in/out
//     produce the same output (subject to source-file changes).
type Transcoder interface {
	ToWav16kMono(ctx context.Context, in, out string) error
}
