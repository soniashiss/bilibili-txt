package pipeline

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"bilibili-txt/internal/asr"
	"bilibili-txt/internal/audio"
	"bilibili-txt/internal/config"
	"bilibili-txt/internal/downloader"
	"bilibili-txt/internal/formatter"
	"bilibili-txt/internal/logger"
	"bilibili-txt/internal/naming"
	"bilibili-txt/internal/subtitle"
)

// projectTempDir returns a fresh sandbox-friendly temp dir living
// inside the repo under .tmp/. Even though pipeline tests never spawn
// child processes, os.MkdirTemp("", …) under /var/folders/… can still
// leave stray dirs that the sandbox occasionally trips on when large
// numbers of parallel tests run. Mirroring the pattern from
// [internal/audio/ffmpeg_test.go] keeps every artefact under a single
// repo-local sweep point.
//
// The directory is auto-cleaned at test end via t.Cleanup.
func projectTempDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(1)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	base := filepath.Join(repoRoot, ".tmp", "pipeline-test")
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

// redirectTmpDir points os.MkdirTemp("", …) at a sandbox-safe dir
// under the repo so pipeline.Run's internal tmpDir creation (which
// uses the empty-string form of MkdirTemp) doesn't land in
// /var/folders/. Also asserts the destination exists so Run's tmpDir
// creation itself doesn't ENOENT.
//
// Returns the dir for tests that want to inspect leftover state after
// Run returns (e.g. cleanup / keep-intermediate assertions).
func redirectTmpDir(t *testing.T) string {
	t.Helper()
	d := projectTempDir(t)
	t.Setenv("TMPDIR", d)
	return d
}

// baseCfg returns a config the pipeline can drive directly. Format
// defaults to txt (the simplest branch); tests override individual
// fields as needed.
func baseCfg(t *testing.T, outDir string) *config.Config {
	t.Helper()
	return &config.Config{
		OutputDir: outDir,
		Format:    "txt",
		Naming: config.Naming{
			// Default the strategy to overwrite so tests that
			// don't explicitly test conflict logic never trip on
			// pre-existing files.
			OnConflict: naming.StrategyOverwrite,
		},
	}
}

// stubMetadataFn returns a FetchMetadataFn that yields a plausible
// [downloader.Metadata] with the caller-supplied caption tracks. We
// avoid the default stub (which advertises a fixed zh-CN + ai-zh
// pair) so language-priority tests can vary the input freely.
func stubMetadataFn(bvid, title string, subs, autos []downloader.SubtitleTrack) func(context.Context, string) (*downloader.Metadata, error) {
	return func(_ context.Context, _ string) (*downloader.Metadata, error) {
		return &downloader.Metadata{
			BVID:         bvid,
			Title:        title,
			TotalPages:   1,
			Subtitles:    subs,
			AutoCaptions: autos,
		}, nil
	}
}

// TestRun_SubtitleBranch_HappyPath_TXT verifies the golden path:
// metadata → CC subtitle download → parse → txt format → disk.
// Also asserts the returned Result carries the expected fields.
func TestRun_SubtitleBranch_HappyPath_TXT(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVfake001", "标题", []downloader.SubtitleTrack{
			{Lang: "zh-CN", Ext: "srt"},
		}, nil),
	}

	res, err := Run(context.Background(), Input{URL: "https://example.invalid/BVfake001"}, cfg, Deps{Downloader: fake})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil {
		t.Fatal("Run returned nil result without error")
	}
	if res.Source != SourceSubtitle {
		t.Errorf("Source = %q, want %q", res.Source, SourceSubtitle)
	}
	if !strings.HasSuffix(res.OutputPath, ".txt") {
		t.Errorf("OutputPath = %q, want .txt suffix", res.OutputPath)
	}
	if _, err := os.Stat(res.OutputPath); err != nil {
		t.Errorf("output file not written: %v", err)
	}
	if res.Metadata == nil || res.Metadata.BVID != "BVfake001" {
		t.Errorf("Metadata not propagated: %+v", res.Metadata)
	}
	if res.Duration <= 0 {
		t.Errorf("Duration must be positive, got %s", res.Duration)
	}
	if len(fake.SubtitleCalls) != 1 {
		t.Fatalf("expected 1 subtitle call, got %d", len(fake.SubtitleCalls))
	}
	if got := fake.SubtitleCalls[0].LangPref; len(got) != 1 || got[0] != "zh-CN" {
		t.Errorf("LangPref = %v, want [zh-CN]", got)
	}
}

// TestRun_SubtitleBranch_AllFormats sweeps the three supported
// formatter outputs to make sure Run correctly routes each Format
// value through formatter.Convert.
func TestRun_SubtitleBranch_AllFormats(t *testing.T) {
	cases := []struct {
		format string
		ext    string
	}{
		{"txt", ".txt"},
		{"md", ".md"},
		{"srt", ".srt"},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			redirectTmpDir(t)
			outDir := projectTempDir(t)
			cfg := baseCfg(t, outDir)
			cfg.Format = tc.format

			fake := &downloader.FakeDownloader{
				FetchMetadataFn: stubMetadataFn("BVfmt", "T", []downloader.SubtitleTrack{
					{Lang: "zh-CN", Ext: "srt"},
				}, nil),
			}
			res, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !strings.HasSuffix(res.OutputPath, tc.ext) {
				t.Errorf("OutputPath = %q, want %s suffix", res.OutputPath, tc.ext)
			}
			b, err := os.ReadFile(res.OutputPath)
			if err != nil {
				t.Fatalf("read output: %v", err)
			}
			if len(b) == 0 {
				t.Errorf("output file is empty")
			}
		})
	}
}

// TestRun_LanguagePriority verifies that when several caption tracks
// are advertised, the frozen zh-CN > zh-Hans > ai-zh priority list
// picks the highest-ranked available track. Official CC also outranks
// AI captions of a lower-priority language.
func TestRun_LanguagePriority(t *testing.T) {
	cases := []struct {
		name       string
		subs       []downloader.SubtitleTrack
		autos      []downloader.SubtitleTrack
		wantLang   string
		wantSource Source
	}{
		{
			name: "zh-CN wins over zh-Hans in CC pool",
			subs: []downloader.SubtitleTrack{
				{Lang: "zh-Hans", Ext: "srt"},
				{Lang: "zh-CN", Ext: "srt"},
			},
			wantLang:   "zh-CN",
			wantSource: SourceSubtitle,
		},
		{
			name: "zh-Hans wins when zh-CN absent",
			subs: []downloader.SubtitleTrack{
				{Lang: "en", Ext: "srt"},
				{Lang: "zh-Hans", Ext: "srt"},
			},
			wantLang:   "zh-Hans",
			wantSource: SourceSubtitle,
		},
		{
			name:  "ai-zh (auto) picked when no CC match",
			subs:  []downloader.SubtitleTrack{{Lang: "en", Ext: "srt"}},
			autos: []downloader.SubtitleTrack{{Lang: "ai-zh", Ext: "srt"}},
			// The CC pool has a track but no zh-*/ai-zh match; we
			// fall through to auto captions.
			wantLang:   "ai-zh",
			wantSource: SourceAuto,
		},
		{
			name: "CC zh-Hans beats auto zh-CN — CC pool tried first",
			subs: []downloader.SubtitleTrack{{Lang: "zh-Hans", Ext: "srt"}},
			autos: []downloader.SubtitleTrack{
				{Lang: "zh-CN", Ext: "srt"},
			},
			wantLang:   "zh-Hans",
			wantSource: SourceSubtitle,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			redirectTmpDir(t)
			outDir := projectTempDir(t)
			cfg := baseCfg(t, outDir)

			fake := &downloader.FakeDownloader{
				FetchMetadataFn: stubMetadataFn("BVprio", "T", tc.subs, tc.autos),
			}
			res, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", res.Source, tc.wantSource)
			}
			if len(fake.SubtitleCalls) != 1 {
				t.Fatalf("expected 1 subtitle call, got %d", len(fake.SubtitleCalls))
			}
			got := fake.SubtitleCalls[0].LangPref
			if len(got) != 1 || got[0] != tc.wantLang {
				t.Errorf("LangPref = %v, want [%s]", got, tc.wantLang)
			}
		})
	}
}

