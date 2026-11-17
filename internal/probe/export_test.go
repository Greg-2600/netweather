package probe

import (
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// fakePacketConn drives the ping transport without a privileged socket. It is
// white-box because icmpPing is deliberately unexported; the file name is in
// the testpackage skip list.
type fakePacketConn struct {
	reply   []byte
	wrote   []byte
	addr    net.Addr
	closed  bool
	readErr error
}

func (f *fakePacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	f.wrote = append([]byte(nil), b...)
	f.addr = addr
	return len(b), nil
}

func (f *fakePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	if f.readErr != nil {
		return 0, nil, f.readErr
	}
	return copy(b, f.reply), &net.IPAddr{IP: net.IPv4(198, 51, 100, 7)}, nil
}

func (f *fakePacketConn) SetReadDeadline(time.Time) error { return nil }

func (f *fakePacketConn) Close() error {
	f.closed = true
	return nil
}

func echoReply(t *testing.T, seq int) []byte {
	t.Helper()

	payload, err := (&icmp.Message{
		Type: ipv4.ICMPTypeEchoReply,
		Body: &icmp.Echo{ID: 1, Seq: seq, Data: []byte("netweather")},
	}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestICMPPingSendsAnEchoAndReadsTheReply(t *testing.T) {
	t.Parallel()

	fake := &fakePacketConn{reply: echoReply(t, 7)}
	ping := &icmpPing{conn: fake}

	dst := netip.MustParseAddr("198.51.100.7")
	if err := ping.SendEcho(dst, 42, 7); err != nil {
		t.Fatalf("SendEcho: %v", err)
	}

	sent, err := icmp.ParseMessage(1, fake.wrote)
	if err != nil {
		t.Fatalf("sent payload is not ICMP: %v", err)
	}
	if sent.Type != ipv4.ICMPTypeEcho {
		t.Fatalf("sent type = %v, want echo request", sent.Type)
	}
	echo, ok := sent.Body.(*icmp.Echo)
	if !ok || echo.ID != 42 || echo.Seq != 7 {
		t.Fatalf("sent echo = %+v", sent.Body)
	}
	if ip, ok := fake.addr.(*net.IPAddr); !ok || !ip.IP.Equal(net.IP(dst.AsSlice())) {
		t.Fatalf("sent to %v, want %s", fake.addr, dst)
	}

	from, msg, err := ping.Recv(time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if from != dst {
		t.Fatalf("reply from = %s, want %s", from, dst)
	}
	if msg.Kind != "echo_reply" || msg.Seq != 7 {
		t.Fatalf("message = %+v", msg)
	}

	if err := ping.Close(); err != nil {
		t.Fatal(err)
	}
	if !fake.closed {
		t.Fatal("Close did not close the socket")
	}
}

func TestICMPPingIgnoresWhatIsNotAReply(t *testing.T) {
	t.Parallel()

	// An echo request on the receive path is noise, not a reply.
	request, err := (&icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Body: &icmp.Echo{ID: 1, Seq: 1},
	}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	ping := &icmpPing{conn: &fakePacketConn{reply: request}}
	if _, msg, err := ping.Recv(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	} else if msg.Kind != "other" {
		t.Fatalf("message = %+v, want kind other", msg)
	}

	// A truncated payload is unparseable and must not be an error; a noisy
	// network should not make the instrument fail.
	ping = &icmpPing{conn: &fakePacketConn{reply: []byte{0x00}}}
	if _, msg, err := ping.Recv(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("malformed payload returned an error: %v", err)
	} else if msg.Kind != "other" {
		t.Fatalf("message = %+v, want kind other", msg)
	}

	// A socket read error is reported.
	want := errors.New("read failed")
	ping = &icmpPing{conn: &fakePacketConn{readErr: want}}
	if _, _, err := ping.Recv(time.Now().Add(time.Second)); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestICMPPingMarshalFailureIsReported(t *testing.T) {
	t.Parallel()

	// ICMPv4 can only carry IPv4 addresses; a v6 destination must fail rather
	// than write a malformed packet.
	ping := &icmpPing{conn: &fakePacketConn{}}
	if err := ping.SendEcho(netip.MustParseAddr("2001:db8::1"), 1, 1); err == nil {
		t.Fatal("a v6 destination was accepted by the v4 ping transport")
	}
}
