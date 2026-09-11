package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// FetchMetadata
// ---------------------------------------------------------------------------

func TestFakeDownloader_FetchMetadata_DefaultStub_ReturnsCanonicalMetadata(t *testing.T) {
	f := &FakeDownloader{}

	meta, err := f.FetchMetadata(context.Background(), "https://www.bilibili.com/video/BV1fake000000")
	if err != nil {
		t.Fatalf("FetchMetadata: unexpected err %v", err)
	}
	if meta == nil {
		t.Fatal("FetchMetadata: got nil Metadata, want non-nil default stub")
	}
	if meta.BVID != "BV1fake000000" {
		t.Fatalf("FetchMetadata: BVID = %q, want BV1fake000000", meta.BVID)
	}
	if meta.TotalPages != 1 {
		t.Fatalf("FetchMetadata: TotalPages = %d, want 1", meta.TotalPages)
	}
	if meta.Title == "" {
		t.Fatalf("FetchMetadata: Title is empty; default stub should carry a title")
	}
}

func TestFakeDownloader_FetchMetadata_InjectedFn_TakesPrecedence(t *testing.T) {
	want := &Metadata{
		BVID:       "BV1custom001",
		Title:      "custom title",
		TotalPages: 2,
		Subtitles: []SubtitleTrack{
			{Lang: "zh-CN", Ext: "srt", URL: "https://example.invalid/sub.srt"},
		},
	}
	f := &FakeDownloader{
		FetchMetadataFn: func(ctx context.Context, url string) (*Metadata, error) {
			if url != "https://example.invalid/foo" {
				t.Fatalf("FetchMetadataFn received url = %q", url)
			}
			return want, nil
		},
	}
	got, err := f.FetchMetadata(context.Background(), "https://example.invalid/foo")
	if err != nil {
		t.Fatalf("FetchMetadata: unexpected err %v", err)
	}
	if got != want {
		t.Fatalf("FetchMetadata: got %+v, want same pointer as injected", got)
	}
}

func TestFakeDownloader_FetchMetadata_InjectedError_Propagates(t *testing.T) {
	f := &FakeDownloader{
		FetchMetadataFn: func(ctx context.Context, url string) (*Metadata, error) {
			return nil, ErrVideoNotFound
		},
	}
	_, err := f.FetchMetadata(context.Background(), "https://x")
	if !errors.Is(err, ErrVideoNotFound) {
		t.Fatalf("FetchMetadata: err = %v, want errors.Is ErrVideoNotFound", err)
	}
}

