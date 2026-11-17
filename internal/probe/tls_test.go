package probe_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

// selfSignedFor builds a self-signed certificate usable as both the leaf and
// a trusted root, so a chain can be assembled without a CA file.
func selfSignedFor(t *testing.T, commonName string, ips []net.IP) (tls.Certificate, *x509.Certificate) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           ips,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, cert
}

func TestTLSCapturesTheChainShape(t *testing.T) {
	t.Parallel()

	leaf, leafCert := selfSignedFor(t, "leaf", []net.IP{net.ParseIP("127.0.0.1")})
	intermediate, intermediateCert := selfSignedFor(t, "intermediate", nil)
	// The server sends a second certificate, so the chain is visibly two long.
	leaf.Certificate = append(leaf.Certificate, intermediate.Certificate[0])

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{leaf},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = tls.Server(c, &tls.Config{
					MinVersion:   tls.VersionTLS12,
					Certificates: []tls.Certificate{leaf},
				}).Handshake()
			}(conn)
		}
	}()

	pool := x509.NewCertPool()
	pool.AddCert(leafCert)

	p := &probe.TLS{Dial: (&net.Dialer{}).DialContext, Config: &tls.Config{RootCAs: pool}}
	res := p.Do(context.Background(), ln.Addr().String())
	if res.Failed() {
		t.Fatalf("handshake failed: %s: %v", res.ErrorClass, res.Err)
	}
	if res.ChainLength != 2 {
		t.Fatalf("chain length = %d, want 2", res.ChainLength)
	}

	der, err := x509.MarshalPKIXPublicKey(intermediateCert.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	if res.IntermediateSPKI != hex.EncodeToString(sum[:]) {
		t.Fatalf("intermediate spki = %q, want the signed intermediate's hash", res.IntermediateSPKI)
	}

	// A single-certificate handshake reports length one and no intermediate,
	// which is a real answer rather than a missing measurement.
	single := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer single.Close()
	pool = x509.NewCertPool()
	pool.AddCert(single.Certificate())
	p = &probe.TLS{Dial: (&net.Dialer{}).DialContext, Config: &tls.Config{RootCAs: pool}}
	res = p.Do(context.Background(), single.Listener.Addr().String())
	if res.Failed() {
		t.Fatalf("single-cert handshake failed: %s: %v", res.ErrorClass, res.Err)
	}
	if res.ChainLength != 1 || res.IntermediateSPKI != "" {
		t.Fatalf("single chain = (%d, %q), want (1, empty)", res.ChainLength, res.IntermediateSPKI)
	}
}
