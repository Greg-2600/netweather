package probe

import (
	"context"
	"net"
	"time"

	"github.com/Greg-2600/netweather/internal/netx"
)

// TCP is the handshake-only instrument. It answers "does something accept a
// connection here" without sending a byte.
type TCP struct {
	Dial DialFunc
}

// NewTCP returns a TCP instrument over the guarded dialer.
func NewTCP(d *netx.Dialer) *TCP {
	if d == nil {
		d = netx.New()
	}
	return &TCP{Dial: d.DialContext}
}

// TCPResult is one connect attempt.
type TCPResult struct {
	Latency    time.Duration
	ErrorClass string
	Err        error
}

// Failed reports a transport-class failure.
func (r TCPResult) Failed() bool { return r.Err != nil || r.ErrorClass != "" }

// Do connects and immediately closes.
func (p *TCP) Do(ctx context.Context, address string) TCPResult {
	start := time.Now()

	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}

	conn, err := dial(ctx, "tcp", address)
	res := TCPResult{Latency: time.Since(start)}
	if err != nil {
		res.ErrorClass = ClassifyDialError(err)
		res.Err = err
		return res
	}
	_ = conn.Close()
	return res
}
