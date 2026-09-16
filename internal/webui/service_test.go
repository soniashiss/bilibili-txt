package webui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bilibili-txt/internal/config"
	"bilibili-txt/internal/downloader"
	"bilibili-txt/internal/history"
	"bilibili-txt/internal/logger"
	"bilibili-txt/internal/pipeline"
)

// mustRecv / findEvent are provided by broker_test.go (same package).

// drainUntilClosed consumes events until the subscription is closed or
// the timeout elapses; used by the shutdown test.
func drainUntilClosed(t *testing.T, s *sub, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case _, ok := <-s.ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("subscription was not closed within %v after Shutdown", d)
		}
	}
}

// waitForHistory consumes events up to and including the history
// broadcast and returns every event seen in publication order.
func waitForHistory(t *testing.T, s *sub) []Event {
	t.Helper()
	var got []Event
	for {
		e := mustRecv(t, s, 5*time.Second)
		got = append(got, e)
		if e.Type == "history" {
			return got
		}
	}
}

// fakeRunner is a scripted Runner: it optionally emits a phase and an
// external-tool line, signals start, blocks until release/cancel, and
// returns a preset result/error.
type fakeRunner struct {
	result    *pipeline.Result
	err       error
	block     bool
	emitPhase bool
	emitTool  bool

	started chan struct{}
	release chan struct{}

	mu     sync.Mutex
	ranURL string
	ran    int
}

