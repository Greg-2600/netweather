package probe

import (
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// packetConn is the subset of *icmp.PacketConn the ping transport uses. It is
// an interface so a test can drive the read path with a synthetic reply without
// needing a privileged socket.
type packetConn interface {
	WriteTo(b []byte, addr net.Addr) (int, error)
	ReadFrom(b []byte) (int, net.Addr, error)
	SetReadDeadline(t time.Time) error
	Close() error
}

// icmpPing is the unprivileged ICMP echo transport. On Linux it uses a "ping"
// socket (icmp.ListenPacket("udp4", ...)), which the kernel permits to any group
// in net.ipv4.ping_group_range. That is what lets the release image, which
// drops all capabilities, keep the ping instrument working (THREAT_MODEL.md
// T53); CAP_NET_RAW is only needed by traceroute and by this fallback.
type icmpPing struct {
	conn packetConn
}

// openICMPPing opens a datagram ICMP socket. A host that has not enabled
// ping_group_range for this group gets a permission error here, which the
// caller treats as a reason to try the raw socket.
func openICMPPing() (*icmpPing, error) {
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("probe: unprivileged ICMP socket unavailable (check net.ipv4.ping_group_range): %w", err)
	}
	return &icmpPing{conn: conn}, nil
}

func (p *icmpPing) SendEcho(dst netip.Addr, id, seq int) error {
	if !dst.Is4() {
		return fmt.Errorf("probe: ping transport is IPv4 only, got %s", dst)
	}
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("netweather")},
	}
	payload, err := msg.Marshal(nil)
	if err != nil {
		return fmt.Errorf("probe: ping marshal echo: %w", err)
	}
	if _, err := p.conn.WriteTo(payload, &net.IPAddr{IP: net.IP(dst.AsSlice())}); err != nil {
		return fmt.Errorf("probe: ping send echo: %w", err)
	}
	return nil
}

// Recv returns the next echo reply. A ping socket rewrites the ICMP identifier,
// so the caller matches on the sequence only; ICMP.Do already does exactly that.
func (p *icmpPing) Recv(deadline time.Time) (netip.Addr, TraceMessage, error) {
	if err := p.conn.SetReadDeadline(deadline); err != nil {
		return netip.Addr{}, TraceMessage{}, err
	}

	buf := make([]byte, 1500)
	n, peer, err := p.conn.ReadFrom(buf)
	if err != nil {
		return netip.Addr{}, TraceMessage{}, err
	}

	parsed, err := icmp.ParseMessage(1, buf[:n])
	if err != nil {
		return netip.Addr{}, TraceMessage{Kind: "other"}, nil //nolint:nilerr // ignore unparseable noise
	}

	var from netip.Addr
	if ipPeer, ok := peer.(*net.IPAddr); ok {
		if addr, ok := netip.AddrFromSlice(ipPeer.IP); ok {
			from = addr.Unmap()
		}
	}

	if parsed.Type != ipv4.ICMPTypeEchoReply {
		return from, TraceMessage{Kind: "other"}, nil
	}
	msg := TraceMessage{Kind: "echo_reply"}
	if echo, ok := parsed.Body.(*icmp.Echo); ok {
		msg.ID, msg.Seq = echo.ID, echo.Seq
	}
	return from, msg, nil
}

func (p *icmpPing) Close() error { return p.conn.Close() }
