package probe_test

import (
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
)

// TestDocumentedProbeDeadlines pins the numbers ACCEPTABLE_USE.md §3 publishes.
// A deadline is a politeness and false-positive control, so the document and the
// constants must not drift apart silently.
func TestDocumentedProbeDeadlines(t *testing.T) {
	t.Parallel()

	if probe.DefaultTimeout != 15*time.Second {
		t.Errorf("HTTP total deadline = %v, want 15s", probe.DefaultTimeout)
	}
	if got := probe.NewHTTP(nil).Client.Timeout; got != probe.DefaultTimeout {
		t.Errorf("HTTP client timeout = %v, want the documented total", got)
	}
	if got := probe.NewDNS(nil).Timeout; got != 5*time.Second {
		t.Errorf("DNS deadline = %v, want 5s", got)
	}
}
