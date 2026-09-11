package downloader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// YtdlpDownloader is the production [Downloader] that shells out to
// the yt-dlp binary. All three method families lock their argv to the
// recipes frozen in plan §5.1–§5.3; any drift will be caught by
// [ytdlp_test.go] argv-shape assertions.
//
// The struct's zero value is usable: [Binary] defaults to the string
// "yt-dlp" (looked up via the ambient $PATH by [exec.CommandContext])
// and [Stderr] defaults to [io.Discard]. Callers that need per-tool
// debug logs (see [logger.StderrSink]) inject their own Stderr;
// callers that pinned a yaml-supplied binary path (see preflight
// two-tier fallback) inject the resolved absolute path into [Binary]
// so we do not re-do the $PATH lookup at every call site.
//
// Concurrency: safe to call concurrently across methods (no shared
// mutable state). Pipeline drives it serially per URL, so this is
// just a "no surprises" guarantee, not a design goal.
type YtdlpDownloader struct {
	// Binary is the yt-dlp command name or absolute path. Empty ⇒
	// "yt-dlp", resolved via $PATH. Passing an absolute path here is
	// how preflight-resolved paths reach this layer.
	Binary string
	// Stderr is where the child process's stderr is drained. nil ⇒
	// [io.Discard]. Wire logger.StderrSink("yt-dlp") here in
	// production; leave nil in tests that do not care about the
	// child's chatter.
	//
	// Implementation note: we internally tee stderr into a small
	// bounded buffer so we can inspect it for the "Video unavailable"
	// keyword AFTER a non-zero exit. The caller's Stderr writer
	// always receives the full stream — the internal capture is only
	// used for keyword scanning, never surfaced back.
	Stderr io.Writer
	// CookiesFromBrowser, when non-empty and not equal to the sentinel
	// "none" (case-insensitive), causes all three yt-dlp invocations
	// (FetchMetadata / DownloadSubtitle / DownloadAudio) to be
	// prefixed with `--cookies-from-browser <value>`. The value is
	// passed through verbatim; yt-dlp is the authority on what
	// browsers / profile suffixes are legal. See [config.Auth] for
	// the config-layer contract and the "chrome" default.
	CookiesFromBrowser string
}

// stderrCaptureLimit caps the internal keyword-scan buffer. yt-dlp's
// error line for a missing video is ~120 bytes; 8 KiB is far above
// any realistic error blob while preventing a runaway child that
// spams megabytes of warnings from ballooning our RSS. Bytes past
// the cap still reach the caller's Stderr writer via [io.MultiWriter]
// — only the keyword scan gets a truncated view, which is fine
// because the sentinel keywords all appear in the first line yt-dlp
// prints on a not-found error.
const stderrCaptureLimit = 8 * 1024

// ytdlpMetadata is the on-the-wire projection of `yt-dlp -J` we
// actually consume. yt-dlp emits many more fields (uploader,
// thumbnail, formats[], webpage_url, …) but we deliberately
// only decode the pipeline-relevant subset — smaller struct, faster
// json.Unmarshal, zero risk of drift when yt-dlp adds fields.
type ytdlpMetadata struct {
	ID            string                     `json:"id"`
	Title         string                     `json:"title"`
	PlaylistCount int                        `json:"playlist_count"`
	Duration      float64                    `json:"duration"`
	Subtitles     map[string][]ytdlpSubtitle `json:"subtitles"`
	AutoCaptions  map[string][]ytdlpSubtitle `json:"automatic_captions"`
}

type ytdlpSubtitle struct {
	Ext string `json:"ext"`
	URL string `json:"url"`
}

// FetchMetadata implements [Downloader]. Runs the plan §5.1 recipe:
//
//	yt-dlp --no-warnings --dump-single-json \
//	  --write-subs --write-auto-subs \
//	  --sub-langs "zh-CN,zh-Hans,ai-zh" \
//	  --skip-download <URL>
//
// The JSON blob on stdout is decoded into [Metadata]. Errors fall
// through the standard envelope `downloader: ytdlp: ...`.
func (y *YtdlpDownloader) FetchMetadata(ctx context.Context, url string) (*Metadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("downloader: ytdlp: %w", err)
	}
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("downloader: ytdlp: empty url")
	}

	args := []string{
		"--no-warnings",
		"--dump-single-json",
		"--write-subs", "--write-auto-subs",
		"--sub-langs", strings.Join(PreferredSubtitleLangs, ","),
		"--skip-download",
		url,
	}
	args = y.withAuth(args)

	var stdout bytes.Buffer
	if err := y.run(ctx, args, &stdout); err != nil {
		return nil, err
	}

	var raw ytdlpMetadata
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("downloader: ytdlp: parse metadata json: %w", err)
	}
	return toMetadata(&raw), nil
}

