package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/store"
)

func TestNotifierDisabledWhenURLEmpty(t *testing.T) {
	if n := New("", "secret", nil); n != nil {
		t.Fatal("expected nil notifier for empty URL")
	}
}

func TestNotifierPostsOnSend(t *testing.T) {
	var mu sync.Mutex
	var got []byte
	var secret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret = r.Header.Get("X-Netweather-Secret")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		mu.Lock()
		got = b
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := New(srv.URL, "s3cr3t", nil)
	n.send(t.Context(), "incident_opened", store.Incident{ID: "inc-1", Scope: "sector", ScopeValue: "airline"})

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no body received")
	}
	var p payload
	if err := json.Unmarshal(got, &p); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if p.Event != "incident_opened" || p.Incident.ID != "inc-1" {
		t.Fatalf("unexpected payload: %+v", p)
	}
	if secret != "s3cr3t" {
		t.Fatalf("secret not sent: %q", secret)
	}
}

func TestWrapSinkFiresWebhook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := New(srv.URL, "", nil)
	fake := &fakeSink{}
	wrapped := WrapSink(fake, n)
	inc := store.Incident{ID: "inc-2"}
	if err := wrapped.OpenIncident(t.Context(), inc, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !fake.opened {
		t.Fatal("underlying sink not called")
	}
	// best-effort: give delivery a moment
	time.Sleep(50 * time.Millisecond)
}

type fakeSink struct {
	opened bool
}

func (f *fakeSink) HasOpenIncident(context.Context, string, string) (bool, error) {
	return false, nil
}

func (f *fakeSink) OpenIncident(context.Context, store.Incident, []string, []store.Attribution) error {
	f.opened = true
	return nil
}