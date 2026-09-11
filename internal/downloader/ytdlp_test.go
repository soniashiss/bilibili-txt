//go:build !windows

package downloader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// projectTempDir returns a fresh sandbox-friendly temp dir living
// inside the repo under .tmp/. Mirrors the pattern in
// internal/audio/ffmpeg_test.go — Trae's sandbox refuses
// /var/folders/... paths whenever we spawn a child (yt-dlp stub via
// exec.CommandContext).
func projectTempDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(1)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	base := filepath.Join(repoRoot, ".tmp", "downloader-test")
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

// writeStub drops a POSIX shell stub at scriptPath that runs body.
// The script is created 0755 so exec.CommandContext can spawn it.
func writeStub(t *testing.T, scriptPath, body string) {
	t.Helper()
	full := "#!/bin/sh\n" + body
	if err := os.WriteFile(scriptPath, []byte(full), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", scriptPath, err)
	}
}

// writeArgvCaptureStub emits one argv token per line to captureFile,
// optionally writes stdoutBody to stdout and stderrBody to stderr,
// and exits with exitCode. Used to lock argv shape against plan §5.
//
// stdoutBody / stderrBody are written to sidecar files and streamed
// via `cat` — Go's %q would corrupt embedded newlines (POSIX sh
// keeps backslash escapes literal inside "..."), so we side-step
// the shell escaping entirely.
func writeArgvCaptureStub(t *testing.T, scriptPath, captureFile, stdoutBody, stderrBody string, exitCode int) {
	t.Helper()
	stdoutPath := scriptPath + ".stdout"
	stderrPath := scriptPath + ".stderr"
	if err := os.WriteFile(stdoutPath, []byte(stdoutBody), 0o644); err != nil {
		t.Fatalf("write stdout body: %v", err)
	}
	if err := os.WriteFile(stderrPath, []byte(stderrBody), 0o644); err != nil {
		t.Fatalf("write stderr body: %v", err)
	}
	body := fmt.Sprintf(`: > %[1]q
for arg in "$@"; do
  printf '%%s\n' "$arg" >> %[1]q
done
if [ -s %[2]q ]; then
  cat %[2]q
fi
if [ -s %[3]q ]; then
  cat %[3]q >&2
fi
exit %[4]d
`, captureFile, stdoutPath, stderrPath, exitCode)
	writeStub(t, scriptPath, body)
}

func readCaptureLines(t *testing.T, path string) []string {
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

// argvIndex returns the index of tok in argv, or -1 if absent.
func argvIndex(argv []string, tok string) int {
	for i, a := range argv {
		if a == tok {
			return i
		}
	}
	return -1
}

// argvContains is a convenience for "flag present" assertions.
func argvContains(argv []string, tok string) bool {
	return argvIndex(argv, tok) >= 0
}

// ---------------------------------------------------------------------------
// FetchMetadata
// ---------------------------------------------------------------------------

const sampleMetaJSON = `{
  "id": "BVfake123",
  "title": "Fake Video Title — 测试样例",
  "duration": 187,
  "webpage_url": "https://www.bilibili.com/video/BVfake123",
  "playlist_count": 1,
  "subtitles": {
    "zh-CN": [{"ext": "srt", "url": "https://example.invalid/subs/zh-CN.srt"}]
  },
  "automatic_captions": {
    "ai-zh": [{"ext": "srt", "url": "https://example.invalid/auto/ai-zh.srt"}]
  }
}`

func TestYtdlp_FetchMetadata_ArgvMatchesPlan_5_1(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureStub(t, script, capture, sampleMetaJSON, "", 0)

	d := &YtdlpDownloader{Binary: script}
	url := "https://www.bilibili.com/video/BVfake123"

	_, err := d.FetchMetadata(context.Background(), url)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	argv := readCaptureLines(t, capture)
	// plan §5.1 freezes argv order literally.
	want := []string{
		"--no-warnings",
		"--dump-single-json",
		"--write-subs", "--write-auto-subs",
		"--sub-langs", "zh-CN,zh-Hans,ai-zh",
		"--skip-download",
		url,
	}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv mismatch;\n got=%v\nwant=%v", argv, want)
	}
}

func TestYtdlp_FetchMetadata_ParsesJSON(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	writeArgvCaptureStub(t, script, filepath.Join(dir, "argv.txt"), sampleMetaJSON, "", 0)

	d := &YtdlpDownloader{Binary: script}
	meta, err := d.FetchMetadata(context.Background(), "https://www.bilibili.com/video/BVfake123")
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	if meta == nil {
		t.Fatal("meta is nil")
	}
	if meta.BVID != "BVfake123" {
		t.Errorf("BVID = %q, want %q", meta.BVID, "BVfake123")
	}
	if meta.Title != "Fake Video Title — 测试样例" {
		t.Errorf("Title = %q", meta.Title)
	}
	if meta.TotalPages != 1 {
		t.Errorf("TotalPages = %d, want 1", meta.TotalPages)
	}
	if meta.Duration != 187*time.Second {
		t.Errorf("Duration = %s, want 187s", meta.Duration)
	}
	if len(meta.Subtitles) != 1 || meta.Subtitles[0].Lang != "zh-CN" || meta.Subtitles[0].Ext != "srt" {
		t.Errorf("Subtitles = %+v, want one zh-CN srt", meta.Subtitles)
	}
	if meta.Subtitles[0].URL != "https://example.invalid/subs/zh-CN.srt" {
		t.Errorf("Subtitles[0].URL = %q", meta.Subtitles[0].URL)
	}
	if len(meta.AutoCaptions) != 1 || meta.AutoCaptions[0].Lang != "ai-zh" {
		t.Errorf("AutoCaptions = %+v, want one ai-zh", meta.AutoCaptions)
	}
}

