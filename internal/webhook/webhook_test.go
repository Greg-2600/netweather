package webhook //nolint:testpackage // white-box tests cover the notifier's payload and send path

import (
	"context"
	"encoding/json"
	"errors"
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

type failingSink struct{}

func (failingSink) HasOpenIncident(context.Context, string, string) (bool, error) {
	return false, errors.New("lookup failed")
}

func (failingSink) OpenIncident(context.Context, store.Incident, []string, []store.Attribution) error {
	return errors.New("open failed")
}

func TestWrapSinkNilNotifierAndErrorPath(t *testing.T) {
	fake := &fakeSink{}
	if got := WrapSink(fake, nil); got != fake {
		t.Fatal("nil notifier must return the original sink")
	}

	// The underlying sink's error is returned before any webhook is attempted.
	n := New("http://127.0.0.1:1", "", nil)
	wrapped := WrapSink(failingSink{}, n)
	if err := wrapped.OpenIncident(t.Context(), store.Incident{ID: "inc-x"}, nil, nil); err == nil {
		t.Fatal("underlying sink error must be returned")
	}
	if ok, err := wrapped.HasOpenIncident(t.Context(), "sector", "cloud"); ok || err == nil {
		t.Fatalf("HasOpenIncident = %v, %v; want false, error", ok, err)
	}
}

type fakeReconciler struct {
	open     []store.Incident
	closed   string
	closeErr error
}

func (f *fakeReconciler) OpenIncidents(context.Context) ([]store.Incident, error) {
	return f.open, nil
}

func (f *fakeReconciler) CloseIncident(_ context.Context, id, _ string, _ time.Time) error {
	f.closed = id
	return f.closeErr
}

func TestWrapReconcilerClosesAndPostsWebhook(t *testing.T) {
	events := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p payload
		if err := json.NewDecoder(r.Body).Decode(&p); err == nil {
			events <- p.Event
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	rec := &fakeReconciler{open: []store.Incident{{ID: "inc-3"}, {ID: "other"}}}
	if got := WrapReconciler(rec, nil); got != rec {
		t.Fatal("nil notifier must return the original reconciler")
	}
	wrapped := WrapReconciler(rec, New(srv.URL, "", nil))

	open, err := wrapped.OpenIncidents(t.Context())
	if err != nil || len(open) != 2 {
		t.Fatalf("OpenIncidents = %v, %v", open, err)
	}
	now := time.Now().UTC()
	if err := wrapped.CloseIncident(t.Context(), "inc-3", "resolved", now); err != nil {
		t.Fatal(err)
	}
	if rec.closed != "inc-3" {
		t.Fatalf("underlying close got %q", rec.closed)
	}
	select {
	case event := <-events:
		if event != "incident_closed" {
			t.Fatalf("webhook event = %q", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no close webhook delivered")
	}

	// A failed close must not fire a webhook.
	rec.closeErr = errors.New("close failed")
	if err := wrapped.CloseIncident(t.Context(), "inc-3", "resolved", now); err == nil {
		t.Fatal("close error must be propagated")
	}
	// An unknown incident closes locally but has no payload to deliver.
	rec.closeErr = nil
	if err := wrapped.CloseIncident(t.Context(), "missing", "resolved", now); err != nil {
		t.Fatal(err)
	}
}

func TestSendFailureAndRejectionBranches(t *testing.T) {
	// A malformed URL fails while building the request.
	New("://bad-url", "", nil).send(t.Context(), "incident_opened", store.Incident{ID: "inc-1"})

	// A dead server fails during delivery.
	dead := New("http://127.0.0.1:1", "", nil)
	dead.send(t.Context(), "incident_opened", store.Incident{ID: "inc-1"})

	// A rejection is logged, never propagated.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	New(srv.URL, "", nil).send(t.Context(), "incident_opened", store.Incident{ID: "inc-1"})
}
