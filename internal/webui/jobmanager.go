package webui

import (
	"context"
	"sync"
)

// jobManager is the single-job state machine: at most one conversion may
// run at a time. All state transitions happen under mu, so the busy
// check in startIfIdle and the running-flag flip are one atomic
// critical section — there is no lock-free TOCTOU window in which two
// Submit calls could both observe "idle".
type jobManager struct {
	mu      sync.Mutex
	running bool
	// cancel is the running job's cancellation function, nil when idle.
	cancel context.CancelFunc
	// done is closed once the running job has marked itself idle; it is
	// nil while idle. waitIdle snapshots it under mu and waits on the
	// snapshot outside the lock.
	done chan struct{}
}

// startIfIdle transitions idle -> running and returns the job context
// (canceled by cancelRunning) and true. When a job is already running it
// returns (nil, false) and changes nothing.
func (m *jobManager) startIfIdle() (context.Context, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.running = true
	m.cancel = cancel
	m.done = make(chan struct{})
	return ctx, true
}

// markIdle transitions running -> idle: running becomes false, the
// cancel function is dropped and the current done channel is closed,
// unblocking waitIdle.
//
// Contract: the job goroutine that won startIfIdle owns this call and
// MUST invoke it BEFORE broadcasting any terminal event (done / error /
// canceled), so a client reacting to the terminal event by submitting a
// new job can never hit the busy window. It is idempotent: a deferred
// panic guard may call it again after the normal-path call, and the
// duplicate is a no-op. It must never be called by a goroutine that does
// not own the current job.
func (m *jobManager) markIdle() {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	m.running = false
	m.cancel = nil
	done := m.done
	m.done = nil
	m.mu.Unlock()

	close(done)
}

// cancelRunning cancels the running job, if any. It is nil-safe and may
// be called concurrently with the job's own exit: it only invokes the
// stored context.CancelFunc and never touches the running/done state,
// which the owning job goroutine retires via markIdle.
func (m *jobManager) cancelRunning() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// waitIdle blocks until no job is running. It is used during shutdown
// after new submissions have been rejected, so the channel snapshot
// taken here can only transition to closed, never be replaced by a
// newer job's channel.
func (m *jobManager) waitIdle() {
	m.mu.Lock()
	done := m.done
	m.mu.Unlock()
	if done != nil {
		<-done
	}
}
