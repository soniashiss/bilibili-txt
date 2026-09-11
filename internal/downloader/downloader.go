// Package downloader wraps yt-dlp behind a narrow interface so the
// pipeline layer can drive metadata / subtitle / audio downloads
// without ever calling os/exec directly.
//
// The contract is frozen in docs/contracts.md §一.1 (Metadata) and
// §二.1 (Downloader). Any implementation — real ytdlp binary shell-out
// (T-2.2) or in-memory fake (this file's sibling [FakeDownloader]) —
// must obey identical semantics so pipeline tests can swap them
// without behavioural drift.
//
// # Sentinel errors
//
//   - [ErrNoSubtitle]     — every language in langPref missed. Pipeline
//     matches with errors.Is and routes to the ASR branch (exit code
//     unaffected, this is expected control flow, not a fatal error).
//   - [ErrVideoNotFound]  — yt-dlp reports "Video unavailable" or the
//     BV id resolves to a 404. Pipeline maps to exit code 1.
//
// Everything else (exec failed, non-executable, stderr blob, timeout,
// context cancelled) should be returned wrapped via
// `fmt.Errorf("...: %w", err)` so callers keep the root cause but
// stable sentinel checks still work on the two values above.
//
// # Context handling
//
// All three methods take ctx as their first arg. Implementations must
// honour ctx.Done() and return ctx.Err() (context.Canceled /
// context.DeadlineExceeded) as promptly as feasible. For exec-backed
// impls that means [os/exec.CommandContext] rather than [os/exec.Command].
//
// # Directory contract
//
// DownloadSubtitle / DownloadAudio require outDir to exist and be
// writable. They do NOT create outDir; the pipeline layer owns
// tmpDir/output-dir lifecycle (see contracts.md §五 tmpDir 责任).
package downloader

import (
	"context"
	"errors"
	"time"
)

// Metadata is the type-safe projection of `yt-dlp -J` output. Only the
// fields the pipeline actually consumes are kept — future needs
// (uploader, view count, thumbnail, …) may extend this struct, but
// existing fields' semantics are frozen by contracts.md §一.1.
type Metadata struct {
	// BVID is the canonical bilibili identifier, e.g. "BV1abcdefghij".
	// The caller (CLI / preflight) guarantees the shape; downstream
	// consumers may skip re-validation.
	BVID string
	// Title is the raw video title straight from yt-dlp, no slug
	// processing applied. naming.Slug is the layer that sanitises it.
	Title string
	// TotalPages is the number of parts (分P). Single-page videos
	// have 1. Zero or negative values are contract violations: the
	// downloader must never emit them.
	TotalPages int
	// Duration is the total video length as reported by yt-dlp's
	// `duration` field (seconds → time.Duration). For multi-page
	// videos yt-dlp returns the length of the currently selected
	// page, not the whole playlist — pipeline logs this verbatim so
	// operators can eyeball obvious mismatches (e.g. an audio-only
	// download that finished suspiciously fast for a 2h talk).
	// Zero means yt-dlp omitted the field; downstream must NOT
	// interpret zero as "instant video".
	Duration time.Duration
	// Subtitles are the official CC subtitles from yt-dlp's
	// `subtitles` field. May be empty. NOT merged with AutoCaptions —
	// the pipeline picks by its own language priority.
	Subtitles []SubtitleTrack
	// AutoCaptions are AI-generated captions from yt-dlp's
	// `automatic_captions` field. May be empty. See Subtitles.
	AutoCaptions []SubtitleTrack
}

// SubtitleTrack is one downloadable subtitle stream advertised by
// yt-dlp. See contracts.md §一.1 for the semantic split between
// [Metadata.Subtitles] (official CC) and [Metadata.AutoCaptions] (AI).
type SubtitleTrack struct {
	// Lang is the language tag as yt-dlp reports it, e.g. "zh-CN",
	// "zh-Hans", "ai-zh", "en".
	Lang string
	// Ext is the on-disk format: "srt", "vtt", "json", …. Pipeline
	// prefers "srt"; other formats are converted via yt-dlp's
	// `--convert-subs`.
	Ext string
	// URL is the direct download URL yt-dlp advertised. Optional:
	// yt-dlp -J usually includes it, but callers should not depend on
	// it being non-empty (yt-dlp handles the actual GET internally).
	URL string
}

