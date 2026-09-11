package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// FfmpegTranscoder is the production [Transcoder] that shells out to
// the ffmpeg binary. Argv is fixed to plan §5.4:
//
//	ffmpeg -y -i <in> -vn -ar 16000 -ac 1 -c:a pcm_s16le <out>
//
// -y            overwrite <out> without prompting (matches the "idempotent"
//               contract clause: repeated calls with the same paths succeed)
// -i <in>       source container (m4a from yt-dlp -x, in practice)
// -vn           discard any video stream (m4a is audio-only but yt-dlp
//               fallbacks may hand us a muxed file; -vn keeps the transcode
//               deterministic regardless)
// -ar 16000     16 kHz sample rate — whisper.cpp's required input rate
// -ac 1         downmix to mono — matches the whisper-cli model contract
// -c:a pcm_s16le  signed 16-bit little-endian PCM inside a WAV container;
//               again a whisper.cpp requirement
// <out>         destination path (must end in .wav for downstream tools;
//               we don't enforce the extension here, since ffmpeg picks
//               the muxer from -c:a + filename and callers own naming)
//
// The struct's zero value is usable: Binary defaults to the string
// "ffmpeg" (looked up via the ambient $PATH by [exec.CommandContext])
// and Stderr defaults to [io.Discard]. Callers that need per-tool
// debug logs (see logger.StderrSink) inject their own Stderr; callers
// that pinned a yaml-supplied binary path (see preflight two-tier
// fallback) inject the resolved absolute path into Binary so we do not
// re-do the $PATH lookup at every call site.
type FfmpegTranscoder struct {
	// Binary is the ffmpeg command name or absolute path. Empty ⇒
	// "ffmpeg", resolved via $PATH. Passing an absolute path here is
	// how preflight-resolved paths reach this layer.
	Binary string
	// Stderr is where the child process's stderr is drained. nil ⇒
	// [io.Discard]. Wire logger.StderrSink("ffmpeg") here in
	// production; leave nil in tests that do not care about the
	// child's chatter.
	Stderr io.Writer
}

// ToWav16kMono implements [Transcoder].
//
// The method returns nil iff ffmpeg exits 0 within the caller's ctx
// deadline. Failure modes are mapped as follows:
//
//   - ctx already done on entry ⇒ short-circuit before spawn and
//     return `audio: ffmpeg: <ctx.Err()>` wrapped for `errors.Is`
//     matching. Checked BEFORE empty-path validation so the fake and
//     real implementations agree on the "ctx wins over every other
//     branch" ordering (contracts.md §四.1).
//   - Empty in/out ⇒ synchronous validation error (never spawns a
//     child) — ffmpeg would misinterpret an empty argv slot as stdin
//     ("-") and silently truncate <out>, so we reject up front.
//   - ctx cancelled/expired mid-run ⇒ [exec.CommandContext] kills the
//     child, we detect via ctx.Err() and return `audio: ffmpeg:
//     <ctx.Err()>` with the sentinel wrapped for `errors.Is` matching
//     against [context.Canceled] / [context.DeadlineExceeded].
//   - Non-zero exit / spawn failure ⇒ error wraps *exec.ExitError (or
//     the exec spawn error) inside the envelope
//     `audio: ffmpeg <in> -> <out>: %w`, so the pipeline log line
//     at least tells the user which pair failed.
//
// stdout is discarded — ffmpeg only writes progress there when
// `-progress pipe:1` is passed, which we do not.
func (f *FfmpegTranscoder) ToWav16kMono(ctx context.Context, in, out string) error {
	// ctx first — matches [FakeTranscoder.ToWav16kMono] ordering and
	// contracts.md §四.1 ("尽快返回 ctx.Err()"). A caller that hands
	// us a cancelled ctx + empty paths should see context.Canceled,
	// not the empty-path validation error, so pipeline layer's
	// `errors.Is(err, context.Canceled)` exit-code-130 mapping fires.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("audio: ffmpeg: %w", err)
	}
	if strings.TrimSpace(in) == "" {
		return errors.New("audio: ffmpeg: empty input path")
	}
	if strings.TrimSpace(out) == "" {
		return errors.New("audio: ffmpeg: empty output path")
	}

	bin := f.Binary
	if bin == "" {
		bin = "ffmpeg"
	}

	// Argv order matters for reviewers cross-checking plan §5.4:
	// keep it identical to the doc string above.
	args := []string{
		"-y",
		"-i", in,
		"-vn",
		"-ar", "16000",
		"-ac", "1",
		"-c:a", "pcm_s16le",
		out,
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = io.Discard
	if f.Stderr != nil {
		cmd.Stderr = f.Stderr
	} else {
		cmd.Stderr = io.Discard
	}

	if err := cmd.Run(); err != nil {
		// ctx-cancelled runs surface as *exec.ExitError from the
		// SIGKILL exec.CommandContext sends. Preferring ctx.Err()
		// here gives callers a wrapped sentinel they can errors.Is
		// against, matching contracts.md §四.1.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("audio: ffmpeg: %w", ctxErr)
		}
		return fmt.Errorf("audio: ffmpeg %s -> %s: %w", in, out, err)
	}
	return nil
}

// Compile-time check: FfmpegTranscoder satisfies [Transcoder]. If the
// interface signature drifts (contract update) the build breaks here
// before any caller notices.
var _ Transcoder = (*FfmpegTranscoder)(nil)