func TestYtdlp_FetchMetadata_NoSubtitles_EmptyMap(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	nosubJSON := `{
  "id": "BVnosub999",
  "title": "No Subtitle Video",
  "duration": 92,
  "webpage_url": "https://www.bilibili.com/video/BVnosub999",
  "playlist_count": 1,
  "subtitles": {},
  "automatic_captions": {}
}`
	writeArgvCaptureStub(t, script, filepath.Join(dir, "argv.txt"), nosubJSON, "", 0)

	d := &YtdlpDownloader{Binary: script}
	meta, err := d.FetchMetadata(context.Background(), "https://www.bilibili.com/video/BVnosub999")
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	if len(meta.Subtitles) != 0 {
		t.Errorf("Subtitles should be empty; got %+v", meta.Subtitles)
	}
	if len(meta.AutoCaptions) != 0 {
		t.Errorf("AutoCaptions should be empty; got %+v", meta.AutoCaptions)
	}
}

func TestYtdlp_FetchMetadata_TotalPagesDefaultsToOne(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	// Video with no playlist_count field (single-P video where yt-dlp
	// omits the key) — TotalPages must default to 1, never 0.
	partialJSON := `{
  "id": "BVsingle",
  "title": "Single Page",
  "duration": 42,
  "webpage_url": "https://www.bilibili.com/video/BVsingle",
  "subtitles": {},
  "automatic_captions": {}
}`
	writeArgvCaptureStub(t, script, filepath.Join(dir, "argv.txt"), partialJSON, "", 0)

	d := &YtdlpDownloader{Binary: script}
	meta, err := d.FetchMetadata(context.Background(), "https://x.example/BVsingle")
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	if meta.TotalPages != 1 {
		t.Errorf("TotalPages should default to 1 when json omits it; got %d", meta.TotalPages)
	}
	if meta.Duration != 42*time.Second {
		t.Errorf("Duration = %s, want 42s", meta.Duration)
	}
}

// TestYtdlp_FetchMetadata_DurationMissingOrNonPositive verifies that a
// missing / zero / negative "duration" field in yt-dlp's JSON output
// maps to Metadata.Duration == 0 (which pipeline log formatter renders
// as "unknown"). yt-dlp very rarely omits the field, but livestream
// recordings and some 分P 索引页 do — we must never fabricate a
// duration for those, and we must never negate the value.
func TestYtdlp_FetchMetadata_DurationMissingOrNonPositive(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "missing",
			body: `{"id":"BVmiss","title":"missing","playlist_count":1,"subtitles":{},"automatic_captions":{}}`,
		},
		{
			name: "zero",
			body: `{"id":"BVzero","title":"zero","duration":0,"playlist_count":1,"subtitles":{},"automatic_captions":{}}`,
		},
		{
			name: "negative",
			body: `{"id":"BVneg","title":"neg","duration":-3.14,"playlist_count":1,"subtitles":{},"automatic_captions":{}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := projectTempDir(t)
			script := filepath.Join(dir, "yt-dlp-stub")
			writeArgvCaptureStub(t, script, filepath.Join(dir, "argv.txt"), tc.body, "", 0)

			d := &YtdlpDownloader{Binary: script}
			meta, err := d.FetchMetadata(context.Background(), "https://x.example/BV")
			if err != nil {
				t.Fatalf("FetchMetadata: %v", err)
			}
			if meta.Duration != 0 {
				t.Errorf("Duration = %s, want 0 (yt-dlp did not report a positive duration)", meta.Duration)
			}
		})
	}
}

func TestYtdlp_FetchMetadata_MalformedJSON_WrapsError(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	writeArgvCaptureStub(t, script, filepath.Join(dir, "argv.txt"), "not-json-at-all", "", 0)

	d := &YtdlpDownloader{Binary: script}
	_, err := d.FetchMetadata(context.Background(), "https://x.example/BV1")
	if err == nil {
		t.Fatal("expected JSON parse error; got nil")
	}
	if !strings.Contains(err.Error(), "downloader: ytdlp") {
		t.Errorf("error should carry envelope; got %q", err.Error())
	}
}

func TestYtdlp_FetchMetadata_EmptyURL_RejectedWithoutSpawn(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureStub(t, script, capture, sampleMetaJSON, "", 0)

	d := &YtdlpDownloader{Binary: script}
	_, err := d.FetchMetadata(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "empty url") {
		t.Errorf("expected empty url rejection; got %v", err)
	}
	if _, statErr := os.Stat(capture); statErr == nil {
		t.Errorf("empty url must reject BEFORE spawn; capture appeared")
	}
}

func TestYtdlp_FetchMetadata_CtxAlreadyCancelled(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureStub(t, script, capture, sampleMetaJSON, "", 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := &YtdlpDownloader{Binary: script}
	_, err := d.FetchMetadata(ctx, "https://x.example/BV1")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want errors.Is(err, context.Canceled); got %v", err)
	}
}

func TestYtdlp_FetchMetadata_CtxCancelWinsOverEmptyURL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := &YtdlpDownloader{Binary: "/nonexistent"}
	_, err := d.FetchMetadata(ctx, "")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("ctx should win over empty-url; got %v", err)
	}
}

func TestYtdlp_FetchMetadata_VideoUnavailable_ErrVideoNotFound(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	writeArgvCaptureStub(t, script, filepath.Join(dir, "argv.txt"),
		"", "ERROR: [BiliBili] BV1notfound: Video unavailable\n", 1)

	d := &YtdlpDownloader{Binary: script}
	_, err := d.FetchMetadata(context.Background(), "https://x.example/BV1notfound")
	if !errors.Is(err, ErrVideoNotFound) {
		t.Errorf("want errors.Is(err, ErrVideoNotFound); got %v", err)
	}
}

func TestYtdlp_FetchMetadata_SpawnFailure(t *testing.T) {
	d := &YtdlpDownloader{Binary: "/nonexistent/definitely-not-here-xyz"}
	_, err := d.FetchMetadata(context.Background(), "https://x.example/BV1")
	if err == nil {
		t.Fatal("expected spawn failure; got nil")
	}
	if !strings.Contains(err.Error(), "downloader: ytdlp") {
		t.Errorf("error should carry envelope; got %q", err.Error())
	}
}

func TestYtdlp_FetchMetadata_StderrIsPipedToWriter(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	writeArgvCaptureStub(t, script, filepath.Join(dir, "argv.txt"),
		sampleMetaJSON, "yt-dlp debug: fetched 1 page\n", 0)

	var buf bytes.Buffer
	d := &YtdlpDownloader{Binary: script, Stderr: &buf}
	if _, err := d.FetchMetadata(context.Background(), "https://x.example/BV1"); err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	if !strings.Contains(buf.String(), "yt-dlp debug: fetched 1 page") {
		t.Errorf("stderr should reach caller's writer; got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// DownloadSubtitle
// ---------------------------------------------------------------------------

// writeSubtitleFileStub creates a stub that reads `--paths <dir>`
// and `--sub-langs <csv>` from argv, then drops `<bvid>.<lang>.srt`
// for each lang listed in the CSV present in the emitFor set. bvid
// is grepped from the trailing URL positional. emitFor lets tests
// pick which langs produce a file (mimicking yt-dlp emitting only
// the langs it actually finds).
//
// This stub matches the plan §5.2 single-call semantics: yt-dlp is
// invoked ONCE with a comma-joined --sub-langs list and it decides
// per-lang whether the track exists.
func writeSubtitleFileStub(t *testing.T, scriptPath, captureFile string, emitFor []string) {
	t.Helper()
	// Build a shell case-list from emitFor for the "emit this lang"
	// filter. Empty emitFor => yt-dlp writes nothing (all langs miss).
	emitCase := strings.Join(emitFor, "|")
	if emitCase == "" {
		emitCase = "__none__" // impossible match
	}
	body := fmt.Sprintf(`: > %[1]q
paths=""; langs_csv=""; url=""
prev=""
for arg in "$@"; do
  printf '%%s\n' "$arg" >> %[1]q
  if [ "$prev" = "--paths" ]; then paths="$arg"; fi
  if [ "$prev" = "--sub-langs" ]; then langs_csv="$arg"; fi
  case "$arg" in http*://*) url="$arg" ;; esac
  prev="$arg"
done
[ -z "$paths" ] && { echo "no --paths" >&2; exit 2; }
[ -z "$langs_csv" ] && { echo "no --sub-langs" >&2; exit 2; }
bv=$(printf '%%s' "$url" | grep -oE 'BV[[:alnum:]]+' | head -n1)
[ -z "$bv" ] && bv=BVstub
mkdir -p "$paths"
# Split CSV on comma; for each lang, emit srt only if it matches the emitCase set.
IFS=,
for lang in $langs_csv; do
  case "$lang" in
    %[2]s) printf 'stub-srt-%%s' "$lang" > "$paths/${bv}.${lang}.srt" ;;
  esac
