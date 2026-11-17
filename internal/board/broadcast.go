package board

import "sync"

// Broadcast is the write-side half of a board source: the agent calls Notify
// after a batch is persisted, and every SSE connection takes a fresh snapshot.
//
// It carries a signal, never data, so a slow subscriber cannot pin a state in
// memory.
type Broadcast struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

// NewBroadcast returns an empty broadcaster.
func NewBroadcast() *Broadcast {
	return &Broadcast{subs: make(map[chan struct{}]struct{})}
}

// Subscribe returns a coalescing change channel and its cancel function.
func (b *Broadcast) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

// Notify wakes every subscriber without blocking.
func (b *Broadcast) Notify() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default: // a pending signal already covers this change
		}
	}
}
