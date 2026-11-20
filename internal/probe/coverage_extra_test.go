package probe_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/probe"
)

// timeoutError is a net.Error whose Timeout method is true, covering the
// classifier branch that context.DeadlineExceeded alone does not reach.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// failingTransport returns a response whose body fails on the first read. It
// exercises the read-error branches without opening a socket.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       failingBody{},
	}, nil
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("body read failed") }
func (failingBody) Close() error             { return nil }

func TestTCPZeroValueInstallsTheDefaultDialer(t *testing.T) {
	t.Parallel()

	// A malformed port fails inside net.Dialer before any connection is made.
	got := (&probe.TCP{}).Do(t.Context(), "127.0.0.1:not-a-port")
	if got.Err == nil {
		t.Fatal("the default dialer did not report an error")
	}
}

func TestTLSZeroValueInstallsTheDefaultDialer(t *testing.T) {
	t.Parallel()

	got := (&probe.TLS{}).Do(t.Context(), "127.0.0.1:not-a-port")
	if got.Err == nil || got.ErrorClass == "" {
		t.Fatalf("default dialer result = %+v", got)
	}
}

func TestClassifyDialErrorRecognisesANetTimeout(t *testing.T) {
	t.Parallel()

	err := &net.OpError{Op: "dial", Err: timeoutError{}}
	if got := probe.ClassifyDialError(err); got != "tcp_timeout" {
		t.Fatalf("class = %q, want tcp_timeout", got)
	}
	if got := probe.ClassifyError(timeoutError{}); got != "http_timeout" {
		t.Fatalf("ClassifyError class = %q, want http_timeout", got)
	}
}

func TestICMPNeedsAHost(t *testing.T) {
	t.Parallel()

	got := probe.NewICMP().Do(context.Background(), "")
	if got.ErrorClass != "icmp_config" || got.Err == nil {
		t.Fatalf("class = %q, want icmp_config (%v)", got.ErrorClass, got.Err)
	}
}

func TestHTTPContextHelpersCoverTheirDefaults(t *testing.T) {
	t.Parallel()

	// A non-positive redirect budget leaves the instrument's own limit alone.
	ctx := probe.WithRedirectLimit(t.Context(), 0)
	if got := probe.WithHeadersOnly(ctx); got == nil {
		t.Fatal("WithHeadersOnly returned a nil context")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	t.Cleanup(srv.Close)

	h := probe.NewHTTPWithTransport(http.DefaultTransport)
	h.MaxBytes = 0 // headers-only must install its own 2 KiB cap
	got := h.Do(probe.WithHeadersOnly(ctx), srv.URL)
	if got.Status != http.StatusOK || len(got.Body) > 2048 {
		t.Fatalf("headers-only = %d, %d bytes", got.Status, len(got.Body))
	}
}

func TestDoHWithoutAClient(t *testing.T) {
	// t.Setenv forbids t.Parallel; clearing the proxy keeps the fallback
	// client strictly on loopback.
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")

	doh := &probe.DoH{}
	// A config failure never reaches the client, but a zero-value instrument
	// must still be safe to call.
	if got := doh.Do(t.Context(), "", "example.com"); got.ErrorClass != "doh_config" {
		t.Fatalf("class = %q, want doh_config", got.ErrorClass)
	}

	// A valid HTTPS URL with no client installed falls back to
	// http.DefaultClient. Port 1 on loopback refuses the connection, so this
	// exercises the default-client path without leaving the host.
	got := doh.Do(t.Context(), "https://127.0.0.1:1/dns-query", "example.com")
	if got.Err == nil || got.ErrorClass == "" {
		t.Fatalf("client failure = %+v", got)
	}
}

func TestHTTPReportsABodyReadFailure(t *testing.T) {
	t.Parallel()

	h := probe.NewHTTPWithTransport(failingTransport{})
	got := h.Do(t.Context(), "https://example.test/")
	if got.Status != http.StatusOK || got.Err == nil || got.ErrorClass == "" {
		t.Fatalf("result = %+v", got)
	}
}

func TestDoHReportsABodyReadFailure(t *testing.T) {
	t.Parallel()

	doh := probe.NewDoHWithClient(&http.Client{Transport: failingTransport{}})
	got := doh.Do(t.Context(), "https://example.test/dns-query", "example.com")
	if got.ErrorClass != "doh_error" || got.Err == nil {
		t.Fatalf("result = %+v", got)
	}
}
