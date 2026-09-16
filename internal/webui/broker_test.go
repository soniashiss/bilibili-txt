package webui

import (
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func recvEvent(t *testing.T, s *sub, timeout time.Duration) (Event, bool) {
	t.Helper()
	select {
	case e, ok := <-s.ch:
		return e, ok
	case <-time.After(timeout):
		return Event{}, false
	}
}

func mustRecv(t *testing.T, s *sub, timeout time.Duration) Event {
	t.Helper()
	e, ok := recvEvent(t, s, timeout)
	if !ok {
		t.Fatalf("expected event on subscriber within %s", timeout)
	}
	return e
}

func assertNoEvent(t *testing.T, s *sub, wait time.Duration) {
	t.Helper()
	if e, ok := recvEvent(t, s, wait); ok {
		t.Fatalf("unexpected event %+v", e)
	}
}

func drain(s *sub, idle time.Duration) []Event {
	var out []Event
	for {
		select {
		case e, ok := <-s.ch:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-time.After(idle):
			return out
		}
	}
}

func findEvent(events []Event, typ string) (Event, bool) {
	for _, e := range events {
		if e.Type == typ {
			return e, true
		}
	}
	return Event{}, false
}

func TestBrokerReplay(t *testing.T) {
	b := newBrokerWithCap(256, 50000)
	t.Cleanup(b.closeAll)

	b.Log("info", "a")
	b.Log("info", "b")

	s1 := b.subscribe()
	if e := mustRecv(t, s1, time.Second); e.Text != "a" {
		t.Fatalf("replay[0] = %q, want a", e.Text)
	}
	if e := mustRecv(t, s1, time.Second); e.Text != "b" {
		t.Fatalf("replay[1] = %q, want b", e.Text)
	}

	b.Log("info", "c")
	if e := mustRecv(t, s1, time.Second); e.Text != "c" {
		t.Fatalf("live = %q, want c", e.Text)
	}

	s2 := b.subscribe()
	got := drain(s2, 50*time.Millisecond)
	if len(got) != 3 || got[0].Text != "a" || got[1].Text != "b" || got[2].Text != "c" {
		t.Fatalf("late subscriber replay = %v, want a b c", got)
	}

	b.Done(map[string]any{"ok": true})
	if e := mustRecv(t, s1, time.Second); e.Type != "done" {
		t.Fatalf("s1 done = %+v", e)
	} else if m, ok := e.Data.(map[string]any); !ok || m["ok"] != true {
		t.Fatalf("s1 done data = %#v", e.Data)
	}
	if e := mustRecv(t, s2, time.Second); e.Type != "done" {
		t.Fatalf("s2 done = %+v", e)
	}

	s3 := b.subscribe()
	got = drain(s3, 50*time.Millisecond)
	var doneCount int
	for _, e := range got {
		if e.Type == "done" {
			doneCount++
		}
	}
	if doneCount != 1 {
		t.Fatalf("done delivered %d times to late subscriber, events = %v", doneCount, got)
	}
	last := got[len(got)-1]
	if last.Type != "done" {
		t.Fatalf("last replayed event = %q, want done (terminal snapshot)", last.Type)
	}
	if texts := []string{got[0].Text, got[1].Text, got[2].Text}; texts[0] != "a" || texts[1] != "b" || texts[2] != "c" {
		t.Fatalf("replay prefix = %v, want a b c", texts)
	}
}

func TestBrokerReset(t *testing.T) {
	b := newBrokerWithCap(256, 50000)
	t.Cleanup(b.closeAll)

	b.Log("info", "before")
	s1 := b.subscribe()
	if e := mustRecv(t, s1, time.Second); e.Text != "before" {
		t.Fatalf("replay = %q, want before", e.Text)
	}

	b.Reset()
	if e := mustRecv(t, s1, time.Second); e.Type != "reset" {
		t.Fatalf("live reset = %+v", e)
	}

	s2 := b.subscribe()
	assertNoEvent(t, s2, 50*time.Millisecond)

	b.Log("info", "after")
	if e := mustRecv(t, s1, time.Second); e.Text != "after" {
		t.Fatalf("s1 post-reset = %q, want after", e.Text)
	}
	if e := mustRecv(t, s2, time.Second); e.Text != "after" {
		t.Fatalf("s2 post-reset = %q, want after", e.Text)
	}

	s3 := b.subscribe()
	got := drain(s3, 50*time.Millisecond)
	if len(got) != 1 || got[0].Text != "after" {
		t.Fatalf("late subscriber after reset = %v, want only [after]", got)
	}
}

func TestBrokerTerminalSnapshot(t *testing.T) {
	b := newBrokerWithCap(256, 50000)
	t.Cleanup(b.closeAll)

	b.Phase("download", "start")
	b.Done("first")
	b.Error("boom")

	s := b.subscribe()
	got := drain(s, 50*time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("replay = %v, want [phase error]", got)
	}
	if got[0].Type != "phase" || got[0].Step != "download" || got[0].Status != "start" {
		t.Fatalf("replay[0] = %+v, want phase download/start", got[0])
	}
	if got[1].Type != "error" || got[1].Data != "boom" {
		t.Fatalf("replay[1] = %+v, want latest terminal error/boom", got[1])
	}

	b2 := newBrokerWithCap(256, 50000)
	t.Cleanup(b2.closeAll)
	b2.Done("done-payload")
	b2.Canceled()
	s2 := b2.subscribe()
	got = drain(s2, 50*time.Millisecond)
	if len(got) != 1 || got[0].Type != "canceled" {
		t.Fatalf("terminal override replay = %v, want only [canceled]", got)
	}
}

func TestBrokerPhaseSnapshotSurvivesTruncation(t *testing.T) {
	b := newBrokerWithCap(256, 2)
	t.Cleanup(b.closeAll)

	b.Phase("p1", "start")
	b.Log("info", "x")
	b.Log("info", "y")
	b.Log("info", "z")

	s := b.subscribe()
	got := drain(s, 50*time.Millisecond)

	var phases []Event
	var texts []string
	var notices int
	for _, e := range got {
		switch {
		case e.Type == "phase":
			phases = append(phases, e)
		case e.Type == "log" && e.Text == truncationNotice:
			notices++
		case e.Type == "log":
			texts = append(texts, e.Text)
		}
	}
	if len(phases) != 1 || phases[0].Step != "p1" {
		t.Fatalf("phase snapshot = %+v, want exactly one p1", phases)
	}
	if notices != 1 {
		t.Fatalf("truncation notices = %d, want 1; events = %v", notices, got)
	}
	if len(texts) != 1 || texts[0] != "z" {
		t.Fatalf("surviving buffer = %v, want only newest real event [z] (cap=2 => notice+1)", texts)
	}
	if got[0].Type != "log" || got[0].Text != truncationNotice {
		t.Fatalf("replay must open with the truncation notice, got %+v", got[0])
	}
	if got[len(got)-1].Type != "phase" {
		t.Fatalf("state snapshots must follow the buffered events, got %+v", got[len(got)-1])
	}
}

func TestBrokerSlowSubscriberDrops(t *testing.T) {
	b := newBrokerWithCap(2, 50000)
	t.Cleanup(b.closeAll)

	s := b.subscribe()
	assertNoEvent(t, s, 20*time.Millisecond)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			b.Log("info", fmt.Sprintf("e%d", i))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publishing 10 events to a non-reading subscriber blocked")
	}

	got := drain(s, 50*time.Millisecond)
	if len(got) != 2 || got[0].Text != "e0" || got[1].Text != "e1" {
		t.Fatalf("queued events = %v, want e0 e1", got)
	}
	for _, e := range got {
		if e.Dropped != 0 {
			t.Fatalf("queued event unexpectedly carries dropped count: %+v", e)
		}
	}

	b.Log("info", "marker")
	e := mustRecv(t, s, time.Second)
	if e.Text != "marker" || e.Dropped < 1 {
		t.Fatalf("marker = %+v, want Dropped>=1", e)
	}
	if e.Dropped != 8 {
		t.Fatalf("Dropped = %d, want 8", e.Dropped)
	}

	b.Log("info", "clean")
	e = mustRecv(t, s, time.Second)
	if e.Text != "clean" || e.Dropped != 0 {
		t.Fatalf("post-marker event = %+v, want Dropped==0", e)
	}
}