func newFakeRunner(result *pipeline.Result, err error) *fakeRunner {
	return &fakeRunner{
		result:  result,
		err:     err,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (f *fakeRunner) Run(ctx context.Context, url string) (*pipeline.Result, error) {
	f.mu.Lock()
	f.ranURL = url
	f.ran++
	f.mu.Unlock()

	if f.emitPhase {
		logger.Default().Slog().Info("step start", "step", "metadata")
	}
	if f.emitTool {
		if _, err := logger.StderrSink("yt-dlp").Write([]byte("[tool] hi\n")); err != nil {
			return nil, err
		}
	}
	close(f.started)
	if f.block {
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.result, f.err
}

type serviceHarness struct {
	cfg *config.Config
	b   *broker
	s   *service
	sub *sub
}

func newServiceHarness(t *testing.T, r Runner) *serviceHarness {
	t.Helper()
	restoreTestLogger(t)
	cfg := testConfig(t)
	cfg.Logging.Level = "error" // keep the standard text sink quiet; UI fan-out still sees Info+
	b := newBroker()
	s := newService(cfg, b, r)
	t.Cleanup(b.closeAll)
	return &serviceHarness{cfg: cfg, b: b, s: s, sub: b.subscribe()}
}

const testBVID = "BV123abc4567"

func testURL() string {
	return "https://www.bilibili.com/video/" + testBVID
}

func successResult(dir string) *pipeline.Result {
	p := filepath.Join(dir, "我的视频__"+testBVID+".txt")
	return &pipeline.Result{
		OutputPath: p,
		Metadata:   &downloader.Metadata{BVID: testBVID, Title: "我的视频"},
	}
}

func TestService_SubmitExtractReject(t *testing.T) {
	h := newServiceHarness(t, newFakeRunner(nil, errors.New("unused")))

	if err := h.s.Submit("这根本不是一个链接"); err == nil {
		t.Fatal("Submit with garbage URL returned nil, want the biliurl error")
	}

	h.s.mgr.mu.Lock()
	running := h.s.mgr.running
	h.s.mgr.mu.Unlock()
	if running {
		t.Fatal("job manager is running after a rejected URL")
	}

	select {
	case e := <-h.sub.ch:
		t.Fatalf("rejected URL produced a broker event: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestService_BusyRejectsSecondSubmit(t *testing.T) {
	r := newFakeRunner(successResult(t.TempDir()), nil)
	r.block = true
	h := newServiceHarness(t, r)
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) {
		return nil, nil
	}
	h.s.enforceLimit = func(context.Context, string, int) ([]history.RemovedGroup, error) {
		return nil, nil
	}

	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	mustRecv(t, h.sub, time.Second) // reset
	<-r.started

	if err := h.s.Submit(testURL()); !errors.Is(err, ErrServiceBusy) {
		t.Fatalf("second Submit while busy = %v, want ErrServiceBusy", err)
	}

	close(r.release)
	waitForHistory(t, h.sub)
}

func TestService_SuccessFlow(t *testing.T) {
	entries := []history.Entry{{BVID: testBVID, Name: "我的视频"}}
	r := newFakeRunner(successResult(t.TempDir()), nil)
	r.emitPhase = true
	r.emitTool = true
	h := newServiceHarness(t, r)
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) {
		return entries, nil
	}
	h.s.enforceLimit = func(context.Context, string, int) ([]history.RemovedGroup, error) {
		return nil, nil
	}

	if err := h.s.Submit("  " + testURL() + "  "); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	evs := waitForHistory(t, h.sub)

	if evs[0].Type != "reset" {
		t.Fatalf("first event = %q, want reset (plan: reset precedes everything)", evs[0].Type)
	}

	phase, ok := findEvent(evs, "phase")
	if !ok {
		t.Fatal("no phase event observed from slog step start")
	}
	if phase.Step != "metadata" || phase.Status != "start" {
		t.Fatalf("phase = %q/%q, want metadata/start", phase.Step, phase.Status)
	}

	var sawStepLog, sawToolLog bool
	for _, e := range evs {
		if e.Type == "log" && strings.Contains(e.Text, "step start") &&
			strings.Contains(e.Text, "step=metadata") {
			sawStepLog = true
		}
		if e.Type == "log" && strings.Contains(e.Text, "[tool] hi") {
			sawToolLog = true
		}
	}
	if !sawStepLog {
		t.Fatal("slog step start never reached the broker log panel")
	}
	if !sawToolLog {
		t.Fatal("external-tool line never reached the broker (deps/logger timing broken)")
	}

	doneIdx, historyIdx := -1, -1
	for i, e := range evs {
		switch e.Type {
		case "done":
			doneIdx = i
		case "history":
			historyIdx = i
		}
	}
	if doneIdx < 0 || historyIdx < 0 || doneIdx > historyIdx {
		t.Fatalf("want done before history, got doneIdx=%d historyIdx=%d", doneIdx, historyIdx)
	}

	done := evs[doneIdx]
	d, ok := done.Data.(doneData)
	if !ok {
		t.Fatalf("done data = %T, want doneData", done.Data)
	}
	if d.BVID != testBVID {
		t.Fatalf("done bvid = %q, want %q", d.BVID, testBVID)
	}
	if d.Name != "我的视频__"+testBVID {
		t.Fatalf("done name = %q, want basename without extension", d.Name)
	}
	if filepath.Ext(d.Path) != ".txt" {
		t.Fatalf("done path = %q, want the produced .txt path", d.Path)
	}

	hist := evs[historyIdx]
	gotEntries, ok := hist.Data.([]history.Entry)
	if !ok {
		t.Fatalf("history data = %T, want []history.Entry", hist.Data)
	}
	if len(gotEntries) != 1 || gotEntries[0].BVID != testBVID {
		t.Fatalf("history entries = %+v", gotEntries)
	}
}

// TestService_IdleAtDoneReceipt pins the plan's most important window:
// the moment a subscriber receives the terminal "done" event, the state
// machine must already be idle, so submitting immediately can never hit
// 409. A second subscriber submits job 2 from inside its done-receive
// goroutine.
func TestService_IdleAtDoneReceipt(t *testing.T) {
	dir := t.TempDir()
	script := &scriptedRunner{
		results: []*pipeline.Result{
			{OutputPath: filepath.Join(dir, "first__BV10000001.txt"),
				Metadata: &downloader.Metadata{BVID: "BV10000001"}},
			{OutputPath: filepath.Join(dir, "second__BV10000002.txt"),
				Metadata: &downloader.Metadata{BVID: "BV10000002"}},
		},
	}
	h := newServiceHarness(t, script)
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) { return nil, nil }
	h.s.enforceLimit = func(context.Context, string, int) ([]history.RemovedGroup, error) { return nil, nil }

	probe := h.b.subscribe()
	submitErr := make(chan error, 1)
	go func() {
		for e := range probe.ch {
			if e.Type == "done" {
				// Must run strictly after markIdle.
				h.s.mgr.mu.Lock()
				running := h.s.mgr.running
				h.s.mgr.mu.Unlock()
				if running {
					submitErr <- errors.New("mgr still running when done was delivered")
					return
				}
				submitErr <- h.s.Submit(testURL())
				return
			}
		}
	}()

	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("first Submit: %v", err)
	}

	select {
	case err := <-submitErr:
		if err != nil {
			t.Fatalf("Submit immediately at done receipt = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe never observed done")
	}

	// Both jobs run to completion (two done events fan out live).
	var dones int
	deadline := time.After(5 * time.Second)
	for dones < 2 {
		select {
		case e := <-h.sub.ch:
			if e.Type == "done" {
				dones++
			}
		case <-deadline:
			t.Fatalf("only %d/2 jobs finished after immediate resubmit", dones)
		}
	}
}

type scriptedRunner struct {
	mu      sync.Mutex
	results []*pipeline.Result
	call    int
}

func (r *scriptedRunner) Run(_ context.Context, _ string) (*pipeline.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.call
	r.call++
	if i >= len(r.results) {
		return nil, fmt.Errorf("scriptedRunner: unexpected call %d", i+1)
	}
	return r.results[i], nil
}

func TestService_SuccessFlowAcceptsResubmit(t *testing.T) {
	r := newFakeRunner(successResult(t.TempDir()), nil)
	h := newServiceHarness(t, r)
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) { return nil, nil }
	h.s.enforceLimit = func(context.Context, string, int) ([]history.RemovedGroup, error) { return nil, nil }

	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	waitForHistory(t, h.sub)

	r2 := newFakeRunner(successResult(t.TempDir()), nil)
	h.s.runner = r2
	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("resubmit after terminal broadcast = %v, want nil (idle window must not exist)", err)
	}
	waitForHistory(t, h.sub)
}

