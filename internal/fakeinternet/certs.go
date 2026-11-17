// Package fakeinternet stands up a deterministic, offline internet for tests
// (QUALITY.md §5.3): a local CA, one HTTPS listener that answers per SNI, a
// UDP resolver, a SOCKS5 forwarder, and controllable clock and egress
// endpoints.
//
// It exists so the real instruments — the guarded dialer, crypto/tls, the HTTP
// client, the DNS wire client — are exercised end to end instead of being
// replaced by hand-built Result values. Nothing here mocks netweather's own
// code; the seams are the dialer and the clock, which are already design
// requirements.
package fakeinternet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"
)

// tlsMode is how a scenario's leaf certificate is deliberately broken.
type tlsMode string

const (
	tlsValid     tlsMode = ""
	tlsExpired   tlsMode = "expired"
	tlsMismatch  tlsMode = "mismatch"
	tlsUntrusted tlsMode = "untrusted"
)

// authority is a self-signed CA that can sign leaves for the fake hosts.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// newAuthority generates a CA valid from an hour before now until a year later.
func newAuthority(now time.Time) (*authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("fakeinternet: ca key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "netweather fake internet CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("fakeinternet: ca certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("fakeinternet: parse ca: %w", err)
	}
	return &authority{cert: cert, key: key}, nil
}

// rootPool returns a pool that trusts this CA.
func (a *authority) rootPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	return pool
}

// leaf issues a certificate for host with the requested deliberate defect.
//
// The SAN is the host for a valid leaf; "mismatch" instead names a different
// host, which is exactly the certificate-error path the classifier must
// recognise.
func (a *authority) leaf(host string, mode tlsMode, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("fakeinternet: leaf key for %s: %w", host, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}

	sanHost := host
	notBefore := now.Add(-time.Hour)
	notAfter := now.Add(24 * time.Hour)
	switch mode {
	case tlsExpired:
		notBefore = now.Add(-48 * time.Hour)
		notAfter = now.Add(-24 * time.Hour)
	case tlsMismatch:
		sanHost = "not-" + host
	case tlsValid, tlsUntrusted:
		// A valid leaf and an untrusted one differ only in the signing CA,
		// handled below.
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: sanHost},
		DNSNames:     []string{sanHost},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	// An untrusted leaf is real: it is signed by a CA the client does not
	// trust, not marked so by a flag.
	parent, signer := a.cert, a.key
	if mode == tlsUntrusted {
		other, err := newAuthority(now)
		if err != nil {
			return tls.Certificate{}, err
		}
		parent, signer = other.cert, other.key
	}

	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, signer)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("fakeinternet: leaf for %s: %w", host, err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("fakeinternet: serial: %w", err)
	}
	return serial, nil
}
