package webui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"

	"bilibili-txt/internal/biliurl"
	"bilibili-txt/internal/config"
	"bilibili-txt/internal/history"
	"bilibili-txt/internal/logger"
	"bilibili-txt/internal/pipeline"
)

// historyKeep is the number of newest BVID groups retained in the output
// directory after every successful run.
const historyKeep = 5

// ErrServiceBusy is returned by Submit while another job is running. The
// HTTP layer maps it to 409.
var ErrServiceBusy = errors.New("服务正忙：已有转换任务正在运行")

// ErrServiceClosed is returned by Submit after Shutdown has stopped the
// service from accepting new jobs. The HTTP layer maps it to 503.
var ErrServiceClosed = errors.New("服务已关闭，不再接受新任务")

// doneData is the JSON payload of a terminal "done" event.
type doneData struct {
	Path string `json:"path"`
	BVID string `json:"bvid"`
	Name string `json:"name"`
}

// errorData is the JSON payload of a terminal "error" event. The
// frontend reads the message from data.message.
type errorData struct {
	Message string `json:"message"`
}

type (
	historyListFunc  func(ctx context.Context, dir string) ([]history.Entry, error)
	enforceLimitFunc func(ctx context.Context, dir string, keep int) ([]history.RemovedGroup, error)
	loggerInitFunc   func(opts logger.Options) error
)

// service owns job orchestration without any HTTP dependency: URL
// validation, the single-job state machine, per-job logger installation,
// terminal classification and history broadcasting. The HTTP layer (Task
// 11) is a thin adapter in front of Submit / Cancel / Shutdown.
type service struct {
	cfg    *config.Config
	b      *broker
	runner Runner
	mgr    *jobManager

	acceptMu  sync.Mutex
	accepting bool

	// Indirected dependencies; tests replace them to avoid the real
	// filesystem / global-logger side effects. Defaults wire the
	// production packages.
	historyList  historyListFunc
	enforceLimit enforceLimitFunc
	loggerInit   loggerInitFunc
}

func newService(cfg *config.Config, b *broker, runner Runner) *service {
	return &service{
		cfg:          cfg,
		b:            b,
		runner:       runner,
		mgr:          &jobManager{},
		accepting:    true,
		historyList:  history.List,
		enforceLimit: history.EnforceLimit,
		loggerInit:   logger.Init,
	}
}

// Submit validates rawURL and, when idle, starts one conversion in the
// background. It returns immediately: biliurl's error on an
// unrecognised URL (HTTP 400), ErrServiceBusy while a job is running
// (HTTP 409), or ErrServiceClosed after Shutdown.
func (s *service) Submit(rawURL string) error {
	in, err := biliurl.Extract(rawURL)
	if err != nil {
		return err
	}

	// Hold acceptMu across the idle check and state flip so Shutdown
	// cannot interleave between "still accepting" and job start (which
	// would leak a job whose cancel call already happened).
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	if !s.accepting {
		return ErrServiceClosed
	}

	ctx, ok := s.mgr.startIfIdle()
	if !ok {
		return ErrServiceBusy
	}
	go s.runJob(ctx, in)
	return nil
}

// Cancel requests cancellation of the running job, if any. Nil-safe when
// idle.
func (s *service) Cancel() {
	s.mgr.cancelRunning()
}

// Shutdown rejects new submissions, cancels any running job, waits for it
// to exit (bounded by ctx) and closes every broker subscription. It is
// safe to call when idle, and safe to call more than once (including
// concurrently): only the first call does work.
//
// Bounded wait semantics: if ctx expires before the job goroutine exits,
// Shutdown stops waiting and proceeds to close the broker anyway. The job
// goroutine is NOT forcibly killed — a bounded goroutine residue is
// accepted — but its context has already been canceled, so the real
// pipeline's child processes are terminated and the goroutine exits once
// the pipeline unwinds. Any terminal broadcast arriving after that point
// is silently dropped by the closed broker (publish on a closed broker is
// a no-op). Callers should therefore grant a generous ctx when a clean
// exit matters.
func (s *service) Shutdown(ctx context.Context) {
	s.acceptMu.Lock()
	s.accepting = false
	s.acceptMu.Unlock()

	s.mgr.cancelRunning()

	exited := make(chan struct{})
	go func() {
		s.mgr.waitIdle()
		close(exited)
	}()
	select {
	case <-exited:
	case <-ctx.Done():
	}

	s.b.closeAll()
}

// refreshHistory scans the output directory and republishes the history
// snapshot; called once at server startup (Task 11) and at the end of
// every job. A scan failure only produces a warning.
func (s *service) refreshHistory(ctx context.Context) {
	entries, err := s.historyList(ctx, s.cfg.OutputDir)
	if err != nil {
		s.b.Log("warn", "读取历史记录失败: "+err.Error())
		return
	}
	s.b.History(entries)
}

