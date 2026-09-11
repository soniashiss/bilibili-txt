package logger

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Options struct {
	// Level picks the slog level filter: "debug" | "info" | "warn" |
	// "error". Empty string is treated as "info", so callers that
	// don't care can leave the field at its zero value. Matching is
	// case-insensitive; any other value returns an error from New.
	Level string
	// WriteDebugFile toggles the debug fan-out target (a per-run file
	// that captures external-tool stderr with a tool prefix). It is
	// deliberately independent of Level — some debug flows want the
	// fan-out on with a normal `info` main logger, or vice versa —
	// so the CLI layer maps the user-facing `--debug` flag onto BOTH
	// (Level="debug" and WriteDebugFile=true) while yaml-driven
	// configurations get to twiddle each knob separately.
	WriteDebugFile bool
	Format         string // "text" | "json"
	File           string // main log fan-out target; "" ⇒ stderr only
	DebugLogFile   string // absolute path for the debug fan-out target; ignored when WriteDebugFile=false
	Bvid           string
	Now            func() time.Time
	Stderr         io.Writer
}

type Logger struct {
	// slog is stored behind an atomic pointer so Close() can swap it to a
	// discard-only handler without racing concurrent Info/Debug/StepStart
	// readers. Every read MUST go through slogPtr.Load(); direct field
	// access would reintroduce the P1 data race the -race detector fires
	// on.
	slogPtr      atomic.Pointer[slog.Logger]
	debugSink    *debugSink
	debugLogPath string
	now          func() time.Time
	closers      []io.Closer
	closeOnce    sync.Once
	closeErr     error
}

// debugSink is a mutable holder around the current stderr-sink writer.
// StderrSink returns prefixWriters that reference the holder (not the raw
// file), so Close can atomically swap the underlying writer to io.Discard
// before closing the file descriptor. That guarantees post-close writes are
// silently swallowed instead of returning "file already closed" errors.
type debugSink struct {
	mu sync.Mutex
	w  io.Writer
}

func newDebugSink(w io.Writer) *debugSink {
	if w == nil {
		w = io.Discard
	}
	return &debugSink{w: w}
}

// writeLine writes a single fully-assembled buffer to the underlying writer
// under the sink mutex. The caller is responsible for including any per-line
// prefix inside `line`; issuing exactly one Write call per log line
// guarantees atomicity (P2-2) — a partial-success prefix write can never
// leave an orphan "[HH:MM:SS.mmm tool] " on the sink without its payload.
func (d *debugSink) writeLine(line []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.w.Write(line)
}

func (d *debugSink) swap(w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	d.mu.Lock()
	d.w = w
	d.mu.Unlock()
}

// isDiscard reports whether the current sink target is io.Discard. Used by
// StderrSink to short-circuit and hand out io.Discard directly, avoiding a
// per-tool prefixWriter allocation when debug logging is disabled.
func (d *debugSink) isDiscard() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.w == io.Discard
}

var (
	global         *Logger
	globalMu       sync.Mutex
	fallbackOnce   sync.Once
	fallbackLogger *Logger
)

func Init(opts Options) error {
	lg, err := New(opts)
	if err != nil {
		return err
	}
	globalMu.Lock()
	prev := global
	global = lg
	slog.SetDefault(lg.Slog())
	globalMu.Unlock()
	if prev != nil {
		_ = prev.Close()
	}
	return nil
}

func Default() *Logger {
	globalMu.Lock()
	g := global
	globalMu.Unlock()
	if g != nil {
		return g
	}
	fallbackOnce.Do(func() {
		lg, err := New(Options{})
		if err != nil {
			lg = &Logger{
				debugSink: newDebugSink(io.Discard),
				now:       time.Now,
			}
			lg.slogPtr.Store(slog.Default())
		}
		fallbackLogger = lg
	})
	return fallbackLogger
}

