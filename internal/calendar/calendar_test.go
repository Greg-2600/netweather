package calendar_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Greg-2600/netweather/internal/calendar"
)

func TestParse(t *testing.T) {
	t.Parallel()

	entries, err := calendar.Parse([]byte(`
dates:
  - date: 2026-12-25
    regions: [us, DE]
  - date: 2026-01-01
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	// Sorted by date, whichever order the file used.
	if entries[0].Date != "2026-01-01" || entries[1].Date != "2026-12-25" {
		t.Fatalf("dates = %v, %v", entries[0].Date, entries[1].Date)
	}
	if len(entries[1].Regions) != 2 ||
		entries[1].Regions[0] != "DE" || entries[1].Regions[1] != "US" {
		t.Fatalf("regions = %v, want sorted upper-case codes", entries[1].Regions)
	}

	// A day with no regions applies to every scope; a regional day narrows to
	// that country's baseline.
	if scopes := entries[0].Scopes(); len(scopes) != 1 || scopes[0] != [2]string{"", ""} {
		t.Fatalf("global scopes = %v", scopes)
	}
	scopes := entries[1].Scopes()
	if len(scopes) != 2 || scopes[0] != [2]string{"country", "DE"} || scopes[1] != [2]string{"country", "US"} {
		t.Fatalf("regional scopes = %v", scopes)
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"not a date", "dates:\n  - date: Christmas\n"},
		{"date with a time", "dates:\n  - date: 2026-12-25T00:00:00Z\n"},
		{"duplicate date", "dates:\n  - date: 2026-12-25\n  - date: 2026-12-25\n"},
		{"region not two letters", "dates:\n  - date: 2026-12-25\n    regions: [USA]\n"},
		{"unknown field", "dates:\n  - date: 2026-12-25\n    region: US\n"},
		{"unknown top-level field", "holidays:\n  - date: 2026-12-25\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := calendar.Parse([]byte(tc.body)); err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
		})
	}
}

func TestLoadAbsentIsNotAnError(t *testing.T) {
	t.Parallel()

	entries, err := calendar.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if entries != nil {
		t.Fatalf("entries = %+v, want nil", entries)
	}
}

func TestLoadReadsAFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "calendar.yaml")
	if err := os.WriteFile(path, []byte("dates:\n  - date: 2026-12-25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := calendar.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Date != "2026-12-25" {
		t.Fatalf("entries = %+v", entries)
	}
}