func TestService_EnforceLimitKeepsFive(t *testing.T) {
	h := newServiceHarness(t, nil)
	dir := h.cfg.OutputDir
	base := time.Now().Add(-7 * 24 * time.Hour)
	for i := 1; i <= 7; i++ {
		p := filepath.Join(dir, fmt.Sprintf("title%d__BV100000%d.txt", i, i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		m := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(p, m, m); err != nil {
			t.Fatal(err)
		}
	}

	result := &pipeline.Result{
		OutputPath: filepath.Join(dir, "title7__BV1000007.txt"),
		Metadata:   &downloader.Metadata{BVID: "BV1000007"},
	}
	h.s.runner = newFakeRunner(result, nil)

	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	evs := waitForHistory(t, h.sub)

	var cleaned []string
	var histData []history.Entry
	for _, e := range evs {
		if e.Type == "log" && strings.Contains(e.Text, "已自动清理旧视频 ") {
			cleaned = append(cleaned, e.Text)
		}
		if e.Type == "history" {
			histData = e.Data.([]history.Entry)
		}
	}
	if len(cleaned) != 2 {
		t.Fatalf("cleanup warnings = %d (%v), want 2", len(cleaned), cleaned)
	}
	joined := strings.Join(cleaned, "|")
	if !strings.Contains(joined, "BV1000001") || !strings.Contains(joined, "BV1000002") {
		t.Fatalf("cleanup warnings %v must name the two oldest groups", cleaned)
	}
	if len(histData) != 5 {
		t.Fatalf("history entries after enforce = %d, want 5", len(histData))
	}
	if histData[0].BVID != "BV1000007" || histData[4].BVID != "BV1000003" {
		t.Fatalf("history order = %v, want newest-first BV1000007..BV1000003", bvids(histData))
	}
	if _, err := os.Stat(filepath.Join(dir, "title1__BV1000001.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest group still on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "title3__BV1000003.txt")); err != nil {
		t.Fatalf("kept group missing: %v", err)
	}
}

func bvids(es []history.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.BVID
	}
	return out
}

func TestService_ErrorFlow(t *testing.T) {
	h := newServiceHarness(t, newFakeRunner(nil, errors.New("boom")))
	entries := []history.Entry{{BVID: "BVold0000001", Name: "old"}}
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) { return entries, nil }

	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	evs := waitForHistory(t, h.sub)

	term, ok := findEvent(evs, "error")
	if !ok {
		t.Fatal("no error terminal event")
	}
	d, ok := term.Data.(errorData)
	if !ok {
		t.Fatalf("error data = %T, want errorData", term.Data)
	}
	if d.Message != "boom" {
		t.Fatalf("error message = %q, want boom", d.Message)
	}
	hist, ok := findEvent(evs, "history")
	if !ok {
		t.Fatal("error flow must still refresh history")
	}
	if len(hist.Data.([]history.Entry)) != 1 {
		t.Fatal("error flow history payload mismatch")
	}

	// Idle again: a follow-up Submit is accepted.
	r2 := newFakeRunner(successResult(t.TempDir()), nil)
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) { return nil, nil }
	h.s.enforceLimit = func(context.Context, string, int) ([]history.RemovedGroup, error) { return nil, nil }
	h.s.runner = r2
	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("resubmit after error = %v, want nil", err)
	}
	waitForHistory(t, h.sub)
}