func New(opts Options) (*Logger, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	format := strings.ToLower(strings.TrimSpace(opts.Format))
	if format == "" {
		format = "text"
	}
	if format != "text" && format != "json" {
		return nil, fmt.Errorf("logger: unsupported format %q (want text|json)", opts.Format)
	}

	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, err
	}
	handlerOpts := &slog.HandlerOptions{Level: level}

	writers := []io.Writer{opts.Stderr}
	closers := make([]io.Closer, 0, 2)

	success := false
	defer func() {
		if success {
			return
		}
		for _, c := range closers {
			_ = c.Close()
		}
	}()

	if strings.TrimSpace(opts.File) != "" {
		f, err := openLogFile(opts.File)
		if err != nil {
			return nil, fmt.Errorf("logger: open main log file: %w", err)
		}
		writers = append(writers, f)
		closers = append(closers, f)
	}

	primary := io.MultiWriter(writers...)
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(primary, handlerOpts)
	} else {
		handler = slog.NewTextHandler(primary, handlerOpts)
	}

	lg := &Logger{
		debugSink: newDebugSink(io.Discard),
		now:       opts.Now,
	}
	lg.slogPtr.Store(slog.New(handler))

	if opts.WriteDebugFile {
		path, err := resolveDebugLogPath(opts.DebugLogFile, opts.Bvid, opts.Now(), lg.Slog())
		if err != nil {
			return nil, fmt.Errorf("logger: init debug log file: %w", err)
		}
		f, err := openLogFile(path)
		if err != nil {
			return nil, fmt.Errorf("logger: open debug log file %s: %w", path, err)
		}
		lg.debugSink.swap(f)
		lg.debugLogPath = path
		closers = append(closers, f)
		lg.Slog().Debug("debug log file created", "path", path)
	}

	lg.closers = closers
	success = true
	return lg, nil
}

// parseLevel maps the string form to a slog level. Empty ⇒ Info so a
// zero-valued Options behaves the same as before the Level refactor.
// The set is intentionally the same four names accepted by
// config.Config.Logging.Level so the two layers stay in lockstep.
func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("logger: unsupported level %q (want debug|info|warn|error)", raw)
	}
}

func (l *Logger) Slog() *slog.Logger { return l.slogPtr.Load() }

func (l *Logger) DebugLogPath() string { return l.debugLogPath }

// With returns a derived logger that shares the parent's debug sink and file
// descriptors. Callers MUST NOT invoke Close on the derived logger; the parent
// owns the underlying fds. The derived logger snapshots the parent's slog at
// the moment of the call — later Close on the parent will not mutate this
// snapshot, so derived writes after parent Close are still routed to the
// original writers. In practice ownership is single-shot (parent Close ends
// the process), so this is intentional and matches how child loggers are
// used across pipeline steps.
func (l *Logger) With(kv ...any) *Logger {
	child := &Logger{
		debugSink:    l.debugSink,
		debugLogPath: l.debugLogPath,
		now:          l.now,
	}
	child.slogPtr.Store(l.Slog().With(kv...))
	return child
}

// WithComponent tags a derived logger with the given component name. Same
// lifecycle constraints as With apply.
func (l *Logger) WithComponent(name string) *Logger {
	return l.With("component", name)
}

func (l *Logger) StderrSink(tool string) io.Writer {
	if l.debugSink == nil || l.debugSink.isDiscard() {
		return io.Discard
	}
	now := l.now
	if now == nil {
		now = time.Now
	}
	return &prefixWriter{sink: l.debugSink, tool: tool, now: now}
}

