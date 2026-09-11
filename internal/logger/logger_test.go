package logger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixedNow() time.Time {
	return time.Date(2026, 9, 1, 17, 30, 45, 0, time.UTC)
}

func TestNew_FormatValidation(t *testing.T) {
	cases := map[string]bool{
		"":     true,
		"text": true,
		"json": true,
		"TEXT": true,
		"xml":  false,
	}
	for f, ok := range cases {
		f, ok := f, ok
		t.Run("format="+f, func(t *testing.T) {
			_, err := New(Options{Format: f, Stderr: io.Discard})
			if ok && err != nil {
				t.Fatalf("expected ok, got %v", err)
			}
			if !ok && err == nil {
				t.Fatal("expected error for unsupported format")
			}
		})
	}
}

// TestNew_LevelValidation pins the string ⇒ slog.Level mapping and the
// "unknown level" error path. This is the logger-side twin of
// config.Validate's level enum check; keeping both green protects the
// invariant that a value which survives config.Validate never trips
// logger.New (§4.5 hand-off contract).
func TestNew_LevelValidation(t *testing.T) {
	okCases := []string{"", "info", "INFO", "debug", "warn", "warning", "error"}
	for _, lvl := range okCases {
		lvl := lvl
		t.Run("ok/"+lvl, func(t *testing.T) {
			_, err := New(Options{Level: lvl, Stderr: io.Discard})
			if err != nil {
				t.Fatalf("Level=%q unexpected err: %v", lvl, err)
			}
		})
	}
	_, err := New(Options{Level: "loud", Stderr: io.Discard})
	if err == nil {
		t.Fatal("Level=loud must be rejected")
	}
	if !strings.Contains(err.Error(), "loud") {
		t.Errorf("err=%v should mention offending level", err)
	}
}

