package probe_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Greg-2600/netweather/internal/probe"
)

func TestTLSReadsTheCertificate(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	p := &probe.TLS{
		Dial:   (&net.Dialer{}).DialContext,
		Config: &tls.Config{RootCAs: pool},
	}

	res := p.Do(context.Background(), srv.Listener.Addr().String())
	if res.Failed() {
		t.Fatalf("Failed: %v (%s)", res.Err, res.ErrorClass)
	}
	if res.Certificate == nil {
		t.Fatal("no certificate recorded")
	}
	if res.Issuer == "" {
		t.Fatal("issuer empty")
	}
	if res.NotAfter.IsZero() {
		t.Fatal("NotAfter zero")
	}
	if res.Latency <= 0 {
		t.Fatal("latency not measured")
	}
}

func TestTLSRejectsUntrustedCertificate(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the rejection is the point of the test
	defer srv.Close()

	p := &probe.TLS{Dial: (&net.Dialer{}).DialContext}
	res := p.Do(context.Background(), srv.Listener.Addr().String())

	if !res.Failed() {
		t.Fatal("an untrusted certificate must fail")
	}
	if res.ErrorClass != "tls_cert_untrusted" {
		t.Fatalf("class = %q, want tls_cert_untrusted (%v)", res.ErrorClass, res.Err)
	}
}

func TestTLSClassifiesDialFailure(t *testing.T) {
	t.Parallel()

	p := &probe.TLS{Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: errors.New("refused")}
	}}

	res := p.Do(context.Background(), "example.com:443")
	if res.ErrorClass != "tcp_refused" {
		t.Fatalf("class = %q", res.ErrorClass)
	}
}

func TestTLSRejectsAnAddressWithoutPort(t *testing.T) {
	t.Parallel()

	p := &probe.TLS{}
	res := p.Do(context.Background(), "example.com")
	if res.ErrorClass != "tls_config" {
		t.Fatalf("class = %q", res.ErrorClass)
	}
}

func TestTLSConstructorUsesDefaults(t *testing.T) {
	t.Parallel()

	p := probe.NewTLS(nil)
	if p.Dial == nil {
		t.Fatal("NewTLS left Dial nil without installing a default")
	}

	withDial := probe.NewTLSWithDial(nil)
	if withDial.Dial == nil {
		t.Fatal("NewTLSWithDial left Dial nil without installing a default")
	}
}