// TestRun_MetadataError propagates the downloader's error and
// preserves the [downloader.ErrVideoNotFound] sentinel through the
// wrap so callers can errors.Is against it.
func TestRun_MetadataError(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: func(_ context.Context, _ string) (*downloader.Metadata, error) {
			return nil, downloader.ErrVideoNotFound
		},
	}
	_, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, downloader.ErrVideoNotFound) {
		t.Errorf("errors.Is(err, ErrVideoNotFound) = false; got %v", err)
	}
}

// TestRun_NoSubtitleFallsThroughToASR ensures a downloader that
// advertises tracks but then returns [downloader.ErrNoSubtitle] on
// actual download falls through to the ASR branch. Post T-3.3 the
// branch runs end-to-end and produces a real transcript.
func TestRun_NoSubtitleFallsThroughToASR(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVns", "T", []downloader.SubtitleTrack{
			{Lang: "zh-CN", Ext: "srt"},
		}, nil),
		DownloadSubtitleFn: func(_ context.Context, _, _ string, _ []string) (string, error) {
			return "", downloader.ErrNoSubtitle
		},
	}
	transcoder := &audio.FakeTranscoder{}
	recognizer := &asr.FakeRecognizer{}
	deps := Deps{Downloader: fake, Transcoder: transcoder, Recognizer: recognizer}

	res, err := Run(context.Background(), Input{URL: "u"}, cfg, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Source != SourceASR {
		t.Errorf("Source = %q, want %q", res.Source, SourceASR)
	}
	if len(fake.AudioCalls) != 1 {
		t.Errorf("expected 1 DownloadAudio call, got %d", len(fake.AudioCalls))
	}
	if len(transcoder.Calls) != 1 {
		t.Errorf("expected 1 transcode call, got %d", len(transcoder.Calls))
	}
	if len(recognizer.Calls) != 1 {
		t.Errorf("expected 1 recognizer call, got %d", len(recognizer.Calls))
	}
	if _, err := os.Stat(res.OutputPath); err != nil {
		t.Errorf("output file not written: %v", err)
	}
}

// TestRun_NoTracksSkipsToASR — metadata advertises zero subtitle and
// zero auto-caption tracks. Pipeline should route straight to ASR
// without ever calling DownloadSubtitle, and run the full ASR pipeline
// to completion.
func TestRun_NoTracksSkipsToASR(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVnt", "T", nil, nil),
	}
	deps := Deps{
		Downloader: fake,
		Transcoder: &audio.FakeTranscoder{},
		Recognizer: &asr.FakeRecognizer{},
	}
	res, err := Run(context.Background(), Input{URL: "u"}, cfg, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Source != SourceASR {
		t.Errorf("Source = %q, want %q", res.Source, SourceASR)
	}
	if len(fake.SubtitleCalls) != 0 {
		t.Errorf("no subtitle call should have been made, got %d", len(fake.SubtitleCalls))
	}
	if len(fake.AudioCalls) != 1 {
		t.Errorf("expected 1 DownloadAudio call, got %d", len(fake.AudioCalls))
	}
}

// TestRun_ParseError_ErrEmpty_FallsThroughToASR writes an empty SRT
// so subtitle.Parse surfaces subtitle.ErrEmpty. F2 mandates that an
// empty CC/AI track — semantically equivalent to "no subtitle" — MUST
// fall through to the ASR branch rather than fail the whole run;
// otherwise a mirage-only caption listing (yt-dlp sometimes advertises
// a track whose file is a zero-byte stub) would deny the user any
// transcript at all. Mirrors TestRun_NoSubtitleFallsThroughToASR.
func TestRun_ParseError_ErrEmpty_FallsThroughToASR(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVpe", "T", []downloader.SubtitleTrack{
			{Lang: "zh-CN", Ext: "srt"},
		}, nil),
		DownloadSubtitleFn: func(_ context.Context, _, outDir string, langPref []string) (string, error) {
			dst := filepath.Join(outDir, "empty.srt")
			// Empty file → subtitle.Parse returns ErrEmpty.
			if err := os.WriteFile(dst, nil, 0o644); err != nil {
				return "", err
			}
			return dst, nil
		},
	}
	transcoder := &audio.FakeTranscoder{}
	recognizer := &asr.FakeRecognizer{}
	deps := Deps{Downloader: fake, Transcoder: transcoder, Recognizer: recognizer}

	res, err := Run(context.Background(), Input{URL: "u"}, cfg, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Source != SourceASR {
		t.Errorf("Source = %q, want %q", res.Source, SourceASR)
	}
	if len(fake.SubtitleCalls) != 1 {
		t.Errorf("expected 1 subtitle download call, got %d", len(fake.SubtitleCalls))
	}
	if len(fake.AudioCalls) != 1 {
		t.Errorf("expected 1 DownloadAudio call, got %d", len(fake.AudioCalls))
	}
	if len(transcoder.Calls) != 1 {
		t.Errorf("expected 1 transcode call, got %d", len(transcoder.Calls))
	}
	if len(recognizer.Calls) != 1 {
		t.Errorf("expected 1 recognizer call, got %d", len(recognizer.Calls))
	}
	if _, err := os.Stat(res.OutputPath); err != nil {
		t.Errorf("output file not written: %v", err)
	}
}

// TestRun_ParseError_ErrMalformed_FallsThroughToASR is the twin of the
// ErrEmpty fallback: a syntactically broken SRT (invalid timestamp)
// is functionally as useless as an absent track, so F2 routes it to
// ASR instead of failing the run. The malformed line here trips
// parseTimestamp's ErrMalformed wrap without ever producing a valid
// cue.
func TestRun_ParseError_ErrMalformed_FallsThroughToASR(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVpm", "T", []downloader.SubtitleTrack{
			{Lang: "zh-CN", Ext: "srt"},
		}, nil),
		DownloadSubtitleFn: func(_ context.Context, _, outDir string, langPref []string) (string, error) {
			dst := filepath.Join(outDir, "malformed.srt")
			// Timing line with a garbage left-hand timestamp →
			// parseTimestamp returns ErrMalformed.
			body := "1\nbadtimestamp --> 00:00:02,000\nhello\n"
			if err := os.WriteFile(dst, []byte(body), 0o644); err != nil {
				return "", err
			}
			return dst, nil
		},
	}
	transcoder := &audio.FakeTranscoder{}
	recognizer := &asr.FakeRecognizer{}
	deps := Deps{Downloader: fake, Transcoder: transcoder, Recognizer: recognizer}

	res, err := Run(context.Background(), Input{URL: "u"}, cfg, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Source != SourceASR {
		t.Errorf("Source = %q, want %q", res.Source, SourceASR)
	}
	if len(fake.SubtitleCalls) != 1 {
		t.Errorf("expected 1 subtitle download call, got %d", len(fake.SubtitleCalls))
	}
	if len(fake.AudioCalls) != 1 {
		t.Errorf("expected 1 DownloadAudio call, got %d", len(fake.AudioCalls))
	}
	if len(transcoder.Calls) != 1 {
		t.Errorf("expected 1 transcode call, got %d", len(transcoder.Calls))
	}
	if len(recognizer.Calls) != 1 {
		t.Errorf("expected 1 recognizer call, got %d", len(recognizer.Calls))
	}
	if _, err := os.Stat(res.OutputPath); err != nil {
		t.Errorf("output file not written: %v", err)
	}
}

