package webui

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func newUIHarness(t *testing.T) (*uiHandler, *broker, *sub) {
	t.Helper()
	b := newBrokerWithCap(256, 50000)
	t.Cleanup(b.closeAll)
	s := b.subscribe()
	return newUIHandler(b), b, s
}

func TestUIHandlerStepStart(t *testing.T) {
	h, _, s := newUIHarness(t)

	slog.New(h).Info("step start", "step", "metadata")

	log := mustRecv(t, s, time.Second)
	if log.Type != "log" || log.Level != "info" {
		t.Fatalf("first event = %+v, want info log", log)
	}
	want := regexp.MustCompile(`^INFO step start step=metadata$`)
	if !want.MatchString(log.Text) {
		t.Fatalf("log text = %q, want TIME LEVEL message k=v", log.Text)
	}

	phase := mustRecv(t, s, time.Second)
	if phase.Type != "phase" || phase.Step != "metadata" || phase.Status != "start" {
		t.Fatalf("second event = %+v, want phase{metadata,start}", phase)
	}
	assertNoEvent(t, s, 50*time.Millisecond)
}

func TestUIHandlerStepDone(t *testing.T) {
	h, _, s := newUIHarness(t)

	slog.New(h).Info("step done", "step", "metadata", "took", "12.3ms")

	log := mustRecv(t, s, time.Second)
	if log.Type != "log" || log.Level != "info" {
		t.Fatalf("event = %+v, want info log", log)
	}
	if !strings.Contains(log.Text, "step done") ||
		!strings.Contains(log.Text, "step=metadata") ||
		!strings.Contains(log.Text, "took=12.3ms") {
		t.Fatalf("log text = %q, want step done with step and took", log.Text)
	}

	phase := mustRecv(t, s, time.Second)
	if phase.Type != "phase" || phase.Step != "metadata" || phase.Status != "done" {
		t.Fatalf("event = %+v, want phase{metadata,done}", phase)
	}
}

func TestUIHandlerStepFail(t *testing.T) {
	h, _, s := newUIHarness(t)

	slog.New(h).Error("step fail",
		"step", "transcribe",
		"took", "1.2s",
		"err", "whisper exited with status 1",
		"debug_log", "/tmp/debug.log",
	)

	log := mustRecv(t, s, time.Second)
	if log.Type != "log" || log.Level != "error" {
		t.Fatalf("event = %+v, want error log", log)
	}
	for _, frag := range []string{
		"ERROR step fail",
		"step=transcribe",
		"took=1.2s",
		"err=whisper exited with status 1",
		"debug_log=/tmp/debug.log",
	} {
		if !strings.Contains(log.Text, frag) {
			t.Fatalf("log text = %q, missing %q", log.Text, frag)
		}
	}

	phase := mustRecv(t, s, time.Second)
	if phase.Type != "phase" || phase.Step != "transcribe" || phase.Status != "fail" {
		t.Fatalf("event = %+v, want phase{transcribe,fail}", phase)
	}
}

func TestUIHandlerWhitelist(t *testing.T) {
	steps := []string{
		"metadata",
		"download-subtitle",
		"parse-subtitle",
		"download-audio",
		"transcode",
		"transcribe",
		"parse-asr",
		"format",
	}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			h, _, s := newUIHarness(t)
			slog.New(h).Info("step start", "step", step)
			log := mustRecv(t, s, time.Second)
			if log.Type != "log" {
				t.Fatalf("first event = %+v, want log", log)
			}
			phase := mustRecv(t, s, time.Second)
			if phase.Type != "phase" || phase.Step != step || phase.Status != "start" {
				t.Fatalf("event = %+v, want phase{%s,start}", phase, step)
			}
		})
	}
}

func TestUIHandlerUnknownStepAndMissingStep(t *testing.T) {
	t.Run("unknown step", func(t *testing.T) {
		h, _, s := newUIHarness(t)
		slog.New(h).Info("step start", "step", "future-step")
		log := mustRecv(t, s, time.Second)
		if log.Type != "log" || !strings.Contains(log.Text, "step=future-step") {
			t.Fatalf("event = %+v, want plain log with future-step attr", log)
		}
		assertNoEvent(t, s, 50*time.Millisecond)
	})

	t.Run("missing step attr", func(t *testing.T) {
		h, _, s := newUIHarness(t)
		slog.New(h).Info("step start", "url", "u1")
		log := mustRecv(t, s, time.Second)
		if log.Type != "log" || !strings.Contains(log.Text, "url=u1") {
			t.Fatalf("event = %+v, want plain log", log)
		}
		assertNoEvent(t, s, 50*time.Millisecond)
	})
}

