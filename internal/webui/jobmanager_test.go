package webui

import (
	"sync"
	"testing"
	"time"
)

func TestJobManager_StartBusyMarkAndRestart(t *testing.T) {
	m := &jobManager{}

	ctx, ok := m.startIfIdle()
	if !ok || ctx == nil {
		t.Fatal("first startIfIdle = false/nil, want true with a context")
	}
	if _, ok := m.startIfIdle(); ok {
		t.Fatal("second startIfIdle while running = true, want busy (false)")
	}

	// cancelRunning cancels the job context.
	m.cancelRunning()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("job context not canceled after cancelRunning")
	}

	// Cancellation alone does not flip the machine to idle: the owning
	// goroutine must still markIdle after terminal classification.
	if _, ok := m.startIfIdle(); ok {
		t.Fatal("startIfIdle succeeded before markIdle, want still busy")
	}

	m.markIdle()
	ctx2, ok := m.startIfIdle()
	if !ok || ctx2 == nil {
		t.Fatal("startIfIdle after markIdle = false/nil, want reusable")
	}
	m.markIdle()
}

func TestJobManager_MarkIdleIdempotent(t *testing.T) {
	m := &jobManager{}
	ctx, ok := m.startIfIdle()
	if !ok {
		t.Fatal("first startIfIdle failed")
	}
	m.markIdle()
	m.markIdle() // deferred panic guard may call it again
	m.markIdle()

	// Exactly one close of the done channel happened (no panic); the
	// machine is idle and restartable.
	if _, ok := m.startIfIdle(); !ok {
		t.Fatal("startIfIdle after repeated markIdle failed")
	}
	_ = ctx
	m.markIdle()
}

func TestJobManager_CancelRunningNilSafeWhenIdle(t *testing.T) {
	m := &jobManager{}
	m.cancelRunning() // must not panic with no active job
	m.markIdle()      // defensive call while idle must not panic either
}

func TestJobManager_WaitIdle(t *testing.T) {
	t.Run("idle returns immediately", func(t *testing.T) {
		m := &jobManager{}
		done := make(chan struct{})
		go func() { m.waitIdle(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("waitIdle blocked while idle")
		}
	})

	t.Run("waits for running job then returns", func(t *testing.T) {
		m := &jobManager{}
		if _, ok := m.startIfIdle(); !ok {
			t.Fatal("startIfIdle failed")
		}

		// White-box: snapshot the exact done channel that waitIdle
		// blocks on, so the ordering under test is pinned structurally
		// (the channel only ever closes inside markIdle) rather than
		// inferred purely from a wall-clock wait.
		m.mu.Lock()
		done := m.done
		m.mu.Unlock()
		if done == nil {
			t.Fatal("done channel is nil while a job is running")
		}

		returned := make(chan struct{})
		go func() { m.waitIdle(); close(returned) }()

		// Weak liveness probe only: since nothing in this test calls
		// markIdle yet, done provably cannot close, so these short waits
		// merely give a faulty/early-returning waitIdle a chance to
		// expose itself. They are NOT the assertion under test and a
		// slow/loaded machine cannot make them fail spuriously (the
		// checked events simply never happen). The strong assertion is
		// the positive ordering immediately below.
		for i := 0; i < 2; i++ {
			select {
			case <-done:
				t.Fatal("done channel closed before markIdle")
			case <-returned:
				t.Fatal("waitIdle returned before markIdle")
			case <-time.After(20 * time.Millisecond):
			}
		}

		// Strong positive assertion: once markIdle runs, waitIdle must
		// observe the closed done channel and return promptly (1s bound),
		// and the white-box channel must itself be closed.
		m.markIdle()
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Fatal("waitIdle did not return within 1s after markIdle")
		}
		select {
		case <-done:
		default:
			t.Fatal("done channel still open after markIdle")
		}
	})
}

// TestJobManager_ConcurrentStartExactlyOne is the -race guarantee: 100
// goroutines racing startIfIdle must yield exactly one winner, and the
// busy check plus the running-flag flip must share one critical section.
func TestJobManager_ConcurrentStartExactlyOne(t *testing.T) {
	const n = 100
	m := &jobManager{}
	var winners int64
	var wg sync.WaitGroup
	var startMu sync.Mutex
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := m.startIfIdle(); ok {
				startMu.Lock()
				winners++
				startMu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners != 1 {
		t.Fatalf("startIfIdle winners = %d, want exactly 1", winners)
	}
	m.markIdle()
}

func TestJobManager_CancelConcurrentWithExit(t *testing.T) {
	m := &jobManager{}
	ctx, ok := m.startIfIdle()
	if !ok {
		t.Fatal("startIfIdle failed")
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); m.cancelRunning() }()
		go func() { defer wg.Done(); m.markIdle() }()
	}
	wg.Wait()
	if _, ok := m.startIfIdle(); !ok {
		t.Fatal("machine not idle after concurrent cancel/mark storm")
	}
	_ = ctx
	m.markIdle()
}