done
IFS=' '
exit 0
`, captureFile, emitCase)
	writeStub(t, scriptPath, body)
}

func TestYtdlp_DownloadSubtitle_ArgvMatchesPlan_5_2(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	capture := filepath.Join(dir, "argv.txt")
	// Miss stub: captures argv, produces no file → surfaces to
	// ErrNoSubtitle downstream. Here we only assert argv shape.
	writeSubtitleFileStub(t, script, capture, nil)

	outDir := filepath.Join(dir, "sub-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}

	d := &YtdlpDownloader{Binary: script}
	url := "https://www.bilibili.com/video/BV1abcdefghij"
	_, _ = d.DownloadSubtitle(context.Background(), url, outDir, []string{"zh-CN", "zh-Hans", "ai-zh"})

	argv := readCaptureLines(t, capture)
	// plan §5.2 freezes argv order literally — one shot with the
	// comma-joined lang list.
	want := []string{
		"--no-warnings",
		"--write-subs", "--write-auto-subs",
		"--sub-langs", "zh-CN,zh-Hans,ai-zh",
		"--sub-format", "srt/best",
		"--convert-subs", "srt",
		"--skip-download",
		"-o", "%(id)s.%(ext)s",
		"--paths", outDir,
		url,
	}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv mismatch;\n got=%v\nwant=%v", argv, want)
	}
}

func TestYtdlp_DownloadSubtitle_SingleShotInvocation(t *testing.T) {
	// plan §5.2 requires exactly ONE exec, not one-per-lang. The
	// stub counts invocations by appending to counter.txt.
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	counter := filepath.Join(dir, "count.txt")
	body := fmt.Sprintf(`printf 'x\n' >> %q
paths=""; langs_csv=""; url=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--paths" ]; then paths="$arg"; fi
  if [ "$prev" = "--sub-langs" ]; then langs_csv="$arg"; fi
  case "$arg" in http*://*) url="$arg" ;; esac
  prev="$arg"
done
bv=$(printf '%%s' "$url" | grep -oE 'BV[[:alnum:]]+' | head -n1)
mkdir -p "$paths"
# Simulate yt-dlp producing all requested langs.
IFS=,
for lang in $langs_csv; do
  printf 'stub' > "$paths/${bv}.${lang}.srt"
