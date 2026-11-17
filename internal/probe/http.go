// Package probe holds the instruments. This first one is HTTP: it builds the
// request, bounds the response, and turns transport failures into the error
// classes the classifier understands.
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/Greg-2600/netweather/internal/netx"
)

const (
	// DefaultMaxBytes is the response cap from CLASSIFIER.md §4. The classifier
	// only needs a prefix; storing or reading more is how a monitor becomes a
	// scraper.
	DefaultMaxBytes int64 = 32 << 10
	// DefaultMaxRedirects is the redirect budget from DESIGN.md §14.
	DefaultMaxRedirects = 5
	// DefaultTimeout bounds one HTTP exchange end to end.
	DefaultTimeout = 15 * time.Second

	userAgent = "netweather (+https://github.com/Greg-2600/netweather)"
)

// Result is one HTTP observation, already bounded and timed.
type Result struct {
	Status     int
	Header     http.Header
	Body       []byte
	FinalURL   string
	Latency    time.Duration
	ErrorClass string
	Err        error
}

// Failed reports whether the exchange produced a transport-class failure.
func (r Result) Failed() bool { return r.Err != nil || r.ErrorClass != "" }

// HTTP is the HTTP instrument. Client, MaxBytes and MaxRedirects are injectable
// so tests can use a local server without weakening the production dial path.
type HTTP struct {
	Client       *http.Client
	MaxBytes     int64
	MaxRedirects int
	Method       string
}

// NewHTTP builds the production instrument: every connection goes through the
// netx dialer, so the SSRF policy cannot be bypassed by a redirect.
func NewHTTP(dialer *netx.Dialer) *HTTP {
	return NewHTTPWithTransport(Transport(dialer))
}

// NewHTTPWithTransport builds an instrument over an arbitrary transport. Tests
// use it to reach httptest servers; production should use NewHTTP.
func NewHTTPWithTransport(rt http.RoundTripper) *HTTP {
	h := &HTTP{
		MaxBytes:     DefaultMaxBytes,
		MaxRedirects: DefaultMaxRedirects,
		Method:       http.MethodGet,
	}
	h.Client = &http.Client{
		Transport:     rt,
		Timeout:       DefaultTimeout,
		CheckRedirect: h.checkRedirect,
	}
	return h
}

// Transport wraps the guarded dialer in an HTTP transport. Proxy is set to nil
// explicitly: an HTTP_PROXY in the environment must not silently reroute probes
// (THREAT_MODEL.md T31).
func Transport(d *netx.Dialer) *http.Transport {
	if d == nil {
		d = netx.New()
	}
	return TransportForDial(d.DialContext)
}

// TransportForDial builds the same transport over an arbitrary dial function.
// The Tor vantage uses this: identical HTTP behaviour, different last hop.
func TransportForDial(dial DialFunc) *http.Transport {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dial,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// NewHTTPWithDial returns an HTTP instrument over dial. Production uses NewHTTP;
// the Tor vantage uses this with a SOCKS5 dialer.
func NewHTTPWithDial(dial DialFunc) *HTTP {
	return NewHTTPWithTransport(TransportForDial(dial))
}

func (h *HTTP) checkRedirect(_ *http.Request, via []*http.Request) error {
	if len(via) >= h.MaxRedirects {
		// Return the last 3xx rather than an error: the classifier sees the
		// status and can say "redirects exhausted" instead of "network failure".
		return http.ErrUseLastResponse
	}
	return nil
}

// Do performs one GET (or HEAD) and never returns an error: failures are in
// Result so the caller has one code path.
func (h *HTTP) Do(ctx context.Context, target string) Result {
	start := time.Now()
	res := Result{}

	method := h.Method
	if method != http.MethodGet && method != http.MethodHead {
		method = http.MethodGet
	}

	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		res.Latency = time.Since(start)
		// Construction failure is our configuration, not the target's health.
		res.ErrorClass = "http_request"
		res.Err = err
		return res
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := h.Client.Do(req)
	res.Latency = time.Since(start)
	if err != nil {
		res.ErrorClass = ClassifyError(err)
		res.Err = err
		return res
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, h.MaxBytes))
	if err != nil {
		res.ErrorClass = ClassifyError(err)
		res.Err = err
		res.Status = resp.StatusCode
		res.Header = resp.Header
		return res
	}

	res.Status = resp.StatusCode
	res.Header = resp.Header
	res.Body = body
	res.FinalURL = resp.Request.URL.String()

	// HEAD is often rejected by servers that answer GET. The spec says retry
	// once with GET, then treat 405 as a config error (CLASSIFIER.md §5.2).
	if method == http.MethodHead && resp.StatusCode == http.StatusMethodNotAllowed {
		retry := *h
		retry.Method = http.MethodGet
		return retry.Do(ctx, target)
	}
	return res
}

// ClassifyError maps a Go transport error to the stable class strings the
// classifier consumes. The order matters: x509 and TLS errors wrap net errors.
func ClassifyError(err error) string {
	if err == nil {
		return ""
	}

	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		if invalid.Reason == x509.Expired {
			return "tls_cert_expired"
		}
		return "tls_cert_invalid"
	}

	// crypto/tls wraps verification failures in this type (Go 1.20+); unwrap it
	// so the x509 cases below can classify the real cause.
	var verification *tls.CertificateVerificationError
	if errors.As(err, &verification) {
		return ClassifyError(verification.Err)
	}

	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return "tls_cert_mismatch"
	}

	var authority x509.UnknownAuthorityError
	if errors.As(err, &authority) {
		return "tls_cert_untrusted"
	}

	var record tls.RecordHeaderError
	if errors.As(err, &record) {
		return "tls_handshake"
	}

	var alert tls.AlertError
	if errors.As(err, &alert) {
		return "tls_handshake"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return "dns_nxdomain"
		case dnsErr.IsTimeout:
			return "dns_timeout"
		default:
			return "dns_error"
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return "http_timeout"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "http_timeout"
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		switch opErr.Op {
		case "dial":
			return "tcp_refused"
		case "read", "write":
			return "tcp_reset"
		}
	}

	return "http_error"
}

// String is for logs: class plus the underlying error without the request dump.
func (r Result) String() string {
	if !r.Failed() {
		return fmt.Sprintf("%d %s in %s", r.Status, r.FinalURL, r.Latency.Round(time.Millisecond))
	}
	return fmt.Sprintf("%s: %v", r.ErrorClass, r.Err)
}
