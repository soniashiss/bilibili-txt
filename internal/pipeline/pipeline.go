// Package pipeline is the orchestration layer that glues the frozen
// contracts.md components together into one end-to-end transcript run:
// downloader → (subtitle or ASR) → parser → formatter → disk.
//
// T-2.4 introduced the [Run] entry point and implemented the **subtitle
// branch** in full (metadata → language selection → download srt →
// parse → format → write). T-3.3 extends the ASR side of the fork:
// download audio → transcode to 16 kHz mono wav → whisper ASR → parse
// srt → format → disk. Both branches funnel through the same
// [formatter.Convert] terminal step so downstream (CLI, logger, tests)
// observe an identical [Result] shape.
//
// # Contract highlights (see docs/contracts.md §一.3 / 四 / 五)
//
//   - Dependencies are injected through [Deps] — no package-level exec,
//     no direct instantiation of yt-dlp / ffmpeg / whisper-cli. Pipeline
//     unit tests wire in [downloader.FakeDownloader] / [audio.FakeTranscoder]
//     / [asr.FakeRecognizer] and exercise every path without touching a
//     real binary.
//   - Language priority for the subtitle branch is zh-CN > zh-Hans > ai-zh
//     (plan §5.1). Official CC tracks in [downloader.Metadata.Subtitles]
//     rank ahead of AI captions in [downloader.Metadata.AutoCaptions];
//     the result's [Source] reflects which pool won.
//   - Conflict resolution is done *before* any heavy download work
//     (see [naming.ResolveConflict]) so users get an "already exists"
//     verdict without waiting on the network.
//   - tmpDir management: [Run] owns the "bilibili-txt-<bvid>-*" tmp dir
//     under [os.TempDir]; when [Input.KeepIntermediate] is false the dir
//     is torn down on exit even on error paths. Interfaces themselves
//     do not touch tmpDir (contracts.md §五 tmpDir 责任).
//
// # Design constraints (why the signature looks the way it does)
//
// The final [Run] signature is
//
//	Run(ctx context.Context, in Input, cfg *config.Config, deps Deps) (*Result, error)
//
// even though plan §4.4 sketches `Run(ctx, url, cfg)`. Reasons:
//
//   - We MUST inject the three exec-fronted interfaces so pipeline tests
//     never depend on a real yt-dlp. [Deps] is the injection point.
//   - The v1 CLI feeds one URL per invocation, but future callers (LSP
//     server, HTTP wrapper) may want to pass in more per-run knobs
//     (force-asr, keep-intermediate, language override). Bundling them
//     inside [Input] keeps the surface stable when we add fields.
//   - Returning `*Result` (rather than just an error) matches
//     contracts.md §一.3 and lets the CLI layer report OutputPath /
//     Source without a second stat call.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bilibili-txt/internal/asr"
	"bilibili-txt/internal/audio"
	"bilibili-txt/internal/config"
	"bilibili-txt/internal/downloader"
	"bilibili-txt/internal/formatter"
	"bilibili-txt/internal/logger"
	"bilibili-txt/internal/naming"
	"bilibili-txt/internal/subtitle"
)

// Source enumerates the four ways a run may produce its output.
// See contracts.md §一.3 — the constant values are wire-visible in
// logs and Result JSON, so they are frozen strings.
type Source string

const (
	// SourceSubtitle — the transcript came from an official CC
	// track in [downloader.Metadata.Subtitles].
	SourceSubtitle Source = "cc-subtitle"
	// SourceAuto — the transcript came from an AI-generated caption
	// in [downloader.Metadata.AutoCaptions].
	SourceAuto Source = "auto-caption"
	// SourceASR — the transcript came from local whisper.cpp ASR.
	// T-3.3 populates this on both the ForceASR and no-subtitle
	// fallback paths.
	SourceASR Source = "asr"
	// SourceCached — the target output already existed on disk and
	// the conflict strategy resolved to skip. No new bytes were
	// written; [Result.OutputPath] points at the pre-existing file.
	SourceCached Source = "cached"
)

// Result is what [Run] returns on success. Frozen by contracts.md §一.3.
type Result struct {
	// OutputPath is the absolute path of the produced transcript.
	// For [SourceCached] it points at the pre-existing file.
	OutputPath string
	// Source records which branch produced the transcript.
	Source Source
	// Duration is the wall-clock time [Run] took, sampled with
	// [time.Since] against the value captured at Run entry. Callers
	// use this for the "took=…" tail in success logs.
	Duration time.Duration
	// Metadata is the raw [downloader.Metadata] fetched during
	// step 1; callers can lift Title / BVID for CLI banners without
	// re-hitting yt-dlp.
	Metadata *downloader.Metadata
}

