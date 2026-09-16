// Package webui will host the local Web UI. broker is the in-process
// publish/subscribe event bus that streams one conversion task's live
// events to browser SSE connections (multiple tabs allowed).
//
// Hard invariant: the pipeline and its external subprocesses must NEVER
// stall on browser consumption speed. Every delivery is a non-blocking
// channel send (select/default); a slow tab loses events instead of
// applying back-pressure, and is told how many it lost on its next
// successful receive.
package webui

import (
	"sync"
	"time"
)

const (
	// subChanCap is the per-subscriber live channel capacity in production.
	subChanCap = 256
	// maxBufferedEvents bounds the linear replay buffer for late tabs; the
	// oldest events are discarded past this limit.
	maxBufferedEvents = 50000
)

// truncationNotice is emitted once, at the front of a late subscriber's
// replay, when events it could have wanted have fallen out of the bounded
// buffer. It is a synthetic buffered event, not a state snapshot.
const truncationNotice = "早期日志已截断"

// Event is one SSE record. Data is any JSON-serializable payload; the
// broker never serializes it itself.
type Event struct {
	Type    string `json:"type"` // log | phase | done | error | canceled | history | reset
	Time    string `json:"time,omitempty"`
	Level   string `json:"level,omitempty"`   // log: info|warn|error
	Text    string `json:"text,omitempty"`    // log text
	Step    string `json:"step,omitempty"`    // phase step name
	Status  string `json:"status,omitempty"`  // phase: start|done|fail
	Dropped int    `json:"dropped,omitempty"` // events this subscriber missed before this one
	// Data is the optional JSON-serializable payload for
	// done/history/error events. Contract: once handed to the broker a
	// payload is treated as immutable, i.e. the caller must never mutate
	// it after publication, because every subscriber channel shares the
	// same reference while SSE JSON encoding reads it, which would be a
	// data race. Keep it small (e.g. a {path,bvid,name} object or a list
	// of history entries); never put large objects such as a full
	// transcript here, because the replay buffer and all live subscribers
	// retain the reference long-term.
	Data any `json:"data,omitempty"`

	// seq is the broker's internal publication order, used to tell whether
	// the phase snapshot is already present inside the replay buffer. It is
	// never serialized.
	seq uint64 `json:"-"`
}

// sub is one live SSE subscription. ch is closed by unsubscribe/closeAll;
// handlers range over it and exit when it closes. All fields are guarded
// by broker.mu: every channel send and every close happens under that
// lock, so a send can never race a close onto a closed channel.
type sub struct {
	ch      chan Event
	dropped int
	closed  bool
}

// broker is a single-task event bus. Reset clears all task state when a
// new task starts; events produced before the first Reset (startup
// auto-eviction notices, history) are buffered and replay normally.
type broker struct {
	mu sync.Mutex

	// buffer is the linear, time-ordered replay log. When truncated is
	// true its first entry is the one-shot truncation notice and the rest
	// are the newest (capacity-1) real events. It contains only
	// log/phase/history events: terminal events (done/error/canceled) live
	// exclusively in terminal.
	buffer    []Event
	capacity  int
	subCap    int
	truncated bool
	// nextSeq numbers real publications; bufStartSeq is the seq of the
	// oldest real event still in buffer (the buffer tail is contiguous),
	// so a phase snapshot with seq >= bufStartSeq is already replayed via
	// the buffer and must not be sent again.
	nextSeq     uint64
	bufStartSeq uint64

	// phase is the latest phase event; terminal is the latest
	// done/error/canceled (later terminals overwrite earlier ones).
	phase    *Event
	terminal *Event

	subs   map[*sub]struct{}
	closed bool
}

// newBroker builds a production broker (subChanCap / maxBufferedEvents).
func newBroker() *broker {
	return newBrokerWithCap(subChanCap, maxBufferedEvents)
}

// newBrokerWithCap builds a broker with the given per-subscriber channel
// capacity and replay-buffer capacity. It exists for deterministic tests
// of slow-consumer and truncation behaviour.
func newBrokerWithCap(subCap, bufCap int) *broker {
	if subCap < 1 {
		subCap = 1
	}
	if bufCap < 1 {
		bufCap = 1
	}
	return &broker{
		buffer:   make([]Event, 0, bufCap),
		capacity: bufCap,
		subCap:   subCap,
		subs:     make(map[*sub]struct{}),
	}
}