func TestBrokerBufferTruncation(t *testing.T) {
	b := newBrokerWithCap(256, 5)
	t.Cleanup(b.closeAll)

	live := b.subscribe()

	for i := 0; i < 20; i++ {
		b.Log("info", fmt.Sprintf("L%02d", i))
	}

	// The pre-truncation subscriber is subscribed before the burst: with
	// subCap=256 all 20 logs plus the one live notice fit, so it must
	// receive the truncation notice in real time too.
	liveEvents := drain(live, 50*time.Millisecond)
	liveNotices := 0
	for _, ev := range liveEvents {
		if ev.Type == "log" && ev.Text == truncationNotice {
			liveNotices++
		}
	}
	if liveNotices != 1 {
		t.Fatalf("live subscriber truncation notices = %d, want exactly 1", liveNotices)
	}
	if len(liveEvents) != 21 {
		t.Fatalf("live subscriber received %d events, want 20 logs + 1 notice", len(liveEvents))
	}

	s := b.subscribe()
	got := drain(s, 50*time.Millisecond)
	if len(got) > 5 {
		t.Fatalf("replayed %d events, want at most notice(1)+cap-1(4)", len(got))
	}

	var texts []string
	var notices int
	for _, ev := range got {
		if ev.Type == "log" && ev.Text == truncationNotice {
			notices++
		} else if ev.Type == "log" {
			texts = append(texts, ev.Text)
		}
	}
	if notices != 1 {
		t.Fatalf("truncation notices = %d, want 1; events = %v", notices, got)
	}
	want := []string{"L16", "L17", "L18", "L19"}
	if len(texts) != 4 {
		t.Fatalf("surviving events = %v, want %v", texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Fatalf("surviving events = %v, want %v", texts, want)
		}
	}
}

