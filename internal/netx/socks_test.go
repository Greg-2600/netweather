package netx_test

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/netx"
)

// startSOCKS5 runs a minimal SOCKS5 server that records the requested
// destination. It proves the dialer sends a domain name (ATYP=3) rather than a
// pre-resolved address, which is the difference between socks5h and socks5.
func startSOCKS5(t *testing.T, seen chan<- string) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSOCKS5(conn, seen)
		}
	}()

	return ln.Addr().String()
}

func serveSOCKS5(conn net.Conn, seen chan<- string) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Greeting: VER, NMETHODS, METHODS. Only "no auth" is offered.
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// Request: VER, CMD, RSV, ATYP.
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	if req[1] != 0x01 {
		return
	}

	var host string
	switch req[3] {
	case 0x03: // domain name: what socks5h must send
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return
		}
		name := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, name); err != nil {
			return
		}
		host = string(name)
	case 0x01: // IPv4
		addr := make([]byte, 4)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return
		}
		host = net.IP(addr).String()
	default:
		return
	}

	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return
	}
	seen <- host + ":" + itoa(int(port[0])<<8|int(port[1]))

	// Success reply, then act as the destination: the client reads a greeting.
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0}); err != nil {
		return
	}
	_, _ = conn.Write([]byte("hello from the proxy\n"))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestSocks5SendsDomainsToTheProxy(t *testing.T) {
	t.Parallel()

	seen := make(chan string, 1)
	proxyAddr := startSOCKS5(t, seen)

	dial, err := netx.Socks5(proxyAddr)
	if err != nil {
		t.Fatalf("Socks5: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := dial(ctx, "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	select {
	case got := <-seen:
		if !strings.HasPrefix(got, "example.com:") {
			t.Fatalf("proxy saw %q, want the hostname (socks5h)", got)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy never saw a request")
	}

	buf := make([]byte, len("hello from the proxy\n"))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read through proxy: %v", err)
	}
	if string(buf) != "hello from the proxy\n" {
		t.Fatalf("payload = %q", buf)
	}
}

func TestSocks5RejectsBadProxyAddresses(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"", "127.0.0.1", "127.0.0.1:not-a-port", "127.0.0.1:70000"} {
		if _, err := netx.Socks5(addr); err == nil {
			t.Errorf("Socks5(%q) should fail", addr)
		}
	}
}

func TestSocks5RejectsUDP(t *testing.T) {
	t.Parallel()

	dial, err := netx.Socks5("127.0.0.1:9050")
	if err != nil {
		// No listener is needed to build the dialer.
		t.Fatalf("Socks5: %v", err)
	}
	if _, err := dial(context.Background(), "udp", "example.com:53"); err == nil {
		t.Fatal("udp should be rejected: the DNS instrument does not run on Tor")
	}
}
