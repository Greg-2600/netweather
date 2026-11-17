package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

// TLSResult is one handshake observation.
type TLSResult struct {
	Latency     time.Duration
	Certificate *x509.Certificate
	Issuer      string
	NotAfter    time.Time
	ErrorClass  string
	Err         error
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
	if len(state.PeerCertificates) > 0 {
		leaf := state.PeerCertificates[0]
		res.Certificate = leaf
		res.Issuer = leaf.Issuer.String()
		res.NotAfter = leaf.NotAfter
	}
	return res
}
