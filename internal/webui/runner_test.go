package webui

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"bilibili-txt/internal/config"
	"bilibili-txt/internal/logger"
	"bilibili-txt/internal/pipeline"
)

// restoreTestLogger reinstalls a quiet process-wide logger after a test
// that mutated global logger state. Tests in this package never run in
// parallel (they share the global logger), and go test isolates packages
// from each other.
func restoreTestLogger(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = logger.Init(logger.Options{
			Format: "text",
			Level:  "error",
			Stderr: io.Discard,
		})
	})
}

func quietLogger(t *testing.T) {
	t.Helper()
	restoreTestLogger(t)
	if err := logger.Init(logger.Options{
		Format: "text",
		Level:  "error",
		Stderr: io.Discard,
	}); err != nil {
		t.Fatalf("quiet logger init: %v", err)
	}
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OutputDir = t.TempDir()
	cfg.Naming.OnConflict = "ask"
	return cfg
}

// TestJobRunner_DepsBuilderInvokedDuringRun proves the timing contract
// from plan:521/542 — the deps builder runs INSIDE Run (after the
// orchestration layer has installed the UI logger), never at
// construction time. A line written through logger.StderrSink from
// inside the builder must reach the broker via the injected
// ExternalWriter; pre-installing the sink (e.g. in newJobRunner) would
// have bound it to a discard-only logger and the probe would be lost.
func TestJobRunner_DepsBuilderInvokedDuringRun(t *testing.T) {
	restoreTestLogger(t)

	b := newBrokerWithCap(256, 50000)
	t.Cleanup(b.closeAll)
	sub := b.subscribe()

	if err := logger.Init(logger.Options{
		Format:         "text",
		Level:          "debug",
		Stderr:         io.Discard,
		ExternalWriter: newLineWriter(b),
	}); err != nil {
		t.Fatalf("logger init: %v", err)
	}

	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	r := newJobRunner(cfg)

	called := false
	r.deps = func(c *config.Config) pipeline.Deps {
		called = true
		if _, err := logger.StderrSink("yt-dlp").Write([]byte("[probe]\n")); err != nil {
			t.Errorf("probe write: %v", err)
		}
		return pipeline.Deps{}
	}

	// Empty deps make pipeline.Run fail fast, but the builder has
	// already executed by then.
	if _, err := r.Run(context.Background(), "https://www.bilibili.com/video/BV1xxxxxxxxxx"); err == nil {
		t.Fatal("Run with empty deps unexpectedly succeeded")
	}
	if !called {
		t.Fatal("injected deps builder was not called during Run")
	}

	e := mustRecv(t, sub, time.Second)
	if e.Type != "log" || !strings.Contains(e.Text, "[probe]") {
		t.Fatalf("event = %+v, want log containing [probe] (deps built after logger.Init)", e)
	}
}

func TestJobRunner_SetsSkipConflictAndCopiesConfig(t *testing.T) {
	quietLogger(t)

	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Naming.OnConflict = "ask"

	r := newJobRunner(cfg)
	var seen *config.Config
	r.deps = func(c *config.Config) pipeline.Deps {
		seen = c
		return pipeline.Deps{}
	}

	if _, err := r.Run(context.Background(), "https://www.bilibili.com/video/BV1xxxxxxxxxx"); err == nil {
		t.Fatal("Run with empty deps unexpectedly succeeded")
	}

	if seen == nil {
		t.Fatal("deps builder was not called")
	}
	if seen == cfg {
		t.Fatal("deps builder received the original config pointer; want a shallow copy")
	}
	if seen.Naming.OnConflict != "skip" {
		t.Fatalf("copied OnConflict = %q, want skip", seen.Naming.OnConflict)
	}
	if cfg.Naming.OnConflict != "ask" {
		t.Fatalf("original config mutated: OnConflict = %q, want ask", cfg.Naming.OnConflict)
	}
}