done
IFS=' '
exit 0
`, counter)
	writeStub(t, script, body)

	outDir := filepath.Join(dir, "sub-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	d := &YtdlpDownloader{Binary: script}
	if _, err := d.DownloadSubtitle(context.Background(),
		"https://x.example/BV1", outDir, []string{"zh-CN", "zh-Hans", "ai-zh"}); err != nil {
		t.Fatalf("DownloadSubtitle: %v", err)
	}
	calls := readCaptureLines(t, counter)
	if len(calls) != 1 {
		t.Errorf("plan §5.2 requires 1 exec; got %d", len(calls))
	}
}

func TestYtdlp_DownloadSubtitle_FirstLangHitWins(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	// Stub emits ALL requested langs — mimics best-case yt-dlp
	// producing every available track in one shot. Downloader must
	// pick the first lang in langPref order.
	writeSubtitleFileStub(t, script, filepath.Join(dir, "argv.txt"),
		[]string{"zh-CN", "zh-Hans", "ai-zh"})

	outDir := filepath.Join(dir, "sub-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}

	d := &YtdlpDownloader{Binary: script}
	url := "https://www.bilibili.com/video/BV1abcdefghij"
	got, err := d.DownloadSubtitle(context.Background(), url, outDir, []string{"zh-CN", "zh-Hans", "ai-zh"})
	if err != nil {
		t.Fatalf("DownloadSubtitle: %v", err)
	}
	if !strings.HasSuffix(got, "BV1abcdefghij.zh-CN.srt") {
		t.Errorf("srtPath = %q, want suffix BV1abcdefghij.zh-CN.srt", got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("srtPath must be absolute; got %q", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("srt should exist at %s: %v", got, err)
	}
}

func TestYtdlp_DownloadSubtitle_SecondLangHitWhenFirstMissing(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	// yt-dlp only had a zh-Hans track available; zh-CN was
	// requested but no CC exists. Downloader must fall through to
	// the second langPref entry.
	writeSubtitleFileStub(t, script, filepath.Join(dir, "argv.txt"),
		[]string{"zh-Hans"})

	outDir := filepath.Join(dir, "sub-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	d := &YtdlpDownloader{Binary: script}
	got, err := d.DownloadSubtitle(context.Background(),
		"https://www.bilibili.com/video/BV1xyz", outDir, []string{"zh-CN", "zh-Hans", "ai-zh"})
	if err != nil {
		t.Fatalf("DownloadSubtitle: %v", err)
	}
	if !strings.HasSuffix(got, "BV1xyz.zh-Hans.srt") {
		t.Errorf("srtPath = %q, want suffix BV1xyz.zh-Hans.srt", got)
	}
}

func TestYtdlp_DownloadSubtitle_ThirdLangHitWhenFirstTwoMissing(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	// Only ai-zh is available — verifies the outDir scan walks the
	// FULL langPref before giving up.
	writeSubtitleFileStub(t, script, filepath.Join(dir, "argv.txt"),
		[]string{"ai-zh"})

	outDir := filepath.Join(dir, "sub-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	d := &YtdlpDownloader{Binary: script}
	got, err := d.DownloadSubtitle(context.Background(),
		"https://x.example/BV1third", outDir, []string{"zh-CN", "zh-Hans", "ai-zh"})
	if err != nil {
		t.Fatalf("DownloadSubtitle: %v", err)
	}
	if !strings.HasSuffix(got, "BV1third.ai-zh.srt") {
		t.Errorf("srtPath = %q, want suffix BV1third.ai-zh.srt", got)
	}
}

func TestYtdlp_DownloadSubtitle_AllLangsMiss_ErrNoSubtitle(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	// emitFor=nil → stub exits 0 with no output, matching yt-dlp's
	// behaviour when none of the requested lang tracks exist.
	writeSubtitleFileStub(t, script, filepath.Join(dir, "argv.txt"), nil)

	outDir := filepath.Join(dir, "sub-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}

	d := &YtdlpDownloader{Binary: script}
	_, err := d.DownloadSubtitle(context.Background(),
		"https://x.example/BV1miss", outDir, []string{"zh-CN", "zh-Hans", "ai-zh"})
	if !errors.Is(err, ErrNoSubtitle) {
		t.Errorf("want errors.Is(err, ErrNoSubtitle); got %v", err)
	}
	// ErrNoSubtitle must be wrapped in the downloader envelope so
	// pipeline log lines carry the same "downloader: ytdlp: ..."
	// prefix as every other error path.
	if err != nil && !strings.Contains(err.Error(), "downloader: ytdlp") {
		t.Errorf("ErrNoSubtitle should carry envelope; got %q", err.Error())
	}
}

func TestYtdlp_DownloadSubtitle_EmptyURL(t *testing.T) {
	dir := projectTempDir(t)
	outDir := filepath.Join(dir, "sub-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	d := &YtdlpDownloader{Binary: "/nonexistent"}
	_, err := d.DownloadSubtitle(context.Background(), "", outDir, []string{"zh-CN"})
	if err == nil || !strings.Contains(err.Error(), "empty url") {
		t.Errorf("expected empty url rejection; got %v", err)
	}
}

func TestYtdlp_DownloadSubtitle_EmptyOutDir(t *testing.T) {
	d := &YtdlpDownloader{Binary: "/nonexistent"}
	_, err := d.DownloadSubtitle(context.Background(), "https://x.example/BV1", "", []string{"zh-CN"})
	if err == nil || !strings.Contains(err.Error(), "empty outDir") {
		t.Errorf("expected empty outDir rejection; got %v", err)
	}
}

func TestYtdlp_DownloadSubtitle_EmptyLangPref(t *testing.T) {
	dir := projectTempDir(t)
	outDir := filepath.Join(dir, "sub-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	d := &YtdlpDownloader{Binary: "/nonexistent"}
	_, err := d.DownloadSubtitle(context.Background(), "https://x.example/BV1", outDir, nil)
	if err == nil || !strings.Contains(err.Error(), "empty langPref") {
		t.Errorf("expected empty langPref rejection; got %v", err)
	}
}

func TestYtdlp_DownloadSubtitle_OutDirMissing(t *testing.T) {
	dir := projectTempDir(t)
	missing := filepath.Join(dir, "does-not-exist")
	d := &YtdlpDownloader{Binary: "/nonexistent"}
	_, err := d.DownloadSubtitle(context.Background(), "https://x.example/BV1", missing, []string{"zh-CN"})
	if err == nil {
		t.Fatal("expected error for missing outDir; got nil")
	}
	if !strings.Contains(err.Error(), "outDir") {
		t.Errorf("error should mention outDir; got %q", err.Error())
	}
}

func TestYtdlp_DownloadSubtitle_OutDirIsFile(t *testing.T) {
	dir := projectTempDir(t)
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write not-a-dir: %v", err)
	}
	d := &YtdlpDownloader{Binary: "/nonexistent"}
	_, err := d.DownloadSubtitle(context.Background(), "https://x.example/BV1", file, []string{"zh-CN"})
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected 'not a directory'; got %v", err)
	}
}

func TestYtdlp_DownloadSubtitle_CtxAlreadyCancelled(t *testing.T) {
	dir := projectTempDir(t)
	outDir := filepath.Join(dir, "sub-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &YtdlpDownloader{Binary: "/nonexistent"}
	_, err := d.DownloadSubtitle(ctx, "https://x.example/BV1", outDir, []string{"zh-CN"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled; got %v", err)
	}
}

func TestYtdlp_DownloadSubtitle_ExecFailure_WrapsWithURL(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	writeArgvCaptureStub(t, script, filepath.Join(dir, "argv.txt"),
		"", "yt-dlp: something bad\n", 1)

	outDir := filepath.Join(dir, "sub-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	d := &YtdlpDownloader{Binary: script}
	_, err := d.DownloadSubtitle(context.Background(),
		"https://x.example/BV1boom", outDir, []string{"zh-CN"})
	if err == nil {
		t.Fatal("expected error for exit 1; got nil")
	}
	// Not sentinel — pipeline will see this as fatal.
	if errors.Is(err, ErrNoSubtitle) {
		t.Errorf("exec failure must NOT alias to ErrNoSubtitle; got %v", err)
	}
	if !strings.Contains(err.Error(), "downloader: ytdlp") {
		t.Errorf("error should carry envelope; got %q", err.Error())
	}
}

func TestYtdlp_DownloadSubtitle_VideoUnavailable_ErrVideoNotFound(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	writeArgvCaptureStub(t, script, filepath.Join(dir, "argv.txt"),
		"", "ERROR: [BiliBili] BV1x: Video unavailable\n", 1)

	outDir := filepath.Join(dir, "sub-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	d := &YtdlpDownloader{Binary: script}
	_, err := d.DownloadSubtitle(context.Background(),
		"https://x.example/BV1x", outDir, []string{"zh-CN"})
	if !errors.Is(err, ErrVideoNotFound) {
		t.Errorf("want errors.Is(err, ErrVideoNotFound); got %v", err)
	}
}

func TestYtdlp_DownloadSubtitle_StderrIsPipedToWriter(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	// Success path: emit whatever lang is requested (single-shot
	// semantics, matches plan §5.2) + write to stderr so we can
	// verify the caller's Stderr writer sees it.
	body := `paths=""; langs_csv=""; url=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--paths" ]; then paths="$arg"; fi
  if [ "$prev" = "--sub-langs" ]; then langs_csv="$arg"; fi
  case "$arg" in http*://*) url="$arg" ;; esac
  prev="$arg"