func TestBrokerCloseAll(t *testing.T) {
	b := newBrokerWithCap(8, 16)

	s := b.subscribe()
	b.Log("info", "x")
	if e := mustRecv(t, s, time.Second); e.Text != "x" {
		t.Fatalf("live = %q, want x", e.Text)
	}

	b.closeAll()
	if _, ok := <-s.ch; ok {
		t.Fatal("subscriber channel not closed after closeAll")
	}
	b.closeAll() // idempotent
	b.Log("info", "after-close")
	b.Reset()
	if ns := b.subscribe(); ns != nil {
		t.Fatalf("subscribe after closeAll = %v, want nil", ns)
	}
	b.unsubscribe(nil)

	b2 := newBrokerWithCap(8, 16)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				b2.Log("info", "x")
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				ns := b2.subscribe()
				if ns != nil {
					b2.unsubscribe(ns)
				}
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	b2.closeAll()
	wg.Wait()
}

func TestBrokerUnsubscribeCloses(t *testing.T) {
	b := newBrokerWithCap(256, 50000)
	t.Cleanup(b.closeAll)

	s := b.subscribe()
	b.unsubscribe(s)
	if _, ok := <-s.ch; ok {
		t.Fatal("subscriber channel not closed after unsubscribe")
	}
	b.unsubscribe(s) // idempotent
}