// runJob is the background job goroutine. The event ordering below is
// frozen by docs/webui-plan Task 10 and must not be reordered:
//
//  1. broker Reset (clears the previous task);
//  2. logger.Init with the UI slog handler and external-line writer;
//  3. runner.Run (the production runner builds its deps only now, after
//     Init, so subprocess output reaches the UI);
//  4. terminal classification: history housekeeping (success only),
//     THEN markIdle, THEN the terminal broadcast, so a client reacting
//     to the terminal event can immediately submit again.
func (s *service) runJob(ctx context.Context, in biliurl.Input) {
	marked := false
	markIdle := func() {
		if marked {
			return
		}
		marked = true
		s.mgr.markIdle()
	}

	// Panic guard: convert a crash into a terminal error event instead
	// of killing the process. markIdle still happens before the
	// broadcast; the normal path below calls markIdle explicitly.
	defer func() {
		if r := recover(); r != nil {
			markIdle()
			// The UI only shows the short message; the full stack goes
			// to the server process's stderr for postmortem debugging.
			// This goroutine's stderr is fixed at os.Stderr; tests do
			// not assert on the stderr output.
			fmt.Fprintf(os.Stderr, "webui: recovered task panic: %v\n%s\n", r, debug.Stack())
			s.b.Error(errorData{Message: fmt.Sprintf("内部错误: %v", r)})
		}
	}()

	s.b.Reset()

	uiHandler := newUIHandler(s.b)
	lineWriter := newLineWriter(s.b)
	if err := s.loggerInit(logger.Options{
		Handler:        uiHandler,
		ExternalWriter: lineWriter,
		Format:         "text",
		Level:          s.cfg.Logging.Level,
		Stderr:         os.Stderr,
	}); err != nil {
		// Non-fatal: the broker does not go through slog and terminal
		// events still flow. Only the live log/phase panels suffer.
		s.b.Log("warn", "日志初始化失败，界面日志可能不完整: "+err.Error())
	}

	result, err := s.runner.Run(ctx, in.URL)
	switch {
	case err == nil:
		s.finishSuccess(ctx, in, result, markIdle)
	case errors.Is(err, context.Canceled):
		s.finishCanceled(markIdle)
	default:
		s.finishError(err, markIdle)
	}
}

// finishSuccess runs history housekeeping while the job is still marked
// running (its ctx is not canceled on the success path), then flips
// idle BEFORE the terminal event and broadcasts done -> cleanup warnings
// -> history in that order.
func (s *service) finishSuccess(ctx context.Context, in biliurl.Input, result *pipeline.Result, markIdle func()) {
	if result == nil {
		s.finishError(errors.New("转换任务未返回结果"), markIdle)
		return
	}

	removed, enforceErr := s.enforceLimit(ctx, s.cfg.OutputDir, historyKeep)
	entries, listErr := s.historyList(ctx, s.cfg.OutputDir)

	markIdle()

	bvid := resultBVID(result, in)
	s.b.Done(doneData{
		Path: result.OutputPath,
		BVID: bvid,
		Name: displayName(result.OutputPath),
	})

	if enforceErr != nil {
		s.b.Log("warn", "清理旧视频失败: "+enforceErr.Error())
	}
	for _, g := range removed {
		s.b.Log("warn", "已自动清理旧视频 "+g.BVID)
	}

	if listErr != nil {
		s.b.Log("warn", "读取历史记录失败: "+listErr.Error())
		return
	}
	s.b.History(entries)
}

// finishCanceled flips idle before broadcasting: canceled -> 已取消
// warning -> refreshed history. The history scan derives a fresh
// background context because the job ctx is already canceled.
func (s *service) finishCanceled(markIdle func()) {
	markIdle()
	s.b.Canceled()
	s.b.Log("warn", "已取消")
	s.refreshHistory(context.Background())
}

// finishError flips idle before broadcasting: error -> refreshed history,
// scanned with a fresh background context.
func (s *service) finishError(err error, markIdle func()) {
	markIdle()
	s.b.Error(errorData{Message: err.Error()})
	s.refreshHistory(context.Background())
}

// resultBVID prefers the metadata's BVID and falls back to the BVID
// extracted from the submitted URL (short links leave it empty).
func resultBVID(result *pipeline.Result, in biliurl.Input) string {
	if result.Metadata != nil && strings.TrimSpace(result.Metadata.BVID) != "" {
		return result.Metadata.BVID
	}
	return in.BVID
}

// displayName is the output file's basename without its extension.
func displayName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}