// subscribe returns a new subscriber. Under one lock hold it creates the
// sub, queues the full replay backlog (truncation notice first if armed,
// then surviving buffered events in time order, then the latest phase and
// terminal snapshots) and registers the sub as live. Holding the lock
// across "replay + register" excludes publishers for the whole window, so
// no live event can be delivered before the sub is registered (no gap)
// and no event can be delivered twice (no dup). Replay sends are
// non-blocking: a fresh channel always has spare capacity, but nothing in
// this critical section ever waits on a browser. After closeAll,
// subscribe returns nil.
func (b *broker) subscribe() *sub {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	// Size the channel to fit the ENTIRE replay in addition to its normal
	// live capacity: replay delivery happens under this lock (before any
	// handler can read) and must neither block nor silently drop, even when
	// the backlog (up to maxBufferedEvents) dwarfs subChanCap. Memory is
	// bounded per subscriber and released on unsubscribe/closeAll.
	// Capacity = full replay length PLUS normal live headroom, so even
	// before the handler drains any history there are still subCap slots
	// for fresh live events.
	chCap := len(b.buffer) + replaySnapshots(b) + b.subCap
	s := &sub{ch: make(chan Event, chCap)}
	for _, e := range b.buffer {
		b.deliverLocked(s, e)
	}
	// Send the phase snapshot only when it has fallen out of the bounded
	// buffer; otherwise the identical event was just replayed from it.
	if b.phase != nil && b.phase.seq < b.bufStartSeq {
		b.deliverLocked(s, *b.phase)
	}
	if b.terminal != nil {
		b.deliverLocked(s, *b.terminal)
	}
	b.subs[s] = struct{}{}
	return s
}

// replaySnapshots reports how many state snapshots will be appended after
// the buffered events during replay. Caller holds b.mu.
func replaySnapshots(b *broker) int {
	n := 0
	if b.phase != nil && b.phase.seq < b.bufStartSeq {
		n++
	}
	if b.terminal != nil {
		n++
	}
	return n
}

// unsubscribe removes a subscriber and closes its channel so an SSE
// handler's range loop exits. It is idempotent and nil-safe, and safe to
// race with publish/closeAll: all sends and closes are serialized by
// broker.mu, so this can never trigger a send-on-closed-channel panic.
func (b *broker) unsubscribe(s *sub) {
	if s == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.closed {
		return
	}
	if _, ok := b.subs[s]; !ok {
		// Never registered here (post-closeAll subscribe returns nil, and
		// closeAll marks every sub it knew about closed=true above).
		return
	}
	delete(b.subs, s)
	s.closed = true
	close(s.ch)
}

// closeAll closes every subscriber channel and forgets all subscribers
// and buffered state. Later publish calls are silently dropped and later
// subscribe calls return nil. Idempotent.
func (b *broker) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for s := range b.subs {
		s.closed = true
		close(s.ch)
	}
	b.subs = nil
	b.buffer = nil
	b.phase = nil
	b.terminal = nil
	b.truncated = false
}

// Log publishes a log event. Time is stamped with the current local
// HH:MM:SS while holding broker.mu, immediately before buffering and
// fanning out, so timestamps are generated in the same critical section
// that orders publications and a subscriber never sees events out of
// timestamp order.
func (b *broker) Log(level, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := Event{Type: "log", Time: time.Now().Format("15:04:05"), Level: level, Text: text}
	b.publishBufferedLocked(e)
}

// Phase publishes a phase event and records it as the latest phase
// snapshot.
func (b *broker) Phase(step, status string) {
	e := Event{Type: "phase", Step: step, Status: status}
	b.publishState(e, true)
}

// Done publishes a terminal done event, replacing any earlier terminal.
func (b *broker) Done(data any) {
	b.publishState(Event{Type: "done", Data: data}, false)
}

// Error publishes a terminal error event, replacing any earlier terminal.
func (b *broker) Error(data any) {
	b.publishState(Event{Type: "error", Data: data}, false)
}

// Canceled publishes a terminal canceled event, replacing any earlier
// terminal.
func (b *broker) Canceled() {
	b.publishState(Event{Type: "canceled"}, false)
}

// History publishes a history payload into the linear buffer (typically a
// startup event before the first task Reset).
func (b *broker) History(entries any) {
	b.publishBuffered(Event{Type: "history", Data: entries})
}

// Reset starts a new task: the replay buffer, truncation flag, phase
// snapshot and terminal snapshot are all cleared, then one non-blocking
// reset event is fanned out to current subscribers. The reset event is
// NOT buffered, so a subscriber connecting after Reset sees an empty
// task. The buffer is replaced by a fresh allocation rather than resliced
// so the old backing array (and every Event.Data it referenced, e.g.
// history payloads) becomes garbage. Per-subscriber drop tallies are
// zeroed as well: dropped counts never carry across tasks, and the reset
// event itself is therefore delivered with Dropped == 0.
func (b *broker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.buffer = make([]Event, 0, b.capacity)
	b.truncated = false
	b.phase = nil
	b.terminal = nil
	b.nextSeq = 0
	b.bufStartSeq = 0
	for s := range b.subs {
		s.dropped = 0
	}
	e := Event{Type: "reset"}
	for s := range b.subs {
		b.deliverLocked(s, e)
	}
}