// Input bundles the per-run knobs [Run] cannot infer from [config.Config].
// New knobs go here — never as bare arguments to [Run] — so the signature
// stays stable across milestones.
type Input struct {
	// URL is the raw bilibili video URL or bare BVID the CLI passes
	// through. The downloader is responsible for normalising it
	// (yt-dlp accepts both forms); the pipeline does not validate.
	URL string
	// ForceASR bypasses the subtitle branch entirely and routes
	// straight into the ASR flow, matching plan §4.5's --force-asr.
	// Since T-3.3 the ASR branch runs end-to-end and produces a
	// [SourceASR] [Result]; the T-2.4 stub that returned a
	// not-implemented sentinel has been removed.
	ForceASR bool
	// KeepIntermediate leaves the tmpDir on disk after [Run] exits.
	// Mirrors plan §4.5's --keep-intermediate; useful when a real
	// user needs to inspect the raw yt-dlp / whisper artefacts.
	KeepIntermediate bool
	// IsTTY / NoInteractive plumb the user-facing conflict UX into
	// [naming.ResolveConflict]. The CLI layer detects TTY once and
	// forwards it here so the pipeline itself never has to look at
	// os.Stdout.
	IsTTY         bool
	NoInteractive bool
	// Prompter is invoked when the conflict strategy is "ask" and
	// the process is attached to a TTY. May be nil (see
	// [naming.ResolveConflict] — nil is rejected iff a prompt is
	// actually required).
	Prompter naming.Prompter
}

// Deps is the injection point for the three exec-fronted interfaces.
// All three are required for a fully wired production run; unit tests
// typically fill in only the ones a specific branch exercises and
// leave the others nil (helpers guard against nil-deref via
// [errDepsMissing] before calling any method).
type Deps struct {
	Downloader downloader.Downloader
	Transcoder audio.Transcoder
	Recognizer asr.Recognizer
}

// ErrConflictUnresolved wraps every "target output already exists and
// the user (or the environment) told us to stop" outcome from
// [resolveConflictIfExists]. The CLI layer uses [errors.Is] against
// this sentinel to map to exit code 3 per contracts.md §四 without
// coupling to the underlying naming.ResolveConflict message text.
//
// The wrapped errors always carry the offending path in their message
// so users don't have to correlate log lines to figure out which file
// blocked the run.
var ErrConflictUnresolved = errors.New("pipeline: output conflict unresolved")

// preferredLanguages is the frozen language priority list for the
// subtitle branch (plan §5.1). Sourced from
// [downloader.PreferredSubtitleLangs] so the pipeline's track-selection
// loop and the yt-dlp argv (`--sub-langs`) can never drift apart.
// We try each in order; the first track (across
// [downloader.Metadata.Subtitles] first, then
// [downloader.Metadata.AutoCaptions]) whose Lang matches wins.
//
// Callers MUST NOT mutate it — treat as read-only.
var preferredLanguages = downloader.PreferredSubtitleLangs