func TestUIHandlerPlainMessagesAndLevels(t *testing.T) {
	h, _, s := newUIHarness(t)
	lg := slog.New(h)

	lg.Info("skip: output already exists", "path", "/tmp/out.txt")
	e := mustRecv(t, s, time.Second)
	if e.Type != "log" || e.Level != "info" ||
		!strings.Contains(e.Text, "INFO skip: output already exists") ||
		!strings.Contains(e.Text, "path=/tmp/out.txt") {
		t.Fatalf("event = %+v, want info log with attrs", e)
	}
	assertNoEvent(t, s, 50*time.Millisecond)

	lg.Warn("disk almost full", "free", "1%")
	e = mustRecv(t, s, time.Second)
	if e.Level != "warn" || !strings.Contains(e.Text, "WARN disk almost full") {
		t.Fatalf("event = %+v, want warn log", e)
	}

	lg.Debug("verbose detail", "k", "v")
	e = mustRecv(t, s, time.Second)
	if e.Level != "debug" || !strings.Contains(e.Text, "DEBUG verbose detail") {
		t.Fatalf("event = %+v, want debug log", e)
	}
}

func TestUIHandlerEnabled(t *testing.T) {
	h, _, _ := newUIHarness(t)
	ctx := context.Background()
	if !h.Enabled(ctx, slog.LevelDebug) {
		t.Fatal("Enabled(Debug) = false, want true")
	}
	if !h.Enabled(ctx, slog.LevelInfo) || !h.Enabled(ctx, slog.LevelWarn) || !h.Enabled(ctx, slog.LevelError) {
		t.Fatal("Enabled must be true for info/warn/error")
	}
	if h.Enabled(ctx, slog.LevelDebug-1) {
		t.Fatal("Enabled(Debug-1) = true, want false")
	}
}

func TestUIHandlerWithAttrs(t *testing.T) {
	h, _, s := newUIHarness(t)

	child := slog.New(h).With("url", "u1")
	child.Info("step start", "step", "metadata")

	log := mustRecv(t, s, time.Second)
	want := regexp.MustCompile(`^INFO step start url=u1 step=metadata$`)
	if !want.MatchString(log.Text) {
		t.Fatalf("log text = %q, want pre-bound attrs before record attrs", log.Text)
	}
	phase := mustRecv(t, s, time.Second)
	if phase.Type != "phase" || phase.Step != "metadata" {
		t.Fatalf("event = %+v, want phase", phase)
	}

	slog.New(h).Info("parent message")
	parent := mustRecv(t, s, time.Second)
	if strings.Contains(parent.Text, "url=") {
		t.Fatalf("parent handler polluted by child attrs: %q", parent.Text)
	}
}

func TestUIHandlerPreBoundStepDoesNotTriggerPhase(t *testing.T) {
	h, _, s := newUIHarness(t)

	slog.New(h).With("step", "metadata").Info("step start")

	log := mustRecv(t, s, time.Second)
	if log.Type != "log" || !strings.Contains(log.Text, "step=metadata") {
		t.Fatalf("event = %+v, want plain log rendering pre-bound step", log)
	}
	assertNoEvent(t, s, 50*time.Millisecond)
}

func TestUIHandlerGroupedStepDoesNotTriggerPhase(t *testing.T) {
	h, _, s := newUIHarness(t)

	slog.New(h).Info("step start", slog.Group("g", slog.String("step", "metadata")))

	log := mustRecv(t, s, time.Second)
	if log.Type != "log" || !strings.Contains(log.Text, "g.step=metadata") {
		t.Fatalf("event = %+v, want plain log rendering g.step=metadata", log)
	}
	assertNoEvent(t, s, 50*time.Millisecond)
}

func TestUIHandlerDuplicateRecordStepLastWins(t *testing.T) {
	h, _, s := newUIHarness(t)

	slog.New(h).Info("step start", "step", "metadata", "step", "format")

	log := mustRecv(t, s, time.Second)
	if log.Type != "log" {
		t.Fatalf("event = %+v, want log", log)
	}
	phase := mustRecv(t, s, time.Second)
	if phase.Type != "phase" || phase.Step != "format" || phase.Status != "start" {
		t.Fatalf("event = %+v, want phase{format,start} (last record step wins)", phase)
	}
}

func TestUIHandlerWithAttrsChains(t *testing.T) {
	h, _, s := newUIHarness(t)

	slog.New(h).With("a", "1").With("b", "2").Info("m", "c", "3")
	log := mustRecv(t, s, time.Second)
	want := regexp.MustCompile(`^INFO m a=1 b=2 c=3$`)
	if !want.MatchString(log.Text) {
		t.Fatalf("log text = %q, want chained pre-bound attrs first", log.Text)
	}
}

