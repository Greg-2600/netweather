package logging

import (
	"io"
	"sync"
	"sync/atomic"
)

// AsyncWriter decouples log writes from the caller.
//
// Logging must never stall the probe scheduler: a slow stdout (a paused
// `docker logs`, a full disk, a remote logging driver) would otherwise turn into
// missed probes, which the rest of the system would report as an outage. When
// the buffer is full the write is dropped and counted, not blocked.
type AsyncWriter struct {
	mu     sync.RWMutex
	ch     chan []byte
	inner  io.Writer
	closed bool
	done   chan struct{}

	dropped   atomic.Uint64
	writeErrs atomic.Uint64
}

// NewAsyncWriter starts one goroutine draining to inner. buffer is the number of
// pending lines; values <= 0 use 1024.
func NewAsyncWriter(inner io.Writer, buffer int) *AsyncWriter {
	if buffer <= 0 {
		buffer = 1024
	}
	w := &AsyncWriter{
		ch:    make(chan []byte, buffer),
		inner: inner,
		done:  make(chan struct{}),
	}
	go w.loop()
	return w
}

func (w *AsyncWriter) loop() {
	defer close(w.done)
	for b := range w.ch {
		if _, err := w.inner.Write(b); err != nil {
			w.writeErrs.Add(1)
		}
	}
}

// Write never blocks and never returns an error: a log write must not change
// control flow.
func (w *AsyncWriter) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	copy(b, p)

	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return len(p), nil
	}

	select {
	case w.ch <- b:
	default:
		w.dropped.Add(1)
	}
	return len(p), nil
}

// Close flushes what is already buffered. Writes after Close are discarded.
func (w *AsyncWriter) Close() error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
	w.mu.Unlock()

	<-w.done
	return nil
}

// Dropped reports how many lines were discarded because the buffer was full.
func (w *AsyncWriter) Dropped() uint64 { return w.dropped.Load() }

// WriteErrors reports how many writes the inner writer rejected.
func (w *AsyncWriter) WriteErrors() uint64 { return w.writeErrs.Load() }
