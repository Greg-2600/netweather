package probe_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/probe"
)

func TestDoCapturesResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("probe sent an Authorization header")
		}
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "netweather") {
			t.Errorf("User-Agent = %q", ua)
		}
		w.Header().Set("X-Health", "green")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "hello")
	}))
	defer srv.Close()

	h := probe.NewHTTPWithTransport(http.DefaultTransport)
	got := h.Do(context.Background(), srv.URL)

	if got.Failed() {
		t.Fatalf("Failed: %s", got)
	}
	if got.Status != http.StatusOK {
		t.Fatalf("status = %d", got.Status)
	}
	if string(got.Body) != "hello" {
		t.Fatalf("body = %q", got.Body)
	}
	if got.Header.Get("X-Health") != "green" {
		t.Fatalf("header not captured")
	}
	if got.FinalURL != srv.URL {
		t.Fatalf("FinalURL = %q", got.FinalURL)
	}
	// Latency is measured on loopback, where a coarse platform timer can read
	// zero. The assertion is that it is not negative, not that it is non-zero.
	if got.Latency < 0 {
		t.Fatalf("latency = %v", got.Latency)
	}
}

func TestDoPreservesErrorStatuses(t *testing.T) {
	t.Parallel()

	for _, status := range []int{404, 429, 500, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))

		got := probe.NewHTTPWithTransport(http.DefaultTransport).Do(context.Background(), srv.URL)
		srv.Close()

		if got.Failed() {
			t.Fatalf("status %d reported as transport failure: %s", status, got)
		}
		if got.Status != status {
			t.Fatalf("status = %d, want %d", got.Status, status)
		}
	}
}

func TestBodyIsCapped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 1<<20))
	}))
	defer srv.Close()

	h := probe.NewHTTPWithTransport(http.DefaultTransport)
	h.MaxBytes = 64

	got := h.Do(context.Background(), srv.URL)
	if len(got.Body) != 64 {
		t.Fatalf("body length = %d, want the 64-byte cap", len(got.Body))
	}
}

func TestRedirectChainRecordsFinalURL(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/b", http.StatusFound)
	})
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/c", http.StatusFound)
	})
	mux.HandleFunc("/c", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "done")
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	got := probe.NewHTTPWithTransport(http.DefaultTransport).Do(context.Background(), srv.URL+"/a")
	if got.Status != http.StatusOK || string(got.Body) != "done" {
		t.Fatalf("got %d %q", got.Status, got.Body)
	}
	if !strings.HasSuffix(got.FinalURL, "/c") {
		t.Fatalf("FinalURL = %q", got.FinalURL)
	}
}

func TestRedirectExhaustionReturnsLastResponse(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/again", http.StatusFound)
	}))
	defer srv.Close()

	h := probe.NewHTTPWithTransport(http.DefaultTransport)
	h.MaxRedirects = 3

	got := h.Do(context.Background(), srv.URL)
	if got.Failed() {
		t.Fatalf("exhaustion should surface the 3xx, not a transport error: %s", got)
	}
	if got.Status != http.StatusFound {
		t.Fatalf("status = %d, want 302", got.Status)
	}
}

func TestHeadRetriesWithGetOn405(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := probe.NewHTTPWithTransport(http.DefaultTransport)
	h.Method = http.MethodHead

	got := h.Do(context.Background(), srv.URL)
	if got.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the GET retry", got.Status)
	}
}

func TestTLSCertificateFailureIsClassified(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	got := probe.NewHTTPWithTransport(http.DefaultTransport).Do(context.Background(), srv.URL)
	if !got.Failed() {
		t.Fatal("an untrusted certificate should fail the probe")
	}
	if got.ErrorClass != "tls_cert_untrusted" {
		t.Fatalf("class = %q, want tls_cert_untrusted (%v)", got.ErrorClass, got.Err)
	}
}

func TestTimeoutIsClassified(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := probe.NewHTTPWithTransport(http.DefaultTransport)
	h.Client.Timeout = 40 * time.Millisecond

	got := h.Do(context.Background(), srv.URL)
	if got.ErrorClass != "http_timeout" {
		t.Fatalf("class = %q, want http_timeout (%v)", got.ErrorClass, got.Err)
	}
}