func TestService_CancelFlow(t *testing.T) {
	// Cancel with no job must be nil-safe.
	newServiceHarness(t, newFakeRunner(nil, nil)).s.Cancel()

	r := newFakeRunner(nil, context.Canceled)
	r.block = true
	h := newServiceHarness(t, r)
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) {
		return []history.Entry{{BVID: "BVold0000002"}}, nil
	}

	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	mustRecv(t, h.sub, time.Second) // reset
	<-r.started
	h.s.Cancel()

	evs := waitForHistory(t, h.sub)
	if _, ok := findEvent(evs, "canceled"); !ok {
		t.Fatal("no canceled terminal event")
	}
	var sawCancelWarn bool
	for _, e := range evs {
		if e.Type == "log" && strings.Contains(e.Text, "已取消") {
			sawCancelWarn = true
		}
	}
	if !sawCancelWarn {
		t.Fatal("missing 已取消 warning")
	}
	if _, ok := findEvent(evs, "history"); !ok {
		t.Fatal("cancel flow must refresh history with a background context")
	}

	// History scan ran with a fresh context even though the job ctx was
	// canceled: the injected list would otherwise observe a canceled ctx.
	// Resubmission is accepted afterwards.
	r2 := newFakeRunner(successResult(t.TempDir()), nil)
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) { return nil, nil }
	h.s.enforceLimit = func(context.Context, string, int) ([]history.RemovedGroup, error) { return nil, nil }
	h.s.runner = r2
	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("resubmit after cancel = %v, want nil", err)
	}
	waitForHistory(t, h.sub)
}

// panicRunner crashes unconditionally inside Run. The service must turn
// the crash into an error terminal instead of letting the panic escape
// the job goroutine.
type panicRunner struct{}

func (panicRunner) Run(context.Context, string) (*pipeline.Result, error) {
	panic("boom-panic")
}

// waitForTerminal consumes events until a terminal event whose type is in
// want arrives and returns it. The panic path broadcasts only the
// terminal (no history refresh), so waitForHistory cannot be used there.
func waitForTerminal(t *testing.T, s *sub, want ...string) Event {
	t.Helper()
	allowed := make(map[string]bool, len(want))
	for _, ty := range want {
		allowed[ty] = true
	}
	for {
		e := mustRecv(t, s, 5*time.Second)
		if allowed[e.Type] {
			return e
		}
	}
}

