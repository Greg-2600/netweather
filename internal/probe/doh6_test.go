package probe_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Greg-2600/netweather/internal/probe"
)

// doh6Server applies the same assertions as the A-record helper, but for the
// AAAA query Do6 must send.
func doh6Server(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("type"); got != "AAAA" {
			t.Errorf("type query = %q, want AAAA", got)
		}
		if got := r.URL.Query().Get("name"); got != "example.com" {
			t.Errorf("name query = %q", got)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoHDo6ResolvesAnAAAARecord(t *testing.T) {
	t.Parallel()

	srv := doh6Server(t, http.StatusOK, `{"Status":0,"Answer":[{"type":28,"data":"2001:db8::1"}]}`)
	doh := probe.NewDoHWithClient(srv.Client())

	got := doh.Do6(context.Background(), srv.URL+"/dns-query", "example.com")
	if got.Failed() {
		t.Fatalf("Do6 failed: %s: %v", got.ErrorClass, got.Err)
	}
	if len(got.Addresses) != 1 || got.Addresses[0].String() != "2001:db8::1" {
		t.Fatalf("addresses = %v", got.Addresses)
	}
}

func TestDoHDo6ClassifiesAnswers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"only A records", `{"Status":0,"Answer":[{"type":1,"data":"192.0.2.1"}]}`, "doh_no_answer"},
		{"nxdomain", `{"Status":3}`, "doh_nxdomain"},
		{"servfail", `{"Status":2}`, "doh_servfail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := doh6Server(t, http.StatusOK, tc.body)
			got := probe.NewDoHWithClient(srv.Client()).Do6(context.Background(), srv.URL+"/dns-query", "example.com")
			if got.ErrorClass != tc.want {
				t.Fatalf("class = %q, want %q (%v)", got.ErrorClass, tc.want, got.Err)
			}
		})
	}
}