// TestRun_UnknownFormatRejected — an unknown Format string should be
// rejected before any tmpDir/network work happens.
func TestRun_UnknownFormatRejected(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Format = "vtt" // not supported

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVuf", "T", []downloader.SubtitleTrack{
			{Lang: "zh-CN", Ext: "srt"},
		}, nil),
	}
	_, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake})
	if !errors.Is(err, formatter.ErrUnknownFormat) {
		t.Fatalf("expected ErrUnknownFormat, got %v", err)
	}
	if len(fake.SubtitleCalls) != 0 {
		t.Errorf("unknown format must be rejected before downloader is touched (calls=%d)", len(fake.SubtitleCalls))
	}
}

// TestRun_ConflictSkipReturnsCached — pre-existing output + skip
// strategy returns SourceCached with the same path and touches
// neither the downloader nor tmpDir.
func TestRun_ConflictSkipReturnsCached(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Naming.OnConflict = naming.StrategySkip

	meta := &downloader.Metadata{
		BVID: "BVcs", Title: "T", TotalPages: 1,
		Subtitles: []downloader.SubtitleTrack{{Lang: "zh-CN", Ext: "srt"}},
	}
	expected := naming.BuildPath(outDir, meta.Title, meta.BVID, 1, 1, "txt")
	if err := os.WriteFile(expected, []byte("pre-existing"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: func(_ context.Context, _ string) (*downloader.Metadata, error) {
			return meta, nil
		},
	}
	res, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Source != SourceCached {
		t.Errorf("Source = %q, want %q", res.Source, SourceCached)
	}
	abs, _ := filepath.Abs(expected)
	if res.OutputPath != abs {
		t.Errorf("OutputPath = %q, want %q", res.OutputPath, abs)
	}
	if len(fake.SubtitleCalls) != 0 {
		t.Errorf("skip must not trigger subtitle download, got %d calls", len(fake.SubtitleCalls))
	}
	// Content preserved (not overwritten).
	b, _ := os.ReadFile(expected)
	if string(b) != "pre-existing" {
		t.Errorf("existing file content mutated: %q", string(b))
	}
}

// TestRun_ConflictOverwriteWrites — pre-existing output + overwrite
// strategy re-runs the branch and replaces the file bytes.
func TestRun_ConflictOverwriteWrites(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Naming.OnConflict = naming.StrategyOverwrite

	meta := &downloader.Metadata{
		BVID: "BVco", Title: "T", TotalPages: 1,
		Subtitles: []downloader.SubtitleTrack{{Lang: "zh-CN", Ext: "srt"}},
	}
	expected := naming.BuildPath(outDir, meta.Title, meta.BVID, 1, 1, "txt")
	if err := os.WriteFile(expected, []byte("PRE-EXISTING"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: func(_ context.Context, _ string) (*downloader.Metadata, error) {
			return meta, nil
		},
	}
	res, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Source != SourceSubtitle {
		t.Errorf("Source = %q, want %q", res.Source, SourceSubtitle)
	}
	b, _ := os.ReadFile(expected)
	if string(b) == "PRE-EXISTING" {
		t.Errorf("overwrite did not replace file content")
	}
	if len(b) == 0 {
		t.Errorf("output file is empty after overwrite")
	}
}

// TestRun_ConflictAbortReturnsError — pre-existing output + abort
// strategy short-circuits with an error and does not touch the
// downloader.
func TestRun_ConflictAbortReturnsError(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Naming.OnConflict = naming.StrategyAbort

	meta := &downloader.Metadata{
		BVID: "BVca", Title: "T", TotalPages: 1,
		Subtitles: []downloader.SubtitleTrack{{Lang: "zh-CN", Ext: "srt"}},
	}
	expected := naming.BuildPath(outDir, meta.Title, meta.BVID, 1, 1, "txt")
	if err := os.WriteFile(expected, []byte("EXISTING"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: func(_ context.Context, _ string) (*downloader.Metadata, error) {
			return meta, nil
		},
	}
	_, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(fake.SubtitleCalls) != 0 {
		t.Errorf("abort must not trigger subtitle download, got %d calls", len(fake.SubtitleCalls))
	}
}

// TestRun_CtxAlreadyCancelled — ctx.Err() before any tool call should
// return immediately without hitting the downloader.
func TestRun_CtxAlreadyCancelled(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fake := &downloader.FakeDownloader{}
	_, err := Run(ctx, Input{URL: "u"}, cfg, Deps{Downloader: fake})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(fake.MetadataCalls) != 0 {
		t.Errorf("cancelled ctx must not touch downloader, got %d metadata calls", len(fake.MetadataCalls))
	}
}

// TestRun_TmpDirCleanedByDefault — after a successful run the tmpDir
// under $TMPDIR must be gone (KeepIntermediate defaults to false).
func TestRun_TmpDirCleanedByDefault(t *testing.T) {
	tmpRoot := redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVcln", "T", []downloader.SubtitleTrack{
			{Lang: "zh-CN", Ext: "srt"},
		}, nil),
	}
	if _, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatalf("read tmpRoot: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bilibili-txt-") {
			t.Errorf("tmpDir %q not cleaned up", e.Name())
		}
	}
}

// TestRun_KeepIntermediateLeavesTmpDir — KeepIntermediate=true must
// leave the tmpDir on disk so users can inspect the raw artefacts.
func TestRun_KeepIntermediateLeavesTmpDir(t *testing.T) {
	tmpRoot := redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVkeep", "T", []downloader.SubtitleTrack{
			{Lang: "zh-CN", Ext: "srt"},
		}, nil),
	}
	if _, err := Run(context.Background(), Input{URL: "u", KeepIntermediate: true}, cfg, Deps{Downloader: fake}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatalf("read tmpRoot: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bilibili-txt-") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("KeepIntermediate=true should leave tmp dir behind, none found in %s", tmpRoot)
	}
}

// TestRun_ForceASRRoutesToASRBranch — ForceASR bypasses the subtitle
// branch entirely and enters ASR. Post T-3.3 this means we run the
// download-audio → transcode → transcribe → parse → format chain and
// emit a SourceASR result even when caption tracks are advertised.
func TestRun_ForceASRRoutesToASRBranch(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVfa", "T", []downloader.SubtitleTrack{
			{Lang: "zh-CN", Ext: "srt"},
		}, nil),
	}
	deps := Deps{
		Downloader: fake,
		Transcoder: &audio.FakeTranscoder{},
		Recognizer: &asr.FakeRecognizer{},
	}
	res, err := Run(context.Background(), Input{URL: "u", ForceASR: true}, cfg, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Source != SourceASR {
		t.Errorf("Source = %q, want %q", res.Source, SourceASR)
	}
	if len(fake.SubtitleCalls) != 0 {
		t.Errorf("ForceASR must not download subtitles, got %d calls", len(fake.SubtitleCalls))
	}
	if len(fake.AudioCalls) != 1 {
		t.Errorf("expected 1 DownloadAudio call, got %d", len(fake.AudioCalls))
	}
	if _, err := os.Stat(res.OutputPath); err != nil {
		t.Errorf("output file not written: %v", err)
	}
}

// TestRun_ASRBranchMissingDeps — entering the ASR branch without
// Transcoder or Recognizer must produce a clear "deps.X is nil" error
// rather than a nil-deref panic.
func TestRun_ASRBranchMissingDeps(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVdm", "T", nil, nil),
	}
	_, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "transcoder") && !strings.Contains(err.Error(), "recognizer") {
		t.Errorf("expected missing-deps error, got %v", err)
	}
}