// TestService_PanicRunnerReachesErrorTerminalAndCanResubmit pins the
// panic guard end to end: a Run that panics yields an error terminal
// carrying the panic value, never leaks the panic out of the goroutine,
// and leaves the machine idle so an immediate resubmit succeeds (no
// permanent 409).
func TestService_PanicRunnerReachesErrorTerminalAndCanResubmit(t *testing.T) {
	h := newServiceHarness(t, panicRunner{})
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) { return nil, nil }
	h.s.enforceLimit = func(context.Context, string, int) ([]history.RemovedGroup, error) { return nil, nil }

	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	term := waitForTerminal(t, h.sub, "error")
	d, ok := term.Data.(errorData)
	if !ok {
		t.Fatalf("error data = %T, want errorData", term.Data)
	}
	if !strings.Contains(d.Message, "boom-panic") {
		t.Fatalf("error message = %q, want it to contain boom-panic", d.Message)
	}

	// Idle after the recovered panic: a normal job must be accepted and
	// reach done (the panic guard's markIdle must have run).
	h.s.runner = newFakeRunner(successResult(t.TempDir()), nil)
	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("resubmit after panic = %v, want nil (no permanent busy window)", err)
	}
	waitForHistory(t, h.sub)
}

// TestService_NilResultNoErrorReachesErrorTerminal covers the (nil, nil)
// contract violation: success classification with no result is an error
// terminal, after which submission works again.
func TestService_NilResultNoErrorReachesErrorTerminal(t *testing.T) {
	h := newServiceHarness(t, newFakeRunner(nil, nil))
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) { return nil, nil }
	h.s.enforceLimit = func(context.Context, string, int) ([]history.RemovedGroup, error) { return nil, nil }

	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	evs := waitForHistory(t, h.sub)

	term, ok := findEvent(evs, "error")
	if !ok {
		t.Fatal("no error terminal event for a (nil, nil) Run result")
	}
	d, ok := term.Data.(errorData)
	if !ok {
		t.Fatalf("error data = %T, want errorData", term.Data)
	}
	if !strings.Contains(d.Message, "转换任务未返回结果") {
		t.Fatalf("error message = %q, want 转换任务未返回结果", d.Message)
	}

	h.s.runner = newFakeRunner(successResult(t.TempDir()), nil)
	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("resubmit after nil-result error = %v, want nil", err)
	}
	waitForHistory(t, h.sub)
}

// TestService_IdleAtCanceledReceipt mirrors TestService_IdleAtDoneReceipt
// for the canceled terminal: at the instant a subscriber receives
// "canceled", markIdle has already run, so an immediate Submit from the
// receive goroutine can never hit the 409 window.
func TestService_IdleAtCanceledReceipt(t *testing.T) {
	r := newFakeRunner(nil, context.Canceled)
	r.block = true
	h := newServiceHarness(t, r)
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) { return nil, nil }
	h.s.enforceLimit = func(context.Context, string, int) ([]history.RemovedGroup, error) { return nil, nil }

	probe := h.b.subscribe()
	submitErr := make(chan error, 1)
	go func() {
		for e := range probe.ch {
			if e.Type != "canceled" {
				continue
			}
			h.s.mgr.mu.Lock()
			running := h.s.mgr.running
			h.s.mgr.mu.Unlock()
			if running {
				submitErr <- errors.New("mgr still running when canceled was delivered")
				return
			}
			// The still-blocked first fake cannot serve job 2; hand the
			// service a healthy runner before immediately resubmitting.
			h.s.runner = newFakeRunner(successResult(t.TempDir()), nil)
			submitErr <- h.s.Submit(testURL())
			return
		}
	}()

	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	mustRecv(t, h.sub, time.Second) // reset
	<-r.started
	h.s.Cancel()

	select {
	case err := <-submitErr:
		if err != nil {
			t.Fatalf("Submit immediately at canceled receipt = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe never observed canceled")
	}

	// Job 1 reached canceled and the immediately submitted job 2 done.
	var canceled, done int
	deadline := time.After(5 * time.Second)
	for canceled < 1 || done < 1 {
		select {
		case e := <-h.sub.ch:
			switch e.Type {
			case "canceled":
				canceled++
			case "done":
				done++
			}
		case <-deadline:
			t.Fatalf("missing terminal after canceled-resubmit: canceled=%d done=%d", canceled, done)
		}
	}
}

