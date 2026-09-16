package wire

import (
	"testing"

	"bilibili-txt/internal/asr"
	"bilibili-txt/internal/audio"
	"bilibili-txt/internal/config"
	"bilibili-txt/internal/downloader"
)

func TestBuildDeps(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default() unexpected err = %v", err)
	}

	const (
		fakeYtdlp      = "/fake/bin/yt-dlp"
		fakeFfmpeg     = "/fake/bin/ffmpeg"
		fakeWhisperCLI = "/fake/bin/whisper-cli"
		fakeBrowser    = "firefox:/tmp/profile"
	)
	cfg.Binaries.Ytdlp = fakeYtdlp
	cfg.Binaries.Ffmpeg = fakeFfmpeg
	cfg.Binaries.WhisperCLI = fakeWhisperCLI
	cfg.Auth.CookiesFromBrowser = fakeBrowser

	deps := BuildDeps(cfg)

	dl, ok := deps.Downloader.(*downloader.YtdlpDownloader)
	if !ok {
		t.Fatalf("Downloader concrete type = %T, want *downloader.YtdlpDownloader", deps.Downloader)
	}
	if dl.Binary != fakeYtdlp {
		t.Errorf("downloader.Binary = %q, want %q", dl.Binary, fakeYtdlp)
	}
	if dl.CookiesFromBrowser != fakeBrowser {
		t.Errorf("downloader.CookiesFromBrowser = %q, want %q", dl.CookiesFromBrowser, fakeBrowser)
	}
	if dl.Stderr == nil {
		t.Error("downloader.Stderr is nil, want logger sink")
	}

	tx, ok := deps.Transcoder.(*audio.FfmpegTranscoder)
	if !ok {
		t.Fatalf("Transcoder concrete type = %T, want *audio.FfmpegTranscoder", deps.Transcoder)
	}
	if tx.Binary != fakeFfmpeg {
		t.Errorf("transcoder.Binary = %q, want %q", tx.Binary, fakeFfmpeg)
	}
	if tx.Stderr == nil {
		t.Error("transcoder.Stderr is nil, want logger sink")
	}

	rec, ok := deps.Recognizer.(*asr.WhisperRecognizer)
	if !ok {
		t.Fatalf("Recognizer concrete type = %T, want *asr.WhisperRecognizer", deps.Recognizer)
	}
	if rec.Binary != fakeWhisperCLI {
		t.Errorf("recognizer.Binary = %q, want %q", rec.Binary, fakeWhisperCLI)
	}
	if rec.Stdout == nil {
		t.Error("recognizer.Stdout is nil, want logger sink")
	}
	if rec.Stderr == nil {
		t.Error("recognizer.Stderr is nil, want logger sink")
	}
}