// TestRun_NilDownloader — leaving deps.Downloader nil must fail fast
// before any other work.
func TestRun_NilDownloader(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	_, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "downloader") {
		t.Errorf("expected downloader-missing error, got %v", err)
	}
}

// TestRun_EmptyURL / TestRun_NilConfig — defensive input validation
// per contracts.md §五 (pipeline never reaches yt-dlp with garbage).
func TestRun_EmptyURL(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	fake := &downloader.FakeDownloader{}
	_, err := Run(context.Background(), Input{URL: "   "}, cfg, Deps{Downloader: fake})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(fake.MetadataCalls) != 0 {
		t.Errorf("empty URL must not touch downloader, got %d calls", len(fake.MetadataCalls))
	}
}

func TestRun_NilConfig(t *testing.T) {
	fake := &downloader.FakeDownloader{}
	_, err := Run(context.Background(), Input{URL: "u"}, nil, Deps{Downloader: fake})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestRun_NilMetadataFromDownloader — a downloader that returns
// (nil, nil) is a contract violation; pipeline should reject cleanly
// rather than nil-deref.
func TestRun_NilMetadataFromDownloader(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: func(_ context.Context, _ string) (*downloader.Metadata, error) {
			return nil, nil
		},
	}
	_, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "nil metadata") {
		t.Errorf("expected nil-metadata error, got %v", err)
	}
}

// TestRun_MalformedBVIDPanicIsRecovered — naming.BuildPath panics on
// obviously malformed BV ids; the pipeline must recover and surface a
// normal error instead of taking the whole process down.
func TestRun_MalformedBVIDPanicIsRecovered(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: func(_ context.Context, _ string) (*downloader.Metadata, error) {
			return &downloader.Metadata{
				BVID:       "bad/../bvid", // path separator triggers panic
				Title:      "T",
				TotalPages: 1,
			}, nil
		},
	}
	_, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "build output path") {
		t.Errorf("expected build output path error, got %v", err)
	}
}

// TestRun_DownloadSubtitleGenericError — any non-ErrNoSubtitle error
// from DownloadSubtitle must be surfaced (wrapped) and NOT trigger
// the ASR fallback.
func TestRun_DownloadSubtitleGenericError(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVdg", "T", []downloader.SubtitleTrack{
			{Lang: "zh-CN", Ext: "srt"},
		}, nil),
		DownloadSubtitleFn: func(_ context.Context, _, _ string, _ []string) (string, error) {
			return "", errors.New("network is bad")
		},
	}
	// Even with ASR deps wired the generic error must NOT fall
	// through to ASR — only ErrNoSubtitle triggers the fallback.
	deps := Deps{
		Downloader: fake,
		Transcoder: &audio.FakeTranscoder{},
		Recognizer: &asr.FakeRecognizer{},
	}
	_, err := Run(context.Background(), Input{URL: "u"}, cfg, deps)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(fake.AudioCalls) != 0 {
		t.Errorf("generic download error must NOT trigger ASR fallback (AudioCalls=%d)", len(fake.AudioCalls))
	}
	if !strings.Contains(err.Error(), "download subtitle") {
		t.Errorf("expected 'download subtitle' in error, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// T-3.3 ASR branch tests
//
// Rationale: the ASR branch has a five-step sequence (download-audio →
// transcode → transcribe → parse → format) that the subtitle branch does not
// exercise. Each step carries its own error mode and intermediate-file naming
// convention (contracts.md §二.2/§二.3 + plan §4.4), so we test them
// individually rather than piggy-backing on the subtitle happy-path assertions.
// -----------------------------------------------------------------------------

// TestRun_ASRBranch_HappyPath_SRT drives the full ASR branch with the
// srt output format so the raw ASR-produced srt bytes travel through
// formatter.Convert unchanged. Verifies:
//   - Source == SourceASR
//   - Result.Metadata / Duration populated
//   - The output file exists AND its content came from the ASR chain
//     (matches the FakeRecognizer's default srt payload).
//   - DownloadAudio / ToWav16kMono / Transcribe each called exactly once.
func TestRun_ASRBranch_HappyPath_SRT(t *testing.T) {
	tmpRoot := redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Format = "srt"
	cfg.Model = "/tmp/fake-model.bin"

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVasrHP", "标题-asr", nil, nil),
	}
	transcoder := &audio.FakeTranscoder{}
	recognizer := &asr.FakeRecognizer{}
	deps := Deps{Downloader: fake, Transcoder: transcoder, Recognizer: recognizer}

	res, err := Run(context.Background(), Input{URL: "https://example.invalid/BVasrHP"}, cfg, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil {
		t.Fatal("Run returned nil result without error")
	}
	if res.Source != SourceASR {
		t.Errorf("Source = %q, want %q", res.Source, SourceASR)
	}
	if res.Metadata == nil || res.Metadata.BVID != "BVasrHP" {
		t.Errorf("Metadata not propagated: %+v", res.Metadata)
	}
	if res.Duration <= 0 {
		t.Errorf("Duration must be positive, got %s", res.Duration)
	}
	if !strings.HasSuffix(res.OutputPath, ".srt") {
		t.Errorf("OutputPath = %q, want .srt suffix", res.OutputPath)
	}
	if len(fake.AudioCalls) != 1 {
		t.Fatalf("expected 1 DownloadAudio call, got %d", len(fake.AudioCalls))
	}
	if len(transcoder.Calls) != 1 {
		t.Fatalf("expected 1 transcode call, got %d", len(transcoder.Calls))
	}
	if len(recognizer.Calls) != 1 {
		t.Fatalf("expected 1 recognizer call, got %d", len(recognizer.Calls))
	}
	body, err := os.ReadFile(res.OutputPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.Contains(string(body), "ASR识别出的文本") {
		t.Errorf("output missing FakeRecognizer default payload text, got:\n%s", string(body))
	}
	// P2-5 from T-3.3 review: assert tmpDir was swept on the success
	// path (KeepIntermediate defaults to false). Complements
	// TestRun_ASRBranch_TmpDirCleanedOnRecognizerError's error-path
	// coverage so any regression in the defer cleanup lands on both
	// branches, not just failure.
	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatalf("read tmpRoot: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bilibili-txt-") {
			t.Errorf("tmpDir %q not cleaned up on ASR happy path", e.Name())
		}
	}
}

// TestRun_ASRBranch_IntermediateNaming verifies the intermediate file
// contract that plan §4.4 sets: wav lands at <tmpDir>/<bvid>.wav, the
// recognizer receives outPrefix=<tmpDir>/<bvid> (no `.srt` suffix per
// asr contract), and the resulting srt is <tmpDir>/<bvid>.srt.
//
// The transcoder writes wav bytes so we can also sanity-check that the
// recognizer sees the wav path the transcoder wrote to (rather than the
// downloaded audio path).
func TestRun_ASRBranch_IntermediateNaming(t *testing.T) {
	tmpRoot := redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	const bvid = "BVasrNAME"
	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn(bvid, "T", nil, nil),
	}
	transcoder := &audio.FakeTranscoder{Payload: []byte("RIFFfake-wav")}
	recognizer := &asr.FakeRecognizer{}
	deps := Deps{Downloader: fake, Transcoder: transcoder, Recognizer: recognizer}

	if _, err := Run(context.Background(), Input{URL: "u", KeepIntermediate: true}, cfg, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Locate the tmp dir the pipeline created (kept because
	// KeepIntermediate=true).
	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatalf("read tmpRoot: %v", err)
	}
	var tmpDir string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bilibili-txt-"+bvid+"-") {
			tmpDir = filepath.Join(tmpRoot, e.Name())
			break
		}
	}
	if tmpDir == "" {
		t.Fatalf("could not find pipeline tmp dir under %s", tmpRoot)
	}

	wantWav := filepath.Join(tmpDir, bvid+".wav")
	wantPrefix := filepath.Join(tmpDir, bvid)
	if len(transcoder.Calls) != 1 {
		t.Fatalf("expected 1 transcode call, got %d", len(transcoder.Calls))
	}
	if transcoder.Calls[0].Out != wantWav {
		t.Errorf("transcode Out = %q, want %q", transcoder.Calls[0].Out, wantWav)
	}
	if len(recognizer.Calls) != 1 {
		t.Fatalf("expected 1 recognizer call, got %d", len(recognizer.Calls))
	}
	if recognizer.Calls[0].Prefix != wantPrefix {
		t.Errorf("recognizer Prefix = %q, want %q", recognizer.Calls[0].Prefix, wantPrefix)
	}
	if recognizer.Calls[0].Wav != wantWav {
		t.Errorf("recognizer Wav = %q, want %q (must match transcode output)", recognizer.Calls[0].Wav, wantWav)
	}
	if strings.HasSuffix(strings.ToLower(recognizer.Calls[0].Prefix), ".srt") {
		t.Errorf("recognizer Prefix %q must NOT end with .srt (asr contract)", recognizer.Calls[0].Prefix)
	}
	// The wav bytes the transcoder wrote should actually be on disk
	// where the recognizer expected them.
	if _, err := os.Stat(wantWav); err != nil {
		t.Errorf("wav not written at %q: %v", wantWav, err)
	}
}