func TestService_HistoryFailureNonFatal(t *testing.T) {
	h := newServiceHarness(t, newFakeRunner(successResult(t.TempDir()), nil))
	h.s.enforceLimit = func(context.Context, string, int) ([]history.RemovedGroup, error) {
		return nil, errors.New("enforce boom")
	}
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) {
		return nil, errors.New("list boom")
	}

	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	evs := waitAfterDone(t, h.sub)

	if _, ok := findEvent(evs, "done"); !ok {
		t.Fatal("done must still be broadcast when history housekeeping fails")
	}
	var texts []string
	for _, e := range evs {
		if e.Type == "log" {
			texts = append(texts, e.Text)
		}
	}
	joined := strings.Join(texts, "|")
	if !strings.Contains(joined, "enforce boom") || !strings.Contains(joined, "list boom") {
		t.Fatalf("want warnings for both failures, got %v", texts)
	}
}

// waitAfterDone consumes events until done arrives followed by a quiet
// grace period. The history-failure flow ends with warnings rather than a
// history event, so callers wait on the grace window instead.
func waitAfterDone(t *testing.T, s *sub) []Event {
	t.Helper()
	var got []Event
	for {
		e := mustRecv(t, s, 5*time.Second)
		got = append(got, e)
		if e.Type != "done" {
			continue
		}
		// Drain trailing warnings; goroutine scheduling is prompt here.
		for {
			select {
			case e := <-s.ch:
				got = append(got, e)
			case <-time.After(200 * time.Millisecond):
				return got
			}
		}
	}
}

func TestService_LoggerInitFailureNonFatal(t *testing.T) {
	h := newServiceHarness(t, newFakeRunner(successResult(t.TempDir()), nil))
	h.s.loggerInit = func(logger.Options) error {
		return errors.New("init boom")
	}
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) { return nil, nil }
	h.s.enforceLimit = func(context.Context, string, int) ([]history.RemovedGroup, error) { return nil, nil }

	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	evs := waitForHistory(t, h.sub)

	if _, ok := findEvent(evs, "done"); !ok {
		t.Fatal("job must finish and broadcast done despite logger.Init failure")
	}
	var found bool
	for _, e := range evs {
		if e.Type == "log" && strings.Contains(e.Text, "日志初始化失败") &&
			strings.Contains(e.Text, "init boom") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing non-fatal logger init warning")
	}
}

func TestService_Shutdown(t *testing.T) {
	r := newFakeRunner(nil, nil)
	r.block = true
	h := newServiceHarness(t, r)

	if err := h.s.Submit(testURL()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-r.started

	shutdownDone := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.s.Shutdown(ctx)
		close(shutdownDone)
	}()

	// The running job observes cancellation and exits; Shutdown then
	// closes every broker subscription.
	drainUntilClosed(t, h.sub, 5*time.Second)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after the job exited")
	}

	if err := h.s.Submit(testURL()); !errors.Is(err, ErrServiceClosed) {
		t.Fatalf("Submit after Shutdown = %v, want ErrServiceClosed", err)
	}
	if h.b.subscribe() != nil {
		t.Fatal("subscribe after Shutdown must return nil")
	}
}

// stuckRunner blocks on release regardless of ctx cancellation, so a
// Shutdown genuinely hits its ctx deadline (bounded goroutine residue).
type stuckRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r *stuckRunner) Run(ctx context.Context, _ string) (*pipeline.Result, error) {
	close(r.started)
	<-r.release
	return nil, ctx.Err()
}

