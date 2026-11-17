package logging_test

import (
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/logging"
)

// FuzzScrubStringIsIdempotent proves the redactor is a fixed point: scrubbing an
// already-scrubbed string must not change it. Without that property, a URL can
// survive one pass and leak through a second log line.
func FuzzScrubStringIsIdempotent(f *testing.F) {
	f.Add("https://user:pass@example.com/x?token=abc&page=2")
	f.Add("")
	f.Add("no url here")
	f.Add("http://a:b@c/?sig=deadbeef")
	f.Add("https://example.com/?key=&access_token=x")

	f.Fuzz(func(t *testing.T, s string) {
		once := logging.ScrubString(s)
		twice := logging.ScrubString(once)
		if once != twice {
			t.Fatalf("ScrubString is not idempotent:\n once=%q\ntwice=%q", once, twice)
		}
	})
}

// FuzzParseLevel proves every input either yields a named level or an error, and
// never panics or silently produces a level with no name.
func FuzzParseLevel(f *testing.F) {
	f.Add("info")
	f.Add("TRACE")
	f.Add("")
	f.Add("warn")
	f.Add("\x00")
	f.Add(strings.Repeat("debug", 100))

	f.Fuzz(func(t *testing.T, s string) {
		level, err := logging.ParseLevel(s)
		if err != nil {
			return
		}
		if logging.LevelName(level) == "" {
			t.Fatalf("ParseLevel(%q) accepted a level with no name", s)
		}
	})
}