func TestClassifyError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"nxdomain", &net.DNSError{IsNotFound: true}, "dns_nxdomain"},
		{"dns timeout", &net.DNSError{IsTimeout: true}, "dns_timeout"},
		{"dns other", &net.DNSError{Err: "servfail"}, "dns_error"},
		{"expired", x509.CertificateInvalidError{Reason: x509.Expired}, "tls_cert_expired"},
		{"invalid", x509.CertificateInvalidError{Reason: x509.NotAuthorizedToSign}, "tls_cert_invalid"},
		{"hostname", x509.HostnameError{}, "tls_cert_mismatch"},
		{"authority", x509.UnknownAuthorityError{}, "tls_cert_untrusted"},
		{"record", tls.RecordHeaderError{Msg: "bad record"}, "tls_handshake"},
		{"alert", tls.AlertError(40), "tls_handshake"},
		{"deadline", context.DeadlineExceeded, "http_timeout"},
		{"dial", &net.OpError{Op: "dial"}, "tcp_refused"},
		{"read", &net.OpError{Op: "read"}, "tcp_reset"},
		{"blocked address", netx.ErrBlocked, "policy_blocked"},
		{"blocked port", netx.ErrBlockedPort, "policy_blocked_port"},
		{"other", errors.New("boom"), "http_error"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := probe.ClassifyError(tc.err); got != tc.want {
				t.Fatalf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestTransportDisablesProxy(t *testing.T) {
	t.Parallel()

	tr := probe.Transport(nil)
	if tr.Proxy != nil {
		t.Fatal("HTTP_PROXY must not reroute probes")
	}
	if tr.DialContext == nil {
		t.Fatal("transport has no guarded dialer")
	}
}

func TestDoRejectsInvalidURL(t *testing.T) {
	t.Parallel()

	got := probe.NewHTTPWithTransport(http.DefaultTransport).Do(context.Background(), "://not-a-url")
	if !got.Failed() {
		t.Fatal("an invalid URL must fail")
	}
	if got.ErrorClass != "http_request" {
		t.Fatalf("class = %q, want http_request", got.ErrorClass)
	}
}

func TestDoDefaultsToGETForUnsupportedMethod(t *testing.T) {
	t.Parallel()

	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := probe.NewHTTPWithTransport(http.DefaultTransport)
	h.Method = http.MethodPost // never allowed on the wire

	_ = h.Do(context.Background(), srv.URL)
	if seen != http.MethodGet {
		t.Fatalf("method = %q, want GET", seen)
	}
}

func TestNewHTTPUsesGuardedTransport(t *testing.T) {
	t.Parallel()

	h := probe.NewHTTP(nil)
	if h.Client == nil || h.Client.Transport == nil {
		t.Fatal("NewHTTP did not build a client")
	}
	if h.MaxBytes != probe.DefaultMaxBytes {
		t.Fatalf("MaxBytes = %d", h.MaxBytes)
	}
	if h.MaxRedirects != probe.DefaultMaxRedirects {
		t.Fatalf("MaxRedirects = %d", h.MaxRedirects)
	}
	if h.Client.Timeout != probe.DefaultTimeout {
		t.Fatalf("Timeout = %v", h.Client.Timeout)
	}
}

func TestNewHTTPWithDial(t *testing.T) {
	t.Parallel()

	h := probe.NewHTTPWithDial(nil)
	if h.Client == nil || h.Client.Transport == nil {
		t.Fatal("NewHTTPWithDial did not build a client")
	}
	tr, ok := h.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", h.Client.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("a custom dialer must not pick up HTTP_PROXY")
	}
	if tr.DialContext == nil {
		t.Fatal("transport has no dial function")
	}
	if h.Client.Timeout != probe.DefaultTimeout {
		t.Fatalf("Timeout = %v", h.Client.Timeout)
	}
}

func TestHTTPUsesTheInjectedDialer(t *testing.T) {
	t.Parallel()

	called := make(chan string, 1)
	h := probe.NewHTTPWithDial(func(_ context.Context, _, address string) (net.Conn, error) {
		called <- address
		return nil, errors.New("dial refused by test")
	})

	res := h.Do(context.Background(), "http://example.com/")
	if !res.Failed() {
		t.Fatal("a failing dial should fail the probe")
	}
	select {
	case addr := <-called:
		if !strings.HasPrefix(addr, "example.com:") {
			t.Fatalf("dial address = %q, want the target host", addr)
		}
	case <-time.After(time.Second):
		t.Fatal("the injected dialer was never used")
	}
}

func TestResultString(t *testing.T) {
	t.Parallel()

	ok := probe.Result{Status: 200, FinalURL: "https://example.com", Latency: time.Millisecond}
	if !strings.Contains(ok.String(), "200") {
		t.Fatalf("ok.String() = %q", ok.String())
	}

	bad := probe.Result{ErrorClass: "tcp_refused", Err: errors.New("refused")}
	if !strings.Contains(bad.String(), "tcp_refused") {
		t.Fatalf("bad.String() = %q", bad.String())
	}
}

func TestBuildUserAgent(t *testing.T) {
	t.Parallel()

	got := probe.BuildUserAgent("1.2.3", "abuse@example.net")
	want := "netweather/1.2.3 (+https://github.com/Greg-2600/netweather; abuse: abuse@example.net)"
	if got != want {
		t.Fatalf("user agent = %q, want %q", got, want)
	}

	// An absent contact is shown, not dropped: that is the thing the operator
	// has to notice.
	if got := probe.BuildUserAgent("", ""); !strings.Contains(got, "<contact>") ||
		!strings.Contains(got, "netweather/dev") {
		t.Fatalf("unattributed user agent = %q", got)
	}
}

func TestHTTPUsesTheConfiguredUserAgent(t *testing.T) {
	t.Parallel()

	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := probe.NewHTTPWithTransport(http.DefaultTransport)
	h.UserAgent = "netweather/9.9.9 (+https://example.net; abuse: ops@example.net)"
	if res := h.Do(context.Background(), srv.URL); res.Failed() {
		t.Fatalf("probe failed: %v", res.Err)
	}
	if seen != h.UserAgent {
		t.Fatalf("User-Agent = %q, want %q", seen, h.UserAgent)
	}

	// Empty keeps the documented default rather than sending an empty header.
	seen = ""
	h.UserAgent = ""
	if res := h.Do(context.Background(), srv.URL); res.Failed() {
		t.Fatalf("probe failed: %v", res.Err)
	}
	if seen != probe.DefaultUserAgent {
		t.Fatalf("default User-Agent = %q", seen)
	}
}

func TestRedirectLimitFromContextScopesOneExchange(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/b", http.StatusFound)
	})
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/c", http.StatusFound)
	})
	mux.HandleFunc("/c", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "done")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	h := probe.NewHTTPWithTransport(http.DefaultTransport)
	h.MaxRedirects = 5

	// A one-hop budget returns the first 3xx rather than following to /c.
	got := h.Do(probe.WithRedirectLimit(context.Background(), 1), srv.URL+"/a")
	if got.Failed() {
		t.Fatalf("exhaustion surfaced a transport error: %s", got)
	}
	if got.Status != http.StatusFound {
		t.Fatalf("status with a one-hop budget = %d, want 302", got.Status)
	}

	// The shared instrument keeps its own budget: the context limit is scoped
	// to one exchange.
	if h.MaxRedirects != 5 {
		t.Fatalf("instrument budget = %d after a scoped call, want 5", h.MaxRedirects)
	}
	if got := h.Do(context.Background(), srv.URL+"/a"); got.Status != http.StatusOK {
		t.Fatalf("status with the instrument budget = %d, want 200", got.Status)
	}
}

func TestHeadersOnlyAsksForASmallRange(t *testing.T) {
	t.Parallel()

	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Range")
		if seen != "" {
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = io.WriteString(w, strings.Repeat("x", 4096))
	}))
	defer srv.Close()

	h := probe.NewHTTPWithTransport(http.DefaultTransport)

	// A normal exchange asks for no range and reads the full bounded body.
	got := h.Do(context.Background(), srv.URL)
	if seen != "" {
		t.Fatalf("a normal probe sent Range %q", seen)
	}
	if got.Status != http.StatusOK || len(got.Body) != 4096 {
		t.Fatalf("normal = %d, %d bytes", got.Status, len(got.Body))
	}

	// Headers-only asks for 2 KiB and accepts the 206 that earns.
	seen = ""
	got = h.Do(probe.WithHeadersOnly(context.Background()), srv.URL)
	if seen != "bytes=0-2047" {
		t.Fatalf("Range = %q, want bytes=0-2047", seen)
	}
	if got.Status != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", got.Status)
	}
	if len(got.Body) > 2048 {
		t.Fatalf("headers-only read %d bytes, want at most 2048", len(got.Body))
	}
}
