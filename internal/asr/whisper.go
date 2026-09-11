package asr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// WhisperRecognizer is the production [Recognizer] that shells out to
// the whisper-cli binary (from whisper.cpp). Argv is fixed to plan §5.5:
//
//	whisper-cli -m <model> -f <wav> -l <lang> -osrt -of <outPrefix> \
//	  --threads <n> --print-progress
//
// -m <model>       whisper.cpp ggml model file (preflight has verified
//                  existence and the 100 MiB floor; we do not re-check).
// -f <wav>         input wav — MUST be the 16 kHz mono 16-bit PCM
//                  produced by [internal/audio.Transcoder] (whisper.cpp
//                  will error otherwise; we do not defensively re-check
//                  since preflight cannot inspect run-time wavs).
// -l <lang>        forced source language ("zh" by default, matching
//                  plan §5.5). Exposed via [WhisperRecognizer.Language]
//                  so future callers can transcribe non-zh videos
//                  without another interface method.
// -osrt            emit srt output; the sole subtitle format we support.
// -of <outPrefix>  output *prefix*; whisper-cli appends ".srt"
//                  automatically. We return `<outPrefix>.srt` as the
//                  first return value so pipeline callers do not have
//                  to hard-code that convention.
// --threads <n>    CPU worker count. Defaults to runtime.NumCPU() —
//                  the Go-side equivalent of the `sysctl -n hw.ncpu`
//                  invocation plan §5.5 spells out for macOS.
// --print-progress emit progress lines (e.g. `progress = 25%`) to
//                  stdout so debug-mode users can see whisper is still
//                  alive on long transcodes.
//
// The struct's zero value is usable: [Binary] defaults to the string
// "whisper-cli" (looked up via the ambient $PATH by
// [exec.CommandContext]), [Stderr]/[Stdout] default to [io.Discard],
// [Language] defaults to "zh", and [Threads] defaults to
// [runtime.NumCPU]. Callers that need per-tool debug logs (see
// logger.StderrSink) inject their own Stdout/Stderr; callers that
// pinned a yaml-supplied binary path (see preflight two-tier
// fallback) inject the resolved absolute path into [Binary] so we do
// not re-do the $PATH lookup at every call site.
type WhisperRecognizer struct {
	// Binary is the whisper-cli command name or absolute path. Empty
	// ⇒ "whisper-cli", resolved via $PATH. Passing an absolute path
	// here is how preflight-resolved paths reach this layer.
	Binary string
	// Language is the ISO 639-1 code passed via -l. Empty ⇒ "zh".
	// Exposed on the struct rather than as a per-call arg because
	// pipeline callers set it once per invocation (M3 has no
	// multi-language use case, but this keeps the door open without
	// churning [Recognizer]).
	Language string
	// Threads is the value passed via --threads. Zero or negative ⇒
	// runtime.NumCPU() at call time (i.e. this is *not* memoised, so
	// tests can vary GOMAXPROCS between runs and see the change).
	Threads int
	// Stdout is where the child process's stdout is drained. nil ⇒
	// [io.Discard]. --print-progress writes progress lines here so
	// wiring `logger.StderrSink("whisper-progress")` in production
	// pipes them straight into the debug log (plan §5.5 explicitly
	// says progress is visible in debug logs).
	Stdout io.Writer
	// Stderr is where the child process's stderr is drained. nil ⇒
	// [io.Discard]. Wire logger.StderrSink("whisper") here in
	// production; leave nil in tests that do not care about the
	// child's chatter.
	Stderr io.Writer
}

