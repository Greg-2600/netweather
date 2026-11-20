package probe_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
)

// quicConn is a scripted net.Conn: every failure mode of the UDP middlebox
// probe can be driven without sending a datagram anywhere.
type quicConn struct {
	writeErr    error
	readErr     error
	readN       int
	written     []byte
	closed      bool
	deadlineSet bool
}

func (c *quicConn) Read([]byte) (int, error) { return c.readN, c.readErr }

func (c *quicConn) Write(b []byte) (int, error) {
	c.written = append([]byte(nil), b...)
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return len(b), nil
}

func (c *quicConn) Close() error                     { c.closed = true; return nil }
func (c *quicConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (c *quicConn) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (c *quicConn) SetDeadline(time.Time) error      { c.deadlineSet = true; return nil }
func (c *quicConn) SetReadDeadline(time.Time) error  { return nil }
func (c *quicConn) SetWriteDeadline(time.Time) error { return nil }

// quicTimeout implements the net.Error Timeout() convention that isTimeout
// checks. The probe must not wrap this error before testing it.
type quicTimeout struct{}

func (quicTimeout) Error() string { return "i/o timeout" }
func (quicTimeout) Timeout() bool { return true }

func quicDial(conn net.Conn, err error) probe.DialFunc {
	return func(context.Context, string, string) (net.Conn, error) { return conn, err }
}

func TestNewQUICUsesProductionDefaults(t *testing.T) {
	t.Parallel()

	q := probe.NewQUIC(nil)
	if q.Dial == nil {
		t.Fatal("NewQUIC left Dial nil")
	}
	if q.Timeout != 5*time.Second {
		t.Fatalf("timeout = %v, want 5s", q.Timeout)
	}

	custom := probe.NewQUIC(quicDial(nil, errors.New("unused")))
	if custom.Dial == nil || custom.Timeout != 5*time.Second {
		t.Fatalf("custom probe = %+v", custom)
	}
}

func TestQUICClassifiesDialAndExchangeFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		q    *probe.QUIC
		want string
	}{
		{
			name: "dial refused",
			q:    probe.NewQUIC(quicDial(nil, errors.New("blocked by middlebox"))),
			want: "quic_blocked",
		},
		{
			name: "write refused",
			q:    probe.NewQUIC(quicDial(&quicConn{writeErr: errors.New("write refused")}, nil)),
			want: "quic_blocked",
		},
		{
			name: "read timeout",
			q:    probe.NewQUIC(quicDial(&quicConn{readErr: quicTimeout{}}, nil)),
			want: "quic_no_response",
		},
		{
			name: "read refused",
			q:    probe.NewQUIC(quicDial(&quicConn{readErr: errors.New("connection reset")}, nil)),
			want: "quic_blocked",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.q.Do(t.Context(), "198.51.100.7:443")
			if got.ErrorClass != tc.want {
				t.Fatalf("class = %q, want %q (%v)", got.ErrorClass, tc.want, got.Err)
			}
			if got.Err == nil {
				t.Fatal("a classified failure has no error detail")
			}
			if got.Latency < 0 {
				t.Fatalf("latency = %v", got.Latency)
			}
		})
	}
}

func TestQUICAcceptsAResponse(t *testing.T) {
	t.Parallel()

	conn := &quicConn{readN: 5}
	q := probe.NewQUIC(quicDial(conn, nil))
	got := q.Do(t.Context(), "198.51.100.7:443")

	if got.Failed() {
		t.Fatalf("Failed: %s: %v", got.ErrorClass, got.Err)
	}
	if got.Status != 200 {
		t.Fatalf("status = %d, want 200", got.Status)
	}
	if string(conn.written) != string([]byte{0xc0, 0x00, 0x00, 0x00, 0x01}) {
		t.Fatalf("probe bytes = %x", conn.written)
	}
	if !conn.deadlineSet {
		t.Fatal("the context deadline was not applied to the connection")
	}
	if !conn.closed {
		t.Fatal("the connection was not closed")
	}
}

func TestQUICZeroValueUsesDefaults(t *testing.T) {
	t.Parallel()

	// A zero Timeout falls back to five seconds; an empty answer is a valid
	// observation (no Status) but not a failure.
	conn := &quicConn{readN: 0}
	q := &probe.QUIC{Dial: quicDial(conn, nil)}
	got := q.Do(t.Context(), "198.51.100.7:443")
	if got.Failed() || got.Status != 0 {
		t.Fatalf("empty answer = %+v", got)
	}

	// A zero-value probe installs the standard dialer. A malformed port fails
	// before any packet is sent, so the default path stays deterministic.
	got = (&probe.QUIC{}).Do(t.Context(), "198.51.100.7:not-a-port")
	if got.ErrorClass != "quic_blocked" || got.Err == nil {
		t.Fatalf("default dialer result = %+v", got)
	}
}
