package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Greg-2600/netweather/internal/netx"
)

// ntpEpochDelta is the number of seconds between the NTP epoch (1900) and the
// Unix epoch (1970).
const ntpEpochDelta = 2208988800

// NTP is the clock instrument. Its output is self-health: an offset is a fact
// about this vantage, never about a target's availability.
type NTP struct {
	Dial    DialFunc
	Timeout time.Duration
}

// NewNTP returns an NTP instrument over the guarded dialer (UDP/123 is in the
// port allowlist).
func NewNTP(d *netx.Dialer) *NTP {
	if d == nil {
		d = netx.New()
	}
	return &NTP{Dial: d.DialContext, Timeout: 5 * time.Second}
}

// NewNTPWithDial returns an NTP instrument over an arbitrary dialer.
func NewNTPWithDial(dial DialFunc) *NTP {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	return &NTP{Dial: dial, Timeout: 5 * time.Second}
}

// NTPResult is one exchange.
type NTPResult struct {
	RTT        time.Duration
	Offset     time.Duration
	Stratum    int
	ErrorClass string
	Err        error
}

// Failed reports a transport or protocol failure.
func (r NTPResult) Failed() bool { return r.Err != nil || r.ErrorClass != "" }

// Do sends one client request and reads one server response.
func (p *NTP) Do(ctx context.Context, address string) NTPResult {
	out := NTPResult{}

	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	conn, err := dial(ctx, "udp", address)
	if err != nil {
		out.ErrorClass = ClassifyNTPError(err)
		out.Err = err
		return out
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	request := make([]byte, 48)
	request[0] = 0x23 // LI=0, VN=4, Mode=3 (client)

	t1 := time.Now()
	if _, err := conn.Write(request); err != nil {
		out.ErrorClass = ClassifyNTPError(err)
		out.Err = err
		return out
	}

	response := make([]byte, 48)
	n, err := conn.Read(response)
	t4 := time.Now()
	if err != nil {
		out.ErrorClass = ClassifyNTPError(err)
		out.Err = err
		return out
	}
	if n < 48 {
		out.ErrorClass = "ntp_bad_response"
		out.Err = fmt.Errorf("probe: NTP response is %d bytes", n)
		return out
	}

	mode := response[0] & 0x07
	stratum := int(response[1])
	if mode != 4 {
		out.ErrorClass = "ntp_bad_response"
		out.Err = fmt.Errorf("probe: NTP mode %d is not a server response", mode)
		return out
	}
	if stratum == 0 {
		// Kiss-o'-death: the server is telling us to stop, not answering.
		out.ErrorClass = "ntp_denied"
		out.Err = errors.New("probe: NTP kiss-o'-death")
		return out
	}
	if stratum > 15 {
		out.ErrorClass = "ntp_bad_response"
		out.Err = fmt.Errorf("probe: NTP stratum %d is unsynchronised", stratum)
		return out
	}

	t2 := ntpTime(response[32:40])
	t3 := ntpTime(response[40:48])

	out.Stratum = stratum
	out.RTT = t4.Sub(t1) - t3.Sub(t2)
	if out.RTT < 0 {
		out.RTT = 0
	}
	out.Offset = (t2.Sub(t1) + t3.Sub(t4)) / 2
	return out
}

func ntpTime(b []byte) time.Time {
	seconds := binary.BigEndian.Uint32(b[0:4])
	fraction := binary.BigEndian.Uint32(b[4:8])
	ns := int64(fraction) * 1e9 >> 32
	return time.Unix(int64(seconds)-ntpEpochDelta, ns).UTC()
}

// ClassifyNTPError maps exchange failures to stable classes. The classifier
// treats every ntp_ class as self-health, never as a target state.
func ClassifyNTPError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "ntp_timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "ntp_timeout"
	}
	return "ntp_unreachable"
}