// Transcribe implements [Recognizer].
//
// The method returns `(<outPrefix>.srt, nil)` iff whisper-cli exits 0
// within the caller's ctx deadline. Failure modes are mapped as
// follows:
//
//   - Empty wavPath / modelPath / outPrefix ⇒ synchronous validation
//     error (never spawns a child) — whisper-cli would either
//     misinterpret the missing flag value or write to a bogus prefix,
//     so we reject up front.
//   - ctx cancelled/expired mid-run ⇒ [exec.CommandContext] kills the
//     child, we detect via ctx.Err() and return `asr: whisper:
//     <ctx.Err()>` with the sentinel wrapped for `errors.Is` matching
//     against [context.Canceled] / [context.DeadlineExceeded].
//   - Non-zero exit / spawn failure ⇒ error wraps *exec.ExitError (or
//     the exec spawn error) with the wav/prefix pair prepended, so
//     the pipeline log line at least tells the user which transcode
//     failed.
//
// Neither stdout nor stderr are consumed synchronously — they are
// wired to [WhisperRecognizer.Stdout] / [WhisperRecognizer.Stderr]
// (defaulting to [io.Discard]) and drained by exec's own goroutines.
func (w *WhisperRecognizer) Transcribe(ctx context.Context, wavPath, modelPath, outPrefix string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("asr: whisper: %w", err)
	}
	wavPath = strings.TrimSpace(wavPath)
	modelPath = strings.TrimSpace(modelPath)
	outPrefix = strings.TrimSpace(outPrefix)
	if wavPath == "" {
		return "", errors.New("asr: whisper: empty wav path")
	}
	if modelPath == "" {
		return "", errors.New("asr: whisper: empty model path")
	}
	if outPrefix == "" {
		return "", errors.New("asr: whisper: empty output prefix")
	}
	if strings.HasSuffix(strings.ToLower(outPrefix), ".srt") {
		return "", fmt.Errorf("asr: whisper: outPrefix %q must not end with .srt (whisper-cli appends the extension itself)", outPrefix)
	}

	bin := w.Binary
	if bin == "" {
		bin = "whisper-cli"
	}
	lang := w.Language
	if lang == "" {
		lang = "zh"
	}
	threads := w.Threads
	if threads <= 0 {
		threads = runtime.NumCPU()
	}

	// Argv order matters for reviewers cross-checking plan §5.5:
	// keep it identical to the doc string above.
	args := []string{
		"-m", modelPath,
		"-f", wavPath,
		"-l", lang,
		"-osrt",
		"-of", outPrefix,
		"--threads", strconv.Itoa(threads),
		"--print-progress",
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	if w.Stdout != nil {
		cmd.Stdout = w.Stdout
	} else {
		cmd.Stdout = io.Discard
	}
	if w.Stderr != nil {
		cmd.Stderr = w.Stderr
	} else {
		cmd.Stderr = io.Discard
	}

	if err := cmd.Run(); err != nil {
		// ctx-cancelled runs surface as *exec.ExitError from the
		// SIGKILL exec.CommandContext sends. Preferring ctx.Err()
		// here gives callers a wrapped sentinel they can errors.Is
		// against, matching contracts.md §四.1.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("asr: whisper: %w", ctxErr)
		}
		return "", fmt.Errorf("asr: whisper: %s -> %s.srt: %w", wavPath, outPrefix, err)
	}

	srtPath := outPrefix + ".srt"
	// Post-condition guard: contracts.md §二.3 mandates that the .srt
	// exists at the returned path after a successful call. A zero-exit
	// whisper-cli that produced nothing (edge cases: cwd removed mid-
	// run, disk full masked by buffered writes) would otherwise defer
	// the failure to subtitle.Parse with a confusing "no such file"
	// error. Fail loudly at the ASR boundary instead.
	if _, err := os.Stat(srtPath); err != nil {
		return "", fmt.Errorf("asr: whisper: exit 0 but %s missing: %w", srtPath, err)
	}
	abs, err := filepath.Abs(srtPath)
	if err != nil {
		return "", fmt.Errorf("asr: whisper: abs(%s): %w", srtPath, err)
	}
	return abs, nil
}

// Compile-time check: WhisperRecognizer satisfies [Recognizer]. If the
// interface signature drifts (contract update) the build breaks here
// before any caller notices.
var _ Recognizer = (*WhisperRecognizer)(nil)