func TestBrokerConcurrent(t *testing.T) {
	b := newBrokerWithCap(64, 50000)

	const numSubs = 3
	recved := make([][]int64, numSubs)
	var readerWg sync.WaitGroup
	for i := 0; i < numSubs; i++ {
		s := b.subscribe()
		readerWg.Add(1)
		go func(idx int, sub *sub) {
			defer readerWg.Done()
			for ev := range sub.ch {
				if ev.Type == "log" {
					var n int64
					if _, err := fmt.Sscanf(ev.Text, "seq-%d", &n); err == nil {
						recved[idx] = append(recved[idx], n)
					}
				}
			}
		}(i, s)
	}

	var seq atomic.Int64
	var wg sync.WaitGroup

	// Single ordered publisher: its publish order is the global sequence order.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			b.Log("info", fmt.Sprintf("seq-%d", seq.Add(1)))
		}
	}()
	// Noise publishers and resetters must never corrupt ordering.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				b.Log("info", "noise")
			}
		}()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Millisecond)
			b.Reset()
		}()
	}

	wg.Wait()
	b.closeAll()
	readerWg.Wait()

	for i, list := range recved {
		for j := 1; j < len(list); j++ {
			if list[j] <= list[j-1] {
				t.Fatalf("subscriber %d out of order at %d: %d after %d (full=%v)",
					i, j, list[j], list[j-1], list)
			}
		}
	}
}

func TestBrokerStartupEvents(t *testing.T) {
	b := newBrokerWithCap(256, 50000)
	t.Cleanup(b.closeAll)

	b.Log("warn", "startup-notice")
	b.History([]string{"h1", "h2"})

	s := b.subscribe()
	got := drain(s, 50*time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("startup replay = %v, want [log history]", got)
	}
	if got[0].Type != "log" || got[0].Text != "startup-notice" {
		t.Fatalf("startup replay[0] = %+v", got[0])
	}
	if got[1].Type != "history" {
		t.Fatalf("startup replay[1] = %+v, want history", got[1])
	}

	b.Reset()
	if e := mustRecv(t, s, time.Second); e.Type != "reset" {
		t.Fatalf("reset = %+v", e)
	}

	s2 := b.subscribe()
	assertNoEvent(t, s2, 50*time.Millisecond)

	b.Log("info", "post-reset")
	if e := mustRecv(t, s2, time.Second); e.Text != "post-reset" {
		t.Fatalf("post-reset event = %+v", e)
	}
	s3 := b.subscribe()
	got = drain(s3, 50*time.Millisecond)
	if len(got) != 1 || got[0].Text != "post-reset" {
		t.Fatalf("late subscriber = %v, want only [post-reset]", got)
	}
}

