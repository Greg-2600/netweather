package seed_test

import (
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/seed"
)

func TestDefaultIsPublicHTTPSpendpoints(t *testing.T) {
	t.Parallel()

	entries := seed.Default()
	if len(entries) < 10 {
		t.Fatalf("len = %d, want at least a working board (10+)", len(entries))
	}

	seenID := map[string]struct{}{}
	seenHost := map[string]struct{}{}
	sectors := map[string]bool{}
	for _, e := range entries {
		if e.ID == "" || e.Name == "" || e.Host == "" || e.URL == "" {
			t.Errorf("incomplete entry: %+v", e)
		}
		if !strings.HasPrefix(e.URL, "https://") {
			t.Errorf("%s: URL is not HTTPS: %s", e.ID, e.URL)
		}
		if !strings.Contains(e.URL, e.Host) {
			t.Errorf("%s: host %q not in URL %q", e.ID, e.Host, e.URL)
		}
		if _, dup := seenID[e.ID]; dup {
			t.Errorf("duplicate id %q", e.ID)
		}
		seenID[e.ID] = struct{}{}
		if _, dup := seenHost[e.Host]; dup {
			t.Errorf("duplicate host %q", e.Host)
		}
		seenHost[e.Host] = struct{}{}
		sectors[e.Sector] = true

		switch e.Class {
		case "service", "reference":
		default:
			t.Errorf("%s: unexpected class %q", e.ID, e.Class)
		}
	}

	// The shipped board must show the sectors the product is about, or the
	// first thing an operator sees is a uniform grid.
	for _, sector := range []string{"financial/banking", "airline", "cloud", "social"} {
		if !sectors[sector] {
			t.Errorf("no starter target in sector %q", sector)
		}
	}

	if entries[len(entries)-1].Tier != 0 {
		t.Error("the reference target should be tier 0")
	}
}