// DownloadSubtitle implements [Downloader]. Runs the plan §5.2
// recipe ONCE with the full comma-joined langPref list, then scans
// outDir in langPref order for a `*.<lang>.srt` file — the first
// language yt-dlp produced a file for wins. Empty result on every
// lang ⇒ wrapped [ErrNoSubtitle].
//
//	yt-dlp --no-warnings --write-subs --write-auto-subs \
//	  --sub-langs "<lang1>,<lang2>,..." --sub-format "srt/best" \
//	  --convert-subs srt --skip-download \
//	  -o "%(id)s.%(ext)s" --paths <outDir> <URL>
//
// Single-shot invocation matches the argv frozen in plan §5.2
// literally; picking the winner by scanning outDir in langPref order
// preserves the "attribution" semantic (caller learns which lang
// landed) without paying for N exec spawns.
func (y *YtdlpDownloader) DownloadSubtitle(ctx context.Context, url, outDir string, langPref []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("downloader: ytdlp: %w", err)
	}
	if strings.TrimSpace(url) == "" {
		return "", errors.New("downloader: ytdlp: empty url")
	}
	if strings.TrimSpace(outDir) == "" {
		return "", errors.New("downloader: ytdlp: empty outDir")
	}
	if len(langPref) == 0 {
		return "", errors.New("downloader: ytdlp: empty langPref")
	}
	if err := statDir(outDir); err != nil {
		return "", err
	}

	args := []string{
		"--no-warnings",
		"--write-subs", "--write-auto-subs",
		"--sub-langs", strings.Join(langPref, ","),
		"--sub-format", "srt/best",
		"--convert-subs", "srt",
		"--skip-download",
		"-o", "%(id)s.%(ext)s",
		"--paths", outDir,
		url,
	}
	args = y.withAuth(args)
	if err := y.run(ctx, args, io.Discard); err != nil {
		return "", err
	}

	// Iterate langPref order so the first-preference language wins
	// even if yt-dlp produced multiple `<id>.<lang>.srt` files.
	// Suffix match `.<lang>.srt` — pipeline gives us a fresh tmp dir
	// per URL (see contracts.md §五 tmpDir 责任), so no stale-file
	// ambiguity.
	for _, lang := range langPref {
		hit, err := findSubtitleForLang(outDir, lang)
		if err != nil {
			return "", fmt.Errorf("downloader: ytdlp: scan %s for %s: %w", outDir, lang, err)
		}
		if hit != "" {
			abs, err := filepath.Abs(hit)
			if err != nil {
				return "", fmt.Errorf("downloader: ytdlp: abs %s: %w", hit, err)
			}
			return abs, nil
		}
	}
	return "", fmt.Errorf("downloader: ytdlp: %w", ErrNoSubtitle)
}

// DownloadAudio implements [Downloader]. Runs the plan §5.3 recipe:
//
//	yt-dlp --no-warnings \
//	  -f "bestaudio[ext=m4a]/bestaudio" \
//	  -x --audio-format m4a --audio-quality 5 \
//	  -o "%(id)s.%(ext)s" --paths <outDir> <URL>
//
// After a successful exit yt-dlp will have written exactly one
// `<id>.m4a` file into outDir; we scan for it and return its
// absolute path. A zero-exit run with no output file surfaces as a
// wrapped error rather than a silent success — that matches the
// contract post-condition ("returns the absolute path of the
// resulting .m4a").
func (y *YtdlpDownloader) DownloadAudio(ctx context.Context, url, outDir string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("downloader: ytdlp: %w", err)
	}
	if strings.TrimSpace(url) == "" {
		return "", errors.New("downloader: ytdlp: empty url")
	}
	if strings.TrimSpace(outDir) == "" {
		return "", errors.New("downloader: ytdlp: empty outDir")
	}
	if err := statDir(outDir); err != nil {
		return "", err
	}

	args := []string{
		"--no-warnings",
		"-f", "bestaudio[ext=m4a]/bestaudio",
		"-x",
		"--audio-format", "m4a",
		"--audio-quality", "5",
		"-o", "%(id)s.%(ext)s",
		"--paths", outDir,
		url,
	}
	args = y.withAuth(args)
	if err := y.run(ctx, args, io.Discard); err != nil {
		return "", err
	}

	hit, err := findAudio(outDir)
	if err != nil {
		return "", fmt.Errorf("downloader: ytdlp: scan %s for m4a: %w", outDir, err)
	}
	if hit == "" {
		return "", fmt.Errorf("downloader: ytdlp: exit 0 but no .m4a written under %s", outDir)
	}
	abs, err := filepath.Abs(hit)
	if err != nil {
		return "", fmt.Errorf("downloader: ytdlp: abs %s: %w", hit, err)
	}
	return abs, nil
}