func TestUIHandlerWithGroup(t *testing.T) {
	t.Run("single group", func(t *testing.T) {
		h, _, s := newUIHarness(t)
		slog.New(h).WithGroup("g").Info("hello", "k", "v")
		log := mustRecv(t, s, time.Second)
		if !strings.HasSuffix(log.Text, "INFO hello g.k=v") {
			t.Fatalf("log text = %q, want g.k=v", log.Text)
		}
	})

	t.Run("chained groups", func(t *testing.T) {
		h, _, s := newUIHarness(t)
		slog.New(h).WithGroup("g").WithGroup("h").Info("hello", "k", "v")
		log := mustRecv(t, s, time.Second)
		if !strings.HasSuffix(log.Text, "INFO hello g.h.k=v") {
			t.Fatalf("log text = %q, want g.h.k=v", log.Text)
		}
	})

	t.Run("attrs before and after group", func(t *testing.T) {
		h, _, s := newUIHarness(t)
		slog.New(h).With("pre", "1").WithGroup("g").Info("hello", "k", "v")
		log := mustRecv(t, s, time.Second)
		if !strings.HasSuffix(log.Text, "INFO hello pre=1 g.k=v") {
			t.Fatalf("log text = %q, want pre=1 g.k=v", log.Text)
		}
	})

	t.Run("attrs bound inside group", func(t *testing.T) {
		h, _, s := newUIHarness(t)
		slog.New(h).WithGroup("g").With("a", "1").Info("hello", "k", "v")
		log := mustRecv(t, s, time.Second)
		if !strings.HasSuffix(log.Text, "INFO hello g.a=1 g.k=v") {
			t.Fatalf("log text = %q, want g.a=1 g.k=v", log.Text)
		}
	})

	t.Run("empty group name is a no-op", func(t *testing.T) {
		h, _, s := newUIHarness(t)
		derived := h.WithGroup("")
		if derived != h {
			t.Fatal("WithGroup(\"\") should return the same handler")
		}
		slog.New(h).WithGroup("").Info("hello", "k", "v")
		log := mustRecv(t, s, time.Second)
		if !strings.HasSuffix(log.Text, "INFO hello k=v") {
			t.Fatalf("log text = %q, want k=v", log.Text)
		}
	})
}

func TestUIHandlerZeroTimeRecord(t *testing.T) {
	h, _, s := newUIHarness(t)

	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "zero", 0)
	rec.AddAttrs(slog.String("k", "v"))
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle = %v, want nil", err)
	}
	log := mustRecv(t, s, time.Second)
	// 时间戳由 broker 统一盖在 Event.Time；slog 文本不含时间前缀。
	if log.Text != "INFO zero k=v" {
		t.Fatalf("log text = %q, want text without time prefix", log.Text)
	}
}

func TestUIHandlerConcurrent(t *testing.T) {
	b := newBrokerWithCap(1024, 100000)
	t.Cleanup(b.closeAll)
	h := newUIHandler(b)

	const goroutines = 20
	const perG = 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if i%2 == 0 {
					slog.New(h).Info("plain", "g", g, "i", i)
				} else {
					child := h.WithAttrs([]slog.Attr{slog.Int("g", g)})
					slog.New(child).Info("step start", "step", "metadata")
				}
			}
		}(g)
	}
	wg.Wait()

	s := b.subscribe()
	events := drain(s, 100*time.Millisecond)
	var logs, phases int
	for _, e := range events {
		switch e.Type {
		case "log":
			logs++
		case "phase":
			phases++
		}
	}
	if want := goroutines * perG; logs != want {
		t.Fatalf("log events = %d, want %d", logs, want)
	}
	if want := goroutines * perG / 2; phases != want {
		t.Fatalf("phase events = %d, want %d", phases, want)
	}
}

func TestUIHandlerAfterCloseAll(t *testing.T) {
	b := newBroker()
	h := newUIHandler(b)
	b.closeAll()

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "after close", 0)
	rec.AddAttrs(slog.String("step", "metadata"))
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle after closeAll = %v, want nil", err)
	}
}

