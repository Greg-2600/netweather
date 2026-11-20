//go:build windows

package probe_test

import (
	"context"
	"testing"

	"github.com/Greg-2600/netweather/internal/probe"
)

func TestNewPMTUUsesProductionDefaults(t *testing.T) {
	t.Parallel()

	p := probe.NewPMTU()
	if p.Size != 1400 {
		t.Fatalf("size = %d, want 1400", p.Size)
	}
	if p.Timeout != 3 {
		t.Fatalf("timeout = %d, want 3 seconds", p.Timeout)
	}
}

func TestPMTUReportsAGuardedDialFailure(t *testing.T) {
	t.Parallel()

	// netx.New() blocks loopback, so no datagram is sent. The zero Size and
	// Timeout must still be replaced by their documented defaults before the
	// dial is attempted, and the failure must surface as a pmtu class.
	p := probe.NewPMTU()
	p.Size = 0
	p.Timeout = 0

	got := p.Do(context.Background(), "127.0.0.1:443")
	if got.ErrorClass != "pmtu_error" {
		t.Fatalf("class = %q, want pmtu_error (%v)", got.ErrorClass, got.Err)
	}
	if got.Err == nil {
		t.Fatal("a pmtu_error has no error detail")
	}
	if got.Latency < 0 {
		t.Fatalf("latency = %v", got.Latency)
	}
	// On Windows the timeout is derived from Timeout at call time, so the
	// struct keeps its zero value while the exchange uses a three-second cap.
	if p.Size != 1400 || p.Timeout != 0 {
		t.Fatalf("defaults not applied: size=%d timeout=%d", p.Size, p.Timeout)
	}
}