// Close releases all owned fds. After Close, future Slog() / StderrSink()
// lookups on this Logger route to a discard slog and a discard sink instead
// of writing to closed file descriptors. Close is safe to call multiple
// times, and is safe to race with concurrent Slog / StepStart / StepDone /
// StepFail / StderrSink calls — but the two paths give different guarantees:
//
//   - Sink path (strict): debugSink.mu covers both swap() and writeLine(),
//     so a sink writer either lands on the real fd (pre-swap) or on
//     io.Discard (post-swap). It can never observe a half-torn state that
//     still points at the closed fd. Post-Close writes are silently
//     accepted.
//   - Slog path (best-effort): the main slog handler is swapped via a
//     single atomic.Pointer.Store. Callers that Load() *after* Close see
//     the discard slog. Callers that Load()ed the pre-Close snapshot
//     BEFORE Close may still be inside an Info/Debug call whose handler
//     writes to the just-closed main-log fd; such writes get an EBADF that
//     slog swallows internally. This will never panic or corrupt other
//     fds, but the write is dropped. If a caller needs strict "no writes
//     after Close" ordering, it must synchronize externally (e.g. drain
//     workers before Close).
//
// If this logger is the current process-wide slog.Default (typically because
// Init() installed it), Close also resets slog.Default to a discard handler.
// Otherwise any bare `slog.Info(...)` call from library code would race with
// this Close and try to write to the file descriptors we're about to close.
func (l *Logger) Close() error {
	l.closeOnce.Do(func() {
		discardSlog := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if l.debugSink != nil {
			l.debugSink.swap(io.Discard)
		}
		mySlog := l.slogPtr.Load()
		l.slogPtr.Store(discardSlog)
		// Only reset the process-wide default if it currently points at *our*
		// slog. This avoids clobbering a slog.Default that some other logger
		// (or the caller's own SetDefault) has since installed.
		if slog.Default() == mySlog {
			slog.SetDefault(discardSlog)
		}
		var firstErr error
		for _, c := range l.closers {
			if err := c.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		l.closeErr = firstErr
	})
	return l.closeErr
}

func (l *Logger) StepStart(step string, kv ...any) {
	l.Slog().Info("step start", appendStep(step, kv)...)
}

func (l *Logger) StepDone(step string, took time.Duration, kv ...any) {
	args := appendStep(step, kv)
	args = append(args, "took", took.String())
	l.Slog().Info("step done", args...)
}

func (l *Logger) StepFail(step string, took time.Duration, err error, kv ...any) {
	args := appendStep(step, kv)
	args = append(args, "took", took.String(), "err", errString(err))
	if l.debugLogPath != "" {
		args = append(args, "debug_log", l.debugLogPath)
	}
	l.Slog().Error("step fail", args...)
}

func StepStart(step string, kv ...any)                    { Default().StepStart(step, kv...) }
func StepDone(step string, took time.Duration, kv ...any) { Default().StepDone(step, took, kv...) }
func StepFail(step string, took time.Duration, err error, kv ...any) {
	Default().StepFail(step, took, err, kv...)
}

func StderrSink(tool string) io.Writer { return Default().StderrSink(tool) }

func appendStep(step string, kv []any) []any {
	out := make([]any, 0, len(kv)+2)
	out = append(out, "step", step)
	out = append(out, kv...)
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func openLogFile(path string) (*os.File, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

type prefixWriter struct {
	sink *debugSink
	tool string
	now  func() time.Time

	bufMu sync.Mutex
	buf   []byte
}

func (p *prefixWriter) prefix() string {
	ts := p.now().Format("15:04:05.000")
	return "[" + ts + " " + p.tool + "] "
}

// Write buffers input per-tool until a newline arrives, then emits complete
// lines atomically under the shared sink mutex. This guarantees:
//   - io.Writer contract (n): on success n == len(b); on error n reports
//     only bytes from the *current input* b that have been fully shipped
//     to the sink or safely buffered for the next newline. Callers that
//     resume from b[n:] never see a double-write. The pre-2026 pendingFramed
//     short-write retry queue was removed (review R5): the only production
//     caller is os/exec via io.Copy, which aborts on any error/short-write
//     without retrying, so queueing framed lines only added complexity we
//     never observed exercising.
//   - No cross-tool interleaving: whole lines from different tools never
//     stitch together, even when multiple tools' stderr pipes race.
//   - Per-line prefix invariant: every downstream line is framed as
//     prefix+payload+"\n" in a single sink Write. A partial-success
//     underlying write may byte-truncate that combined buffer at any
//     offset, but a lone "[HH:MM:SS.mmm tool] " with zero payload bytes
//     can never exist as its own downstream write.
//   - Post-close safety: the sink is swapped to io.Discard atomically by
//     Logger.Close, so writes after Close silently succeed instead of
//     hitting a closed fd.
//
// Trailing partial data (no newline) stays buffered; callers should terminate
// their streams with '\n' or accept that a dangling tail may be lost on
// program exit. yt-dlp/ffmpeg/whisper-cli all emit newline-terminated stderr,
// so v1 does not add an explicit Flush().
//
// On a sink write error the framed line is dropped from our side: the
// unbuffered partial contents that produced this line are gone, and the
// caller (os/exec) will unwind the io.Copy loop anyway. Losing at most one
// stderr line at pipe-teardown time is an acceptable trade for keeping the
// writer state a single []byte buffer.
func (p *prefixWriter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	p.bufMu.Lock()
	defer p.bufMu.Unlock()

	consumed := 0
	rest := b
	for {
		idx := bytes.IndexByte(rest, '\n')
		if idx < 0 {
			p.buf = append(p.buf, rest...)
			consumed += len(rest)
			return consumed, nil
		}

		prefix := p.prefix()
		lineLen := len(p.buf) + (idx + 1)
		framed := make([]byte, 0, len(prefix)+lineLen)
		framed = append(framed, prefix...)
		framed = append(framed, p.buf...)
		framed = append(framed, rest[:idx+1]...)
		p.buf = p.buf[:0]

		n, err := p.sink.writeLine(framed)
		if err != nil || n < len(framed) {
			if err == nil {
				err = io.ErrShortWrite
			}
			// The current line is lost from our side. Report bytes we
			// have safely shipped from *this* input (previous complete
			// lines in the same Write) so os/exec's io.Copy sees a
			// coherent partial-write and aborts.
			return consumed, err
		}

		consumed += idx + 1
		rest = rest[idx+1:]
	}
}
