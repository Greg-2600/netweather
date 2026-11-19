package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
)

func TestMapSmoke2(t *testing.T) {
	m := board.NewMemory(board.Target{ID: "t1", Name: "Example", Host: "example.com", Sector: "cloud", State: board.StateUp})
	h, err := New(Deps{
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Source:    m,
		Token:     "",
		Version:   "test",
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, _ := http.Get(ts.URL + "/map")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("STATUS=%d LEN=%d", resp.StatusCode, len(body))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !contains(string(body), "map-tile") {
		t.Fatalf("no map-tile in body")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}