var hhmmss = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}$`)

func TestBrokerLargeBacklogReplayNoDrops(t *testing.T) {
	// Tiny live capacity with a replay backlog hundreds of times larger:
	// the late subscriber must still receive EVERY buffered event plus the
	// terminal snapshot, without replay drops or blocking.
	b := newBrokerWithCap(2, 1000)
	t.Cleanup(b.closeAll)

	const n = 600
	for i := 0; i < n; i++ {
		b.Log("info", fmt.Sprintf("B%04d", i))
	}
	b.Done("finished")

	s := b.subscribe()
	for i := 0; i < n; i++ {
		e := mustRecv(t, s, 2*time.Second)
		if e.Type != "log" || e.Text != fmt.Sprintf("B%04d", i) {
			t.Fatalf("replay[%d] = %+v, want log B%04d", i, e, i)
		}
		if e.Dropped != 0 {
			t.Fatalf("replay[%d] unexpectedly reports %d dropped", i, e.Dropped)
		}
	}
	if e := mustRecv(t, s, time.Second); e.Type != "done" || e.Data != "finished" {
		t.Fatalf("terminal snapshot = %+v", e)
	}
	assertNoEvent(t, s, 30*time.Millisecond)

	// Live headroom after replay: subCap fresh events still fit before the
	// handler starts draining.
	b.Log("info", "live1")
	b.Log("info", "live2")
	if e := mustRecv(t, s, time.Second); e.Text != "live1" || e.Dropped != 0 {
		t.Fatalf("live1 = %+v", e)
	}
	if e := mustRecv(t, s, time.Second); e.Text != "live2" || e.Dropped != 0 {
		t.Fatalf("live2 = %+v", e)
	}
}

func TestBrokerLogTimestamp(t *testing.T) {
	b := newBrokerWithCap(256, 50000)
	t.Cleanup(b.closeAll)

	b.Log("info", "timed")
	s := b.subscribe()
	e := mustRecv(t, s, time.Second)
	if !hhmmss.MatchString(e.Time) {
		t.Fatalf("log Time = %q, want HH:MM:SS", e.Time)
	}

	b.Phase("p", "start")
	e = mustRecv(t, s, time.Second)
	if e.Time != "" {
		t.Fatalf("phase Time = %q, want empty", e.Time)
	}
}

func TestBufferTruncationFirstOverflow(t *testing.T) {
	b := newBrokerWithCap(256, 5)
	t.Cleanup(b.closeAll)

	// L00..L04 fill the capacity-5 buffer exactly; L05 is the cap+1-th
	// event that arms truncation.
	for i := 0; i < 6; i++ {
		b.Log("info", fmt.Sprintf("L%02d", i))
	}

	s := b.subscribe()
	got := drain(s, 50*time.Millisecond)

	// Window derivation at the first overflow:
	//	1. ordinary drop-oldest enqueue of L05: [L01,L02,L03,L04,L05]
	//	   (L00 falls out);
	//	2. pinning the one-shot notice at the head evicts the head once
	//	   more (L01), giving exactly capacity entries:
	//	   [notice,L02,L03,L04,L05].
	// The arming transition must NOT throw away L02..L04 the way a full
	// buffer reset would.
	want := []string{truncationNotice, "L02", "L03", "L04", "L05"}
	if len(got) != len(want) {
		t.Fatalf("first-overflow replay = %d events %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i].Type != "log" || got[i].Text != w {
			t.Fatalf("replay[%d] = %+v, want log %q; full replay = %v", i, got[i], w, got)
		}
	}
	if got[0].Level != "warn" {
		t.Fatalf("notice level = %q, want warn", got[0].Level)
	}
	for _, banned := range []string{"L00", "L01"} {
		for _, e := range got {
			if e.Text == banned {
				t.Fatalf("replay unexpectedly contains %s: %v", banned, got)
			}
		}
	}
}

func TestTruncationArmingPhaseAtEvictedHead(t *testing.T) {
	// Locks the bufStartSeq choice at arming: bufStartSeq must equal the
	// seq of the oldest RETAINED event, not of the arriving event. Here
	// the phase is the second-oldest event (position old[1]); the notice
	// pinning evicts it, so it is NOT in the post-arm buffer and its
	// snapshot must still be replayed exactly once.
	b := newBrokerWithCap(256, 5)
	t.Cleanup(b.closeAll)

	b.Log("info", "L00")
	b.Phase("p1", "start") // seq 2, position old[1]
	b.Log("info", "L02")
	b.Log("info", "L03")
	b.Log("info", "L04")
	b.Log("info", "L05") // arms truncation

	s := b.subscribe()
	got := drain(s, 50*time.Millisecond)
	var phases []Event
	for _, e := range got {
		if e.Type == "phase" {
			phases = append(phases, e)
		}
	}
	if len(phases) != 1 || phases[0].Step != "p1" {
		t.Fatalf("evicted-head phase snapshot = %+v (events %v), want exactly one p1", phases, got)
	}
}

func TestBrokerResetClearsTerminalSnapshot(t *testing.T) {
	b := newBrokerWithCap(8, 16)
	t.Cleanup(b.closeAll)
	b.Log("info", "before")
	b.Done("result")
	b.Reset()

	s := b.subscribe()
	assertNoEvent(t, s, 50*time.Millisecond)

	b.Done("again")
	b.Reset()
	s2 := b.subscribe()
	assertNoEvent(t, s2, 50*time.Millisecond)

	b2 := newBrokerWithCap(8, 16)
	t.Cleanup(b2.closeAll)
	b2.Canceled()
	late := b2.subscribe()
	got := drain(late, 50*time.Millisecond)
	if len(got) != 1 || got[0].Type != "canceled" {
		t.Fatalf("late subscriber before reset = %v, want exactly one [canceled]", got)
	}
}

func TestBrokerBufferCapOne(t *testing.T) {
	b := newBrokerWithCap(8, 1)
	t.Cleanup(b.closeAll)

	for i := 0; i < 4; i++ {
		b.Log("info", fmt.Sprintf("L%02d", i))
	}
	b.Phase("p1", "start")

	s := b.subscribe()
	got := drain(s, 50*time.Millisecond)

	// cap=1 semantics: the lone slot can only hold the pinned notice;
	// every real log is discarded from the replay buffer, and the latest
	// phase is delivered separately as its snapshot.
	var texts []string
	var notices, phases int
	for _, e := range got {
		switch {
		case e.Type == "log" && e.Text == truncationNotice:
			notices++
		case e.Type == "log":
			texts = append(texts, e.Text)
		case e.Type == "phase":
			phases++
		}
	}
	if notices != 1 {
		t.Fatalf("notices = %d, want 1; events = %v", notices, got)
	}
	if len(texts) != 0 {
		t.Fatalf("real logs replayed with cap=1 = %v, want none", texts)
	}
	if phases != 1 {
		t.Fatalf("phase snapshots = %d, want 1 (latest phase replayed separately); events = %v", phases, got)
	}
	if got[0].Type != "log" || got[0].Text != truncationNotice {
		t.Fatalf("replay[0] = %+v, want truncation notice", got[0])
	}
	if last := got[len(got)-1]; last.Type != "phase" || last.Step != "p1" {
		t.Fatalf("replay last = %+v, want phase p1 snapshot", last)
	}
}

func TestBrokerResetZeroesDropped(t *testing.T) {
	b := newBrokerWithCap(2, 16)
	t.Cleanup(b.closeAll)

	s := b.subscribe()

	// Fill the 2-slot channel and overflow it without reading: e0,e1
	// queue; e2..e4 are dropped and tallied.
	for i := 0; i < 5; i++ {
		b.Log("info", fmt.Sprintf("e%d", i))
	}
	queued := drain(s, 50*time.Millisecond)
	if len(queued) != 2 {
		t.Fatalf("queued = %d events, want 2", len(queued))
	}

	// Reset starts a new task: the outstanding drop tally must not ride
	// on the reset event nor on the first event of the new task.
	b.Reset()
	if e := mustRecv(t, s, time.Second); e.Type != "reset" || e.Dropped != 0 {
		t.Fatalf("reset event = %+v, want type reset with Dropped==0", e)
	}
	b.Log("info", "fresh")
	if e := mustRecv(t, s, time.Second); e.Text != "fresh" || e.Dropped != 0 {
		t.Fatalf("first post-reset event = %+v, want fresh with Dropped==0", e)
	}
}

func TestBrokerEventTypes(t *testing.T) {
	b := newBrokerWithCap(256, 50000)
	t.Cleanup(b.closeAll)

	s := b.subscribe()
	b.Log("error", "logline")
	b.Phase("step", "fail")
	b.History("hist")
	b.Canceled()

	want := []struct {
		typ  string
		text string
	}{
		{"log", "logline"},
		{"phase", ""},
		{"history", ""},
		{"canceled", ""},
	}
	for _, w := range want {
		e := mustRecv(t, s, time.Second)
		if e.Type != w.typ {
			t.Fatalf("got type %q, want %q", e.Type, w.typ)
		}
		if w.text != "" && e.Text != w.text {
			t.Fatalf("got text %q, want %q", e.Text, w.text)
		}
	}
	assertNoEvent(t, s, 50*time.Millisecond)
}