// withAuth prepends `--cookies-from-browser <value>` to args when the
// downloader is configured to borrow browser cookies (the default —
// [config.Auth] seeds "chrome"). Placing the flag at the very front
// keeps all of yt-dlp's argv scanning behaviour identical between the
// login-on and login-off paths: the trailing URL positional and every
// existing flag/pair keeps its original relative order.
//
// The sentinel value "none" (case-insensitive, whitespace-trimmed)
// disables the injection entirely — matching the [config.Auth] "opt
// out of login" contract. A whitespace-only or empty value is treated
// as "not configured" and also injects nothing, so a mis-decoded yaml
// value can never smuggle a bare `--cookies-from-browser ""` flag past
// yt-dlp's arg parser.
func (y *YtdlpDownloader) withAuth(args []string) []string {
	v := strings.TrimSpace(y.CookiesFromBrowser)
	if v == "" || strings.EqualFold(v, "none") {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, "--cookies-from-browser", v)
	out = append(out, args...)
	return out
}

// run is the shared exec envelope. It resolves the binary, wires up
// stderr (caller sink + internal capture for keyword scan), runs
// the child under ctx, and maps exit conditions to sentinel/wrapped
// errors per contracts.md §四.
//
// stdoutSink receives the child's stdout stream (nil ⇒ io.Discard).
// Callers pass a *bytes.Buffer when they need to parse stdout
// (FetchMetadata), or io.Discard for fire-and-forget writes
// (DownloadSubtitle / DownloadAudio).
func (y *YtdlpDownloader) run(ctx context.Context, args []string, stdoutSink io.Writer) error {
	bin := y.Binary
	if bin == "" {
		bin = "yt-dlp"
	}

	// Internal capture for the ErrVideoNotFound keyword scan. Bounded
	// so a chatty child cannot balloon our RSS. Bytes still flow to
	// the caller's Stderr writer via MultiWriter regardless of cap.
	capture := &boundedBuffer{limit: stderrCaptureLimit}
	var stderrDest io.Writer = capture
	if y.Stderr != nil {
		stderrDest = io.MultiWriter(y.Stderr, capture)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	if stdoutSink != nil {
		cmd.Stdout = stdoutSink
	} else {
		cmd.Stdout = io.Discard
	}
	cmd.Stderr = stderrDest

	if err := cmd.Run(); err != nil {
		// ctx-cancelled runs surface as *exec.ExitError from the
		// SIGKILL exec.CommandContext sends. Preferring ctx.Err()
		// gives callers a wrapped sentinel they can errors.Is
		// against, matching contracts.md §四.1.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("downloader: ytdlp: %w", ctxErr)
		}
		// stderr keyword scan wins over generic exec wrap so
		// pipeline can pick up ErrVideoNotFound via errors.Is.
		if isVideoNotFound(capture.String()) {
			return fmt.Errorf("downloader: ytdlp: %w", ErrVideoNotFound)
		}
		return fmt.Errorf("downloader: ytdlp: %w", err)
	}
	return nil
}

// toMetadata converts the wire projection into the public [Metadata].
// Field-by-field so future additions to ytdlpMetadata do not
// accidentally leak internal shapes.
func toMetadata(raw *ytdlpMetadata) *Metadata {
	m := &Metadata{
		BVID:       raw.ID,
		Title:      raw.Title,
		TotalPages: raw.PlaylistCount,
	}
	// contracts.md §一.1: TotalPages has a floor of 1. yt-dlp omits
	// playlist_count for genuinely single-P videos, so a zero
	// value here means "single P", not "invalid".
	if m.TotalPages < 1 {
		m.TotalPages = 1
	}
	// yt-dlp reports duration as a float (seconds). Missing / negative
	// values fall through as zero; contracts.md forbids interpreting
	// zero as "instant video" — it means yt-dlp did not report it.
	if raw.Duration > 0 {
		m.Duration = time.Duration(raw.Duration * float64(time.Second))
	}
	m.Subtitles = flattenSubtitleMap(raw.Subtitles)
	m.AutoCaptions = flattenSubtitleMap(raw.AutoCaptions)
	return m
}

