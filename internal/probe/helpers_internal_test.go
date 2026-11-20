//nolint:testpackage // white-box tests exercise unexported error branches that need fakes.
package probe

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// errNetConn drives the exchange error branches without a real socket.
type errNetConn struct {
	writeErr error
	readErr  error
}

func (c *errNetConn) Read([]byte) (int, error)         { return 0, c.readErr }
func (c *errNetConn) Write([]byte) (int, error)        { return 0, c.writeErr }
func (c *errNetConn) Close() error                     { return nil }
func (c *errNetConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *errNetConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *errNetConn) SetDeadline(time.Time) error      { return nil }
func (c *errNetConn) SetReadDeadline(time.Time) error  { return nil }
func (c *errNetConn) SetWriteDeadline(time.Time) error { return nil }

// junkConn returns malformed bytes so unpackResponse rejects the answer.
type junkConn struct {
	errNetConn
}

func (c *junkConn) Read(b []byte) (int, error) { return copy(b, []byte{0xff, 0xff}), nil }

func dialErrConn(c net.Conn) DialFunc {
	return func(context.Context, string, string) (net.Conn, error) { return c, nil }
}

func TestExchangeTCPErrorBranches(t *testing.T) {
	t.Parallel()

	// A failure writing the two-byte-framed query.
	writeFail := dialErrConn(&errNetConn{writeErr: errors.New("write failed")})
	if _, err := exchange(context.Background(), writeFail, "tcp", "127.0.0.1:53", []byte{1, 2}); err == nil {
		t.Fatal("TCP write failure was not reported")
	}

	// A query too large for the two-byte TCP length prefix.
	noop := dialErrConn(&errNetConn{})
	if _, err := exchange(context.Background(), noop, "tcp", "127.0.0.1:53", make([]byte, 0x10000)); err == nil {
		t.Fatal("oversized TCP query was not rejected")
	}

	// A failure reading the frame-size prefix.
	readFail := dialErrConn(&errNetConn{readErr: errors.New("read failed")})
	if _, err := exchange(context.Background(), readFail, "tcp", "127.0.0.1:53", []byte{1, 2}); err == nil {
		t.Fatal("TCP frame-size read failure was not reported")
	}
}

func TestDoNSErrorsNeedNoPrivileges(t *testing.T) {
	t.Parallel()

	p := &DNS{Dial: dialErrConn(&errNetConn{})}

	// An empty endpoint is a configuration error before any dial.
	if res := p.DoNS(context.Background(), "", "example.com"); res.ErrorClass != "dns_config" {
		t.Fatalf("empty server class = %q", res.ErrorClass)
	}
	if res := p.DoNS(context.Background(), "127.0.0.1:53", ""); res.ErrorClass != "dns_config" {
		t.Fatalf("empty name class = %q", res.ErrorClass)
	}

	// A qname with an over-long label cannot be packed for the wire.
	longLabel := strings.Repeat("a", 64)
	if res := p.DoNS(context.Background(), "127.0.0.1:53", longLabel); res.ErrorClass != "dns_config" {
		t.Fatalf("bad qname class = %q", res.ErrorClass)
	}

	// A malformed response is a dns_error, not a transport failure.
	junk := &DNS{Dial: dialErrConn(&junkConn{})}
	if res := junk.DoNS(context.Background(), "127.0.0.1:53", "example.com"); res.ErrorClass != "dns_error" {
		t.Fatalf("malformed response class = %q", res.ErrorClass)
	}
}
