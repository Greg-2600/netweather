package logging_test

import (
	"bytes"
	"sync"
	"testing"

	"github.com/Greg-2600/netweather/internal/logging"
)

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	written int
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	b.mu.Lock()
	b.written += len(p)
	b.mu.Unlock()
	return len(p), nil
}

func TestAsyncWriterDeliversInOrder(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := logging.NewAsyncWriter(&buf, 4)

	for _, s := range []string{"a", "b", "c"} {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := buf.String(); got != "abc" {
		t.Fatalf("got %q, want %q", got, "abc")
	}
}

func TestAsyncWriterDropsInsteadOfBlocking(t *testing.T) {
	t.Parallel()

	bw := newBlockingWriter()
	w := logging.NewAsyncWriter(bw, 1)

	if _, err := w.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	<-bw.started // consumer is now stuck inside inner.Write

	_, _ = w.Write([]byte("b")) // fills the one-slot buffer
	_, _ = w.Write([]byte("c")) // must be dropped, not block

	if got := w.Dropped(); got != 1 {
		t.Fatalf("Dropped = %d, want 1", got)
	}

	close(bw.release)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	bw.mu.Lock()
	written := bw.written
	bw.mu.Unlock()
	if written != 2 {
		t.Fatalf("inner writer saw %d bytes, want 2 (a and b)", written)
	}
}

func TestAsyncWriterWriteAfterCloseIsSafe(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := logging.NewAsyncWriter(&buf, 1)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if n, err := w.Write([]byte("late")); n != 4 || err != nil {
		t.Fatalf("Write after Close = (%d, %v), want (4, nil)", n, err)
	}
	if got := buf.String(); got != "" {
		t.Fatalf("late write reached the sink: %q", got)
	}
}

func TestAsyncWriterCountsInnerErrors(t *testing.T) {
	t.Parallel()

	w := logging.NewAsyncWriter(failingWriter{}, 1)
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := w.WriteErrors(); got != 1 {
		t.Fatalf("WriteErrors = %d, want 1", got)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, bytes.ErrTooLarge }
