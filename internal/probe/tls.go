package probe

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net"
	"time"

	"github.com/Greg-2600/netweather/internal/netx"
)

// TLS performs a handshake and reads the certificate. It never sends
// application data, and it never disables verification.
type TLS struct {
	Dial DialFunc
	// Config is for tests and for a future custom root store. ServerName is
	// always overwritten from the target address.
	Config *tls.Config
}

// NewTLS returns a TLS instrument over the guarded dialer.
func NewTLS(d *netx.Dialer) *TLS {
	if d == nil {
		d = netx.New()
	}
	return &TLS{Dial: d.DialContext}
}

// NewTLSWithDial returns a TLS instrument over an arbitrary dialer, used by the
// Tor vantage.
func NewTLSWithDial(dial DialFunc) *TLS {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	return &TLS{Dial: dial}
}

// TLSResult is one handshake observation.
type TLSResult struct {
	Latency     time.Duration
	Certificate *x509.Certificate
	Issuer      string
	NotAfter    time.Time
	// ChainLength is how many certificates the server sent, including the leaf.
	// It is a weak tamper signal and informational only (CLASSIFIER.md §3.5).
	ChainLength int
	// IntermediateSPKI is the SHA-256 hex of the first intermediate's DER
	// subjectPublicKeyInfo. Empty when the server sent only a leaf.
	IntermediateSPKI string
	// LeafFingerprint is the SHA-256 hex of the leaf certificate's DER bytes.
	// A change is informational: leaves rotate on a schedule (CLASSIFIER.md §3.5).
	LeafFingerprint string
	ErrorClass      string
	Err             error
}

// Failed reports a transport-class failure.
func (r TLSResult) Failed() bool { return r.Err != nil || r.ErrorClass != "" }

// Do dials, handshakes, records the leaf certificate, and closes.
func (p *TLS) Do(ctx context.Context, address string) TLSResult {
	start := time.Now()

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return TLSResult{Latency: time.Since(start), ErrorClass: "tls_config", Err: err}
	}

	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}

	conn, err := dial(ctx, "tcp", address)
	if err != nil {
		return TLSResult{Latency: time.Since(start), ErrorClass: ClassifyDialError(err), Err: err}
	}
	defer func() { _ = conn.Close() }()

	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if p.Config != nil {
		config = p.Config.Clone()
		config.ServerName = host
		if config.MinVersion == 0 {
			config.MinVersion = tls.VersionTLS12
		}
	}

	client := tls.Client(conn, config)
	if err := client.HandshakeContext(ctx); err != nil {
		return TLSResult{Latency: time.Since(start), ErrorClass: ClassifyError(err), Err: err}
	}

	res := TLSResult{Latency: time.Since(start)}
	state := client.ConnectionState()
	res.ChainLength = len(state.PeerCertificates)
	if len(state.PeerCertificates) > 0 {
		leaf := state.PeerCertificates[0]
		res.Certificate = leaf
		res.Issuer = leaf.Issuer.String()
		res.NotAfter = leaf.NotAfter
		// The leaf rotates on a schedule, so its fingerprint is recorded and
		// reported, never used as a pin (CLASSIFIER.md §3.5 step 4).
		sum := sha256.Sum256(leaf.Raw)
		res.LeafFingerprint = hex.EncodeToString(sum[:])
	}
	if len(state.PeerCertificates) > 1 {
		// The first intermediate is the one that actually signs the leaf, so it
		// is the meaningful part of the chain to remember. A chain that changes
		// shape is recorded and reported, never treated as proof of tampering.
		if der, err := x509.MarshalPKIXPublicKey(state.PeerCertificates[1].PublicKey); err == nil {
			sum := sha256.Sum256(der)
			res.IntermediateSPKI = hex.EncodeToString(sum[:])
		}
	}
	return res
}