done
printf 'ytdlp: downloading subtitle chatter\n' >&2
bv=$(printf '%s' "$url" | grep -oE 'BV[[:alnum:]]+' | head -n1)
mkdir -p "$paths"
IFS=,
for lang in $langs_csv; do
  printf 'stub' > "$paths/${bv}.${lang}.srt"
done
IFS=' '
exit 0
`
	writeStub(t, script, body)

	outDir := filepath.Join(dir, "sub-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	var buf bytes.Buffer
	d := &YtdlpDownloader{Binary: script, Stderr: &buf}
	if _, err := d.DownloadSubtitle(context.Background(),
		"https://x.example/BV1", outDir, []string{"zh-CN"}); err != nil {
		t.Fatalf("DownloadSubtitle: %v", err)
	}
	if !strings.Contains(buf.String(), "downloading subtitle chatter") {
		t.Errorf("stderr should reach caller writer; got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// DownloadAudio
// ---------------------------------------------------------------------------

func writeAudioSuccessStub(t *testing.T, scriptPath string) {
	t.Helper()
	body := `paths=""; url=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--paths" ]; then paths="$arg"; fi
  case "$arg" in http*://*) url="$arg" ;; esac
  prev="$arg"
done
[ -z "$paths" ] && { echo "no --paths" >&2; exit 2; }
bv=$(printf '%s' "$url" | grep -oE 'BV[[:alnum:]]+' | head -n1)
[ -z "$bv" ] && bv=BVstub
mkdir -p "$paths"
printf 'stub-m4a' > "$paths/${bv}.m4a"
exit 0
`
	writeStub(t, scriptPath, body)
}

func TestYtdlp_DownloadAudio_ArgvMatchesPlan_5_3(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureStub(t, script, capture, "", "", 0) // capture but produce no output

	outDir := filepath.Join(dir, "audio-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	d := &YtdlpDownloader{Binary: script}
	url := "https://www.bilibili.com/video/BV1audio"
	_, _ = d.DownloadAudio(context.Background(), url, outDir)

	argv := readCaptureLines(t, capture)
	wantTokens := []string{
		"--no-warnings",
		"-f", "bestaudio[ext=m4a]/bestaudio",
		"-x",
		"--audio-format", "m4a",
		"--audio-quality", "5",
		"-o", "%(id)s.%(ext)s",
		"--paths", outDir,
		url,
	}
	for _, tok := range wantTokens {
		if !argvContains(argv, tok) {
			t.Errorf("argv missing %q; got=%v", tok, argv)
		}
	}
	// URL last.
	if argv[len(argv)-1] != url {
		t.Errorf("URL should be last arg; got last=%q; argv=%v", argv[len(argv)-1], argv)
	}
	pairs := [][2]string{
		{"-f", "bestaudio[ext=m4a]/bestaudio"},
		{"--audio-format", "m4a"},
		{"--audio-quality", "5"},
		{"-o", "%(id)s.%(ext)s"},
		{"--paths", outDir},
	}
	for _, p := range pairs {
		i := argvIndex(argv, p[0])
		if i < 0 || i+1 >= len(argv) || argv[i+1] != p[1] {
			t.Errorf("flag %q must be followed by %q; argv=%v", p[0], p[1], argv)
		}
	}
	// -x is a bare flag (no value).
	if !argvContains(argv, "-x") {
		t.Errorf("-x flag missing; argv=%v", argv)
	}
}

func TestYtdlp_DownloadAudio_ReturnsAbsPathOfWrittenFile(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	writeAudioSuccessStub(t, script)

	outDir := filepath.Join(dir, "audio-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	d := &YtdlpDownloader{Binary: script}
	got, err := d.DownloadAudio(context.Background(),
		"https://www.bilibili.com/video/BV1abcdefghij", outDir)
	if err != nil {
		t.Fatalf("DownloadAudio: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("audioPath must be absolute; got %q", got)
	}
	if !strings.HasSuffix(got, "BV1abcdefghij.m4a") {
		t.Errorf("audioPath = %q, want suffix BV1abcdefghij.m4a", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("audio file should exist at %s: %v", got, err)
	}
}

func TestYtdlp_DownloadAudio_EmptyURL(t *testing.T) {
	dir := projectTempDir(t)
	outDir := filepath.Join(dir, "audio-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	d := &YtdlpDownloader{Binary: "/nonexistent"}
	_, err := d.DownloadAudio(context.Background(), "", outDir)
	if err == nil || !strings.Contains(err.Error(), "empty url") {
		t.Errorf("expected empty url rejection; got %v", err)
	}
}

func TestYtdlp_DownloadAudio_EmptyOutDir(t *testing.T) {
	d := &YtdlpDownloader{Binary: "/nonexistent"}
	_, err := d.DownloadAudio(context.Background(), "https://x.example/BV1", "")
	if err == nil || !strings.Contains(err.Error(), "empty outDir") {
		t.Errorf("expected empty outDir rejection; got %v", err)
	}
}

func TestYtdlp_DownloadAudio_OutDirMissing(t *testing.T) {
	dir := projectTempDir(t)
	missing := filepath.Join(dir, "does-not-exist")
	d := &YtdlpDownloader{Binary: "/nonexistent"}
	_, err := d.DownloadAudio(context.Background(), "https://x.example/BV1", missing)
	if err == nil {
		t.Fatal("expected error for missing outDir; got nil")
	}
	if !strings.Contains(err.Error(), "outDir") {
		t.Errorf("error should mention outDir; got %q", err.Error())
	}
}

func TestYtdlp_DownloadAudio_OutDirIsFile(t *testing.T) {
	dir := projectTempDir(t)
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write not-a-dir: %v", err)
	}
	d := &YtdlpDownloader{Binary: "/nonexistent"}
	_, err := d.DownloadAudio(context.Background(), "https://x.example/BV1", file)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected 'not a directory'; got %v", err)
	}
}

func TestYtdlp_DownloadAudio_CtxAlreadyCancelled(t *testing.T) {
	dir := projectTempDir(t)
	outDir := filepath.Join(dir, "audio-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &YtdlpDownloader{Binary: "/nonexistent"}
	_, err := d.DownloadAudio(ctx, "https://x.example/BV1", outDir)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled; got %v", err)
	}
}

func TestYtdlp_DownloadAudio_CtxCancelWinsOverEmptyURL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &YtdlpDownloader{Binary: "/nonexistent"}
	_, err := d.DownloadAudio(ctx, "", "")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("ctx should win over empty-url; got %v", err)
	}
}

func TestYtdlp_DownloadAudio_ExitNonZero_WrapsError(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	writeArgvCaptureStub(t, script, filepath.Join(dir, "argv.txt"),
		"", "yt-dlp: audio download failed\n", 1)

	outDir := filepath.Join(dir, "audio-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	d := &YtdlpDownloader{Binary: script}
	_, err := d.DownloadAudio(context.Background(), "https://x.example/BV1", outDir)
	if err == nil {
		t.Fatal("expected error for exit 1; got nil")
	}
	if !strings.Contains(err.Error(), "downloader: ytdlp") {
		t.Errorf("error should carry envelope; got %q", err.Error())
	}
}

func TestYtdlp_DownloadAudio_NoOutputFile_Error(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	// exit 0 but produce no file — should surface as an error.
	writeArgvCaptureStub(t, script, filepath.Join(dir, "argv.txt"), "", "", 0)

	outDir := filepath.Join(dir, "audio-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	d := &YtdlpDownloader{Binary: script}
	_, err := d.DownloadAudio(context.Background(), "https://x.example/BV1", outDir)
	if err == nil {
		t.Fatal("expected error when yt-dlp exits 0 with no output; got nil")
	}
}

func TestYtdlp_DownloadAudio_VideoUnavailable_ErrVideoNotFound(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	writeArgvCaptureStub(t, script, filepath.Join(dir, "argv.txt"),
		"", "ERROR: [BiliBili] BV1x: Private video\n", 1)

	outDir := filepath.Join(dir, "audio-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	d := &YtdlpDownloader{Binary: script}
	_, err := d.DownloadAudio(context.Background(), "https://x.example/BV1x", outDir)
	if !errors.Is(err, ErrVideoNotFound) {
		t.Errorf("want errors.Is(err, ErrVideoNotFound); got %v", err)
	}
}

func TestYtdlp_DownloadAudio_StderrIsPipedToWriter(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	body := `paths=""; url=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--paths" ]; then paths="$arg"; fi
  case "$arg" in http*://*) url="$arg" ;; esac
  prev="$arg"