// Downloader is the pipeline's window into yt-dlp. The real
// implementation shells out to yt-dlp; tests inject [FakeDownloader].
//
// See contracts.md §二.1 for the frozen signatures and side-effect
// contract; implementations must not deviate.
type Downloader interface {
	// FetchMetadata runs `yt-dlp -J <url>` and parses the JSON into a
	// [Metadata]. Idempotent: repeated calls with the same URL must
	// yield equivalent results (field ordering inside slices is
	// allowed to differ).
	//
	// Side effects: none — no files are written.
	FetchMetadata(ctx context.Context, url string) (*Metadata, error)

	// DownloadSubtitle runs yt-dlp with `--write-subs --sub-langs <lang>
	// --skip-download` (see plan.md §5.2 for the exact argv) for each
	// lang in langPref (in order); the first language yt-dlp produces
	// a file for wins. Returns the absolute path of the written .srt
	// file, named `<bvid>.<lang>.srt` (see contracts.md §二.1).
	//
	// Side effects: writes exactly one file under outDir. outDir MUST
	// exist and be writable — implementations may not create it.
	//
	// Errors:
	//   - [ErrNoSubtitle] if every language in langPref missed. The
	//     pipeline uses errors.Is to detect this and falls through to
	//     the ASR branch.
	//   - Wrapped exec / IO errors otherwise.
	DownloadSubtitle(ctx context.Context, url, outDir string, langPref []string) (srtPath string, err error)

	// DownloadAudio runs yt-dlp with `-f "bestaudio[ext=m4a]/bestaudio"
	// -x --audio-format m4a --audio-quality 5` (see plan.md §5.3 for
	// the frozen argv) and returns the absolute path of the resulting
	// .m4a, named `<bvid>.m4a` (see contracts.md §二.1). Idempotent
	// only in that repeated calls produce the same file; on-disk
	// state is overwritten if a stale file exists.
	//
	// Side effects: writes exactly one file under outDir. outDir MUST
	// exist and be writable.
	DownloadAudio(ctx context.Context, url, outDir string) (audioPath string, err error)
}

// ErrNoSubtitle is returned by [Downloader.DownloadSubtitle] when every
// language in langPref failed to produce a subtitle file. The pipeline
// layer matches with `errors.Is(err, ErrNoSubtitle)` to switch to the
// ASR branch. It is deliberately not wrapped in a struct so callers can
// use the sentinel directly; use `fmt.Errorf("...: %w", ErrNoSubtitle)`
// when adding context.
var ErrNoSubtitle = errors.New("downloader: no subtitle available for requested languages")

// ErrVideoNotFound is returned when yt-dlp reports the target video is
// unavailable (private, deleted, region-locked, wrong BV id). Pipeline
// maps this to exit code 1 via `errors.Is`. Wrap with fmt.Errorf when
// annotating the URL / BV id so the sentinel is preserved.
var ErrVideoNotFound = errors.New("downloader: video not found or unavailable")

// PreferredSubtitleLangs is the frozen language priority list (plan §5.1)
// shared between the pipeline's track-selection loop and the yt-dlp
// argv `--sub-langs` string. Pipeline picks the first metadata track
// whose Lang matches; FetchMetadata / DownloadSubtitle argv join the
// same list with commas so yt-dlp advertises exactly the languages
// pipeline is willing to consume.
//
// Exported as a package-level var (not const, Go quirk) but callers
// MUST NOT mutate it — treat as read-only.
var PreferredSubtitleLangs = []string{"zh-CN", "zh-Hans", "ai-zh"}
