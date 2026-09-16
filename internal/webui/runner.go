package webui

import (
	"context"

	"bilibili-txt/internal/config"
	"bilibili-txt/internal/naming"
	"bilibili-txt/internal/pipeline"
	"bilibili-txt/internal/wire"
)

// Runner executes one conversion. The caller (the service job goroutine)
// guarantees that logger.Init has already completed before Run is
// invoked, so the UI slog handler and external-line writer are live.
// Production builds its pipeline dependencies inside Run; fakes may
// return a preset result and may emit phase/log events via the global
// logger (logger.Default / logger.StderrSink).
type Runner interface {
	Run(ctx context.Context, url string) (*pipeline.Result, error)
}

// depsBuilder assembles a pipeline.Deps bundle from a config. It is
// declared as a field type rather than wiring wire.BuildDeps directly so
// tests can inject a probe builder (see service_test.go).
type depsBuilder func(*config.Config) pipeline.Deps

// jobRunner is the production Runner. It owns two pieces of UI-mode
// policy on top of the raw pipeline:
//
//   - the config is shallow-copied per Run with the naming conflict
//     strategy pinned to "skip" (the Web UI never prompts), leaving the
//     long-lived service config untouched;
//   - the dependency bundle is constructed LAZILY inside Run and never
//     cached. wire builders call logger.StderrSink at construction time;
//     building them earlier (e.g. in newJobRunner) would bind the
//     yt-dlp/ffmpeg/whisper sinks before logger.Init installs the UI
//     ExternalWriter, and their output would never reach the log panel.
type jobRunner struct {
	cfg  *config.Config
	deps depsBuilder // nil -> wire.BuildDeps
}

func newJobRunner(cfg *config.Config) *jobRunner {
	return &jobRunner{cfg: cfg}
}

func (r *jobRunner) Run(ctx context.Context, url string) (*pipeline.Result, error) {
	// Shallow value copy: isolation covers only the value fields (e.g.
	// Naming.OnConflict below). Slice/map/pointer-typed fields remain
	// shared with the long-lived service config; correctness therefore
	// relies on pipeline and wire treating the config passed to them as
	// read-only (verified) and only ever using those shared reference
	// fields read-only.
	cfgCopy := *r.cfg
	cfgCopy.Naming.OnConflict = naming.StrategySkip

	build := r.deps
	if build == nil {
		build = wire.BuildDeps
	}
	d := build(&cfgCopy)

	in := pipeline.Input{
		URL:           url,
		NoInteractive: true,
		IsTTY:         false,
	}
	return pipeline.Run(ctx, in, &cfgCopy, d)
}
