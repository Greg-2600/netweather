package web_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/web"
)

type cookieOpts struct {
	tls        bool
	secure     bool
	trusted    []netip.Prefix
	requestHdr http.Header
}

// serverWithCookieOpts builds a web server with the cookie-related settings a
// deployment can vary, and returns a response to GET /login (which sets the
// CSRF cookie through the same Secure decision the session cookie uses).
func loginCookie(t *testing.T, opts cookieOpts) *http.Cookie {
	t.Helper()

	m := fixture()
	h, err := web.New(web.Deps{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:         m,
		Watcher:        m,
		Token:          testToken,
		SecureCookies:  opts.secure,
		TrustedProxies: opts.trusted,
		Version:        "test",
		StartedAt:      time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	var ts *httptest.Server
	if opts.tls {
		ts = httptest.NewTLSServer(h)
	} else {
		ts = httptest.NewServer(h)
	}
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, values := range opts.requestHdr {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	client := ts.Client()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	for _, c := range resp.Cookies() {
		if c.Name == "nw_csrf" {
			return c
		}
	}
	t.Fatalf("no CSRF cookie in the login response (status %d)", resp.StatusCode)
	return nil
}

// TestCookieSecureFollowsTheRequestScheme pins NETWORKING.md §7.1: the cookie
// is Secure whenever the browser's effective scheme is HTTPS, and not on a
// plaintext connection where the browser would refuse to send it back.
func TestCookieSecureFollowsTheRequestScheme(t *testing.T) {
	t.Parallel()

	if c := loginCookie(t, cookieOpts{}); c.Secure {
		t.Error("a plaintext response set a Secure cookie")
	}
	if c := loginCookie(t, cookieOpts{tls: true}); !c.Secure {
		t.Error("an HTTPS response did not set a Secure cookie")
	}

	// A TLS-terminating proxy is believed only from a configured peer, and its
	// X-Forwarded-Proto: https counts as the browser's scheme.
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	hdr := http.Header{"X-Forwarded-Proto": []string{"https"}}
	if c := loginCookie(t, cookieOpts{trusted: trusted, requestHdr: hdr}); !c.Secure {
		t.Error("a trusted https proxy did not produce a Secure cookie")
	}

	// The same header from an untrusted peer is not evidence.
	if c := loginCookie(t, cookieOpts{requestHdr: hdr}); c.Secure {
		t.Error("an untrusted X-Forwarded-Proto was believed")
	}

	// NETWEATHER_UI_SECURE_COOKIES=true forces it, which is the operator's
	// explicit choice behind a proxy that cannot be detected.
	if c := loginCookie(t, cookieOpts{secure: true}); !c.Secure {
		t.Error("the SecureCookies override did not apply")
	}
}