// Run is the entry point. See package doc for the frozen contract.
//
// Execution outline (T-2.4 scope):
//  1. Validate [Deps] / [Input] — nil dependencies fail fast so a mis-wired
//     CLI never reaches a real yt-dlp call.
//  2. FetchMetadata — errors from [downloader.ErrVideoNotFound] propagate
//     as-is (contracts.md §四 maps them to exit 1); other errors are
//     wrapped with "fetch metadata: %w".
//  3. Resolve the target output path via [naming.BuildPath], check for
//     conflicts, and short-circuit into [SourceCached] when the user
//     picks skip. Overwrite/abort are dispatched through
//     [naming.ResolveConflict] and the resulting action determines
//     whether Step 4 runs.
//  4. Branch selection:
//     - If [Input.ForceASR] → runASRBranch (T-3.3: full download-audio
//     → transcode → transcribe → parse → format flow).
//     - Else pick the highest-priority subtitle track and run
//     runSubtitleBranch. If no track matches, fall back to ASR.
//  5. Both branches call [formatter.Convert] against the pre-computed
//     [outPath]; tmpDir cleanup runs via defer regardless of success.
//
// The named return `err` is retained purely so the deferred tmpDir
// cleanup can read the pipeline's outgoing error and log it alongside
// any cleanup failure (see the defer block below). Cleanup NEVER
// overwrites `err` — a leaking tmpDir must not invalidate a valid
// [Result] nor mask a real root cause (P2-3 from T-3.3 review).
func Run(ctx context.Context, in Input, cfg *config.Config, deps Deps) (result *Result, err error) {
	start := time.Now()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("pipeline: nil config")
	}
	if strings.TrimSpace(in.URL) == "" {
		return nil, errors.New("pipeline: empty url")
	}
	if deps.Downloader == nil {
		return nil, errDepsMissing("downloader")
	}

	// Step 1: metadata.
	stepStart := time.Now()
	logger.StepStart("metadata", "url", in.URL)
	meta, err := deps.Downloader.FetchMetadata(ctx, in.URL)
	if err != nil {
		logger.StepFail("metadata", time.Since(stepStart), err, "url", in.URL)
		return nil, fmt.Errorf("pipeline: fetch metadata: %w", err)
	}
	if meta == nil {
		err := errors.New("pipeline: downloader returned nil metadata")
		logger.StepFail("metadata", time.Since(stepStart), err, "url", in.URL)
		return nil, err
	}
	durKV := meta.Duration.String()
	if meta.Duration == 0 {
		durKV = "unknown"
	}
	logger.StepDone("metadata", time.Since(stepStart),
		"title", meta.Title, "bvid", meta.BVID,
		"duration", durKV,
		"has_cc", len(meta.Subtitles) > 0, "has_auto", len(meta.AutoCaptions) > 0)

	// Step 2: compute target output path + conflict check.
	format := formatter.Format(strings.ToLower(strings.TrimSpace(cfg.Format)))
	if err := validateFormat(format); err != nil {
		return nil, err
	}
	outPath, err := targetOutputPath(cfg.OutputDir, meta, format)
	if err != nil {
		return nil, err
	}

	skipAsCached, cerr := resolveConflictIfExists(outPath, cfg, in)
	if cerr != nil {
		return nil, cerr
	}
	if skipAsCached {
		logger.Default().Slog().Info("skip: output already exists", "path", outPath, "bvid", meta.BVID)
		return &Result{
			OutputPath: outPath,
			Source:     SourceCached,
			Duration:   time.Since(start),
			Metadata:   meta,
		}, nil
	}
	// Overwrite path: fall through into the branches below which
	// will os.Create-truncate the file. Abort was already turned
	// into an error by resolveConflictIfExists.

	// Ensure the output directory exists — preflight would normally
	// have created it, but --skip-preflight (and any future caller
	// that bypasses preflight) still expects a working pipeline.
	// MkdirAll is idempotent, so double-work here is cheap and safe.
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return nil, fmt.Errorf("pipeline: mkdir output dir: %w", err)
	}

	// Prepare tmpDir for downloader + intermediate files.
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("bilibili-txt-%s-*", meta.BVID))
	if err != nil {
		return nil, fmt.Errorf("pipeline: mkdir tmp: %w", err)
	}
	defer func() {
		if in.KeepIntermediate {
			return
		}
		if rmErr := os.RemoveAll(tmpDir); rmErr != nil {
			// cleanup is best-effort: a leaking tmpDir must never
			// invalidate an otherwise-successful [Result] nor mask
			// a real error's root cause. Downgraded to WARN in both
			// cases so `err != nil ⇒ result invalid` stays a hard
			// invariant callers can rely on (P2-3 from T-3.3
			// second-pass review). Operators still see the leak in
			// logs and can `rm -rf $TMPDIR/bilibili-txt-*` manually.
			logger.Default().Slog().Warn("pipeline: cleanup tmp failed",
				"path", tmpDir, "cleanup_err", rmErr, "main_err", err)
		}
	}()

	// Step 3: branch selection.
	if in.ForceASR {
		return runASRBranch(ctx, in, cfg, deps, meta, outPath, tmpDir, format, start)
	}

	track, ok := pickSubtitleTrack(meta)
	if !ok {
		// No tracks at all → straight into ASR (no down-attempt).
		return runASRBranch(ctx, in, cfg, deps, meta, outPath, tmpDir, format, start)
	}

	res, err := runSubtitleBranch(ctx, in, deps, meta, track, outPath, tmpDir, format, start)
	if err == nil {
		return res, nil
	}
	// contracts.md §四 clause 3: three subtitle-branch failure modes
	// are semantically equivalent to "there is no usable caption
	// track" and MUST fall through to ASR rather than fail the run —
	//
	//   - downloader.ErrNoSubtitle: yt-dlp advertised a language
	//     that 404s on fetch (the classic "mirage" track).
	//   - subtitle.ErrEmpty: the file arrived but contains zero
	//     well-formed cues (yt-dlp occasionally hands back a
	//     zero-byte srt for stubbed captions).
	//   - subtitle.ErrMalformed: the file arrived but its timing
	//     lines are garbled beyond repair, so no cue can be
	//     recovered.
	//
	// In all three cases the user is better served by transparently
	// running ASR than by a hard failure; F2 in the T-3.3 review
	// pinned this behaviour. Any other error (network flake, disk
	// full, ctx cancel, etc.) is fatal and propagates as-is.
	if errors.Is(err, downloader.ErrNoSubtitle) ||
		errors.Is(err, subtitle.ErrEmpty) ||
		errors.Is(err, subtitle.ErrMalformed) {
		return runASRBranch(ctx, in, cfg, deps, meta, outPath, tmpDir, format, start)
	}
	return nil, err
}