done
printf 'ytdlp: audio chatter\n' >&2
bv=$(printf '%s' "$url" | grep -oE 'BV[[:alnum:]]+' | head -n1)
mkdir -p "$paths"
printf 'stub' > "$paths/${bv}.m4a"
exit 0
`
	writeStub(t, script, body)

	outDir := filepath.Join(dir, "audio-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}
	var buf bytes.Buffer
	d := &YtdlpDownloader{Binary: script, Stderr: &buf}
	if _, err := d.DownloadAudio(context.Background(),
		"https://x.example/BV1", outDir); err != nil {
		t.Fatalf("DownloadAudio: %v", err)
	}
	if !strings.Contains(buf.String(), "audio chatter") {
		t.Errorf("stderr should reach caller writer; got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Shared behaviour
// ---------------------------------------------------------------------------

func TestYtdlp_BinaryDefaultsToPATHName(t *testing.T) {
	dir := projectTempDir(t)
	stub := filepath.Join(dir, "yt-dlp") // literal PATH name
	writeArgvCaptureStub(t, stub, filepath.Join(dir, "argv.txt"), sampleMetaJSON, "", 0)

	// Prepend dir to PATH rather than replacing — the stub itself
	// shells out to `cat` / `printf` and needs the base utilities.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := &YtdlpDownloader{} // Binary intentionally empty
	if _, err := d.FetchMetadata(context.Background(), "https://x.example/BV1"); err != nil {
		t.Fatalf("FetchMetadata with default binary: %v", err)
	}
}

func TestYtdlp_FakebinIntegration(t *testing.T) {
	// End-to-end sanity: use the shared testdata/fakebin/yt-dlp
	// script (already exercised by test/fakebin/fakebin_test.go) and
	// verify our downloader plumbing agrees with the fakebin
	// contract.
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	fake := filepath.Join(repoRoot, "testdata", "fakebin", "yt-dlp")
	if _, err := os.Stat(fake); err != nil {
		t.Fatalf("fakebin yt-dlp missing (required fixture): %v", err)
	}

	dir := projectTempDir(t)
	subOut := filepath.Join(dir, "sub-out")
	audioOut := filepath.Join(dir, "audio-out")
	for _, d := range []string{subOut, audioOut} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	d := &YtdlpDownloader{Binary: fake}
	url := "https://www.bilibili.com/video/BVfake123"

	meta, err := d.FetchMetadata(context.Background(), url)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	if meta.BVID != "BVfake123" {
		t.Errorf("BVID = %q, want BVfake123", meta.BVID)
	}

	srt, err := d.DownloadSubtitle(context.Background(), url, subOut, []string{"zh-CN"})
	if err != nil {
		t.Fatalf("DownloadSubtitle: %v", err)
	}
	if !strings.HasSuffix(srt, "BVfake123.zh-CN.srt") {
		t.Errorf("srt path unexpected: %q", srt)
	}
	if _, err := os.Stat(srt); err != nil {
		t.Errorf("srt should exist: %v", err)
	}

	audio, err := d.DownloadAudio(context.Background(), url, audioOut)
	if err != nil {
		t.Fatalf("DownloadAudio: %v", err)
	}
	if !strings.HasSuffix(audio, "BVfake123.m4a") {
		t.Errorf("audio path unexpected: %q", audio)
	}
	if _, err := os.Stat(audio); err != nil {
		t.Errorf("audio should exist: %v", err)
	}
}

func TestYtdlp_SatisfiesDownloaderInterface(t *testing.T) {
	var _ Downloader = (*YtdlpDownloader)(nil)
}

func TestYtdlp_CtxCancelledMidRun_WrapsCtxErr(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-slow")
	body := `exec sleep 5
