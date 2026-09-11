package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// FakeDownloader is the [Downloader] implementation used by pipeline
// and integration tests. It never calls out to a real yt-dlp binary;
// the three methods either invoke a caller-supplied function field
// (when set) or fall back to a canonical default stub that mirrors the
// real yt-dlp semantics closely enough to exercise pipeline plumbing.
//
// The zero value is fully usable — all *Fn fields are nil and the
// default stubs kick in. This matches how [audio.FakeTranscoder] is
// designed and lets test authors write `f := &FakeDownloader{}`
// without further wiring.
//
// Field intent:
//
//   - FetchMetadataFn / DownloadSubtitleFn / DownloadAudioFn: caller
//     override for a single method; nil ⇒ default stub. Injecting a
//     function is how tests emulate specific yt-dlp outcomes
//     (ErrVideoNotFound, ErrNoSubtitle, exec failure, custom metadata).
//   - MetadataCalls / SubtitleCalls / AudioCalls: per-method call log
//     appended to on every invocation, whether the call went to the
//     injected fn or the default stub. Tests assert "was called N
//     times with these args" without wrapping the fake in another mock.
//
// Contract fidelity — the default stubs deliberately reproduce the
// side-effect surface documented in contracts.md §二.1 so that
// swapping in the real ytdlp implementation later does not change
// pipeline behaviour:
//
//   - FetchMetadata returns a non-nil *Metadata with BVID
//     "BV1fake000000", TotalPages 1, a plausible Title, and
//     a single zh-CN CC subtitle track (mimicking testdata/metadata.json).
//   - DownloadSubtitle writes `<outDir>/<bvid>.<lang>.srt` where lang
//     is the FIRST entry of langPref (matching the "first hit wins"
//     rule of the real DownloadSubtitle) and content is a copy of
//     testdata/sample.srt located via runtime.Caller.
//   - DownloadAudio writes `<outDir>/<bvid>.m4a` with content copied
//     from testdata/sample.m4a.
//
// All three default stubs use the fake BV id "BV1fake000000" so tests
// can predict the resulting filenames without wiring metadata through.
// Note: filenames from the default stubs always carry `fakeBVID`
// regardless of the url argument; if a test needs a different BV id,
// inject FetchMetadataFn + DownloadSubtitleFn/DownloadAudioFn together.
//
// # Concurrency
//
// FakeDownloader is NOT safe for concurrent use across methods — the
// three *Calls slices are appended without a mutex, matching the way
// pipeline drives Downloader serially per URL. If a future consumer
// needs parallel invocations, wrap accesses with an external lock or
// switch to a per-call channel; do not change the fake in place, as
// doing so would leak a lock into all pipeline unit tests.
type FakeDownloader struct {
	// FetchMetadataFn overrides FetchMetadata; nil ⇒ default stub.
	FetchMetadataFn func(ctx context.Context, url string) (*Metadata, error)
	// DownloadSubtitleFn overrides DownloadSubtitle; nil ⇒ default stub.
	DownloadSubtitleFn func(ctx context.Context, url, outDir string, langPref []string) (string, error)
	// DownloadAudioFn overrides DownloadAudio; nil ⇒ default stub.
	DownloadAudioFn func(ctx context.Context, url, outDir string) (string, error)

	// MetadataCalls records every url passed to FetchMetadata, in
	// invocation order. Appended AFTER argument validation but BEFORE
	// Fn dispatch, so tests can verify a call happened even when the
	// injected fn returns an error; rejected calls (empty url,
	// cancelled ctx) do NOT enter the log.
	MetadataCalls []string
	// SubtitleCalls records DownloadSubtitle invocations.
	SubtitleCalls []FakeSubtitleCall
	// AudioCalls records DownloadAudio invocations.
	AudioCalls []FakeAudioCall
}

// FakeSubtitleCall captures one DownloadSubtitle invocation. LangPref
// is a copy — the pipeline is free to mutate the slice it passed in
// after the call returns without corrupting the call log.
type FakeSubtitleCall struct {
	URL      string
	OutDir   string
	LangPref []string
}

// FakeAudioCall captures one DownloadAudio invocation.
type FakeAudioCall struct {
	URL    string
	OutDir string
}

// fakeBVID is the canonical BV id the default stubs advertise. All
// three default stubs use the same value so filenames the pipeline
// derives from FetchMetadata line up with the paths DownloadSubtitle /
// DownloadAudio actually write to.
const fakeBVID = "BV1fake000000"

// FetchMetadata dispatches to FetchMetadataFn when set, otherwise
// returns a canonical stub Metadata. ctx cancellation is honoured
// before any user-supplied function is invoked so tests observe the
// same ordering as the real ytdlp implementation would after
// exec.CommandContext receives SIGKILL.
func (f *FakeDownloader) FetchMetadata(ctx context.Context, url string) (*Metadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("downloader: fake: %w", err)
	}
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("downloader: fake: empty url")
	}
	// Record BEFORE dispatching so tests can assert a call happened
	// even when the injected fn returns an error.
	f.MetadataCalls = append(f.MetadataCalls, url)

	if f.FetchMetadataFn != nil {
		return f.FetchMetadataFn(ctx, url)
	}
	return defaultMetadataStub(), nil
}