// shutdownWithDeadline invokes Shutdown with a short deadline in a
// goroutine and fails if it does not return promptly after the deadline.
func shutdownWithDeadline(t *testing.T, s *service, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Shutdown panicked: %v", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		s.Shutdown(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Shutdown blocked well past its %v deadline", d)
	}
}

// TestService_ShutdownIdempotent locks re-entrant Shutdown behaviour:
// repeated and concurrent calls must neither block nor panic (broker
// closeAll is idempotent), and every later Submit is ErrServiceClosed.
func TestService_ShutdownIdempotent(t *testing.T) {
	t.Run("second call after timed-out first call", func(t *testing.T) {
		r := &stuckRunner{started: make(chan struct{}), release: make(chan struct{})}
		h := newServiceHarness(t, r)
		if err := h.s.Submit(testURL()); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		<-r.started

		// First Shutdown: the stuck job ignores cancel, so the wait times
		// out; the broker is closed regardless.
		shutdownWithDeadline(t, h.s, 100*time.Millisecond)
		drainUntilClosed(t, h.sub, time.Second)

		// Second Shutdown with another short deadline must not block on
		// the residue and must not double-close anything.
		shutdownWithDeadline(t, h.s, 100*time.Millisecond)

		if err := h.s.Submit(testURL()); !errors.Is(err, ErrServiceClosed) {
			t.Fatalf("Submit after repeated Shutdown = %v, want ErrServiceClosed", err)
		}
		if h.b.subscribe() != nil {
			t.Fatal("subscribe after Shutdown must return nil")
		}

		// Unwind the bounded residue: snapshot the job's done channel,
		// release the runner, wait for markIdle so no background
		// goroutine outlives the test.
		h.s.mgr.mu.Lock()
		done := h.s.mgr.done
		h.s.mgr.mu.Unlock()
		close(r.release)
		if done != nil {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("residue job never marked idle after release")
			}
		}
	})

	t.Run("concurrent calls while idle", func(t *testing.T) {
		h := newServiceHarness(t, newFakeRunner(nil, nil))

		const n = 8
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				shutdownWithDeadline(t, h.s, 5*time.Second)
			}()
		}
		wg.Wait()

		drainUntilClosed(t, h.sub, time.Second)
		if err := h.s.Submit(testURL()); !errors.Is(err, ErrServiceClosed) {
			t.Fatalf("Submit after concurrent Shutdown = %v, want ErrServiceClosed", err)
		}
		if h.b.subscribe() != nil {
			t.Fatal("subscribe after concurrent Shutdown must return nil")
		}
	})
}

func TestService_RefreshHistory(t *testing.T) {
	h := newServiceHarness(t, newFakeRunner(nil, nil))
	entries := []history.Entry{
		{BVID: "BV10000000001", Name: "one"},
		{BVID: "BV10000000002", Name: "two"},
	}
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) {
		return entries, nil
	}
	h.s.refreshHistory(context.Background())

	e := mustRecv(t, h.sub, time.Second)
	if e.Type != "history" {
		t.Fatalf("refreshHistory event = %q, want history", e.Type)
	}
	if len(e.Data.([]history.Entry)) != 2 {
		t.Fatal("refreshHistory payload mismatch")
	}
}

func TestService_RefreshHistoryFailureWarns(t *testing.T) {
	h := newServiceHarness(t, newFakeRunner(nil, nil))
	h.s.historyList = func(context.Context, string) ([]history.Entry, error) {
		return nil, errors.New("scan boom")
	}
	h.s.refreshHistory(context.Background())

	e := mustRecv(t, h.sub, time.Second)
	if e.Type != "log" || e.Level != "warn" || !strings.Contains(e.Text, "scan boom") {
		t.Fatalf("refreshHistory failure event = %+v, want warn containing scan boom", e)
	}
}
