package api

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
)

// DefaultEventLogSize is the replay window. It is deliberately small: a client
// that has been away longer than this refetches the board instead of receiving
// a partial history, which is cheaper and cannot serve a stale delta as if it
// were current.
const DefaultEventLogSize = 256

// logEntry is one replayable stream event.
type logEntry struct {
	seq   int64
	event string
	data  []byte
}

// eventLog is the fan-out and replay ring behind the SSE stream.
//
// One tracker goroutine appends deltas as the board changes, and every handler
// forwards from the same log. That is what makes Last-Event-ID a real resume:
// the events exist whether or not a client was connected when they happened.
//
// The epoch prefix in every id matters. Sequence numbers restart at 1 with the
// process, so a client that reconnects after a restart would otherwise resume
// at a number that means something different now. A mismatched epoch fails the
// resume and the handler sends a fresh snapshot instead.
type eventLog struct {
	mu      sync.Mutex
	epoch   string
	entries []logEntry
	subs    map[chan logEntry]struct{}
	next    int64
	max     int
}

func newEventLog(max int) *eventLog {
	if max <= 0 {
		max = DefaultEventLogSize
	}

	// A failed read is not worth refusing to start a monitor over; a zero
	// epoch still keeps ids unique within this process.
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)

	return &eventLog{
		epoch: hex.EncodeToString(buf),
		subs:  make(map[chan logEntry]struct{}),
		next:  1,
		max:   max,
	}
}

// append records an event, wakes subscribers, and returns the stored entry.
func (l *eventLog) append(event string, data []byte) logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry := logEntry{
		seq:   l.next,
		event: event,
		data:  append([]byte(nil), data...),
	}
	l.next++

	l.entries = append(l.entries, entry)
	if len(l.entries) > l.max {
		l.entries = l.entries[len(l.entries)-l.max:]
	}

	for ch := range l.subs {
		select {
		case ch <- entry:
		default:
			// A subscriber that cannot keep up would silently diverge, so it is
			// closed instead: the client reconnects and resumes from the ring,
			// which still has every event.
			delete(l.subs, ch)
			close(ch)
		}
	}
	return entry
}

// current is the newest sequence number. A client that attaches here needs no
// replay: everything up to now is in its snapshot.
func (l *eventLog) current() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.next - 1
}

// id renders the opaque SSE id for a sequence.
func (l *eventLog) id(seq int64) string {
	return l.epoch + ":" + strconv.FormatInt(seq, 10)
}

// parseID turns an opaque id back into a sequence. A different epoch means the
// id belongs to a previous process and cannot be resumed.
func (l *eventLog) parseID(id string) (int64, bool) {
	epoch, raw, ok := strings.Cut(strings.TrimSpace(id), ":")
	if !ok || epoch != l.epoch {
		return 0, false
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq < 0 {
		return 0, false
	}
	return seq, true
}

// resume attaches a reconnecting client and returns the events it missed. ok is
// false when the ring cannot bridge the gap, and the caller must send a fresh
// snapshot instead of a partial history.
func (l *eventLog) resume(id string) (backlog []logEntry, ch <-chan logEntry, ok bool, cancel func()) {
	seq, valid := l.parseID(id)
	if !valid {
		return nil, nil, false, func() {}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	backlog, ok = l.sinceLocked(seq)
	if !ok {
		return nil, nil, false, func() {}
	}
	ch, cancel = l.subscribeLocked()
	return backlog, ch, true, cancel
}

// attach subscribes at seq and returns both the backlog after it and the live
// channel, in one critical section, so no event can slip between them.
func (l *eventLog) attach(seq int64) (backlog []logEntry, ch <-chan logEntry, cancel func()) {
	l.mu.Lock()
	defer l.mu.Unlock()

	backlog, _ = l.sinceLocked(seq)
	ch, cancel = l.subscribeLocked()
	return backlog, ch, cancel
}

func (l *eventLog) sinceLocked(seq int64) ([]logEntry, bool) {
	if seq < 0 {
		return nil, false
	}
	if len(l.entries) == 0 {
		return nil, true
	}

	oldest := l.entries[0].seq
	if seq < oldest-1 {
		return nil, false
	}
	if seq >= l.next-1 {
		return nil, true
	}

	out := make([]logEntry, 0, len(l.entries))
	for _, entry := range l.entries {
		if entry.seq > seq {
			out = append(out, entry)
		}
	}
	return out, true
}

func (l *eventLog) subscribeLocked() (<-chan logEntry, func()) {
	sub := make(chan logEntry, 64)
	l.subs[sub] = struct{}{}

	cancel := func() {
		l.mu.Lock()
		if _, exists := l.subs[sub]; exists {
			delete(l.subs, sub)
			close(sub)
		}
		l.mu.Unlock()
	}
	return sub, cancel
}