func TestFakeDownloader_FetchMetadata_CtxCancelled_ReturnsCtxErr(t *testing.T) {
	invoked := false
	f := &FakeDownloader{
		FetchMetadataFn: func(ctx context.Context, url string) (*Metadata, error) {
			invoked = true
			return &Metadata{}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.FetchMetadata(ctx, "https://x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchMetadata: err = %v, want errors.Is context.Canceled", err)
	}
	if invoked {
		t.Fatal("FetchMetadataFn must not be invoked once ctx is cancelled")
	}
}

func TestFakeDownloader_FetchMetadata_EmptyURL_Rejected(t *testing.T) {
	f := &FakeDownloader{}
	if _, err := f.FetchMetadata(context.Background(), ""); err == nil {
		t.Fatal("FetchMetadata(empty url) must fail")
	}
	if _, err := f.FetchMetadata(context.Background(), "   \t"); err == nil {
		t.Fatal("FetchMetadata(whitespace url) must fail")
	}
}

func TestFakeDownloader_FetchMetadata_RecordsCall(t *testing.T) {
	f := &FakeDownloader{}
	_, _ = f.FetchMetadata(context.Background(), "https://a")
	_, _ = f.FetchMetadata(context.Background(), "https://b")
	if len(f.MetadataCalls) != 2 {
		t.Fatalf("MetadataCalls len = %d, want 2", len(f.MetadataCalls))
	}
	if f.MetadataCalls[0] != "https://a" || f.MetadataCalls[1] != "https://b" {
		t.Fatalf("MetadataCalls = %+v", f.MetadataCalls)
	}
}

// ---------------------------------------------------------------------------
// DownloadSubtitle
// ---------------------------------------------------------------------------

func TestFakeDownloader_DownloadSubtitle_DefaultStub_WritesSrtNamedByBvidLang(t *testing.T) {
	f := &FakeDownloader{}
	outDir := t.TempDir()
	langs := []string{"zh-CN", "en"}

	srtPath, err := f.DownloadSubtitle(context.Background(),
		"https://www.bilibili.com/video/BV1fake000000", outDir, langs)
	if err != nil {
		t.Fatalf("DownloadSubtitle: unexpected err %v", err)
	}
	if !filepath.IsAbs(srtPath) {
		t.Fatalf("srtPath = %q, want absolute path (contracts.md §二.1)", srtPath)
	}
	// contracts.md §二.1: outDir/<bvid>.<lang>.srt
	wantName := "BV1fake000000.zh-CN.srt"
	if got := filepath.Base(srtPath); got != wantName {
		t.Fatalf("srtPath basename = %q, want %q", got, wantName)
	}
	if got := filepath.Dir(srtPath); got != outDir {
		t.Fatalf("srtPath dir = %q, want %q", got, outDir)
	}
	// Content: must be non-empty (the default stub copies testdata/sample.srt).
	data, err := os.ReadFile(srtPath)
	if err != nil {
		t.Fatalf("read srt: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("srt is empty; default stub should have copied testdata/sample.srt")
	}
	if !strings.Contains(string(data), "-->") {
		t.Fatalf("srt content missing SRT timing arrow, got:\n%s", data)
	}
}

func TestFakeDownloader_DownloadSubtitle_UsesFirstLang(t *testing.T) {
	f := &FakeDownloader{}
	outDir := t.TempDir()
	srtPath, err := f.DownloadSubtitle(context.Background(),
		"https://x/BV1zzz", outDir, []string{"zh-Hans", "zh-CN", "en"})
	if err != nil {
		t.Fatalf("DownloadSubtitle: %v", err)
	}
	if got := filepath.Base(srtPath); got != "BV1fake000000.zh-Hans.srt" {
		t.Fatalf("basename = %q, want BV1fake000000.zh-Hans.srt", got)
	}
}

func TestFakeDownloader_DownloadSubtitle_NoSubtitleErr_Injectable(t *testing.T) {
	f := &FakeDownloader{
		DownloadSubtitleFn: func(ctx context.Context, url, outDir string, langs []string) (string, error) {
			return "", ErrNoSubtitle
		},
	}
	_, err := f.DownloadSubtitle(context.Background(), "https://x", t.TempDir(), []string{"zh-CN"})
	if !errors.Is(err, ErrNoSubtitle) {
		t.Fatalf("err = %v, want errors.Is ErrNoSubtitle", err)
	}
}

func TestFakeDownloader_DownloadSubtitle_InjectedFn_TakesPrecedence(t *testing.T) {
	sentinel := errors.New("boom")
	f := &FakeDownloader{
		DownloadSubtitleFn: func(ctx context.Context, url, outDir string, langs []string) (string, error) {
			return "/custom/path.srt", sentinel
		},
	}
	dir := t.TempDir()
	got, err := f.DownloadSubtitle(context.Background(), "u", dir, []string{"zh"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want errors.Is sentinel", err)
	}
	if got != "/custom/path.srt" {
		t.Fatalf("path = %q, want /custom/path.srt", got)
	}
	// Contract (fake.go MetadataCalls doc): a rejected-fn call still
	// enters the SubtitleCalls log so tests can prove the fake was
	// exercised even when the injected fn errored out.
	if len(f.SubtitleCalls) != 1 {
		t.Fatalf("SubtitleCalls len = %d, want 1 (call must be recorded even when injected fn errors)", len(f.SubtitleCalls))
	}
}

func TestFakeDownloader_DownloadSubtitle_CtxCancelled_ReturnsCtxErr(t *testing.T) {
	invoked := false
	f := &FakeDownloader{
		DownloadSubtitleFn: func(ctx context.Context, url, outDir string, langs []string) (string, error) {
			invoked = true
			return "", nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.DownloadSubtitle(ctx, "u", t.TempDir(), []string{"zh"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if invoked {
		t.Fatal("injected fn ran despite cancelled ctx")
	}
}

func TestFakeDownloader_DownloadSubtitle_MissingOutDir_Rejected(t *testing.T) {
	f := &FakeDownloader{}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := f.DownloadSubtitle(context.Background(), "u", missing, []string{"zh"}); err == nil {
		t.Fatal("expected error when outDir does not exist (contract: caller creates outDir)")
	}
}

func TestFakeDownloader_DownloadSubtitle_EmptyLangPref_Rejected(t *testing.T) {
	f := &FakeDownloader{}
	if _, err := f.DownloadSubtitle(context.Background(), "u", t.TempDir(), nil); err == nil {
		t.Fatal("expected error when langPref is nil")
	}
	if _, err := f.DownloadSubtitle(context.Background(), "u", t.TempDir(), []string{}); err == nil {
		t.Fatal("expected error when langPref is empty")
	}
}

func TestFakeDownloader_DownloadSubtitle_EmptyURL_Rejected(t *testing.T) {
	f := &FakeDownloader{}
	if _, err := f.DownloadSubtitle(context.Background(), "", t.TempDir(), []string{"zh"}); err == nil {
		t.Fatal("expected error when url is empty")
	}
}

func TestFakeDownloader_DownloadSubtitle_RecordsCall(t *testing.T) {
	f := &FakeDownloader{}
	dir := t.TempDir()
	_, _ = f.DownloadSubtitle(context.Background(), "u1", dir, []string{"zh-CN"})
	_, _ = f.DownloadSubtitle(context.Background(), "u2", dir, []string{"en"})
	if len(f.SubtitleCalls) != 2 {
		t.Fatalf("SubtitleCalls len = %d, want 2", len(f.SubtitleCalls))
	}
	if f.SubtitleCalls[0].URL != "u1" || f.SubtitleCalls[0].OutDir != dir ||
		len(f.SubtitleCalls[0].LangPref) != 1 || f.SubtitleCalls[0].LangPref[0] != "zh-CN" {
		t.Fatalf("SubtitleCalls[0] = %+v", f.SubtitleCalls[0])
	}
}

// ---------------------------------------------------------------------------
// DownloadAudio
// ---------------------------------------------------------------------------

func TestFakeDownloader_DownloadAudio_DefaultStub_WritesM4aNamedByBvid(t *testing.T) {
	f := &FakeDownloader{}
	outDir := t.TempDir()
	audioPath, err := f.DownloadAudio(context.Background(),
		"https://www.bilibili.com/video/BV1fake000000", outDir)
	if err != nil {
		t.Fatalf("DownloadAudio: unexpected err %v", err)
	}
	if !filepath.IsAbs(audioPath) {
		t.Fatalf("audioPath = %q, want absolute", audioPath)
	}
	// contracts.md §二.1: <bvid>.m4a
	if got := filepath.Base(audioPath); got != "BV1fake000000.m4a" {
		t.Fatalf("basename = %q, want BV1fake000000.m4a", got)
	}
	if got := filepath.Dir(audioPath); got != outDir {
		t.Fatalf("dir = %q, want %q", got, outDir)
	}
	info, err := os.Stat(audioPath)
	if err != nil {
		t.Fatalf("stat audio: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("audio is empty; default stub should have copied testdata/sample.m4a")
	}
}

func TestFakeDownloader_DownloadAudio_InjectedFn_TakesPrecedence(t *testing.T) {
	f := &FakeDownloader{
		DownloadAudioFn: func(ctx context.Context, url, outDir string) (string, error) {
			return "/custom/audio.m4a", nil
		},
	}
	dir := t.TempDir()
	got, err := f.DownloadAudio(context.Background(), "u", dir)
	if err != nil {
		t.Fatalf("DownloadAudio: %v", err)
	}
	if got != "/custom/audio.m4a" {
		t.Fatalf("path = %q, want /custom/audio.m4a", got)
	}
}

func TestFakeDownloader_DownloadAudio_InjectedError_Propagates(t *testing.T) {
	sentinel := fmt.Errorf("network hiccup")
	f := &FakeDownloader{
		DownloadAudioFn: func(ctx context.Context, url, outDir string) (string, error) {
			return "", sentinel
		},
	}
	if _, err := f.DownloadAudio(context.Background(), "u", t.TempDir()); err == nil ||
		!errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want errors.Is sentinel", err)
	}
}

func TestFakeDownloader_DownloadAudio_CtxCancelled_ReturnsCtxErr(t *testing.T) {
	invoked := false
	f := &FakeDownloader{
		DownloadAudioFn: func(ctx context.Context, url, outDir string) (string, error) {
			invoked = true
			return "", nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.DownloadAudio(ctx, "u", t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if invoked {
		t.Fatal("injected fn ran despite cancelled ctx")
	}
}

func TestFakeDownloader_DownloadAudio_MissingOutDir_Rejected(t *testing.T) {
	f := &FakeDownloader{}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := f.DownloadAudio(context.Background(), "u", missing); err == nil {
		t.Fatal("expected error when outDir does not exist")
	}
}

func TestFakeDownloader_DownloadAudio_EmptyURL_Rejected(t *testing.T) {
	f := &FakeDownloader{}
	if _, err := f.DownloadAudio(context.Background(), "", t.TempDir()); err == nil {
		t.Fatal("expected error when url is empty")
	}
}

func TestFakeDownloader_DownloadAudio_RecordsCall(t *testing.T) {
	f := &FakeDownloader{}
	dir := t.TempDir()
	_, _ = f.DownloadAudio(context.Background(), "u1", dir)
	_, _ = f.DownloadAudio(context.Background(), "u2", dir)
	if len(f.AudioCalls) != 2 {
		t.Fatalf("AudioCalls len = %d, want 2", len(f.AudioCalls))
	}
	if f.AudioCalls[0].URL != "u1" || f.AudioCalls[0].OutDir != dir {
		t.Fatalf("AudioCalls[0] = %+v", f.AudioCalls[0])
	}
}

// ---------------------------------------------------------------------------
// Cross-cutting
// ---------------------------------------------------------------------------

func TestFakeDownloader_SatisfiesDownloaderInterface(t *testing.T) {
	// Compile-time assertion — this test also catches accidental
	// signature drift when contracts.md is re-frozen.
	var _ Downloader = (*FakeDownloader)(nil)
}

func TestFakeDownloader_ZeroValue_IsUsable(t *testing.T) {
	// The zero value must not panic on any of the three methods with a
	// live ctx and valid arguments — this is the property callers rely
	// on when they type `f := &FakeDownloader{}` in a test.
	f := &FakeDownloader{}
	if _, err := f.FetchMetadata(context.Background(), "https://x"); err != nil {
		t.Fatalf("zero-value FetchMetadata failed: %v", err)
	}
	if _, err := f.DownloadSubtitle(context.Background(), "https://x", t.TempDir(),
		[]string{"zh-CN"}); err != nil {
		t.Fatalf("zero-value DownloadSubtitle failed: %v", err)
	}
	if _, err := f.DownloadAudio(context.Background(), "https://x", t.TempDir()); err != nil {
		t.Fatalf("zero-value DownloadAudio failed: %v", err)
	}
}

// RejectedCalls_NotRecorded proves the doc contract "rejected calls
// (empty url, cancelled ctx) do NOT enter the log" — every negative
// path (ctx cancelled, empty url, empty outDir, empty langPref,
// non-existent outDir, outDir-is-a-file) must leave the *Calls slices
// untouched, otherwise pipeline assertions like `len(f.MetadataCalls)
// == 1 after one successful call` become fragile.
func TestFakeDownloader_RejectedCalls_NotRecorded(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	// A regular file masquerading as outDir — used to hit the new
	// !IsDir branch in DownloadSubtitle / DownloadAudio.
	fileDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(fileDir, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	cases := []struct {
		name string
		fn   func(*FakeDownloader) error
	}{
		{"FetchMetadata_CtxCancelled", func(f *FakeDownloader) error {
			_, err := f.FetchMetadata(cancelled, "https://x")
			return err
		}},
		{"FetchMetadata_EmptyURL", func(f *FakeDownloader) error {
			_, err := f.FetchMetadata(context.Background(), "")
			return err
		}},
		{"DownloadSubtitle_CtxCancelled", func(f *FakeDownloader) error {
			_, err := f.DownloadSubtitle(cancelled, "u", t.TempDir(), []string{"zh"})
			return err
		}},
		{"DownloadSubtitle_EmptyURL", func(f *FakeDownloader) error {
			_, err := f.DownloadSubtitle(context.Background(), "", t.TempDir(), []string{"zh"})
			return err
		}},
		{"DownloadSubtitle_EmptyOutDir", func(f *FakeDownloader) error {
			_, err := f.DownloadSubtitle(context.Background(), "u", "", []string{"zh"})
			return err
		}},
		{"DownloadSubtitle_EmptyLangPref", func(f *FakeDownloader) error {
			_, err := f.DownloadSubtitle(context.Background(), "u", t.TempDir(), nil)
			return err
		}},
		{"DownloadSubtitle_OutDirMissing", func(f *FakeDownloader) error {
			_, err := f.DownloadSubtitle(context.Background(), "u",
				filepath.Join(t.TempDir(), "missing"), []string{"zh"})
			return err
		}},
		{"DownloadSubtitle_OutDirIsFile", func(f *FakeDownloader) error {
			_, err := f.DownloadSubtitle(context.Background(), "u", fileDir, []string{"zh"})
			return err
		}},
		{"DownloadAudio_CtxCancelled", func(f *FakeDownloader) error {
			_, err := f.DownloadAudio(cancelled, "u", t.TempDir())
			return err
		}},
		{"DownloadAudio_EmptyURL", func(f *FakeDownloader) error {
			_, err := f.DownloadAudio(context.Background(), "", t.TempDir())
			return err
		}},
		{"DownloadAudio_EmptyOutDir", func(f *FakeDownloader) error {
			_, err := f.DownloadAudio(context.Background(), "u", "")
			return err
		}},
		{"DownloadAudio_OutDirMissing", func(f *FakeDownloader) error {
			_, err := f.DownloadAudio(context.Background(), "u",
				filepath.Join(t.TempDir(), "missing"))
			return err
		}},
		{"DownloadAudio_OutDirIsFile", func(f *FakeDownloader) error {
			_, err := f.DownloadAudio(context.Background(), "u", fileDir)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &FakeDownloader{}
			if err := tc.fn(f); err == nil {
				t.Fatal("expected rejection error, got nil")
			}
			if len(f.MetadataCalls)+len(f.SubtitleCalls)+len(f.AudioCalls) != 0 {
				t.Fatalf("rejected call must not record: metadata=%d subtitle=%d audio=%d",
					len(f.MetadataCalls), len(f.SubtitleCalls), len(f.AudioCalls))
			}
		})
	}
}

// LangPref_CopiedInto_SubtitleCalls proves the deep-copy contract at
// fake.go langCopy — callers may mutate the langPref slice after
// DownloadSubtitle returns without corrupting the recorded call.
func TestFakeDownloader_DownloadSubtitle_LangPrefIsolated(t *testing.T) {
	f := &FakeDownloader{}
	langs := []string{"zh-CN", "en"}
	if _, err := f.DownloadSubtitle(context.Background(), "u", t.TempDir(), langs); err != nil {
		t.Fatalf("DownloadSubtitle: %v", err)
	}
	// Mutate the original slice; the call log must be unaffected.
	langs[0] = "tampered"
	if got := f.SubtitleCalls[0].LangPref[0]; got != "zh-CN" {
		t.Fatalf("SubtitleCalls[0].LangPref[0] = %q, want zh-CN (deep-copy contract broken)", got)
	}
}

// DefaultMetadataStub_MatchesTestdataMetadataJSON is a drift guard: the
// FakeDownloader default stub advertises "values line up with
// testdata/metadata.json" (fake.go:44,206), which becomes silently
// false the day someone edits either side. This test parses the JSON
// and asserts the two Metadata sources agree on the fields the
// pipeline actually consumes.
func TestFakeDownloader_DefaultMetadataStub_MatchesTestdataMetadataJSON(t *testing.T) {
	// Locate testdata/metadata.json via the same runtime.Caller anchor
	// the fake uses, so this test survives being run from any cwd.
	path, err := testdataPath("metadata.json")
	if err != nil {
		t.Fatalf("locate testdata/metadata.json: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var payload struct {
		PlaylistCount int     `json:"playlist_count"`
		Duration      float64 `json:"duration"`
		Subtitles     map[string][]struct {
			Ext string `json:"ext"`
			URL string `json:"url"`
		} `json:"subtitles"`
		AutomaticCaptions map[string][]struct {
			Ext string `json:"ext"`
			URL string `json:"url"`
		} `json:"automatic_captions"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	stub := defaultMetadataStub()

	if stub.TotalPages != payload.PlaylistCount {
		t.Fatalf("TotalPages: stub=%d json.playlist_count=%d", stub.TotalPages, payload.PlaylistCount)
	}
	if wantDur := time.Duration(payload.Duration * float64(time.Second)); stub.Duration != wantDur {
		t.Fatalf("Duration: stub=%s json.duration=%v (=> %s)", stub.Duration, payload.Duration, wantDur)
	}
	if _, ok := payload.Subtitles["zh-CN"]; !ok {
		t.Fatal("metadata.json missing zh-CN in subtitles; stub declares one")
	}
	if len(stub.Subtitles) != 1 || stub.Subtitles[0].Lang != "zh-CN" || stub.Subtitles[0].Ext != "srt" {
		t.Fatalf("stub.Subtitles = %+v, want single zh-CN/srt entry", stub.Subtitles)
	}
	if _, ok := payload.AutomaticCaptions["ai-zh"]; !ok {
		t.Fatal("metadata.json missing ai-zh in automatic_captions; stub declares one")
	}
	if len(stub.AutoCaptions) != 1 || stub.AutoCaptions[0].Lang != "ai-zh" || stub.AutoCaptions[0].Ext != "srt" {
		t.Fatalf("stub.AutoCaptions = %+v, want single ai-zh/srt entry", stub.AutoCaptions)
	}
}
