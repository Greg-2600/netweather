package probe

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DoT is the DNS-over-TLS instrument: a DNS query carried over a TLS
// connection to port 853 (DESIGN.md §14). It answers "does this name resolve
// over an encrypted channel?" and reuses the DNS wire format and classifier.
type DoT struct {
	Dial    DialFunc
	Timeout time.Duration
	// TLSConfig overrides the client TLS configuration. Nil uses a secure
	// default (system roots, ServerName from the target, TLS 1.2 floor).
	TLSConfig *tls.Config
}

// NewDoT builds the production instrument through the guarded dialer, so the
// endpoint cannot resolve to a private address and the port allowlist applies.
func NewDoT(d DialFunc) *DoT {
	if d == nil {
		d = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	return &DoT{Dial: d, Timeout: 5 * time.Second}
}

// Do queries name's A records against server ("host:853") over TLS.
// Do queries name's A records against server ("host:853") over TLS.
func (p *DoT) Do(ctx context.Context, server, name string) DNSResult {
	return p.do(ctx, server, name, dnsmessage.TypeA)
}

// Do6 queries name's AAAA records against server ("host:853") over TLS.
func (p *DoT) Do6(ctx context.Context, server, name string) DNSResult {
	return p.do(ctx, server, name, dnsmessage.TypeAAAA)
}

func (p *DoT) do(ctx context.Context, server, name string, qtype dnsmessage.Type) DNSResult {
	start := time.Now()
	out := DNSResult{}

	if server == "" || name == "" {
		out.Latency = time.Since(start)
		out.ErrorClass = "dot_config"
		out.Err = fmt.Errorf("probe: dot needs a server and a qname")
		return out
	}

	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	qname, err := dnsmessage.NewName(dnsName(name))
	if err != nil {
		out.Latency = time.Since(start)
		out.ErrorClass = "dot_config"
		out.Err = fmt.Errorf("probe: qname %q: %w", name, err)
		return out
	}
	query, err := packQueryType(qname, qtype)
	if err != nil {
		out.Latency = time.Since(start)
		out.ErrorClass = "dot_config"
		out.Err = err
		return out
	}

	conn, err := dial(lookupCtx, "tcp", server)
	if err != nil {
		out.Latency = time.Since(start)
		out.ErrorClass = classifyExchangeError(err)
		if out.ErrorClass == "dns_timeout" {
			out.ErrorClass = "dot_timeout"
		} else if out.ErrorClass == "dns_error" {
			out.ErrorClass = "dot_error"
		}
		out.Err = err
		return out
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := lookupCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	host, _, err := net.SplitHostPort(server)
	if err != nil {
		host = server
	}
	tlsCfg := p.TLSConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	} else {
		if tlsCfg.ServerName == "" {
			tlsCfg.ServerName = host
		}
		if tlsCfg.MinVersion == 0 {
			tlsCfg.MinVersion = tls.VersionTLS12
		}
	}
	tlsConn := tls.Client(conn, tlsCfg)
	if err := tlsConn.HandshakeContext(lookupCtx); err != nil {
		out.Latency = time.Since(start)
		out.ErrorClass = "dot_tls"
		out.Err = err
		return out
	}
	defer func() { _ = tlsConn.Close() }()
	if deadline, ok := lookupCtx.Deadline(); ok {
		_ = tlsConn.SetDeadline(deadline)
	}

	if len(query) > 0xffff {
		out.Latency = time.Since(start)
		out.ErrorClass = "dot_config"
		out.Err = fmt.Errorf("probe: dot query is %d bytes, over the TCP frame limit", len(query))
		return out
	}
	framed := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(framed[:2], uint16(len(query))) // #nosec G115 -- bounded above.
	copy(framed[2:], query)
	if _, err := tlsConn.Write(framed); err != nil {
		out.Latency = time.Since(start)
		out.ErrorClass = "dot_error"
		out.Err = err
		return out
	}

	var size [2]byte
	if _, err := readFull(tlsConn, size[:]); err != nil {
		out.Latency = time.Since(start)
		out.ErrorClass = classifyExchangeError(err)
		if out.ErrorClass == "dns_timeout" {
			out.ErrorClass = "dot_timeout"
		} else if out.ErrorClass == "dns_error" {
			out.ErrorClass = "dot_error"
		}
		out.Err = err
		return out
	}
	length := int(binary.BigEndian.Uint16(size[:]))
	raw := make([]byte, length)
	if _, err := readFull(tlsConn, raw); err != nil {
		out.Latency = time.Since(start)
		out.ErrorClass = classifyExchangeError(err)
		if out.ErrorClass == "dns_timeout" {
			out.ErrorClass = "dot_timeout"
		} else if out.ErrorClass == "dns_error" {
			out.ErrorClass = "dot_error"
		}
		out.Err = err
		return out
	}

	out.Latency = time.Since(start)
	msg, err := unpackResponse(raw, queryID(query))
	if err != nil {
		out.ErrorClass = "dot_error"
		out.Err = err
		return out
	}

	switch msg.RCode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError:
		out.ErrorClass = "dot_nxdomain"
		out.Err = fmt.Errorf("probe: %s does not exist", name)
		return out
	default:
		out.ErrorClass = "dot_servfail"
		out.Err = fmt.Errorf("probe: dot rcode %d for %s", msg.RCode, name)
		return out
	}

	out.Addresses = answerAddresses(msg)
	if len(out.Addresses) == 0 {
		out.ErrorClass = "dot_no_answer"
		out.Err = fmt.Errorf("probe: dot returned no records for %s", name)
		return out
	}
	return out
}