// DownloadSubtitle dispatches to DownloadSubtitleFn when set,
// otherwise writes `<outDir>/<fakeBVID>.<langPref[0]>.srt` with the
// content of testdata/sample.srt.
//
// Contract enforcement performed before dispatch:
//   - ctx cancellation ⇒ context.Canceled/DeadlineExceeded wrapped in
//     "downloader: fake: %w".
//   - Empty url / empty outDir / empty langPref ⇒ synchronous error;
//     the real implementation would misinterpret these and either
//     shell out to yt-dlp with garbage argv or silently succeed.
//   - Missing outDir ⇒ error, matching contracts.md §二.1 clause
//     "outDir MUST exist — implementations may not create it".
func (f *FakeDownloader) DownloadSubtitle(ctx context.Context, url, outDir string, langPref []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("downloader: fake: %w", err)
	}
	if strings.TrimSpace(url) == "" {
		return "", errors.New("downloader: fake: empty url")
	}
	if strings.TrimSpace(outDir) == "" {
		return "", errors.New("downloader: fake: empty outDir")
	}
	if len(langPref) == 0 {
		return "", errors.New("downloader: fake: empty langPref")
	}
	if info, err := os.Stat(outDir); err != nil {
		return "", fmt.Errorf("downloader: fake: outDir %s: %w", outDir, err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("downloader: fake: outDir %s: not a directory", outDir)
	}

	// Deep-copy langPref for the call log so caller mutations after
	// return do not race with tests reading SubtitleCalls.
	langCopy := append([]string(nil), langPref...)
	f.SubtitleCalls = append(f.SubtitleCalls, FakeSubtitleCall{
		URL:      url,
		OutDir:   outDir,
		LangPref: langCopy,
	})

	if f.DownloadSubtitleFn != nil {
		return f.DownloadSubtitleFn(ctx, url, outDir, langPref)
	}

	// Default stub: <outDir>/<fakeBVID>.<lang>.srt, copy sample.srt.
	// The first language in langPref wins, mirroring the "first hit"
	// contract of the real yt-dlp DownloadSubtitle.
	dst := filepath.Join(outDir, fmt.Sprintf("%s.%s.srt", fakeBVID, langPref[0]))
	if err := copyTestdata("sample.srt", dst); err != nil {
		return "", fmt.Errorf("downloader: fake: %w", err)
	}
	abs, err := filepath.Abs(dst)
	if err != nil {
		return "", fmt.Errorf("downloader: fake: abs %s: %w", dst, err)
	}
	return abs, nil
}

// DownloadAudio dispatches to DownloadAudioFn when set, otherwise
// writes `<outDir>/<fakeBVID>.m4a` with the content of
// testdata/sample.m4a. Contract enforcement mirrors DownloadSubtitle.
func (f *FakeDownloader) DownloadAudio(ctx context.Context, url, outDir string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("downloader: fake: %w", err)
	}
	if strings.TrimSpace(url) == "" {
		return "", errors.New("downloader: fake: empty url")
	}
	if strings.TrimSpace(outDir) == "" {
		return "", errors.New("downloader: fake: empty outDir")
	}
	if info, err := os.Stat(outDir); err != nil {
		return "", fmt.Errorf("downloader: fake: outDir %s: %w", outDir, err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("downloader: fake: outDir %s: not a directory", outDir)
	}

	f.AudioCalls = append(f.AudioCalls, FakeAudioCall{URL: url, OutDir: outDir})

	if f.DownloadAudioFn != nil {
		return f.DownloadAudioFn(ctx, url, outDir)
	}

	dst := filepath.Join(outDir, fmt.Sprintf("%s.m4a", fakeBVID))
	if err := copyTestdata("sample.m4a", dst); err != nil {
		return "", fmt.Errorf("downloader: fake: %w", err)
	}
	abs, err := filepath.Abs(dst)
	if err != nil {
		return "", fmt.Errorf("downloader: fake: abs %s: %w", dst, err)
	}
	return abs, nil
}

// defaultMetadataStub is the Metadata FetchMetadata returns when no
// FetchMetadataFn is injected. Values line up with testdata/metadata.json
// so integration tests that swap in the real ytdlp against fakebin see
// equivalent-shaped data.
func defaultMetadataStub() *Metadata {
	return &Metadata{
		BVID:       fakeBVID,
		Title:      "Fake Video Title — 测试样例",
		TotalPages: 1,
		Duration:   187 * time.Second,
		Subtitles: []SubtitleTrack{
			{Lang: "zh-CN", Ext: "srt", URL: "https://example.invalid/subs/zh-CN.srt"},
		},
		AutoCaptions: []SubtitleTrack{
			{Lang: "ai-zh", Ext: "srt", URL: "https://example.invalid/auto/ai-zh.srt"},
		},
	}
}

// copyTestdata copies the named file from the repo's testdata/ dir to
// dst. testdata/ is located relative to this source file so callers do
// not have to chdir into the repo root — critical because Go tests run
// each package in its own cwd (the package dir).
func copyTestdata(name, dst string) error {
	src, err := testdataPath(name)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}

// testdataPath locates a file under the repo's testdata/ dir using
// runtime.Caller as the anchor so the fake works no matter which
// package's tests invoke it (pipeline tests run in
// internal/pipeline/, integration tests in test/integration/, etc.).
//
// The file is expected at:
//
//	<repo-root>/testdata/<name>
//
// where <repo-root> is derived from this source file's path (three
// directory levels up: internal/downloader/fake.go → repo root).
func testdataPath(name string) (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed to locate fake.go source")
	}
	// thisFile = <repo>/internal/downloader/fake.go
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	full := filepath.Join(repoRoot, "testdata", name)
	if _, err := os.Stat(full); err != nil {
		return "", fmt.Errorf("testdata %s missing at %s: %w", name, full, err)
	}
	return full, nil
}

// Compile-time check: FakeDownloader satisfies [Downloader]. If the
// interface signature drifts (contract update) the build breaks here
// before any caller notices, matching the pattern used by
// [audio.FakeTranscoder].
var _ Downloader = (*FakeDownloader)(nil)