// TestRun_ASRBranch_PassesConfigModelToRecognizer confirms cfg.Model
// (the whisper.cpp ggml model path) is threaded into
// Recognizer.Transcribe verbatim. This is the sole wiring point for
// the model — a preflight failure would surface as a whisper-cli
// non-zero exit, so tests below the pipeline layer cannot check it.
func TestRun_ASRBranch_PassesConfigModelToRecognizer(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/opt/whisper/ggml-large-v3-turbo.bin"

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVmodel", "T", nil, nil),
	}
	recognizer := &asr.FakeRecognizer{}
	deps := Deps{Downloader: fake, Transcoder: &audio.FakeTranscoder{}, Recognizer: recognizer}

	if _, err := Run(context.Background(), Input{URL: "u"}, cfg, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(recognizer.Calls) != 1 {
		t.Fatalf("expected 1 recognizer call, got %d", len(recognizer.Calls))
	}
	if got := recognizer.Calls[0].Model; got != cfg.Model {
		t.Errorf("Recognizer Model = %q, want %q", got, cfg.Model)
	}
}

// TestRun_ASRBranch_DownloadAudioError propagates the downloader's
// error under the "pipeline: download audio: %w" envelope and MUST
// NOT invoke the transcoder or recognizer.
func TestRun_ASRBranch_DownloadAudioError(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	boom := errors.New("yt-dlp exited 1")
	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVdaErr", "T", nil, nil),
		DownloadAudioFn: func(_ context.Context, _, _ string) (string, error) {
			return "", boom
		},
	}
	transcoder := &audio.FakeTranscoder{}
	recognizer := &asr.FakeRecognizer{}
	deps := Deps{Downloader: fake, Transcoder: transcoder, Recognizer: recognizer}

	res, err := Run(context.Background(), Input{URL: "u"}, cfg, deps)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if res != nil {
		t.Errorf("Result must be nil on error, got %+v", res)
	}
	if !errors.Is(err, boom) {
		t.Errorf("errors.Is(err, boom) = false, got %v", err)
	}
	if !strings.Contains(err.Error(), "download audio") {
		t.Errorf("expected 'download audio' in error, got %v", err)
	}
	if len(transcoder.Calls) != 0 {
		t.Errorf("transcoder must not run after download error, got %d calls", len(transcoder.Calls))
	}
	if len(recognizer.Calls) != 0 {
		t.Errorf("recognizer must not run after download error, got %d calls", len(recognizer.Calls))
	}
}

// TestRun_ASRBranch_TranscodeError surfaces the audio.Transcoder error
// wrapped in "pipeline: transcode: %w" and short-circuits before the
// recognizer runs.
func TestRun_ASRBranch_TranscodeError(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	boom := errors.New("ffmpeg exited 1")
	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVtxErr", "T", nil, nil),
	}
	transcoder := &audio.FakeTranscoder{Err: boom}
	recognizer := &asr.FakeRecognizer{}
	deps := Deps{Downloader: fake, Transcoder: transcoder, Recognizer: recognizer}

	res, err := Run(context.Background(), Input{URL: "u"}, cfg, deps)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if res != nil {
		t.Errorf("Result must be nil on error, got %+v", res)
	}
	if !errors.Is(err, boom) {
		t.Errorf("errors.Is(err, boom) = false, got %v", err)
	}
	if !strings.Contains(err.Error(), "transcode") {
		t.Errorf("expected 'transcode' in error, got %v", err)
	}
	if len(fake.AudioCalls) != 1 {
		t.Errorf("DownloadAudio must run before transcode error, got %d calls", len(fake.AudioCalls))
	}
	if len(recognizer.Calls) != 0 {
		t.Errorf("recognizer must not run after transcode error, got %d calls", len(recognizer.Calls))
	}
}

// TestRun_ASRBranch_RecognizerError surfaces the asr.Recognizer error
// wrapped in "pipeline: transcribe: %w" and does NOT write any
// output file.
func TestRun_ASRBranch_RecognizerError(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	boom := errors.New("whisper exited 1")
	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVrcErr", "T", nil, nil),
	}
	transcoder := &audio.FakeTranscoder{}
	recognizer := &asr.FakeRecognizer{Err: boom}
	deps := Deps{
		Downloader: fake,
		Transcoder: transcoder,
		Recognizer: recognizer,
	}

	res, err := Run(context.Background(), Input{URL: "u"}, cfg, deps)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("errors.Is(err, boom) = false, got %v", err)
	}
	if !strings.Contains(err.Error(), "transcribe") {
		t.Errorf("expected 'transcribe' in error, got %v", err)
	}
	if res != nil {
		t.Errorf("Result must be nil on error, got %+v", res)
	}
	if len(fake.AudioCalls) != 1 {
		t.Errorf("DownloadAudio must run before recognizer error, got %d calls", len(fake.AudioCalls))
	}
	if len(transcoder.Calls) != 1 {
		t.Errorf("transcode must run before recognizer error, got %d calls", len(transcoder.Calls))
	}
	// The pre-computed output path must NOT exist since the pipeline
	// failed before formatter.Convert ran.
	wantOut := naming.BuildPath(outDir, "T", "BVrcErr", 1, 1, "txt")
	if _, err := os.Stat(wantOut); err == nil {
		t.Errorf("output file %q must not exist after recognizer error", wantOut)
	}
}

// TestRun_ASRBranch_ParseErrEmpty — an empty srt from the recognizer
// (edge case: whisper-cli produced no cues) is forwarded verbatim as
// subtitle.ErrEmpty through the "pipeline: parse asr: %w" envelope.
func TestRun_ASRBranch_ParseErrEmpty(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	// Point FixturePath at an empty file so the recognizer writes
	// zero-byte srt. Using Payload=[]byte("") wouldn't work: fake
	// treats len(Payload)==0 as "not set" and falls back to default.
	emptyFile := filepath.Join(projectTempDir(t), "empty.srt")
	if err := os.WriteFile(emptyFile, nil, 0o644); err != nil {
		t.Fatalf("seed empty fixture: %v", err)
	}

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVempty", "T", nil, nil),
	}
	deps := Deps{
		Downloader: fake,
		Transcoder: &audio.FakeTranscoder{},
		Recognizer: &asr.FakeRecognizer{FixturePath: emptyFile},
	}
	res, err := Run(context.Background(), Input{URL: "u"}, cfg, deps)
	if !errors.Is(err, subtitle.ErrEmpty) {
		t.Fatalf("expected subtitle.ErrEmpty, got %v", err)
	}
	if res != nil {
		t.Errorf("Result must be nil on error, got %+v", res)
	}
	if !strings.Contains(err.Error(), "parse asr") {
		t.Errorf("expected 'parse asr' in error, got %v", err)
	}
}

