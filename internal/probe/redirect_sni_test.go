package probe_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"

	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/probe"
)

// TestTransportPreservesSNIWhenTheDialGoesElsewhere pins THREAT_MODEL.md
// invariant 3 for the HTTP instrument: the connection is made to the address
// the dial function chose (the validated IP, in production) while the TLS
// handshake still names the configured host.
func TestTransportPreservesSNIWhenTheDialGoesElsewhere(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	seen := ""
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		seen = hello.ServerName
		mu.Unlock()
		return nil, nil
	}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	serverAddr := srv.Listener.Addr().String()
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, serverAddr)
	}

	tr := probe.TransportForDial(dial)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr.TLSClientConfig.RootCAs = pool

	res := probe.NewHTTPWithTransport(tr).Do(context.Background(), "https://example.com/")
	if res.Failed() {
		t.Fatalf("probe failed: %s: %v", res.ErrorClass, res.Err)
	}

	mu.Lock()
	got := seen
	mu.Unlock()
	if got != "example.com" {
		t.Fatalf("SNI = %q, want example.com while the dial went to %s", got, serverAddr)
	}
}

// TestRedirectToABlockedDestinationFailsClosed pins invariant 4: every redirect
// hop is dialled through the guarded function, so a hop the guard refuses ends
// the probe as a policy failure rather than following it.
func TestRedirectToABlockedDestinationFailsClosed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/private":
			http.Redirect(w, r, "http://blocked.example/", http.StatusFound)
		case "/port":
			http.Redirect(w, r, "http://blocked.example:22/", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if host == "blocked.example" {
			if port == "22" {
				return nil, &netx.BlockedPortError{Port: 22}
			}
			return nil, &netx.BlockedError{Addr: netip.MustParseAddr("10.0.0.5"), Reason: "private"}
		}
		var d net.Dialer
		return d.DialContext(ctx, network, address)
	}

	cases := []struct {
		path  string
		class string
	}{
		{"/private", "policy_blocked"},
		{"/port", "policy_blocked_port"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			res := probe.NewHTTPWithTransport(probe.TransportForDial(dial)).
				Do(context.Background(), srv.URL+tc.path)
			if !res.Failed() {
				t.Fatalf("a redirect to a blocked destination succeeded: %+v", res)
			}
			if res.ErrorClass != tc.class {
				t.Fatalf("class = %q, want %q (%v)", res.ErrorClass, tc.class, res.Err)
			}
		})
	}

	// A non-HTTP scheme is refused by the client before another dial. The test
	// only needs it to fail closed; the class is the client's, not the policy's.
	t.Run("unsupported scheme", func(t *testing.T) {
		t.Parallel()

		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "ftp://example.test/", http.StatusFound)
		}))
		t.Cleanup(redirect.Close)

		res := probe.NewHTTPWithTransport(probe.TransportForDial(dial)).
			Do(context.Background(), redirect.URL)
		if !res.Failed() {
			t.Fatalf("a redirect to a non-HTTP scheme succeeded: %+v", res)
		}
	})
}
