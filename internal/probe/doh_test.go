package probe_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/probe"
)

func dohServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("name"); got != "example.com" {
			t.Errorf("name query = %q", got)
		}
		if got := r.URL.Query().Get("type"); got != "A" {
			t.Errorf("type query = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/dns-json" {
			t.Errorf("Accept = %q", got)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoHResolvesAName(t *testing.T) {
	t.Parallel()

	srv := dohServer(t, http.StatusOK, `{"Status":0,"Answer":[{"type":1,"data":"203.0.113.9"}]}`)
	doh := probe.NewDoHWithClient(srv.Client())

	got := doh.Do(context.Background(), srv.URL+"/dns-query", "example.com")
	if got.Failed() {
		t.Fatalf("DoH failed: %s", got.ErrorClass)
	}
	if got.Status != http.StatusOK {
		t.Fatalf("status = %d", got.Status)
	}
	if len(got.Addresses) != 1 || got.Addresses[0].String() != "203.0.113.9" {
		t.Fatalf("addresses = %v", got.Addresses)
	}
}

func TestDoHClassifiesAnswers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		body  string
		class string
	}{
		{"nxdomain", `{"Status":3}`, "doh_nxdomain"},
		{"servfail", `{"Status":2}`, "doh_servfail"},
		{"no A record", `{"Status":0,"Answer":[{"type":5,"data":"alias.example."}]}`, "doh_no_answer"},
		{"malformed", `not json`, "doh_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := dohServer(t, http.StatusOK, tc.body)
			doh := probe.NewDoHWithClient(srv.Client())
			got := doh.Do(context.Background(), srv.URL+"/dns-query", "example.com")
			if got.ErrorClass != tc.class {
				t.Fatalf("class = %q, want %q", got.ErrorClass, tc.class)
			}
			if got.Err == nil {
				t.Fatal("a classified failure had no error detail")
			}
		})
	}
}

// rejectingDoHServer answers the JSON query with 400, as providers that only
// implement RFC 8484 do, and hands the decoded wire query to reply.
func rejectingDoHServer(t *testing.T, reply func(http.ResponseWriter, []byte)) (*httptest.Server, *int) {
	t.Helper()

	wireQueries := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoded := r.URL.Query().Get("dns")
		if encoded == "" {
			if got := r.Header.Get("Accept"); got != "application/dns-json" {
				t.Errorf("JSON Accept = %q", got)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "no 'dns' query parameter found")
			return
		}
		wireQueries++
		if got := r.Header.Get("Accept"); got != "application/dns-message" {
			t.Errorf("wire Accept = %q", got)
		}
		query, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			t.Errorf("wire query is not base64url: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply(w, query)
	}))
	t.Cleanup(srv.Close)
	return srv, &wireQueries
}

// wireDoHServer returns 400 to the JSON API and answers the RFC 8484 retry
// with a DNS response built from the encoded query.
func wireDoHServer(t *testing.T, rcode int, ip string) (*httptest.Server, *int) {
	t.Helper()

	return rejectingDoHServer(t, func(w http.ResponseWriter, query []byte) {
		_, _ = w.Write(dnsResponse(query, rcode, ip))
	})
}

func TestDoHFallsBackToWireFormat(t *testing.T) {
	t.Parallel()

	srv, wireQueries := wireDoHServer(t, 0, "203.0.113.9")
	got := probe.NewDoHWithClient(srv.Client()).Do(context.Background(), srv.URL+"/dns-query", "example.com")
	if got.Failed() {
		t.Fatalf("wire fallback failed: %s: %v", got.ErrorClass, got.Err)
	}
	if *wireQueries != 1 {
		t.Fatalf("wire queries = %d, want 1", *wireQueries)
	}
	if len(got.Addresses) != 1 || got.Addresses[0].String() != "203.0.113.9" {
		t.Fatalf("addresses = %v", got.Addresses)
	}
}

func TestDoHWireClassifiesRcodes(t *testing.T) {
	t.Parallel()

	srv, wireQueries := wireDoHServer(t, 3, "")
	got := probe.NewDoHWithClient(srv.Client()).Do(context.Background(), srv.URL+"/dns-query", "example.com")
	if got.ErrorClass != "doh_nxdomain" {
		t.Fatalf("class = %q, want doh_nxdomain", got.ErrorClass)
	}
	if *wireQueries != 1 {
		t.Fatalf("wire queries = %d, want 1", *wireQueries)
	}
}