// runSubtitleBranch handles the "download srt → parse → format" leg.
// Wraps every step with `fmt.Errorf("pipeline: <step>: %w", err)` so
// callers can `errors.Is` all the way through to sentinels like
// [subtitle.ErrEmpty] / [downloader.ErrNoSubtitle].
func runSubtitleBranch(
	ctx context.Context,
	in Input,
	deps Deps,
	meta *downloader.Metadata,
	track selectedTrack,
	outPath string,
	tmpDir string,
	format formatter.Format,
	start time.Time,
) (*Result, error) {
	dlStart := time.Now()
	logger.StepStart("download-subtitle", "lang", track.lang, "source", string(track.source), "bvid", meta.BVID)
	srtPath, err := deps.Downloader.DownloadSubtitle(ctx, in.URL, tmpDir, []string{track.lang})
	if err != nil {
		logger.StepFail("download-subtitle", time.Since(dlStart), err, "lang", track.lang)
		return nil, fmt.Errorf("pipeline: download subtitle: %w", err)
	}
	logger.StepDone("download-subtitle", time.Since(dlStart), "srt", srtPath)

	// [subtitle.Parse] pre-dates ctx-aware plumbing (contracts.md §一.2),
	// so a ^C between DownloadSubtitle exit and parse start would
	// otherwise silently run parse + format on the already-downloaded
	// srt. Mirror the ASR branch's pre-parse gate (see runASRBranch
	// ctx.Err() bracket) so both branches honour contracts.md §四.1
	// symmetrically.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("pipeline: parse subtitle: %w", err)
	}
	parseStart := time.Now()
	logger.StepStart("parse-subtitle", "srt", srtPath)
	cues, err := subtitle.Parse(srtPath)
	if err != nil {
		logger.StepFail("parse-subtitle", time.Since(parseStart), err, "srt", srtPath)
		return nil, fmt.Errorf("pipeline: parse subtitle: %w", err)
	}
	logger.StepDone("parse-subtitle", time.Since(parseStart), "cues", len(cues))

	// Mirror the ASR branch's pre-format gate: [formatter.Convert]
	// likewise pre-dates ctx-aware plumbing, and a ^C between parse
	// and format should avoid writing a partial transcript to disk.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("pipeline: convert: %w", err)
	}
	convStart := time.Now()
	logger.StepStart("format", "format", string(format), "out", outPath)
	if err := formatter.Convert(cues, outPath, format); err != nil {
		logger.StepFail("format", time.Since(convStart), err, "out", outPath)
		return nil, fmt.Errorf("pipeline: convert: %w", err)
	}
	logger.StepDone("format", time.Since(convStart), "out", outPath)

	return &Result{
		OutputPath: outPath,
		Source:     track.source,
		Duration:   time.Since(start),
		Metadata:   meta,
	}, nil
}