// TestNew_WriteDebugFileIndependentOfLevel: WriteDebugFile controls the
// per-run debug fan-out file; Level controls slog filtering. Callers
// can turn one on without the other. Locks that separation so the CLI
// mapping (`--debug` ⇒ both on) doesn't accidentally get baked into
// New itself.
func TestNew_WriteDebugFileIndependentOfLevel(t *testing.T) {
	dir := t.TempDir()

	// Level=info + WriteDebugFile=true: debug file still gets created.
	debugPath := filepath.Join(dir, "info.log")
	lg, err := New(Options{
		Level:          "info",
		WriteDebugFile: true,
		DebugLogFile:   debugPath,
		Stderr:         io.Discard,
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatalf("Level=info + WriteDebugFile=true unexpected err: %v", err)
	}
	if lg.DebugLogPath() != debugPath {
		t.Errorf("DebugLogPath=%q want %q", lg.DebugLogPath(), debugPath)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	// Level=debug + WriteDebugFile=false: no debug file, but debug-level
	// entries still land in stderr.
	var buf bytes.Buffer
	lg, err = New(Options{
		Level:  "debug",
		Stderr: &buf,
		Now:    fixedNow,
	})
	if err != nil {
		t.Fatalf("Level=debug + WriteDebugFile=false unexpected err: %v", err)
	}
	if lg.DebugLogPath() != "" {
		t.Errorf("DebugLogPath=%q want empty (WriteDebugFile=false)", lg.DebugLogPath())
	}
	lg.Slog().Debug("hi")
	if !strings.Contains(buf.String(), "hi") {
		t.Errorf("Level=debug should include debug events, got %q", buf.String())
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNew_LevelFilter(t *testing.T) {
	var buf bytes.Buffer
	lg, err := New(Options{Level: "info", Stderr: &buf})
	if err != nil {
		t.Fatal(err)
	}
	lg.Slog().Debug("hidden")
	lg.Slog().Info("shown")
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Fatalf("debug leaked in info mode: %s", out)
	}
	if !strings.Contains(out, "shown") {
		t.Fatalf("info missing: %s", out)
	}
}

func TestNew_DebugLevelIncludesDebug(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	lg, err := New(Options{
		Level:          "debug",
		WriteDebugFile: true,
		Stderr:         &buf,
		DebugLogFile:   filepath.Join(dir, "explicit.log"),
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	lg.Slog().Debug("keep me")
	if !strings.Contains(buf.String(), "keep me") {
		t.Fatalf("debug missing when Level=debug: %s", buf.String())
	}
}

func TestNew_JSONHandler(t *testing.T) {
	var buf bytes.Buffer
	lg, err := New(Options{Format: "json", Stderr: &buf})
	if err != nil {
		t.Fatal(err)
	}
	lg.Slog().Info("hi", "k", "v")
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("expected JSON line, got %q err=%v", buf.String(), err)
	}
	if m["msg"] != "hi" || m["k"] != "v" {
		t.Fatalf("unexpected JSON payload: %v", m)
	}
}

func TestNew_MainLogFileFanOut(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "main.log")
	var buf bytes.Buffer
	lg, err := New(Options{File: logPath, Stderr: &buf})
	if err != nil {
		t.Fatal(err)
	}
	lg.Slog().Info("hello world")
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello world") {
		t.Fatalf("main log file missing entry: %s", data)
	}
	if !strings.Contains(buf.String(), "hello world") {
		t.Fatalf("stderr missing entry: %s", buf.String())
	}
}

func TestStderrSink_DiscardWhenNoDebug(t *testing.T) {
	lg, err := New(Options{Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sink := lg.StderrSink("ytdlp")
	if sink != io.Discard {
		t.Fatalf("expected io.Discard sink, got %T", sink)
	}
}

func TestStderrSink_PrefixWhenDebug(t *testing.T) {
	dir := t.TempDir()
	debugPath := filepath.Join(dir, "debug.log")
	lg, err := New(Options{
		WriteDebugFile: true,
		Stderr:         io.Discard,
		DebugLogFile:   debugPath,
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := lg.StderrSink("ffmpeg")
	if _, err := io.WriteString(sink, "line1\nline2\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(sink, "partial"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(sink, " tail\n"); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(debugPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	wantTS := fixedNow().Format("15:04:05.000")
	wantLines := []string{
		"[" + wantTS + " ffmpeg] line1",
		"[" + wantTS + " ffmpeg] line2",
		"[" + wantTS + " ffmpeg] partial tail",
	}
	for _, w := range wantLines {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in %q", w, got)
		}
	}
}

func TestResolveDebugLogPath_ExplicitAbsolute(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "explicit.log")
	got, err := resolveDebugLogPath(want, "BV1", fixedNow(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveDebugLogPath_ExplicitRelativeUsesCwd(t *testing.T) {
	dir := t.TempDir()
	cwd := chdirTemp(t, dir)
	rel := filepath.Join("sub", "explicit.log")
	got, err := resolveDebugLogPath(rel, "BV1", fixedNow(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cwd, rel)
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, err := os.Stat(filepath.Dir(got)); err != nil {
		t.Fatalf("expected parent dir created: %v", err)
	}
}

func TestResolveDebugLogPath_DefaultCwdLogs(t *testing.T) {
	dir := t.TempDir()
	cwd := chdirTemp(t, dir)
	got, err := resolveDebugLogPath("", "BV1abc", fixedNow(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(cwd, "logs")
	if filepath.Dir(got) != wantDir {
		t.Fatalf("expected under %q, got %q", wantDir, got)
	}
	if !strings.HasPrefix(filepath.Base(got), "BV1abc-") {
		t.Fatalf("filename should start with bvid: %s", got)
	}
	if !strings.HasSuffix(got, ".log") {
		t.Fatalf("filename should end with .log: %s", got)
	}
}

func TestResolveDebugLogPath_NoBvidPlaceholder(t *testing.T) {
	dir := t.TempDir()
	chdirTemp(t, dir)
	got, err := resolveDebugLogPath("", "  ", fixedNow(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(got), "nobvid-") {
		t.Fatalf("expected nobvid placeholder, got %s", got)
	}
}

func TestResolveDebugLogPath_TimestampFormat(t *testing.T) {
	dir := t.TempDir()
	chdirTemp(t, dir)
	got, err := resolveDebugLogPath("", "BV1", fixedNow(), nil)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(got)
	want := "BV1-20260901-173045.log"
	if base != want {
		t.Fatalf("timestamp mismatch: got %s want %s", base, want)
	}
}

func TestResolveDebugLogPath_TmpFallbackWhenCwdReadOnly(t *testing.T) {
	dir := t.TempDir()
	chdirTemp(t, dir)
	logsPath := filepath.Join(dir, "logs")
	if err := os.WriteFile(logsPath, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	var warnBuf bytes.Buffer
	warn := newTestSlog(&warnBuf)
	got, err := resolveDebugLogPath("", "BV1", fixedNow(), warn)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(os.TempDir(), "bilibili-txt-logs")
	if filepath.Dir(got) != wantDir {
		t.Fatalf("expected fallback under %q, got %q", wantDir, got)
	}
	warnOut := warnBuf.String()
	if !strings.Contains(warnOut, "falling back") {
		t.Fatalf("expected warning log, got %q", warnOut)
	}
	if !strings.Contains(warnOut, "primary_err=") {
		t.Fatalf("expected primary_err field surfaced in warn, got %q", warnOut)
	}
}

func TestInit_SetsGlobal(t *testing.T) {
	globalMu.Lock()
	prev := global
	globalMu.Unlock()
	t.Cleanup(func() {
		globalMu.Lock()
		cur := global
		global = prev
		globalMu.Unlock()
		if cur != nil && cur != prev {
			_ = cur.Close()
		}
	})

	dir := t.TempDir()
	if err := Init(Options{
		Level:          "debug",
		WriteDebugFile: true,
		DebugLogFile:   filepath.Join(dir, "d.log"),
		Stderr:         io.Discard,
		Now:            fixedNow,
	}); err != nil {
		t.Fatal(err)
	}
	globalMu.Lock()
	cur := global
	globalMu.Unlock()
	if cur == nil {
		t.Fatal("expected global logger set")
	}
	if cur.DebugLogPath() == "" {
		t.Fatal("expected debug log path recorded")
	}
	if err := cur.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestClose_ResetsSlogDefaultWhenOwned locks the P3-新1 hazard: Init() calls
// slog.SetDefault(lg.Slog()), so any code doing plain `slog.Info(...)` writes
// through our handler. Before this fix, Close() only reset lg.slogPtr and left
// slog.Default() pointing at a handler whose underlying fd we then closed —
// the next `slog.Info(...)` from library code would race with the close and
// potentially write to a closed fd. Close() now resets slog.Default() to a
// discard handler if it still points at our slog.
func TestClose_ResetsSlogDefaultWhenOwned(t *testing.T) {
	globalMu.Lock()
	prev := global
	globalMu.Unlock()
	prevDefault := slog.Default()
	t.Cleanup(func() {
		globalMu.Lock()
		cur := global
		global = prev
		globalMu.Unlock()
		if cur != nil && cur != prev {
			_ = cur.Close()
		}
		slog.SetDefault(prevDefault)
	})

	dir := t.TempDir()
	if err := Init(Options{
		Level:          "debug",
		WriteDebugFile: true,
		DebugLogFile:   filepath.Join(dir, "d.log"),
		File:           filepath.Join(dir, "main.log"),
		Stderr:         io.Discard,
		Now:            fixedNow,
	}); err != nil {
		t.Fatal(err)
	}
	globalMu.Lock()
	cur := global
	globalMu.Unlock()

	preClose := cur.Slog()
	if slog.Default() != preClose {
		t.Fatal("precondition: slog.Default should be the logger installed by Init")
	}
	if err := cur.Close(); err != nil {
		t.Fatal(err)
	}
	if slog.Default() == preClose {
		t.Fatal("Close must reset slog.Default away from the pre-close handler when it owned the current default")
	}
	// A bare slog.Info after Close must not panic and must not touch the
	// closed fds — the discard handler swallows it.
	slog.Info("post close via slog.Default")
}

// TestClose_DoesNotResetSlogDefaultWhenNotOwner is the negative twin: if
// slog.Default has been swapped to some other handler between Init and Close,
// our Close must NOT clobber it. This protects callers who install their own
// handlers via slog.SetDefault after we've set ours.
func TestClose_DoesNotResetSlogDefaultWhenNotOwner(t *testing.T) {
	globalMu.Lock()
	prev := global
	globalMu.Unlock()
	prevDefault := slog.Default()
	t.Cleanup(func() {
		globalMu.Lock()
		cur := global
		global = prev
		globalMu.Unlock()
		if cur != nil && cur != prev {
			_ = cur.Close()
		}
		slog.SetDefault(prevDefault)
	})

	dir := t.TempDir()
	if err := Init(Options{
		Level:          "debug",
		WriteDebugFile: true,
		DebugLogFile:   filepath.Join(dir, "d.log"),
		Stderr:         io.Discard,
		Now:            fixedNow,
	}); err != nil {
		t.Fatal(err)
	}
	globalMu.Lock()
	cur := global
	globalMu.Unlock()

	// Some third party overrides slog.Default after Init.
	overriding := slog.New(slog.NewTextHandler(io.Discard, nil))
	slog.SetDefault(overriding)

	if err := cur.Close(); err != nil {
		t.Fatal(err)
	}
	if slog.Default() != overriding {
		t.Fatal("Close must NOT clobber a slog.Default installed by someone else after Init")
	}
}

func TestStepHelpers(t *testing.T) {
	var buf bytes.Buffer
	lg, err := New(Options{Format: "json", Stderr: &buf})
	if err != nil {
		t.Fatal(err)
	}
	lg.StepStart("metadata", "url", "u1")
	lg.StepDone("metadata", 1200*time.Millisecond, "bvid", "BV1")
	lg.StepFail("subtitle", 500*time.Millisecond, errors.New("boom"), "lang", "zh")

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %s", len(lines), buf.String())
	}
	got := decodeLines(t, lines)
	if got[0]["step"] != "metadata" || got[0]["msg"] != "step start" || got[0]["url"] != "u1" {
		t.Fatalf("unexpected start: %v", got[0])
	}
	if got[1]["took"] != "1.2s" {
		t.Fatalf("expected took=1.2s, got %v", got[1]["took"])
	}
	if got[2]["level"] != "ERROR" {
		t.Fatalf("expected ERROR level for fail, got %v", got[2]["level"])
	}
	if got[2]["err"] != "boom" {
		t.Fatalf("expected err=boom, got %v", got[2]["err"])
	}
}

func TestWithComponent(t *testing.T) {
	var buf bytes.Buffer
	lg, err := New(Options{Format: "json", Stderr: &buf})
	if err != nil {
		t.Fatal(err)
	}
	child := lg.WithComponent("preflight")
	child.Slog().Info("hi")
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatal(err)
	}
	if m["component"] != "preflight" {
		t.Fatalf("expected component=preflight, got %v", m)
	}
}

func TestClose_Idempotent(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(Options{
		Level:          "debug",
		WriteDebugFile: true,
		File:           filepath.Join(dir, "main.log"),
		DebugLogFile:   filepath.Join(dir, "debug.log"),
		Stderr:         io.Discard,
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("third close: %v", err)
	}
}

func TestNew_FailedDebugInit_NoFDLeak(t *testing.T) {
	dir := t.TempDir()
	mainLog := filepath.Join(dir, "main.log")
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("regular file"), 0o644); err != nil {
		t.Fatal(err)
	}
	badDebug := filepath.Join(blocker, "child.log")

	before := countOpenFDs(t)
	_, err := New(Options{
		Level:          "debug",
		WriteDebugFile: true,
		File:           mainLog,
		DebugLogFile:   badDebug,
		Stderr:         io.Discard,
		Now:            fixedNow,
	})
	if err == nil {
		t.Fatal("expected debug log init to fail when parent is a regular file")
	}
	after := countOpenFDs(t)
	if after > before {
		t.Fatalf("fd leak on failed New: before=%d after=%d", before, after)
	}
}

func TestInit_SecondCallClosesPrevious_NoFDLeak(t *testing.T) {
	prev := global
	t.Cleanup(func() {
		globalMu.Lock()
		if global != nil && global != prev {
			_ = global.Close()
		}
		global = prev
		globalMu.Unlock()
	})

	dir := t.TempDir()
	baseline := countOpenFDs(t)
	for i := 0; i < 5; i++ {
		if err := Init(Options{
			Level:          "debug",
			WriteDebugFile: true,
			File:           filepath.Join(dir, fmt.Sprintf("main-%d.log", i)),
			DebugLogFile:   filepath.Join(dir, fmt.Sprintf("debug-%d.log", i)),
			Stderr:         io.Discard,
			Now:            fixedNow,
		}); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	globalMu.Lock()
	cur := global
	globalMu.Unlock()
	if cur == nil {
		t.Fatal("global logger unexpectedly nil after Init loop")
	}
	after := countOpenFDs(t)
	if after-baseline > 4 {
		t.Fatalf("fd leak across repeated Init: baseline=%d after=%d", baseline, after)
	}
	if err := cur.Close(); err != nil {
		t.Fatalf("close current: %v", err)
	}
}

func TestNew_MainAndDebugFanOut_Separation(t *testing.T) {
	dir := t.TempDir()
	mainLog := filepath.Join(dir, "main.log")
	debugLog := filepath.Join(dir, "debug.log")
	var stderr bytes.Buffer
	lg, err := New(Options{
		Level:          "debug",
		WriteDebugFile: true,
		File:           mainLog,
		DebugLogFile:   debugLog,
		Stderr:         &stderr,
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	lg.Slog().Info("structured event", "k", "v")
	sink := lg.StderrSink("ytdlp")
	if _, err := io.WriteString(sink, "external raw line\n"); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	mainData, err := os.ReadFile(mainLog)
	if err != nil {
		t.Fatal(err)
	}
	debugData, err := os.ReadFile(debugLog)
	if err != nil {
		t.Fatal(err)
	}
	mainStr := string(mainData)
	debugStr := string(debugData)

	if !strings.Contains(mainStr, "structured event") {
		t.Fatalf("main log missing structured event: %s", mainStr)
	}
	if strings.Contains(mainStr, "external raw line") {
		t.Fatalf("main log leaked external stderr: %s", mainStr)
	}
	if !strings.Contains(debugStr, "external raw line") {
		t.Fatalf("debug log missing external stderr: %s", debugStr)
	}
	if strings.Contains(debugStr, "structured event") {
		t.Fatalf("debug log leaked structured event: %s", debugStr)
	}
	if !strings.Contains(stderr.String(), "structured event") {
		t.Fatalf("stderr missing structured event: %s", stderr.String())
	}
}

func TestPrefixWriter_ConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	sink := newDebugSink(&buf)
	pw := &prefixWriter{sink: sink, tool: "test", now: fixedNow}
	var wg sync.WaitGroup
	const goroutines = 16
	const perGoroutine = 32
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				line := fmt.Sprintf("g%02d-i%02d\n", id, i)
				if _, err := io.WriteString(pw, line); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Fatalf("expected %d lines, got %d\n%s", goroutines*perGoroutine, len(lines), got)
	}
	wantPrefix := "[" + fixedNow().Format("15:04:05.000") + " test] "
	seen := make(map[string]bool, len(lines))
	for _, ln := range lines {
		if !strings.HasPrefix(ln, wantPrefix) {
			t.Fatalf("line missing prefix: %q", ln)
		}
		payload := strings.TrimPrefix(ln, wantPrefix)
		if seen[payload] {
			t.Fatalf("duplicated/garbled payload: %q", payload)
		}
		seen[payload] = true
	}
	if len(seen) != goroutines*perGoroutine {
		t.Fatalf("expected %d unique payloads, got %d", goroutines*perGoroutine, len(seen))
	}
}

func TestDefault_FallbackSingleton(t *testing.T) {
	globalMu.Lock()
	prev := global
	global = nil
	globalMu.Unlock()
	t.Cleanup(func() {
		globalMu.Lock()
		global = prev
		globalMu.Unlock()
	})

	a := Default()
	b := Default()
	if a == nil || b == nil {
		t.Fatal("Default returned nil")
	}
	if a != b {
		t.Fatalf("Default should be a singleton; got two instances: %p vs %p", a, b)
	}
}

func countOpenFDs(t *testing.T) int {
	t.Helper()
	f, err := os.Open("/dev/fd")
	if err != nil {
		t.Skipf("cannot open /dev/fd: %v", err)
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		t.Skipf("cannot read /dev/fd: %v", err)
	}
	return len(names)
}

func TestStderrSink_CrossToolNoInterleave(t *testing.T) {
	dir := t.TempDir()
	debugPath := filepath.Join(dir, "debug.log")
	lg, err := New(Options{
		WriteDebugFile: true,
		Stderr:         io.Discard,
		DebugLogFile:   debugPath,
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	tools := []string{"ytdlp", "ffmpeg", "whisper"}
	const perTool = 200
	var wg sync.WaitGroup
	for _, tool := range tools {
		tool := tool
		sink := lg.StderrSink(tool)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perTool; i++ {
				chunks := []string{
					fmt.Sprintf("%s-msg-%03d-part1 ", tool, i),
					fmt.Sprintf("part2 "),
					fmt.Sprintf("part3\n"),
				}
				for _, c := range chunks {
					if _, err := io.WriteString(sink, c); err != nil {
						t.Errorf("write %s: %v", tool, err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(debugPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimRight(string(data), "\n")
	lines := strings.Split(got, "\n")
	if len(lines) != len(tools)*perTool {
		t.Fatalf("expected %d lines, got %d", len(tools)*perTool, len(lines))
	}
	prefixByTool := map[string]string{}
	for _, tool := range tools {
		prefixByTool[tool] = "[" + fixedNow().Format("15:04:05.000") + " " + tool + "] "
	}
	for _, ln := range lines {
		matched := ""
		for tool, prefix := range prefixByTool {
			if strings.HasPrefix(ln, prefix) {
				matched = tool
				break
			}
		}
		if matched == "" {
			t.Fatalf("line has no valid tool prefix: %q", ln)
		}
		payload := strings.TrimPrefix(ln, prefixByTool[matched])
		if !strings.HasPrefix(payload, matched+"-msg-") {
			t.Fatalf("payload %q does not match its declared tool prefix %q", payload, matched)
		}
		if !strings.HasSuffix(payload, "part1 part2 part3") {
			t.Fatalf("payload %q lost the concatenation of its own chunks", payload)
		}
	}
}

type errAfterNWriter struct {
	remaining int
	err       error
	buf       bytes.Buffer
}

func (w *errAfterNWriter) Write(b []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, w.err
	}
	if len(b) <= w.remaining {
		n, err := w.buf.Write(b)
		w.remaining -= n
		return n, err
	}
	n, err := w.buf.Write(b[:w.remaining])
	w.remaining -= n
	if err != nil {
		return n, err
	}
	return n, w.err
}

func TestPrefixWriter_WriteContract_OnSinkError(t *testing.T) {
	sinkErr := errors.New("boom")

	// Sub-case 1: first line fails outright. n counts zero bytes from
	// the current input as consumed — the failing line is dropped and
	// nothing has actually reached the sink. os/exec's io.Copy will
	// abort on the returned error, which matches production behaviour.
	t.Run("first_line_fails_reports_zero_bytes_consumed", func(t *testing.T) {
		underlying := &errAfterNWriter{remaining: 0, err: sinkErr}
		pw := &prefixWriter{sink: newDebugSink(underlying), tool: "t", now: fixedNow}

		n, err := io.WriteString(pw, "line1\nline2\n")
		if !errors.Is(err, sinkErr) {
			t.Fatalf("expected sink error, got %v", err)
		}
		if n != 0 {
			t.Fatalf("expected n=0 (no line shipped, failing line dropped), got %d", n)
		}
		if underlying.buf.Len() != 0 {
			t.Fatalf("underlying must not have received bytes when remaining=0, got %q", underlying.buf.String())
		}
		if len(pw.buf) != 0 {
			t.Fatalf("expected p.buf empty after failure, got %q", pw.buf)
		}
	})

	// Sub-case 2: first line succeeds, second line fails. n counts only
	// the first successfully-shipped line. The failing line is dropped
	// from our side.
	t.Run("partial_success_n_counts_only_shipped_lines", func(t *testing.T) {
		prefix := "[" + fixedNow().Format("15:04:05.000") + " t] "
		underlying := &errAfterNWriter{remaining: len(prefix) + len("line1\n"), err: sinkErr}
		pw := &prefixWriter{sink: newDebugSink(underlying), tool: "t", now: fixedNow}

		input := "line1\nline2\n"
		n, err := io.WriteString(pw, input)
		if !errors.Is(err, sinkErr) {
			t.Fatalf("expected sink error, got %v", err)
		}
		if n != len("line1\n") {
			t.Fatalf("expected n=%d (only first line shipped), got %d", len("line1\n"), n)
		}
		if got := underlying.buf.String(); got != prefix+"line1\n" {
			t.Fatalf("expected downstream to hold %q, got %q", prefix+"line1\n", got)
		}
	})

	// Sub-case 3: after a failure the caller resumes with new input.
	// The recovery write ships exactly one framed line with its own
	// prefix — the previously-failed line is not re-queued.
	t.Run("recovery_after_failure_ships_only_new_input", func(t *testing.T) {
		prefix := "[" + fixedNow().Format("15:04:05.000") + " t] "
		underlying := &errAfterNWriter{remaining: 0, err: sinkErr}
		pw := &prefixWriter{sink: newDebugSink(underlying), tool: "t", now: fixedNow}

		if _, err := io.WriteString(pw, "line1\n"); !errors.Is(err, sinkErr) {
			t.Fatalf("expected first write to fail, got %v", err)
		}

		var accepted bytes.Buffer
		pw.sink = newDebugSink(&accepted)
		n, err := io.WriteString(pw, "line2\n")
		if err != nil {
			t.Fatalf("recovery write failed: %v", err)
		}
		if n != len("line2\n") {
			t.Fatalf("expected n=%d on recovery, got %d", len("line2\n"), n)
		}
		// Only the new line ships — the failed line1 is gone from our side.
		want := prefix + "line2\n"
		if got := accepted.String(); got != want {
			t.Fatalf("recovery should ship only the new line; got %q want %q", got, want)
		}
		if len(pw.buf) != 0 {
			t.Fatalf("expected p.buf drained after recovery, got %q", pw.buf)
		}
	})
}

// TestPrefixWriter_NoOrphanPrefixOnPartialWrite validates the per-line prefix
// invariant: prefix and payload share a single sink Write. A partial-success
// underlying write may byte-truncate that combined buffer at any offset
// (including slicing into the payload, as this test does), but there can
// never be a standalone downstream write consisting of just
// "[HH:MM:SS.mmm tool] " with zero payload bytes — because we only ever issue
// one Write call per line.
func TestPrefixWriter_NoOrphanPrefixOnPartialWrite(t *testing.T) {
	prefix := "[" + fixedNow().Format("15:04:05.000") + " ffmpeg] "
	// Accept exactly len(prefix)+2 bytes (prefix + first 2 payload chars), then
	// fail; we expect prefix+"pa" downstream. The line is dropped from our
	// side once the sink refuses the rest — matching the R5-simplified
	// contract where the caller (os/exec) aborts on the error anyway.
	underlying := &errAfterNWriter{remaining: len(prefix) + 2, err: errors.New("disk full")}
	pw := &prefixWriter{sink: newDebugSink(underlying), tool: "ffmpeg", now: fixedNow}

	n, err := io.WriteString(pw, "payload\n")
	if err == nil {
		t.Fatalf("expected error, got nil (n=%d)", n)
	}
	if n != 0 {
		t.Fatalf("expected n=0 (single line, sink failed mid-write), got %d", n)
	}
	got := underlying.buf.String()
	if got != prefix+"pa" {
		t.Fatalf("expected downstream to receive %q, got %q", prefix+"pa", got)
	}
}

func TestClose_DisablesFurtherOutput(t *testing.T) {
	dir := t.TempDir()
	mainLog := filepath.Join(dir, "main.log")
	debugLog := filepath.Join(dir, "debug.log")
	lg, err := New(Options{
		Level:          "debug",
		WriteDebugFile: true,
		File:           mainLog,
		DebugLogFile:   debugLog,
		Stderr:         io.Discard,
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := lg.StderrSink("ytdlp")
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	// Post-close writes must not panic and must not resurrect closed fds.
	lg.Slog().Info("post close")
	if _, err := io.WriteString(sink, "post close raw\n"); err != nil {
		t.Fatalf("post-close write should have been silently swallowed, got %v", err)
	}
	dbg, err := os.ReadFile(debugLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(dbg), "post close raw") {
		t.Fatalf("post-close write leaked into debug log: %s", dbg)
	}
}

// TestClose_RaceWithSinkWrites drives the same scenario the -race detector is
// most likely to catch: goroutines still emitting stderr while Close() flips
// the sink to io.Discard. Invariants:
//  1. no data race on the swap vs. write path (validated by -race).
//  2. because Close atomically swaps the sink to io.Discard *before* it closes
//     the underlying file, and both swap() and writeLine() serialize on the
//     same mutex, writers must never observe a "file already closed" error;
//     every write either lands on the real file (pre-swap) or Discard
//     (post-swap).
//  3. no goroutine leak.
func TestClose_RaceWithSinkWrites(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(Options{
		WriteDebugFile: true,
		DebugLogFile:   filepath.Join(dir, "race.log"),
		Stderr:         io.Discard,
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	sinks := []io.Writer{
		lg.StderrSink("ytdlp"),
		lg.StderrSink("ffmpeg"),
		lg.StderrSink("whisper"),
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, len(sinks))
	for i, s := range sinks {
		s := s
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				line := fmt.Sprintf("worker=%d iter=%d\n", i, j)
				if _, err := io.WriteString(s, line); err != nil {
					errCh <- fmt.Errorf("worker %d iter %d: %w", i, j, err)
					return
				}
			}
		}()
	}

	// Give writers a moment to actually start racing.
	time.Sleep(10 * time.Millisecond)
	if err := lg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	close(stop)
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatalf("unexpected write error during Close race: %v", e)
	}
}

// TestClose_RaceWithSlogWrites is the structural twin of
// TestClose_RaceWithSinkWrites, but for the *structured* log path. The P1-1
// review finding was that Close() used to overwrite Logger.slog in place while
// concurrent StepStart/Info reads dereferenced the same field with no
// synchronization — a data race the -race detector flags. The atomic.Pointer
// migration fixes this: readers Load(), Close Store(). We drive both sides
// under -race and require no data race + no write errors, no matter which
// goroutine wins the swap.
func TestClose_RaceWithSlogWrites(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(Options{
		Level:          "debug",
		WriteDebugFile: true,
		File:           filepath.Join(dir, "main.log"),
		DebugLogFile:   filepath.Join(dir, "debug.log"),
		Stderr:         io.Discard,
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	const workers = 4
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				// Exercise every slog-facing entry point: Slog(), StepStart,
				// StepDone, StepFail, With — each racing with Close's
				// atomic.Pointer.Store.
				lg.Slog().Info("info", "worker", i, "iter", j)
				lg.StepStart("step", "worker", i)
				lg.StepDone("step", time.Millisecond, "worker", i)
				lg.StepFail("step", time.Millisecond, errors.New("x"), "worker", i)
				_ = lg.With("worker", i).Slog()
			}
		}()
	}

	time.Sleep(10 * time.Millisecond)
	if err := lg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	close(stop)
	wg.Wait()
}

// TestPrefixWriter_TailLostWithoutNewline pins the v1 documented behavior:
// data without a trailing newline is held in the per-tool buffer until the
// next newline arrives; if Close (or program exit) happens first, the tail
// is lost. This test locks that contract so a future "add Flush" refactor is
// forced to consciously update the docstring instead of silently changing
// behavior.
func TestPrefixWriter_TailLostWithoutNewline(t *testing.T) {
	dir := t.TempDir()
	debugPath := filepath.Join(dir, "debug.log")
	lg, err := New(Options{
		WriteDebugFile: true,
		DebugLogFile:   debugPath,
		Stderr:         io.Discard,
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := lg.StderrSink("ytdlp")
	if _, err := io.WriteString(sink, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(debugPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "abc") {
		t.Fatalf("v1 contract violated: unterminated tail leaked to debug log: %q", data)
	}
}

func TestNew_MainLogOpenFailure_NoFDLeak(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("regular file"), 0o644); err != nil {
		t.Fatal(err)
	}
	badMain := filepath.Join(blocker, "child.log")

	before := countOpenFDs(t)
	_, err := New(Options{
		File:   badMain,
		Stderr: io.Discard,
		Now:    fixedNow,
	})
	if err == nil {
		t.Fatal("expected main log open to fail when parent is a regular file")
	}
	after := countOpenFDs(t)
	if after > before {
		t.Fatalf("fd leak on main log open failure: before=%d after=%d", before, after)
	}
}

func TestNew_TildePathRejected(t *testing.T) {
	_, err := New(Options{
		WriteDebugFile: true,
		DebugLogFile:   "~/logs/x.log",
		Stderr:         io.Discard,
		Now:            fixedNow,
	})
	if err == nil {
		t.Fatal("expected New to reject ~ prefix (config layer is responsible for expansion)")
	}
	if !strings.Contains(err.Error(), "~") {
		t.Fatalf("error should mention ~ expansion, got: %v", err)
	}
}