// TestRun_ASRBranch_KeepIntermediate_KeepsAllArtefacts asserts that
// KeepIntermediate=true preserves the *ASR* intermediate chain (m4a,
// wav, srt) under $TMPDIR. Complements the subtitle-side coverage
// in TestRun_KeepIntermediateLeavesTmpDir.
//
// The fake downloader's default DownloadAudio stub writes
// `<tmpDir>/<fakeBVID>.m4a` (fake.go's `fakeBVID = "BV1fake000000"`),
// which is intentionally *not* tied to whatever BVID the caller-
// supplied FetchMetadataFn returned. We override DownloadAudioFn so
// the m4a lands at `<tmpDir>/<meta.BVID>.m4a`, matching the naming
// convention the transcode / recognize steps use downstream — that
// way the assertion below can look for a single, predictable BVID
// prefix across all three intermediates.
func TestRun_ASRBranch_KeepIntermediate_KeepsAllArtefacts(t *testing.T) {
	tmpRoot := redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	const bvid = "BVkeepASR"
	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn(bvid, "T", nil, nil),
		DownloadAudioFn: func(_ context.Context, _, dir string) (string, error) {
			p := filepath.Join(dir, bvid+".m4a")
			if err := os.WriteFile(p, []byte("fake-m4a"), 0o644); err != nil {
				return "", err
			}
			return p, nil
		},
	}
	deps := Deps{
		Downloader: fake,
		Transcoder: &audio.FakeTranscoder{},
		Recognizer: &asr.FakeRecognizer{},
	}
	if _, err := Run(context.Background(), Input{URL: "u", KeepIntermediate: true}, cfg, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var tmpDir string
	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatalf("read tmpRoot: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bilibili-txt-"+bvid+"-") {
			tmpDir = filepath.Join(tmpRoot, e.Name())
			break
		}
	}
	if tmpDir == "" {
		t.Fatalf("KeepIntermediate=true should leave tmp dir behind, none found in %s", tmpRoot)
	}
	// All three intermediates must survive.
	for _, name := range []string{bvid + ".m4a", bvid + ".wav", bvid + ".srt"} {
		p := filepath.Join(tmpDir, name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("intermediate %q missing under KeepIntermediate=true: %v", p, err)
		}
	}
}

// TestRun_ASRBranch_TmpDirCleanedOnRecognizerError — even on the error
// path the pipeline must sweep tmpDir when KeepIntermediate=false, so
// a failed run does not leak whisper-cli scratch files across users.
func TestRun_ASRBranch_TmpDirCleanedOnRecognizerError(t *testing.T) {
	tmpRoot := redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVclnErr", "T", nil, nil),
	}
	deps := Deps{
		Downloader: fake,
		Transcoder: &audio.FakeTranscoder{},
		Recognizer: &asr.FakeRecognizer{Err: errors.New("whisper failed")},
	}
	if _, err := Run(context.Background(), Input{URL: "u"}, cfg, deps); err == nil {
		t.Fatal("expected error, got nil")
	}

	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatalf("read tmpRoot: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bilibili-txt-") {
			t.Errorf("tmpDir %q not cleaned up on error path", e.Name())
		}
	}
}

// TestRun_ASRBranch_CtxCancelledMidFlow simulates a user hitting ^C
// while yt-dlp is mid-download: DownloadAudio observes the cancel,
// returns a ctx-wrapped error, and neither transcoder nor recognizer
// should be invoked afterwards.
func TestRun_ASRBranch_CtxCancelledMidFlow(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	ctx, cancel := context.WithCancel(context.Background())
	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVctx", "T", nil, nil),
		DownloadAudioFn: func(_ context.Context, _, _ string) (string, error) {
			cancel()
			return "", context.Canceled
		},
	}
	transcoder := &audio.FakeTranscoder{}
	recognizer := &asr.FakeRecognizer{}
	deps := Deps{Downloader: fake, Transcoder: transcoder, Recognizer: recognizer}

	res, err := Run(ctx, Input{URL: "u"}, cfg, deps)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if res != nil {
		t.Errorf("Result must be nil on error, got %+v", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, got %v", err)
	}
	if len(transcoder.Calls) != 0 {
		t.Errorf("transcoder must not run after ctx cancel, got %d calls", len(transcoder.Calls))
	}
	if len(recognizer.Calls) != 0 {
		t.Errorf("recognizer must not run after ctx cancel, got %d calls", len(recognizer.Calls))
	}
}

// stubRecognizer is a minimal asr.Recognizer implementation used by
// tests that need per-call control the FakeRecognizer's Payload /
// FixturePath knobs cannot offer (e.g. cancelling ctx from inside
// Transcribe to simulate a ^C between whisper exit and the pipeline
// resuming). Kept here rather than added to internal/asr/fake.go
// because this is the only test that needs it; promoting it there
// would widen the FakeRecognizer API surface for a single caller.
type stubRecognizer struct {
	fn func(ctx context.Context, wav, model, prefix string) (string, error)
}

func (s *stubRecognizer) Transcribe(ctx context.Context, wav, model, prefix string) (string, error) {
	return s.fn(ctx, wav, model, prefix)
}

// TestRun_ASRBranch_FormatError locks in the P2-1 fix from the T-3.3
// second-pass review: when formatter.Convert fails on the ASR path
// the pipeline must wrap the underlying error as
// "pipeline: convert: %w" and return a nil Result (mirroring the
// subtitle branch's already-covered
// TestRun_UnknownFormatRejected / ParseError paths).
//
// We coerce a deterministic Convert failure by pre-creating a
// **directory** at the pre-computed outPath before Run kicks off:
// the naming.ResolveConflict path resolves to Overwrite (matching
// baseCfg's default strategy), so runASRBranch reaches
// formatter.Convert, whose internal os.Create then trips
// EISDIR-style "is a directory". This exercises the ctx.Err() gate
// preceding format AND the error-wrapping layer without depending on
// a formatter-side flaw. Regressions in either the wrap or the
// Result-nil invariant land here.
func TestRun_ASRBranch_FormatError(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	const bvid = "BVfmtErr"
	// Compute the exact outPath the pipeline will target and create
	// a *directory* there so os.Create inside formatter.Convert
	// surfaces "is a directory". Keep the strategy at the default
	// overwrite so resolveConflictIfExists lets us through.
	expected := naming.BuildPath(outDir, "T", bvid, 1, 1, "txt")
	if err := os.MkdirAll(expected, 0o755); err != nil {
		t.Fatalf("seed outPath as dir: %v", err)
	}

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn(bvid, "T", nil, nil),
	}
	deps := Deps{
		Downloader: fake,
		Transcoder: &audio.FakeTranscoder{},
		Recognizer: &asr.FakeRecognizer{},
	}

	res, err := Run(context.Background(), Input{URL: "u"}, cfg, deps)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if res != nil {
		t.Errorf("Result must be nil on error, got %+v", res)
	}
	if !strings.Contains(err.Error(), "convert") {
		t.Errorf("expected 'convert' envelope in error, got %v", err)
	}
	// All three upstream steps must have completed before format
	// surfaced its failure — this guards against a regression that
	// short-circuits earlier and never reaches the format step.
	if len(fake.AudioCalls) != 1 {
		t.Errorf("expected 1 DownloadAudio call, got %d", len(fake.AudioCalls))
	}
}