// runASRBranch handles the "download audio → transcode → whisper →
// parse → format" leg. It mirrors [runSubtitleBranch]'s wrapping and
// logging conventions so callers can `errors.Is` all the way through
// to sentinels like [subtitle.ErrEmpty], [context.Canceled], or the
// low-level exec errors surfaced by [downloader.YtdlpDownloader] /
// [audio.FfmpegTranscoder] / [asr.WhisperRecognizer].
//
// Intermediate file layout under `tmpDir` (contract-driven, see
// plan §4.4 and the [audio.Transcoder] / [asr.Recognizer] contracts):
//
//   - Audio downloaded by [downloader.Downloader.DownloadAudio] —
//     the callee picks the extension (m4a in the default yt-dlp
//     invocation), so we do not hard-code it here; whatever path
//     the downloader returns is fed straight into the transcoder.
//   - Wav produced by the transcoder at `<tmpDir>/<bvid>.wav`. The
//     naming is deterministic so `--keep-intermediate` produces a
//     predictable artefact set users can inspect / re-run whisper on.
//   - Whisper's outPrefix is `<tmpDir>/<bvid>` (WITHOUT `.srt`, as
//     the [asr.Recognizer] contract mandates); the recognizer
//     appends `.srt` for us and returns the absolute path.
//
// tmpDir cleanup itself is owned by [Run]'s deferred [os.RemoveAll]
// (see the outer function); this branch only writes into it.
func runASRBranch(
	ctx context.Context,
	in Input,
	cfg *config.Config,
	deps Deps,
	meta *downloader.Metadata,
	outPath string,
	tmpDir string,
	format formatter.Format,
	start time.Time,
) (*Result, error) {
	// Defensive check so a caller that injects only Downloader gets
	// a clear "you also need Transcoder + Recognizer" error rather
	// than a nil-deref panic.
	if deps.Transcoder == nil {
		return nil, errDepsMissing("transcoder")
	}
	if deps.Recognizer == nil {
		return nil, errDepsMissing("recognizer")
	}
	// Fast-fail on empty model path so users bypassing preflight
	// (or setting `model: ""` in config.yaml with --skip-preflight)
	// see a pipeline-level error rather than a whisper-cli argv
	// failure surfacing three layers deep as
	// `pipeline: transcribe: asr: whisper: empty model path`
	// (P2-2 from T-3.3 second-pass review).
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("pipeline: asr requires cfg.Model (whisper.cpp ggml model path); run preflight or set config.model")
	}

	// Step 1: download audio. The downloader owns the extension
	// choice (m4a in production, m4a via testdata in the fake), so
	// we thread the returned path forward untouched.
	dlStart := time.Now()
	logger.StepStart("download-audio", "url", in.URL, "bvid", meta.BVID)
	audioPath, err := deps.Downloader.DownloadAudio(ctx, in.URL, tmpDir)
	if err != nil {
		logger.StepFail("download-audio", time.Since(dlStart), err, "url", in.URL)
		return nil, fmt.Errorf("pipeline: download audio: %w", err)
	}
	logger.StepDone("download-audio", time.Since(dlStart), "audio", audioPath)

	// Step 2: transcode to 16 kHz mono 16-bit PCM WAV. We hard-code
	// `<bvid>.wav` under tmpDir (as opposed to reading a path back
	// from the transcoder) because the wav is a transient artefact
	// whose only consumer is whisper-cli in the very next step —
	// keeping the naming pipeline-side means `--keep-intermediate`
	// output is predictable regardless of which [audio.Transcoder]
	// backend is wired in.
	wavPath := filepath.Join(tmpDir, meta.BVID+".wav")
	txStart := time.Now()
	logger.StepStart("transcode", "in", audioPath, "out", wavPath)
	if err := deps.Transcoder.ToWav16kMono(ctx, audioPath, wavPath); err != nil {
		logger.StepFail("transcode", time.Since(txStart), err, "in", audioPath, "out", wavPath)
		return nil, fmt.Errorf("pipeline: transcode: %w", err)
	}
	logger.StepDone("transcode", time.Since(txStart), "wav", wavPath)

	// Step 3: run whisper. outPrefix intentionally omits `.srt` per
	// the [asr.Recognizer] contract — the recognizer appends the
	// extension itself and returns the absolute srt path.
	outPrefix := filepath.Join(tmpDir, meta.BVID)
	asrStart := time.Now()
	logger.StepStart("transcribe", "wav", wavPath, "model", cfg.Model)
	srtPath, err := deps.Recognizer.Transcribe(ctx, wavPath, cfg.Model, outPrefix)
	if err != nil {
		logger.StepFail("transcribe", time.Since(asrStart), err, "wav", wavPath)
		return nil, fmt.Errorf("pipeline: transcribe: %w", err)
	}
	logger.StepDone("transcribe", time.Since(asrStart), "srt", srtPath)

	// Step 4: parse the srt into cues. The [subtitle.ErrEmpty]
	// sentinel is preserved via %w so the CLI layer can map it to
	// the same exit code as an empty CC track.
	//
	// [subtitle.Parse] does not currently accept a ctx (its
	// contract in contracts.md §一.2 predates T-3.3), so we bracket
	// the call with explicit ctx.Err() gates. In practice parse
	// runs <100ms even on large srt inputs, but the gate ensures a
	// ^C between whisper exit and parse start still short-circuits
	// with context.Canceled rather than silently completing the
	// formatter step (P2-CO-1 from T-3.3 review).
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("pipeline: parse asr: %w", err)
	}
	parseStart := time.Now()
	logger.StepStart("parse-asr", "srt", srtPath)
	cues, err := subtitle.Parse(srtPath)
	if err != nil {
		logger.StepFail("parse-asr", time.Since(parseStart), err, "srt", srtPath)
		return nil, fmt.Errorf("pipeline: parse asr: %w", err)
	}
	logger.StepDone("parse-asr", time.Since(parseStart), "cues", len(cues))

	// Step 5: format + write. Identical to runSubtitleBranch's tail
	// so both branches emit the same [Result] shape. Mirror the
	// pre-parse gate: [formatter.Convert] likewise pre-dates
	// ctx-aware plumbing, and a ^C between parse and format should
	// avoid writing a partial transcript to disk.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("pipeline: convert: %w", err)
	}
	convStart := time.Now()
	logger.StepStart("format", "format", string(format), "out", outPath)
	if err := formatter.Convert(cues, outPath, format); err != nil {
		logger.StepFail("format", time.Since(convStart), err, "out", outPath)
		return nil, fmt.Errorf("pipeline: convert: %w", err)
	}
	logger.StepDone("format", time.Since(convStart), "out", outPath)

	return &Result{
		OutputPath: outPath,
		Source:     SourceASR,
		Duration:   time.Since(start),
		Metadata:   meta,
	}, nil
}

