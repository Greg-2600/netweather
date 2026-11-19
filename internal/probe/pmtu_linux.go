//go:build linux

package probe

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/Greg-2600/netweather/internal/netx"
)

// PMTU sends a UDP datagram with the DF bit set to discover whether the path
// accepts a given payload size without fragmentation (ROADMAP "Later").
type PMTU struct {
	Timeout time.Duration
	Size    int
}

// NewPMTU builds the instrument.
func NewPMTU() *PMTU { return &PMTU{Timeout: 3 * time.Second, Size: 1400} }

// Do probes address ("host:port") with a DF-marked UDP datagram.
func (p *PMTU) Do(ctx context.Context, address string) Result {
	start := time.Now()
	res := Result{}
	if p.Size <= 0 {
		p.Size = 1400
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dialer := netx.New()
	conn, err := dialer.DialContext(lookupCtx, "udp", address)
	if err != nil {
		res.Latency = time.Since(start)
		res.ErrorClass = "pmtu_error"
		res.Err = err
		return res
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	// Set DF bit via socket option (Linux).
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
		sockErr = syscall.SetsockoptInt(int(f), syscall.IPPROTO_IP, syscall.IP_MTU_DISCOVER, 2) // IP_PMTUDISC_DO
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
	n, err := conn.Read(buf)
	res.Latency = time.Since(start)
	if err != nil {
		if isTimeout(err) {
			// No response and no error: packet likely delivered (path MTU ok).
			res.Status = 200
			return res
		}
		res.ErrorClass = "pmtu_blocked"
		res.Err = err
		return res
	}
	if n >= 0 {
		res.Status = 200
	}
	_ = lookupCtx
	return res
}
