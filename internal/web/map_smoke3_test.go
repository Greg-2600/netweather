package web

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
)

func TestMapSmoke3(t *testing.T) {
	targets := make([]board.Target, 0, 10)
	for i := 0; i < 10; i++ {
		targets = append(targets, board.Target{ID: fmt.Sprintf("t%d", i), Name: fmt.Sprintf("T%d", i), Host: "example.com", Sector: []string{"cloud", "financial", "airline"}[i%3], State: board.StateUp})
	}
	m := board.NewMemory(targets...)
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
	t.Logf("STATUS=%d LEN=%d tiles=%d", resp.StatusCode, len(body), countSub(string(body), "map-tile"))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func countSub(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
			i += len(sub) - 1
		}
	}
	return n
}