// selectedTrack is what pickSubtitleTrack returns: the winning
// language tag together with the [Source] pool it came from.
type selectedTrack struct {
	lang   string
	source Source
}

// pickSubtitleTrack applies the [preferredLanguages] priority list to
// [downloader.Metadata], preferring official [downloader.Metadata.Subtitles]
// over [downloader.Metadata.AutoCaptions]. Returns ok=false only when
// BOTH pools are empty — a caller-visible signal that we should skip
// straight to ASR without any download attempt.
//
// The "official CC wins over AI captions of a higher-priority language"
// nuance matters: a video that advertises `Subtitles: [{Lang:"zh-Hans"}]`
// AND `AutoCaptions: [{Lang:"zh-CN"}]` should surface as
// [SourceSubtitle]+zh-Hans, NOT [SourceAuto]+zh-CN. The rationale is
// contracts.md §一.1's "Subtitles and AutoCaptions are not merged" —
// treat them as ranked pools, not one unified list.
func pickSubtitleTrack(meta *downloader.Metadata) (selectedTrack, bool) {
	if len(meta.Subtitles) == 0 && len(meta.AutoCaptions) == 0 {
		return selectedTrack{}, false
	}
	if t, ok := firstMatch(meta.Subtitles); ok {
		return selectedTrack{lang: t.Lang, source: SourceSubtitle}, true
	}
	if t, ok := firstMatch(meta.AutoCaptions); ok {
		return selectedTrack{lang: t.Lang, source: SourceAuto}, true
	}
	// Both pools non-empty but none of their languages match our
	// priority list. Fall through to ASR — v1 does not support
	// arbitrary language transcripts.
	return selectedTrack{}, false
}

// firstMatch returns the first track whose Lang appears in
// [preferredLanguages], iterating [preferredLanguages] as the outer
// loop so priority order is honoured (a zh-Hans track always beats a
// later zh-CN one when zh-CN appears first in the priority list).
func firstMatch(tracks []downloader.SubtitleTrack) (downloader.SubtitleTrack, bool) {
	for _, want := range preferredLanguages {
		for _, tr := range tracks {
			if tr.Lang == want {
				return tr, true
			}
		}
	}
	return downloader.SubtitleTrack{}, false
}

