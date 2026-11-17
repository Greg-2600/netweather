package classify_test

import (
	"net/http"
	"testing"

	"github.com/Greg-2600/netweather/internal/classify"
	"github.com/Greg-2600/netweather/internal/state"
)

// FuzzClassify checks the documented classifier contract (QUALITY.md §5.2):
// classification is total, deterministic, and never panics on arbitrary bytes.
func FuzzClassify(f *testing.F) {
	f.Add(uint16(200), []byte("ok"), uint8(0), "application/json", "https://example.com/")
	f.Add(uint16(403), []byte("<html><title>Just a moment...</title></html>"), uint8(0), "text/html", "https://example.com/")
	f.Add(uint16(503), []byte{}, uint8(1), "", "https://example.com/a")
	f.Add(uint16(302), []byte{}, uint8(2), "text/plain", "https://example.com/login")

	f.Fuzz(func(t *testing.T, status uint16, body []byte, mode uint8, contentType, finalURL string) {
		header := make(http.Header)
		if contentType != "" {
			header.Set("Content-Type", contentType)
		}

		expected := classify.Expected{}
		if mode&1 != 0 {
			expected.ICMPAnchor = true
		}
		if mode&2 != 0 {
			expected.BodyContains = "ok"
		}
		if mode&4 != 0 {
			expected.MinBodyBytes = 16
		}

		resp := classify.Response{
			Status: int(status), Header: header, Body: body, FinalURL: finalURL,
		}

		first := classify.Decide(expected, resp)
		second := classify.Decide(expected, resp)

		if first.State != second.State || first.Confidence != second.Confidence || first.Reason != second.Reason {
			t.Fatalf("non-deterministic: %+v vs %+v", first, second)
		}
		if !validState(first.State) {
			t.Fatalf("invalid state %q", first.State)
		}
		if first.Reason == "" {
			t.Fatal("a decision had no reason")
		}
	})
}

func validState(s state.State) bool {
	switch s {
	case state.Up, state.UpReachability, state.Degraded, state.Down,
		state.Blocked, state.Unknown, state.ConfigError, state.Unmeasurable:
		return true
	}
	return false
}
