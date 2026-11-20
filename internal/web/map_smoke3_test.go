package web_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/web"
)

func TestMapSmoke3(t *testing.T) {
	targets := make([]board.Target, 0, 10)
	for i := 0; i < 10; i++ {
		targets = append(targets, board.Target{
			ID: fmt.Sprintf("t%d", i), Name: fmt.Sprintf("T%d", i),
			Host: "example.com", Sector: []string{"cloud", "financial", "airline"}[i%3],
			State: board.StateUp,
		})
	}
	m := board.NewMemory(targets...)
	h, err := web.New(web.Deps{
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

	resp, err := http.Get(ts.URL + "/map")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("STATUS=%d LEN=%d tiles=%d", resp.StatusCode, len(body), strings.Count(string(body), "map-tile"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
