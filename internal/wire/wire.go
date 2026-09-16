// Package wire owns construction of the external, exec-fronted
// dependencies (downloader / transcoder / recognizer) from a
// [config.Config]. Both the CLI (package main) and internal callers
// such as webui share this single assembly point so they can never
// drift in how config fields and logger sinks are wired.
package wire

import (
	"bilibili-txt/internal/asr"
	"bilibili-txt/internal/audio"
	"bilibili-txt/internal/config"
	"bilibili-txt/internal/downloader"
	"bilibili-txt/internal/logger"
	"bilibili-txt/internal/pipeline"
)

// Deps is an alias so callers can reference the wired dependency
// bundle without importing pipeline directly when convenient.
type Deps = pipeline.Deps

// NewDownloader builds the production yt-dlp downloader.
func NewDownloader(cfg *config.Config) downloader.Downloader {
	return &downloader.YtdlpDownloader{
		Binary:             cfg.Binaries.Ytdlp,
		Stderr:             logger.StderrSink("yt-dlp"),
		CookiesFromBrowser: cfg.Auth.CookiesFromBrowser,
	}
}

// NewTranscoder builds the production ffmpeg transcoder.
func NewTranscoder(cfg *config.Config) audio.Transcoder {
	return &audio.FfmpegTranscoder{
		Binary: cfg.Binaries.Ffmpeg,
		Stderr: logger.StderrSink("ffmpeg"),
	}
}

// NewRecognizer builds the production whisper-cli recognizer.
func NewRecognizer(cfg *config.Config) asr.Recognizer {
	return &asr.WhisperRecognizer{
		Binary: cfg.Binaries.WhisperCLI,
		Stdout: logger.StderrSink("whisper-progress"),
		Stderr: logger.StderrSink("whisper"),
	}
}

// BuildDeps assembles the full production [pipeline.Deps] bundle.
func BuildDeps(cfg *config.Config) pipeline.Deps {
	return pipeline.Deps{
		Downloader: NewDownloader(cfg),
		Transcoder: NewTranscoder(cfg),
		Recognizer: NewRecognizer(cfg),
	}
}