// flattenSubtitleMap converts yt-dlp's `{lang: [{ext,url}, ...]}`
// shape into our []SubtitleTrack. Keys are used as Lang; the inner
// slice is expanded so a lang with two srt+vtt entries produces two
// tracks. Empty maps yield nil rather than an empty slice — matches
// the contract clause "may be empty" and lets callers rely on
// `len(...) == 0` uniformly.
func flattenSubtitleMap(src map[string][]ytdlpSubtitle) []SubtitleTrack {
	if len(src) == 0 {
		return nil
	}
	out := make([]SubtitleTrack, 0, len(src))
	for lang, tracks := range src {
		for _, t := range tracks {
			out = append(out, SubtitleTrack{
				Lang: lang,
				Ext:  t.Ext,
				URL:  t.URL,
			})
		}
	}
	return out
}

// statDir enforces the outDir clause of contracts.md §二.1:
// outDir MUST exist and be a directory. Missing → return the
// underlying "no such file or directory" error wrapped with the
// downloader envelope; a file at the path → explicit "not a
// directory" so pipeline logs pinpoint the mis-wiring.
func statDir(outDir string) error {
	info, err := os.Stat(outDir)
	if err != nil {
		return fmt.Errorf("downloader: ytdlp: outDir %s: %w", outDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("downloader: ytdlp: outDir %s: not a directory", outDir)
	}
	return nil
}

// findSubtitleForLang scans outDir for a subtitle file matching
// `*.<lang>.srt`. Returns the first hit; empty string when nothing
// matches (⇒ this lang missed, caller will try the next one).
//
// We match on suffix rather than exact `<bvid>.<lang>.srt` because
// FetchMetadata is not necessarily called before DownloadSubtitle —
// the pipeline layer may drive them independently and we would have
// to plumb bvid through the argv to reconstruct the expected name.
// Since yt-dlp is the only writer under outDir (pipeline gives us a
// fresh tmp dir per URL, see contracts.md §五), suffix match is
// unambiguous.
func findSubtitleForLang(outDir, lang string) (string, error) {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return "", err
	}
	needle := "." + lang + ".srt"
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), needle) {
			return filepath.Join(outDir, e.Name()), nil
		}
	}
	return "", nil
}

// findAudio scans outDir for the first `*.m4a` file yt-dlp produced.
// Same tmp-dir uniqueness rationale as [findSubtitleForLang].
func findAudio(outDir string) (string, error) {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".m4a") {
			return filepath.Join(outDir, e.Name()), nil
		}
	}
	return "", nil
}

// videoNotFoundPatterns lists the case-insensitive keyword phrases
// yt-dlp uses when the target video is missing. Sourced from the
// upstream extractor error messages we've observed:
//   - "Video unavailable"           (generic + BiliBili extractor)
//   - "Private video"               (owner-restricted)
//   - "video is not available"      (region locks, deleted content)
//   - "content is not available"    (some BiliBili region-lock blobs)
//   - "has been removed"            (deleted-by-uploader)
//
// A stderr blob matching ANY pattern (case-insensitive) triggers the
// [ErrVideoNotFound] mapping. Patterns are deliberately specific
// (multi-word) to avoid false positives from generic warnings such as
// "format N is not available, falling back to ..." — a bare
// "not available" would misroute those to the not-found bucket.
// Kept as a package-level slice so tests can grep the source to
// enumerate the recognised phrases.
var videoNotFoundPatterns = []string{
	"video unavailable",
	"private video",
	"video is not available",
	"content is not available",
	"has been removed",
}

func isVideoNotFound(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, p := range videoNotFoundPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// boundedBuffer is a Write-only buffer with a hard byte cap. Writes
// beyond the cap are silently dropped; the recorded contents can be
// inspected via [boundedBuffer.String]. Used by [YtdlpDownloader.run]
// to keep the ErrVideoNotFound keyword-scan buffer under control
// even when yt-dlp spams megabytes of warnings.
//
// The zero value is usable (unbounded, since limit=0 means "no cap"
// — but callers always set a positive limit; the zero-limit case is
// left permissive to avoid a silent-truncation footgun on
// misconstructed instances).
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.limit > 0 {
		remaining := b.limit - b.buf.Len()
		if remaining <= 0 {
			return len(p), nil // drop, but report "written" so the tee doesn't error out
		}
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
			return len(p), nil
		}
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }

// Compile-time check: YtdlpDownloader satisfies [Downloader]. If the
// interface signature drifts (contract update) the build breaks here
// before any caller notices, matching the pattern used by
// [FakeDownloader].
var _ Downloader = (*YtdlpDownloader)(nil)
