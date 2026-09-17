package webui

import (
	"testing"
	"time"
)

func newTestPresence(grace time.Duration) (*presence, chan struct{}) {
	fired := make(chan struct{}, 4)
	p := newPresence(grace, func() { fired <- struct{}{} })
	return p, fired
}

func waitFired(t *testing.T, ch chan struct{}, timeout time.Duration) bool {
	t.Helper()
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

func assertNotFired(t *testing.T, ch chan struct{}, wait time.Duration) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("onAbsent fired, want it suppressed")
	case <-time.After(wait):
	}
}

func TestPresence_FiresAfterGraceWhenLastClientLeaves(t *testing.T) {
	p, fired := newTestPresence(20 * time.Millisecond)
	p.connect()
	p.disconnect()
	if !waitFired(t, fired, time.Second) {
		t.Fatal("onAbsent did not fire after the only client disconnected")
	}
}

func TestPresence_ReconnectWithinGraceSuppressesQuit(t *testing.T) {
	p, fired := newTestPresence(80 * time.Millisecond)
	p.connect()
	p.disconnect()
	time.Sleep(30 * time.Millisecond)
	p.connect()
	assertNotFired(t, fired, 250*time.Millisecond)

	// Leaving again after the reconnect rearms a fresh grace window.
	p.disconnect()
	if !waitFired(t, fired, time.Second) {
		t.Fatal("onAbsent did not re-fire after leaving following a reconnect")
	}
}

func TestPresence_MultipleClients(t *testing.T) {
	p, fired := newTestPresence(20 * time.Millisecond)
	p.connect()
	p.connect()
	p.disconnect()
	assertNotFired(t, fired, 120*time.Millisecond)
	p.disconnect()
	if !waitFired(t, fired, time.Second) {
		t.Fatal("onAbsent did not fire once every client left")
	}
}

func TestPresence_StopCancelsPendingQuit(t *testing.T) {
	p, fired := newTestPresence(10 * time.Minute)
	p.connect()
	p.disconnect()
	p.stop()
	assertNotFired(t, fired, 100*time.Millisecond)
}

func TestPresence_NeverConnectedDoesNotFire(t *testing.T) {
	p, fired := newTestPresence(10 * time.Millisecond)
	p.stop()
	assertNotFired(t, fired, 60*time.Millisecond)
}