// TestRun_ASRBranch_EmptyCfgModelFailsFast locks in the P2-2 fix from
// the T-3.3 second-pass review: an empty cfg.Model (bypassing preflight
// or setting `model: ""` in config.yaml) must fail immediately with a
// clear pipeline-level error rather than cascading into a whisper-cli
// argv failure three layers deep. The check runs *inside*
// runASRBranch, so DownloadAudio / Transcode / Recognize must NOT be
// invoked when it trips.
func TestRun_ASRBranch_EmptyCfgModelFailsFast(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	// Explicitly clear model — baseCfg leaves it empty, but stating
	// the invariant here documents the intent for future readers.
	cfg.Model = ""

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVnoModel", "T", nil, nil),
	}
	transcoder := &audio.FakeTranscoder{}
	recognizer := &asr.FakeRecognizer{}
	deps := Deps{Downloader: fake, Transcoder: transcoder, Recognizer: recognizer}

	res, err := Run(context.Background(), Input{URL: "u", ForceASR: true}, cfg, deps)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if res != nil {
		t.Errorf("Result must be nil on error, got %+v", res)
	}
	if !strings.Contains(err.Error(), "cfg.Model") {
		t.Errorf("expected 'cfg.Model' in error, got %v", err)
	}
	if len(fake.AudioCalls) != 0 {
		t.Errorf("DownloadAudio must not run when cfg.Model is empty, got %d calls", len(fake.AudioCalls))
	}
	if len(transcoder.Calls) != 0 {
		t.Errorf("transcoder must not run when cfg.Model is empty, got %d calls", len(transcoder.Calls))
	}
	if len(recognizer.Calls) != 0 {
		t.Errorf("recognizer must not run when cfg.Model is empty, got %d calls", len(recognizer.Calls))
	}
}

// TestRun_SubtitleBranch_CtxCancelledAfterDownload locks in the
// symmetric pre-parse ctx.Err() gate in runSubtitleBranch (mirrors the
// ASR branch's TestRun_ASRBranch_CtxCancelledAfterTranscribe). When
// the user hits ^C between DownloadSubtitle exit and subtitle.Parse,
// the pipeline MUST short-circuit with context.Canceled wrapped as
// "pipeline: parse subtitle: %w" rather than silently running parse +
// format on the already-downloaded srt.
//
// We coerce the race by cancelling ctx *inside* DownloadSubtitleFn
// after writing a syntactically valid srt: the subtitle download
// completes successfully, then the pre-parse gate sees ctx done and
// returns before touching the parser or formatter. Regressions that
// drop or reorder the gate land here.
func TestRun_SubtitleBranch_CtxCancelledAfterDownload(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)

	const bvid = "BVctxSub"
	ctx, cancel := context.WithCancel(context.Background())
	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn(bvid, "T", []downloader.SubtitleTrack{
			{Lang: "zh-CN", Ext: "srt"},
		}, nil),
		DownloadSubtitleFn: func(_ context.Context, _, outDir string, langPref []string) (string, error) {
			dst := filepath.Join(outDir, bvid+"."+langPref[0]+".srt")
			body := "1\n00:00:00,000 --> 00:00:02,000\nhello\n"
			if err := os.WriteFile(dst, []byte(body), 0o644); err != nil {
				return "", err
			}
			cancel()
			return dst, nil
		},
	}
	res, err := Run(ctx, Input{URL: "u"}, cfg, Deps{Downloader: fake})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if res != nil {
		t.Errorf("Result must be nil on error, got %+v", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, got %v", err)
	}
	if !strings.Contains(err.Error(), "parse subtitle") {
		t.Errorf("expected 'parse subtitle' envelope, got %v", err)
	}
	// DownloadSubtitle must have run once (it's the trigger that
	// cancelled ctx); formatter must NOT have written the output.
	if len(fake.SubtitleCalls) != 1 {
		t.Errorf("expected 1 DownloadSubtitle call, got %d", len(fake.SubtitleCalls))
	}
	wantOut := naming.BuildPath(outDir, "T", bvid, 1, 1, "txt")
	if _, err := os.Stat(wantOut); err == nil {
		t.Errorf("output %q must not exist after ctx cancel before parse", wantOut)
	}
}

// TestRun_ConflictAskNoInteractive_WrapsErrConflictUnresolved locks in
// the P2 fix that guarantees "ask + --no-interactive + existing file"
// surfaces as pipeline.ErrConflictUnresolved so the CLI layer maps to
// exit code 3 (contracts.md §四.3) via errors.Is instead of parsing
// the underlying naming.ResolveConflict message text.
//
// The strategy is set to "ask", the file already exists, and IsTTY is
// false to force the degrade path inside naming.ResolveConflict; the
// resulting error must still carry ErrConflictUnresolved in its chain
// AND the offending path so users don't have to correlate log lines.
func TestRun_ConflictAskNoInteractive_WrapsErrConflictUnresolved(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Naming.OnConflict = naming.StrategyAsk

	meta := &downloader.Metadata{
		BVID: "BVaskNI", Title: "T", TotalPages: 1,
		Subtitles: []downloader.SubtitleTrack{{Lang: "zh-CN", Ext: "srt"}},
	}
	expected := naming.BuildPath(outDir, meta.Title, meta.BVID, 1, 1, "txt")
	if err := os.WriteFile(expected, []byte("EXISTING"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: func(_ context.Context, _ string) (*downloader.Metadata, error) {
			return meta, nil
		},
	}
	_, err := Run(context.Background(), Input{URL: "u", NoInteractive: true}, cfg, Deps{Downloader: fake})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrConflictUnresolved) {
		t.Errorf("errors.Is(err, ErrConflictUnresolved) = false, got %v", err)
	}
	if !strings.Contains(err.Error(), expected) {
		t.Errorf("error must carry offending path %q, got %v", expected, err)
	}
	if len(fake.SubtitleCalls) != 0 {
		t.Errorf("ask+no-interactive conflict must not trigger subtitle download, got %d calls", len(fake.SubtitleCalls))
	}
	// Pre-existing content untouched.
	b, _ := os.ReadFile(expected)
	if string(b) != "EXISTING" {
		t.Errorf("existing file content mutated: %q", string(b))
	}
}

// TestRun_ConflictAbort_WrapsErrConflictUnresolved locks in the same
// ErrConflictUnresolved wrap for the explicit "abort" strategy: an
// on_conflict=abort with a pre-existing file MUST surface as
// pipeline.ErrConflictUnresolved so the CLI's errors.Is check maps to
// exit 3, and the pipeline MUST NOT touch the downloader.
//
// Complements TestRun_ConflictAbortReturnsError (which only asserted
// "some error"), pinning the sentinel wrap the CLI exit-code mapper
// depends on.
func TestRun_ConflictAbort_WrapsErrConflictUnresolved(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Naming.OnConflict = naming.StrategyAbort

	meta := &downloader.Metadata{
		BVID: "BVabortSentinel", Title: "T", TotalPages: 1,
		Subtitles: []downloader.SubtitleTrack{{Lang: "zh-CN", Ext: "srt"}},
	}
	expected := naming.BuildPath(outDir, meta.Title, meta.BVID, 1, 1, "txt")
	if err := os.WriteFile(expected, []byte("EXISTING"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: func(_ context.Context, _ string) (*downloader.Metadata, error) {
			return meta, nil
		},
	}
	_, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrConflictUnresolved) {
		t.Errorf("errors.Is(err, ErrConflictUnresolved) = false, got %v", err)
	}
	if !strings.Contains(err.Error(), expected) {
		t.Errorf("error must carry offending path %q, got %v", expected, err)
	}
	if len(fake.SubtitleCalls) != 0 {
		t.Errorf("abort conflict must not trigger subtitle download, got %d calls", len(fake.SubtitleCalls))
	}
}

