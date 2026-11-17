package enrich_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Greg-2600/netweather/internal/enrich"
)

func TestPeerDBParsesExchangeNames(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/netixlan" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("asn"); got != "13335" {
			t.Errorf("asn = %q", got)
		}
		if got := r.Header.Get("Accept"); !strings.Contains(got, "json") {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "netweather") {
			t.Errorf("User-Agent = %q", got)
		}
		_, _ = w.Write([]byte(`{
		  "data": [
		    {"name": "DE-CIX Frankfurt", "ix_id": 31},
		    {"name": "AMS-IX", "ix_id": 26},
		    {"name": "DE-CIX Frankfurt", "ix_id": 31},
		    {"name": "", "ix_id": 9}
		  ]
		}`))
	}))
	defer srv.Close()

	db := enrich.NewPeerDB(srv.Client())
	db.BaseURL = srv.URL

	names, err := db.IXNames(context.Background(), 13335)
	if err != nil {
		t.Fatalf("IXNames: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("names = %+v, want two distinct exchanges", names)
	}
	if names[0] != "AMS-IX" || names[1] != "DE-CIX Frankfurt" {
		t.Fatalf("names = %+v, want sorted", names)
	}

	// A second lookup is served from the in-process cache: one ASN can sit
	// behind many targets and must not cost one request per target.
	if _, err := db.IXNames(context.Background(), 13335); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 after a cached lookup", got)
	}
}

func TestPeerDBEmptyAnswerIsNotAnError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data": []}`))
	}))
	defer srv.Close()

	db := enrich.NewPeerDB(srv.Client())
	db.BaseURL = srv.URL

	names, err := db.IXNames(context.Background(), 64500)
	if err != nil {
		t.Fatalf("IXNames: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("names = %+v, want none", names)
	}
}

func TestPeerDBCapsTheList(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data": [
		  {"name":"A"},{"name":"B"},{"name":"C"},{"name":"D"},{"name":"E"},{"name":"F"},{"name":"G"}
		]}`))
	}))
	defer srv.Close()

	db := enrich.NewPeerDB(srv.Client())
	db.BaseURL = srv.URL
	db.MaxNames = 3

	names, err := db.IXNames(context.Background(), 64500)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 || names[0] != "A" || names[2] != "C" {
		t.Fatalf("names = %+v, want the first three alphabetically", names)
	}
}

func TestPeerDBFailures(t *testing.T) {
	t.Parallel()

	if _, err := enrich.NewPeerDB(nil).IXNames(context.Background(), 1); err == nil {
		t.Fatal("a nil client should be an error")
	}

	db := enrich.NewPeerDB(http.DefaultClient)
	for _, asn := range []int{0, -1} {
		if _, err := db.IXNames(context.Background(), asn); err == nil {
			t.Errorf("ASN %d was accepted", asn)
		}
	}

	badStatus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer badStatus.Close()
	db = enrich.NewPeerDB(badStatus.Client())
	db.BaseURL = badStatus.URL
	if _, err := db.IXNames(context.Background(), 64500); err == nil {
		t.Fatal("a 502 should be an error")
	}

	badJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer badJSON.Close()
	db = enrich.NewPeerDB(badJSON.Client())
	db.BaseURL = badJSON.URL
	if _, err := db.IXNames(context.Background(), 64500); err == nil {
		t.Fatal("malformed JSON should be an error")
	}
}