`
	writeStub(t, script, body)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	d := &YtdlpDownloader{Binary: script}
	start := time.Now()
	_, err := d.FetchMetadata(ctx, "https://x.example/BV1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected ctx error; got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want errors.Is(err, context.DeadlineExceeded); got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("ctx cancel should short-circuit fast; took %s", elapsed)
	}
}

func TestYtdlp_ErrVideoNotFoundKeywordVariants(t *testing.T) {
	// yt-dlp phrases "video not found" a few different ways depending
	// on the extractor. Lock the recognised patterns so a stderr blob
	// resembling any of them maps to ErrVideoNotFound and pipeline
	// error-mapping stays predictable.
	cases := []struct {
		name   string
		stderr string
	}{
		{"video-unavailable", "ERROR: [BiliBili] BV1x: Video unavailable"},
		{"private-video", "ERROR: [BiliBili] BV1x: Private video"},
		{"lower-case-not-available", "ERROR: [BiliBili] BV1x: this content is not available"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := projectTempDir(t)
			script := filepath.Join(dir, "yt-dlp-stub")
			writeArgvCaptureStub(t, script, filepath.Join(dir, "argv.txt"),
				"", tc.stderr+"\n", 1)

			d := &YtdlpDownloader{Binary: script}
			_, err := d.FetchMetadata(context.Background(), "https://x.example/BV1x")
			if !errors.Is(err, ErrVideoNotFound) {
				t.Errorf("stderr=%q: want ErrVideoNotFound; got %v", tc.stderr, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// CookiesFromBrowser (login-by-default)
// ---------------------------------------------------------------------------

// TestYtdlp_CookiesFromBrowser_Unset_NoInjection is the guardrail for the
// zero-value contract: an unset CookiesFromBrowser must produce argv
// with NO `--cookies-from-browser` flag. The plan-5.x argv-shape
// assertions above already depend on this, so it is codified as its
// own test for clarity.
func TestYtdlp_CookiesFromBrowser_Unset_NoInjection(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureStub(t, script, capture, sampleMetaJSON, "", 0)

	d := &YtdlpDownloader{Binary: script} // CookiesFromBrowser unset
	if _, err := d.FetchMetadata(context.Background(),
		"https://www.bilibili.com/video/BV1noAuth"); err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	argv := readCaptureLines(t, capture)
	if argvContains(argv, "--cookies-from-browser") {
		t.Errorf("argv should not contain --cookies-from-browser when unset; got=%v", argv)
	}
}

// TestYtdlp_CookiesFromBrowser_Chrome_PrependsFlag pins the "login by
// default with Chrome" wiring: when the field is set to "chrome" the
// downloader must prepend `--cookies-from-browser chrome` in front of
// every existing argv, without perturbing the trailing URL positional
// or any downstream flag/value pairs.
func TestYtdlp_CookiesFromBrowser_Chrome_PrependsFlag(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureStub(t, script, capture, sampleMetaJSON, "", 0)

	d := &YtdlpDownloader{Binary: script, CookiesFromBrowser: "chrome"}
	url := "https://www.bilibili.com/video/BV1auth"
	if _, err := d.FetchMetadata(context.Background(), url); err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	argv := readCaptureLines(t, capture)
	want := []string{
		"--cookies-from-browser", "chrome",
		"--no-warnings",
		"--dump-single-json",
		"--write-subs", "--write-auto-subs",
		"--sub-langs", "zh-CN,zh-Hans,ai-zh",
		"--skip-download",
		url,
	}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv mismatch;\n got=%v\nwant=%v", argv, want)
	}
}

// TestYtdlp_CookiesFromBrowser_ProfileValuePassedThrough documents the
// "value is verbatim" contract for profile-qualified browsers like
// "chrome:Profile 1". A space-containing value must reach yt-dlp as a
// single argv token — this is what would break if we ever tried to
// str.Split(v, " ") in withAuth.
func TestYtdlp_CookiesFromBrowser_ProfileValuePassedThrough(t *testing.T) {
	dir := projectTempDir(t)
	script := filepath.Join(dir, "yt-dlp-stub")
	capture := filepath.Join(dir, "argv.txt")
	writeArgvCaptureStub(t, script, capture, sampleMetaJSON, "", 0)

	d := &YtdlpDownloader{Binary: script, CookiesFromBrowser: "chrome:Profile 1"}
	if _, err := d.FetchMetadata(context.Background(),
		"https://www.bilibili.com/video/BV1prof"); err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	argv := readCaptureLines(t, capture)
	i := argvIndex(argv, "--cookies-from-browser")
	if i < 0 {
		t.Fatalf("argv missing --cookies-from-browser; got=%v", argv)
	}
	if i+1 >= len(argv) || argv[i+1] != "chrome:Profile 1" {
		t.Errorf("--cookies-from-browser must be followed by %q as one token; argv=%v",
			"chrome:Profile 1", argv)
	}
}

// TestYtdlp_CookiesFromBrowser_NoneSentinelDisables locks the "opt-out"
// contract: "none" (case-insensitive, whitespace tolerated) suppresses
// the flag entirely. Downstream users who explicitly do not want a
// Keychain prompt rely on this to preserve the pre-login argv shape.
func TestYtdlp_CookiesFromBrowser_NoneSentinelDisables(t *testing.T) {
	for _, sentinel := range []string{"none", "NONE", "  none  "} {
		sentinel := sentinel
		t.Run(sentinel, func(t *testing.T) {
			dir := projectTempDir(t)
			script := filepath.Join(dir, "yt-dlp-stub")
			capture := filepath.Join(dir, "argv.txt")
			writeArgvCaptureStub(t, script, capture, sampleMetaJSON, "", 0)

			d := &YtdlpDownloader{Binary: script, CookiesFromBrowser: sentinel}
			if _, err := d.FetchMetadata(context.Background(),
				"https://www.bilibili.com/video/BV1none"); err != nil {
				t.Fatalf("FetchMetadata: %v", err)
			}
			argv := readCaptureLines(t, capture)
			if argvContains(argv, "--cookies-from-browser") {
				t.Errorf("%q sentinel should suppress the flag; argv=%v", sentinel, argv)
			}
		})
	}
}

// TestYtdlp_CookiesFromBrowser_AppliesToSubtitleAndAudio ensures the
// injection is symmetric across all three yt-dlp entry points — a
// regression here would cause FetchMetadata to see login cookies but
// DownloadAudio to fail on the same URL. We only assert the flag
// pair's presence here; the full argv shape is covered by the plan-5.x
// tests above.
func TestYtdlp_CookiesFromBrowser_AppliesToSubtitleAndAudio(t *testing.T) {
	dir := projectTempDir(t)
	// DownloadSubtitle
	{
		script := filepath.Join(dir, "yt-dlp-sub-stub")
		capture := filepath.Join(dir, "argv-sub.txt")
		writeSubtitleFileStub(t, script, capture, []string{"zh-CN"})

		outDir := filepath.Join(dir, "sub-out")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatalf("mkdir subOut: %v", err)
		}
		d := &YtdlpDownloader{Binary: script, CookiesFromBrowser: "chrome"}
		if _, err := d.DownloadSubtitle(context.Background(),
			"https://www.bilibili.com/video/BV1sub", outDir,
			[]string{"zh-CN", "zh-Hans", "ai-zh"}); err != nil {
			t.Fatalf("DownloadSubtitle: %v", err)
		}
		argv := readCaptureLines(t, capture)
		i := argvIndex(argv, "--cookies-from-browser")
		if i != 0 || i+1 >= len(argv) || argv[i+1] != "chrome" {
			t.Errorf("subtitle argv should start with --cookies-from-browser chrome; got=%v", argv)
		}
	}

	// DownloadAudio
	{
		script := filepath.Join(dir, "yt-dlp-audio-stub")
		capture := filepath.Join(dir, "argv-audio.txt")
		// exit 0 is fine even though no .m4a is produced — we only care
		// about argv here; the returned error is ignored.
		writeArgvCaptureStub(t, script, capture, "", "", 0)

		outDir := filepath.Join(dir, "audio-out")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatalf("mkdir audioOut: %v", err)
		}
		d := &YtdlpDownloader{Binary: script, CookiesFromBrowser: "chrome"}
		_, _ = d.DownloadAudio(context.Background(),
			"https://www.bilibili.com/video/BV1aud", outDir)
		argv := readCaptureLines(t, capture)
		i := argvIndex(argv, "--cookies-from-browser")
		if i != 0 || i+1 >= len(argv) || argv[i+1] != "chrome" {
			t.Errorf("audio argv should start with --cookies-from-browser chrome; got=%v", argv)
		}
	}
}
