//go:build windows

package probe

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/Greg-2600/netweather/internal/netx"
)

// IP_DONTFRAGMENT on Windows.
const ipDontFragment = 14

// PMTU sends a UDP datagram with the DF bit set to probe the path.
type PMTU struct {
	Timeout int
	Size    int
}

func NewPMTU() *PMTU { return &PMTU{Timeout: 3, Size: 1400} }

func (p *PMTU) Do(ctx context.Context, address string) Result {
	start := time.Now()
	res := Result{}
	if p.Size <= 0 {
		p.Size = 1400
	}
	timeout := time.Duration(p.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := netx.New().DialContext(lookupCtx, "udp", address)
	if err != nil {
		res.Latency = time.Since(start)
		res.ErrorClass = "pmtu_error"
		res.Err = err
		return res
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	raw, ok := conn.(*net.UDPConn)
	if !ok {
		res.Latency = time.Since(start)
		res.ErrorClass = "pmtu_error"
		res.Err = fmt.Errorf("probe: not a udp conn")
		return res
	}
	fd, err := raw.SyscallConn()
	if err != nil {
		res.Latency = time.Since(start)
		res.ErrorClass = "pmtu_error"
		res.Err = err
		return res
	}
	var sockErr error
	if err := fd.Control(func(f uintptr) {
		opt := 1
		sockErr = syscall.SetsockoptInt(syscall.Handle(f), syscall.IPPROTO_IP, ipDontFragment, opt)
	}); err != nil {
		sockErr = err
	}
	if sockErr != nil {
		res.Latency = time.Since(start)
		res.ErrorClass = "pmtu_error"
		res.Err = sockErr
		return res
	}

	payload := make([]byte, p.Size)
	if _, err := conn.Write(payload); err != nil {
		res.Latency = time.Since(start)
		res.ErrorClass = "pmtu_blocked"
		res.Err = err
		return res
	}
	buf := make([]byte, 1500)
	_, err = conn.Read(buf)
	res.Latency = time.Since(start)
	if err != nil {
		if isTimeout(err) {
			res.Status = 200
			return res
		}
		res.ErrorClass = "pmtu_blocked"
		res.Err = err
		return res
	}
	res.Status = 200
	_ = lookupCtx
	return res
}