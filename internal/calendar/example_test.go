package calendar_test

import (
	"path/filepath"
	"testing"

	"github.com/Greg-2600/netweather/internal/calendar"
)

// TestShippedExampleParses keeps config/calendar.example.yaml executable
// documentation. The README points operators at it, so a shape the parser no
// longer accepts would send them to a startup error.
func TestShippedExampleParses(t *testing.T) {
	t.Parallel()

	entries, err := calendar.Load(filepath.Join("..", "..", "config", "calendar.example.yaml"))
	if err != nil {
		t.Fatalf("the shipped calendar example does not load: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the shipped calendar example has no dates")
	}
}