// TestRun_ASRBranch_CtxCancelledAfterTranscribe locks in the P2-CO-1
// fix: when the user hits ^C between whisper-cli exit and
// subtitle.Parse, the pipeline must short-circuit with
// context.Canceled wrapped as "pipeline: parse asr: %w" rather than
// silently running parse + format on the already-produced srt.
//
// subtitle.Parse itself does not accept a ctx (contracts.md §一.2),
// so runASRBranch bracket-checks ctx.Err() before parse / format;
// this test asserts that guard fires by cancelling ctx *inside* the
// recognizer stub, which yields a valid srt but sees ctx done on
// return.
func TestRun_ASRBranch_CtxCancelledAfterTranscribe(t *testing.T) {
	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Model = "/tmp/fake-model.bin"

	ctx, cancel := context.WithCancel(context.Background())
	fake := &downloader.FakeDownloader{
		FetchMetadataFn: stubMetadataFn("BVctxParse", "T", nil, nil),
	}
	recognizer := &stubRecognizer{
		fn: func(_ context.Context, _ string, _ string, prefix string) (string, error) {
			// Produce a syntactically valid srt so the parse
			// step would succeed if we let it run.
			srtPath := prefix + ".srt"
			body := "1\n00:00:00,000 --> 00:00:02,000\nhello\n"
			if err := os.WriteFile(srtPath, []byte(body), 0o644); err != nil {
				return "", err
			}
			cancel()
			return srtPath, nil
		},
	}
	deps := Deps{
		Downloader: fake,
		Transcoder: &audio.FakeTranscoder{},
		Recognizer: recognizer,
	}

	res, err := Run(ctx, Input{URL: "u"}, cfg, deps)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if res != nil {
		t.Errorf("Result must be nil on error, got %+v", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, got %v", err)
	}
	if !strings.Contains(err.Error(), "parse asr") {
		t.Errorf("expected 'parse asr' envelope, got %v", err)
	}
	// Recognizer succeeded; formatter must NOT have run: the
	// pre-computed output path must remain absent on disk.
	wantOut := naming.BuildPath(outDir, "T", "BVctxParse", 1, 1, "txt")
	if _, err := os.Stat(wantOut); err == nil {
		t.Errorf("output %q must not exist after ctx cancel before format", wantOut)
	}
}

// captureGlobalLogger swaps [logger.Default] over to a caller-owned
// buffer for the duration of a test. Returns the buffer plus a
// restore closure the caller MUST defer.
//
// **MUST NOT** be used from tests that call t.Parallel(): the helper
// mutates a process-global slog binding and interleaving between
// captures would scramble both buffers. All conflict WARN tests in
// this file run serially by design.
//
// The pipeline emits its `conflict detected` WARN via
// [logger.Default().Slog().Warn], and [logger.Init] is the only
// public seam that rebinds the global. Redirecting the global's
// Stderr sink to a bytes.Buffer therefore is the least-invasive way
// to assert on the log line without touching production code.
//
// Restore uses a second Init(Options{}) call, which replaces the
// capture logger with a fresh stderr-backed default. This is safe as
// long as no `TestMain` up the call chain pre-configures debug/file
// sinks or JSON handlers — the pipeline package currently has no
// such TestMain, and adding one would need to update this helper to
// snapshot-and-restore the exact prior Logger instance.
func captureGlobalLogger(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	if err := logger.Init(logger.Options{Stderr: buf}); err != nil {
		t.Fatalf("logger.Init capture: %v", err)
	}
	return buf, func() {
		if err := logger.Init(logger.Options{}); err != nil {
			t.Logf("logger.Init restore: %v", err)
		}
	}
}

// TestRun_ConflictOverwrite_EmitsWarnLog locks in plan §4.3:345:
// when the pipeline picks the overwrite branch on a pre-existing
// target, it MUST emit a `WARN conflict detected` log line with the
// existing path and `action=overwrite`. This is the only observable
// signal operators get for a silent overwrite; regressing it would
// hide destructive behaviour behind a happy-path exit=0.
func TestRun_ConflictOverwrite_EmitsWarnLog(t *testing.T) {
	buf, restore := captureGlobalLogger(t)
	defer restore()

	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Naming.OnConflict = naming.StrategyOverwrite

	meta := &downloader.Metadata{
		BVID: "BVwarnOverwrite", Title: "T", TotalPages: 1,
		Subtitles: []downloader.SubtitleTrack{{Lang: "zh-CN", Ext: "srt"}},
	}
	expected := naming.BuildPath(outDir, meta.Title, meta.BVID, 1, 1, "txt")
	if err := os.WriteFile(expected, []byte("PRE-EXISTING"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: func(_ context.Context, _ string) (*downloader.Metadata, error) {
			return meta, nil
		},
	}
	if _, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logs := buf.String()
	if !strings.Contains(logs, "level=WARN") {
		t.Errorf("expected WARN log line, got:\n%s", logs)
	}
	if !strings.Contains(logs, `msg="conflict detected"`) {
		t.Errorf("expected msg=\"conflict detected\", got:\n%s", logs)
	}
	if !strings.Contains(logs, "action=overwrite") {
		t.Errorf("expected action=overwrite, got:\n%s", logs)
	}
	if !strings.Contains(logs, "existing="+expected) {
		// slog may quote paths depending on chars; substring on the
		// path itself is a resilient fallback the assertion above
		// still upgrades to a full key=value when unquoted.
		if !strings.Contains(logs, expected) {
			t.Errorf("expected existing=%s in log, got:\n%s", expected, logs)
		}
	}
}

// TestRun_ConflictSkip_EmitsWarnLog is the twin of the overwrite
// case for on_conflict=skip. The `skip: output already exists` Info
// log stays for backwards compat, but the plan §4.3:345 WARN line
// MUST also fire so log aggregators keyed on `conflict detected`
// pick both destructive strategies up uniformly.
func TestRun_ConflictSkip_EmitsWarnLog(t *testing.T) {
	buf, restore := captureGlobalLogger(t)
	defer restore()

	redirectTmpDir(t)
	outDir := projectTempDir(t)
	cfg := baseCfg(t, outDir)
	cfg.Naming.OnConflict = naming.StrategySkip

	meta := &downloader.Metadata{
		BVID: "BVwarnSkip", Title: "T", TotalPages: 1,
		Subtitles: []downloader.SubtitleTrack{{Lang: "zh-CN", Ext: "srt"}},
	}
	expected := naming.BuildPath(outDir, meta.Title, meta.BVID, 1, 1, "txt")
	if err := os.WriteFile(expected, []byte("KEEP-ME"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	fake := &downloader.FakeDownloader{
		FetchMetadataFn: func(_ context.Context, _ string) (*downloader.Metadata, error) {
			return meta, nil
		},
	}
	res, err := Run(context.Background(), Input{URL: "u"}, cfg, Deps{Downloader: fake})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Source != SourceCached {
		t.Errorf("Source = %q, want %q", res.Source, SourceCached)
	}

	logs := buf.String()
	if !strings.Contains(logs, "level=WARN") {
		t.Errorf("expected WARN log line, got:\n%s", logs)
	}
	if !strings.Contains(logs, `msg="conflict detected"`) {
		t.Errorf("expected msg=\"conflict detected\", got:\n%s", logs)
	}
	if !strings.Contains(logs, "action=skip") {
		t.Errorf("expected action=skip, got:\n%s", logs)
	}
	if !strings.Contains(logs, expected) {
		t.Errorf("expected existing path %s in log, got:\n%s", expected, logs)
	}
}