// publishBuffered appends a log/history event to the replay buffer and
// fans it out live, all under one lock hold.
func (b *broker) publishBuffered(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishBufferedLocked(e)
}

// publishBufferedLocked is the lock-held body shared by publishBuffered
// and Log (which stamps Time under the same lock before buffering).
func (b *broker) publishBufferedLocked(e Event) {
	if b.closed {
		return
	}
	e, notice := b.appendLocked(e)
	if notice != nil {
		for s := range b.subs {
			b.deliverLocked(s, *notice)
		}
	}
	for s := range b.subs {
		b.deliverLocked(s, e)
	}
}

// publishState handles phase and terminal events. A phase event is both
// buffered and kept as the latest phase snapshot; a terminal event is
// kept ONLY as the terminal snapshot (done/error/canceled overwrite one
// another), so a late subscriber receives it exactly once via the
// snapshot path even though live subscribers also got it in real time.
func (b *broker) publishState(e Event, isPhase bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	var notice *Event
	if isPhase {
		e, notice = b.appendLocked(e)
		ev := e
		b.phase = &ev
	} else {
		ev := e
		b.terminal = &ev
	}
	if notice != nil {
		for s := range b.subs {
			b.deliverLocked(s, *notice)
		}
	}
	for s := range b.subs {
		b.deliverLocked(s, e)
	}
}

// appendLocked appends to the bounded buffer, returning the event with
// its assigned publication seq and, exactly once (on the first overflow),
// a synthetic truncation notice that callers must ALSO fan out live to
// current subscribers. The notice is kept pinned at the front of the
// buffer thereafter; the buffer holds notice + (capacity-1) newest real
// events, so a late subscriber learns that earlier events were lost and
// still sees the newest `capacity-1` (zero when capacity is 1 — the
// notice alone survives). Caller holds b.mu.
func (b *broker) appendLocked(e Event) (Event, *Event) {
	b.nextSeq++
	e.seq = b.nextSeq

	var armed *Event
	if !b.truncated {
		if len(b.buffer) < b.capacity {
			if len(b.buffer) == 0 {
				b.bufStartSeq = e.seq
			}
			b.buffer = append(b.buffer, e)
			return e, nil
		}
		// First overflow, handled in place on the same backing array
		// instead of discarding the whole window: slide the full buffer
		// left by one (dropping old[0], the oldest real event), write the
		// arriving event at the tail, then pin the one-shot notice at the
		// head. This is equivalent to an ordinary drop-oldest enqueue
		// followed by pinning the notice, so events old[1:] survive
		// instead of being thrown away. Length stays equal to capacity,
		// hence every slot is overwritten and no stale Event reference
		// survives. With capacity 1 the lone slot ends holding just the
		// notice: no real event can fit alongside it.
		b.truncated = true
		notice := Event{Type: "log", Level: "warn", Text: truncationNotice}
		copy(b.buffer, b.buffer[1:])
		b.buffer[b.capacity-1] = e
		b.buffer[0] = notice
		armed = &notice
		if b.capacity < 2 {
			// Window holds only the notice; point just past this event so
			// a snapshot of it is still replayed separately.
			b.bufStartSeq = e.seq + 1
		} else {
			b.bufStartSeq = b.buffer[1].seq
		}
		return e, armed
	}

	// capacity < 2 leaves no slot for a real event once the notice is armed:
	// point the window just past this event so a snapshot of it is still
	// replayed separately.
	if b.capacity < 2 {
		b.bufStartSeq = e.seq + 1
		return e, armed
	}

	if len(b.buffer) < b.capacity {
		b.buffer = append(b.buffer, e)
		if len(b.buffer) == 2 {
			// First real event surviving after the notice was armed.
			b.bufStartSeq = e.seq
		}
		return e, armed
	}
	// Full: drop the oldest REAL event, keeping the notice pinned at head.
	// The copy duplicates the tail into the final slot; that stale copy is
	// overwritten immediately by e, so no vacated slot keeps an old Event.
	real := b.buffer[1:]
	copy(real, real[1:])
	b.buffer[len(b.buffer)-1] = e
	b.bufStartSeq = b.buffer[1].seq
	return e, armed
}

// deliverLocked enqueues one event into a subscriber channel without
// blocking. On a full channel the event is dropped and counted; when a
// subscriber with an outstanding drop tally next receives successfully,
// the count rides on that event copy and the tally resets to zero.
// Caller holds b.mu, which also makes the send mutually exclusive with
// channel closes.
func (b *broker) deliverLocked(s *sub, e Event) {
	if s.dropped > 0 {
		e.Dropped = s.dropped
	}
	select {
	case s.ch <- e:
		if e.Dropped != 0 {
			s.dropped = 0
		}
	default:
		s.dropped++
	}
}