func TestDoHWireFailureClasses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body func(http.ResponseWriter, []byte)
		want string
	}{
		{"http error", func(w http.ResponseWriter, _ []byte) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}, "doh_http"},
		{"not dns", func(w http.ResponseWriter, _ []byte) {
			_, _ = io.WriteString(w, "not a dns message")
		}, "doh_error"},
		{"servfail", func(w http.ResponseWriter, q []byte) {
			_, _ = w.Write(dnsResponse(q, 2, ""))
		}, "doh_servfail"},
		{"no answer", func(w http.ResponseWriter, q []byte) {
			_, _ = w.Write(dnsResponse(q, 0, ""))
		}, "doh_no_answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv, _ := rejectingDoHServer(t, tc.body)
			got := probe.NewDoHWithClient(srv.Client()).Do(context.Background(), srv.URL+"/dns-query", "example.com")
			if got.ErrorClass != tc.want {
				t.Fatalf("class = %q, want %q (%v)", got.ErrorClass, tc.want, got.Err)
			}
		})
	}
}

// secondRequestFails fails only the wire retry, so the JSON 400 is confirmed
// to have triggered it and the transport error is classified on that path.
type secondRequestFails struct{ calls int }

func (s *secondRequestFails) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	if s.calls == 1 {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("no 'dns' query parameter found")),
			Request:    req,
		}, nil
	}
	return nil, errors.New("wire request blocked")
}

func TestDoHWireTransportErrorIsClassified(t *testing.T) {
	t.Parallel()

	rt := &secondRequestFails{}
	got := probe.NewDoHWithClient(&http.Client{Transport: rt}).Do(
		context.Background(), "https://dns.example/dns-query", "example.com")
	if got.ErrorClass != "http_error" || got.Err == nil {
		t.Fatalf("class = %q, err = %v", got.ErrorClass, got.Err)
	}
	if rt.calls != 2 {
		t.Fatalf("requests = %d, want 2 (JSON then wire)", rt.calls)
	}
}

func TestDoHWireRejectsAnUnpackableName(t *testing.T) {
	t.Parallel()

	srv, _ := rejectingDoHServer(t, func(w http.ResponseWriter, q []byte) {
		_, _ = w.Write(dnsResponse(q, 0, "203.0.113.9"))
	})
	longName := strings.Repeat("a", 64) + "." + strings.Repeat("b", 64) + "." +
		strings.Repeat("c", 64) + "." + strings.Repeat("d", 64) + ".com"
	got := probe.NewDoHWithClient(srv.Client()).Do(context.Background(), srv.URL+"/dns-query", longName)
	if got.ErrorClass != "doh_config" {
		t.Fatalf("class = %q, want doh_config (%v)", got.ErrorClass, got.Err)
	}
}

func TestDoHUsesConfiguredUserAgent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "netweather-test/1.0" {
			t.Errorf("User-Agent = %q", got)
		}
		_, _ = io.WriteString(w, `{"Status":0,"Answer":[{"type":1,"data":"203.0.113.9"}]}`)
	}))
	t.Cleanup(srv.Close)

	doh := probe.NewDoHWithClient(srv.Client())
	doh.UserAgent = "netweather-test/1.0"
	if got := doh.Do(context.Background(), srv.URL+"/dns-query", "example.com"); got.Failed() {
		t.Fatalf("Do failed: %s", got.ErrorClass)
	}
}

func TestDoHRejectsWhatItCannotUse(t *testing.T) {
	t.Parallel()

	// A non-200 answer is not a DNS answer.
	srv := dohServer(t, http.StatusServiceUnavailable, `{}`)
	doh := probe.NewDoHWithClient(srv.Client())
	if got := doh.Do(context.Background(), srv.URL+"/dns-query", "example.com"); got.ErrorClass != "doh_http" {
		t.Fatalf("class = %q, want doh_http", got.ErrorClass)
	}

	// A plaintext endpoint is refused before any request is made.
	if got := doh.Do(context.Background(), "http://1.1.1.1/dns-query", "example.com"); got.ErrorClass != "doh_config" {
		t.Fatalf("http endpoint class = %q, want doh_config", got.ErrorClass)
	}
	if got := doh.Do(context.Background(), "", "example.com"); got.ErrorClass != "doh_config" {
		t.Fatalf("empty endpoint class = %q", got.ErrorClass)
	}
	if got := doh.Do(context.Background(), srv.URL+"/dns-query", ""); got.ErrorClass != "doh_config" {
		t.Fatalf("empty qname class = %q", got.ErrorClass)
	}
}

func TestDoHDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			t.Error("the redirect was followed")
			_, _ = io.WriteString(w, `{"Status":0}`)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	doh := probe.NewDoHWithClient(srv.Client())
	got := doh.Do(context.Background(), srv.URL+"/dns-query", "example.com")
	if got.ErrorClass != "doh_http" || got.Status != http.StatusFound {
		t.Fatalf("redirect = (%q, %d), want the 302 returned as doh_http", got.ErrorClass, got.Status)
	}
}

func TestDoHTrimsTheTrailingDot(t *testing.T) {
	t.Parallel()

	// The handler asserts name=example.com, so a trailing dot would fail it.
	srv := dohServer(t, http.StatusOK, `{"Status":0,"Answer":[{"type":1,"data":"203.0.113.9"}]}`)
	doh := probe.NewDoHWithClient(srv.Client())
	if got := doh.Do(context.Background(), srv.URL+"/dns-query", "example.com."); got.Failed() {
		t.Fatalf("a trailing dot failed: %s", got.ErrorClass)
	}
}

func TestDoHAnswerIsCapped(t *testing.T) {
	t.Parallel()

	// A body far larger than the cap is truncated before JSON parsing, which
	// is a stated error rather than an unbounded read.
	huge := `{"Status":0,"padding":"` + strings.Repeat("x", 200<<10) + `"}`
	srv := dohServer(t, http.StatusOK, huge)
	doh := probe.NewDoHWithClient(srv.Client())
	doh.MaxBytes = 1024
	if got := doh.Do(context.Background(), srv.URL+"/dns-query", "example.com"); got.ErrorClass != "doh_error" {
		t.Fatalf("class = %q, want doh_error from the truncated body", got.ErrorClass)
	}
}

// failingRoundTripper makes the transport fail before any socket is opened, so
// the client-error mapping is exercised without a network.
type failingRoundTripper struct{ err error }

func (f failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

func TestDoHTransportErrorIsClassified(t *testing.T) {
	t.Parallel()

	doh := probe.NewDoHWithClient(&http.Client{
		Transport: failingRoundTripper{err: &net.DNSError{Err: "no such host", Name: "dns.example", IsTimeout: true}},
	})
	got := doh.Do(context.Background(), "https://dns.example/dns-query", "example.com")
	if got.ErrorClass == "" {
		t.Fatal("a transport failure had no error class")
	}
	if got.Err == nil {
		t.Fatal("a transport failure had no detail")
	}
}

func TestNewDoHHasProductionDefaults(t *testing.T) {
	t.Parallel()

	doh := probe.NewDoH(netx.New())
	if doh.Client == nil {
		t.Fatal("NewDoH has no client")
	}
	if doh.MaxBytes != probe.DefaultDoHMaxBytes {
		t.Fatalf("MaxBytes = %d", doh.MaxBytes)
	}

	// The production client refuses redirects: the endpoint is configuration,
	// and following one could move the query elsewhere.
	req, err := http.NewRequest(http.MethodGet, "https://dns.example/dns-query", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := doh.Client.CheckRedirect(req, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy = %v, want ErrUseLastResponse", err)
	}
}

func TestDoHRejectsAMalformedURLAndInvalidAnswers(t *testing.T) {
	t.Parallel()

	doh := probe.NewDoHWithClient(&http.Client{})
	if got := doh.Do(context.Background(), "https://[::1", "example.com"); got.ErrorClass != "doh_config" {
		t.Fatalf("malformed URL class = %q, want doh_config", got.ErrorClass)
	}

	// An A record whose data is not an address is no usable answer. MaxBytes
	// <= 0 exercises the default cap rather than skipping the read.
	srv := dohServer(t, http.StatusOK, `{"Status":0,"Answer":[{"type":1,"data":"not-an-ip"}]}`)
	doh = probe.NewDoHWithClient(srv.Client())
	doh.MaxBytes = -1
	if got := doh.Do(context.Background(), srv.URL+"/dns-query", "example.com"); got.ErrorClass != "doh_no_answer" {
		t.Fatalf("invalid answer class = %q, want doh_no_answer", got.ErrorClass)
	}
}
