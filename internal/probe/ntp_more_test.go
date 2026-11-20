package probe_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
)

// ntpConn replays a fixed server response so malformed packets and write
// failures can be tested without a network.
type ntpConn struct {
	response    []byte
	writeErr    error
	readErr     error
	deadlineSet bool
	closed      bool
}

func (c *ntpConn) Read(b []byte) (int, error) {
	if c.readErr != nil {
		return 0, c.readErr
	}
	return copy(b, c.response), nil
}

func (c *ntpConn) Write([]byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return 48, nil
}

func (c *ntpConn) Close() error                     { c.closed = true; return nil }
func (c *ntpConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (c *ntpConn) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (c *ntpConn) SetDeadline(time.Time) error      { c.deadlineSet = true; return nil }
func (c *ntpConn) SetReadDeadline(time.Time) error  { return nil }
func (c *ntpConn) SetWriteDeadline(time.Time) error { return nil }

func ntpServerResponse() []byte {
	reply := make([]byte, 48)
	reply[0] = 0x24 // VN=4, mode=4 (server)
	reply[1] = 2
	return reply
}

func TestNTPZeroValueAppliesDefaultsAndUsesTheDeadline(t *testing.T) {
	t.Parallel()

	conn := &ntpConn{response: ntpServerResponse()}
	p := &probe.NTP{Dial: func(context.Context, string, string) (net.Conn, error) {
		return conn, nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	got := p.Do(ctx, "198.51.100.7:123")
	if got.Failed() {
		t.Fatalf("Failed: %s: %v", got.ErrorClass, got.Err)
	}
	if got.Stratum != 2 {
		t.Fatalf("stratum = %d", got.Stratum)
	}
	if !conn.deadlineSet || !conn.closed {
		t.Fatalf("deadline=%v closed=%v", conn.deadlineSet, conn.closed)
	}

	// Without a context deadline the instrument still bounds the exchange
	// with its own timeout.
	plain := &ntpConn{response: ntpServerResponse()}
	p.Dial = func(context.Context, string, string) (net.Conn, error) { return plain, nil }
	if got := p.Do(context.Background(), "198.51.100.7:123"); got.Failed() {
		t.Fatalf("no-deadline exchange failed: %s: %v", got.ErrorClass, got.Err)
	}
	if !plain.deadlineSet {
		t.Fatal("the fallback deadline was not applied")
	}
}

func TestNTPZeroValueInstallsTheDefaultDialer(t *testing.T) {
	t.Parallel()

	// A malformed port fails inside net.Dialer before any packet is sent.
	got := (&probe.NTP{}).Do(t.Context(), "127.0.0.1:not-a-port")
	if got.ErrorClass != "ntp_unreachable" || got.Err == nil {
		t.Fatalf("class = %q, want ntp_unreachable (%v)", got.ErrorClass, got.Err)
	}
}

func TestNTPRejectsBrokenExchanges(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		conn *ntpConn
		want string
	}{
		{"write failure", &ntpConn{writeErr: errors.New("write failed")}, "ntp_unreachable"},
		{"short response", &ntpConn{response: make([]byte, 10)}, "ntp_bad_response"},
		{"client mode", &ntpConn{response: func() []byte {
			r := ntpServerResponse()
			r[0] = 0x23 // mode 3 (client) is not a server answer
			return r
		}()}, "ntp_bad_response"},
		{"kiss-o-death", &ntpConn{response: func() []byte {
			r := ntpServerResponse()
			r[1] = 0 // stratum zero is a KoD, not a clock
			return r
		}()}, "ntp_denied"},
		{"unsynchronised", &ntpConn{response: func() []byte {
			r := ntpServerResponse()
			r[1] = 16 // strata above 15 are unspecified
			return r
		}()}, "ntp_bad_response"},
		{"rtt clamp", &ntpConn{response: func() []byte {
			r := ntpServerResponse()
			// t2 in 2020 and t3 in 2035 make the derived RTT negative;
			// the instrument must clamp it rather than report negative time.
			binary.BigEndian.PutUint32(r[32:36], 3786825600)
			binary.BigEndian.PutUint32(r[40:44], 4260211200)
			return r
		}()}, ""},
		{"read failure", &ntpConn{readErr: errors.New("read failed")}, "ntp_unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := &probe.NTP{Dial: func(context.Context, string, string) (net.Conn, error) {
				return tc.conn, nil
			}}
			got := p.Do(context.Background(), "127.0.0.1:123")
			if got.ErrorClass != tc.want {
				t.Fatalf("class = %q, want %q (%v)", got.ErrorClass, tc.want, got.Err)
			}
			if tc.want == "" && got.RTT < 0 {
				t.Fatalf("rtt = %v, want a clamped non-negative value", got.RTT)
			}
		})
	}
}