// resolveConflictIfExists inspects [outPath] and, if it exists, applies
// the user-configured [naming.ResolveConflict] strategy. It collapses the
// three [naming.ConflictAction] values into the two-way branch the
// pipeline actually needs:
//
//   - `skipAsCached=true, err=nil` — the file exists and the strategy is
//     [naming.ActionSkip]. The caller should short-circuit and return a
//     [SourceCached] result without any download work.
//   - `skipAsCached=false, err=nil` — either the file does not exist, or
//     the strategy resolved to [naming.ActionOverwrite]. The caller
//     proceeds to the download/format branch, which will truncate the
//     target on write.
//   - `skipAsCached=false, err!=nil` — either os.Stat failed for a
//     non-ENOENT reason, or the strategy resolved to [naming.ActionAbort]
//     (or ResolveConflict itself returned an error, e.g. "ask" degraded
//     to abort under --no-interactive). The pipeline surfaces the error
//     up to the CLI, which maps it to exit code 3 per contracts.md §四.
func resolveConflictIfExists(outPath string, cfg *config.Config, in Input) (skipAsCached bool, err error) {
	if _, statErr := os.Stat(outPath); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("pipeline: stat output %s: %w", outPath, statErr)
	}
	action, rcErr := naming.ResolveConflict(outPath, naming.Options{
		Strategy:      cfg.Naming.OnConflict,
		IsTTY:         in.IsTTY,
		NoInteractive: in.NoInteractive,
		Prompter:      in.Prompter,
	})
	if rcErr != nil {
		// naming.ResolveConflict already produced a user-facing
		// message (mentions the path, mentions the culprit flag).
		// Wrap it in ErrConflictUnresolved so the CLI exit-code
		// mapper picks it up without re-parsing strings.
		return false, fmt.Errorf("%w: %s", ErrConflictUnresolved, rcErr.Error())
	}
	switch action {
	case naming.ActionOverwrite:
		logger.Default().Slog().Warn("conflict detected",
			"existing", outPath, "action", "overwrite")
		return false, nil
	case naming.ActionSkip:
		logger.Default().Slog().Warn("conflict detected",
			"existing", outPath, "action", "skip")
		return true, nil
	case naming.ActionAbort:
		return false, fmt.Errorf("%w: %s (on_conflict=abort)", ErrConflictUnresolved, outPath)
	default:
		// Defence in depth: naming.ResolveConflict only returns the
		// three enumerated actions above, but if a future refactor
		// widens the enum without updating this switch we still
		// want the CLI's `errors.Is(err, ErrConflictUnresolved)`
		// mapping to exit code 3 (contracts.md §四.3) — not to
		// silently fall through to exit 1.
		return false, fmt.Errorf("%w: unknown conflict action %v for %s", ErrConflictUnresolved, action, outPath)
	}
}

// targetOutputPath resolves [cfg.OutputDir] + slug + bvid + ext into
// the absolute path [formatter.Convert] should write to. Panics from
// [naming.BuildPath] (invalid bvid / ext) are recovered here and
// surfaced as a normal error so a malformed [downloader.Metadata]
// cannot crash the pipeline.
func targetOutputPath(outputDir string, meta *downloader.Metadata, format formatter.Format) (path string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pipeline: build output path: %v", r)
			path = ""
		}
	}()
	path = naming.BuildPath(outputDir, meta.Title, meta.BVID, 1, meta.TotalPages, string(format))
	abs, aerr := filepath.Abs(path)
	if aerr != nil {
		return "", fmt.Errorf("pipeline: abs %s: %w", path, aerr)
	}
	return abs, nil
}

// validateFormat rejects [formatter.Format] values [formatter.Convert]
// would refuse, letting the pipeline fail fast before any tmpDir /
// network work. Keeping the check here (rather than trusting Convert
// to catch it) means an unknown format never leaves a phantom tmpDir.
func validateFormat(f formatter.Format) error {
	switch f {
	case formatter.FormatTXT, formatter.FormatMD, formatter.FormatSRT:
		return nil
	default:
		return fmt.Errorf("pipeline: %w: %q", formatter.ErrUnknownFormat, f)
	}
}

// errDepsMissing builds the standard "you forgot to inject X" error
// used across every branch's defensive nil checks.
func errDepsMissing(name string) error {
	return fmt.Errorf("pipeline: deps.%s is nil", name)
}