func TestLineWriter(t *testing.T) {
	t.Run("multiple lines with trailing newline", func(t *testing.T) {
		_, b, s := newUIHarness(t)
		w := newLineWriter(b)

		n, err := w.Write([]byte("line1\nline2\nline3\n"))
		if n != 18 || err != nil {
			t.Fatalf("Write = (%d, %v), want (18, nil)", n, err)
		}
		for _, want := range []string{"line1", "line2", "line3"} {
			e := mustRecv(t, s, time.Second)
			if e.Type != "log" || e.Level != "info" || e.Text != want {
				t.Fatalf("event = %+v, want info log %q", e, want)
			}
		}
		assertNoEvent(t, s, 50*time.Millisecond)
	})

	t.Run("last chunk without trailing newline is emitted immediately", func(t *testing.T) {
		_, b, s := newUIHarness(t)
		w := newLineWriter(b)

		n, err := w.Write([]byte("x\ny"))
		if n != 3 || err != nil {
			t.Fatalf("Write = (%d, %v), want (3, nil)", n, err)
		}
		e := mustRecv(t, s, time.Second)
		if e.Text != "x" {
			t.Fatalf("event = %+v, want x", e)
		}
		e = mustRecv(t, s, time.Second)
		if e.Text != "y" {
			t.Fatalf("event = %+v, want y (flushed despite no trailing newline)", e)
		}
	})

	t.Run("separate writes never stitch", func(t *testing.T) {
		_, b, s := newUIHarness(t)
		w := newLineWriter(b)

		if n, err := w.Write([]byte("abc")); n != 3 || err != nil {
			t.Fatalf("Write(abc) = (%d, %v), want (3, nil)", n, err)
		}
		if n, err := w.Write([]byte("def\n")); n != 4 || err != nil {
			t.Fatalf("Write(def\\n) = (%d, %v), want (4, nil)", n, err)
		}
		e := mustRecv(t, s, time.Second)
		if e.Text != "abc" {
			t.Fatalf("event = %+v, want abc", e)
		}
		e = mustRecv(t, s, time.Second)
		if e.Text != "def" {
			t.Fatalf("event = %+v, want def (no cross-write stitching)", e)
		}
	})

	t.Run("empty write", func(t *testing.T) {
		_, b, s := newUIHarness(t)
		w := newLineWriter(b)
		if n, err := w.Write(nil); n != 0 || err != nil {
			t.Fatalf("Write(nil) = (%d, %v), want (0, nil)", n, err)
		}
		assertNoEvent(t, s, 50*time.Millisecond)
	})

	t.Run("only newlines", func(t *testing.T) {
		_, b, s := newUIHarness(t)
		w := newLineWriter(b)
		if n, err := w.Write([]byte("\n")); n != 1 || err != nil {
			t.Fatalf("Write(\\n) = (%d, %v), want (1, nil)", n, err)
		}
		if n, err := w.Write([]byte("\n\n")); n != 2 || err != nil {
			t.Fatalf("Write(\\n\\n) = (%d, %v), want (2, nil)", n, err)
		}
		assertNoEvent(t, s, 50*time.Millisecond)
	})

	t.Run("crlf multiple lines", func(t *testing.T) {
		_, b, s := newUIHarness(t)
		w := newLineWriter(b)

		n, err := w.Write([]byte("line1\r\nline2\r\n"))
		if n != len("line1\r\nline2\r\n") || err != nil {
			t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len("line1\r\nline2\r\n"))
		}
		for _, want := range []string{"line1", "line2"} {
			e := mustRecv(t, s, time.Second)
			if e.Type != "log" || e.Level != "info" || e.Text != want {
				t.Fatalf("event = %+v, want info log %q", e, want)
			}
		}
		assertNoEvent(t, s, 50*time.Millisecond)
	})

	t.Run("crlf single line", func(t *testing.T) {
		_, b, s := newUIHarness(t)
		w := newLineWriter(b)

		n, err := w.Write([]byte("x\r\n"))
		if n != 3 || err != nil {
			t.Fatalf("Write = (%d, %v), want (3, nil)", n, err)
		}
		e := mustRecv(t, s, time.Second)
		if e.Text != "x" {
			t.Fatalf("event = %+v, want x", e)
		}
		assertNoEvent(t, s, 50*time.Millisecond)
	})

	t.Run("after closeAll", func(t *testing.T) {
		b := newBroker()
		w := newLineWriter(b)
		b.closeAll()
		n, err := w.Write([]byte("post-close line\n"))
		if n != len("post-close line\n") || err != nil {
			t.Fatalf("Write after closeAll = (%d, %v), want (%d, nil)", n, err, len("post-close line\n"))
		}
	})
}

var _ io.Writer = (*lineWriter)(nil)

func TestUIHandlerNilBrokerDiscards(t *testing.T) {
	h := newUIHandler(nil)

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "step start", 0)
	rec.AddAttrs(slog.String("step", "metadata"))
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle with nil broker = %v, want nil", err)
	}
}

func TestLineWriterNilBrokerDiscards(t *testing.T) {
	w := newLineWriter(nil)

	n, err := w.Write([]byte("line1\nline2\n"))
	if n != len("line1\nline2\n") || err != nil {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len("line1\nline2\n"))
	}
	if n, err := w.Write(nil); n != 0 || err != nil {
		t.Fatalf("Write(nil) = (%d, %v), want (0, nil)", n, err)
	}
}
