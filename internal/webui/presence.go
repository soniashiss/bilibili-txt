package webui

import (
	"sync"
	"time"
)

// presence tracks the number of live UI clients (SSE connections). When
// the last client disappears and none reconnects within grace, onAbsent is
// invoked once, which lets Run shut the server down when its window is
// closed. The grace window absorbs page reloads and EventSource's native
// reconnect (the connection drops briefly before it re-opens). onAbsent is
// only eligible after the first connect, so a teardown that precedes any
// client cannot start the quit timer.
type presence struct {
	mu       sync.Mutex
	grace    time.Duration
	onAbsent func()
	n        int
	seen     bool
	timer    *time.Timer
	stopped  bool
}

func newPresence(grace time.Duration, onAbsent func()) *presence {
	return &presence{grace: grace, onAbsent: onAbsent}
}

// connect records a new live client and cancels any pending quit.
func (p *presence) connect() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = true
	p.n++
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

// disconnect records a client leaving and, when none remain, starts the
// grace window after which onAbsent fires unless a client reconnects.
func (p *presence) disconnect() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n > 0 {
		p.n--
	}
	if !p.seen || p.n != 0 || p.stopped || p.timer != nil {
		return
	}
	p.timer = time.AfterFunc(p.grace, p.fire)
}

func (p *presence) fire() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.n != 0 {
		return
	}
	p.timer = nil
	if p.onAbsent != nil {
		p.onAbsent()
	}
}

// stop cancels a pending quit and disarms the tracker permanently; used on
// shutdown so a late connection teardown cannot re-trigger onAbsent.
func (p *presence) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}
