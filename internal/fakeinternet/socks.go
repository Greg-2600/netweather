package fakeinternet

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// serveSOCKS is a minimal SOCKS5 CONNECT-only forwarder. It exists so the Tor
// vantage's real SOCKS client is exercised against a real server: the monitor's
// netx.Socks5 code does the greeting, the request, and the connect on its side.
//
// Authentication is "no auth" because the shipped Tor profile is a local
// sidecar; anything else would be testing a feature the product does not use.
func (i *Internet) serveSOCKS() {
	for {
		conn, err := i.socksLn.Accept()
		if err != nil {
			return
		}
		go i.handleSOCKS(conn)
	}
}

func (i *Internet) handleSOCKS(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Greeting: ver, nmethods, methods.
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 5 {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil { // no auth
		return
	}

	// Request: ver, cmd, rsv, atyp, addr, port.
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil || req[0] != 5 || req[1] != 1 {
		// Only CONNECT is supported; a BIND or UDP ASSOCIATE is refused.
		_, _ = conn.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}

	var host string
	switch req[3] {
	case 1: // IPv4
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	case 3: // domain
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return
		}
		buf := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		host = string(buf)
	case 4: // IPv6
		buf := make([]byte, 16)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	default:
		_, _ = conn.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}

	var portBuf [2]byte
	if _, err := io.ReadFull(conn, portBuf[:]); err != nil {
		return
	}
	port := int(binary.BigEndian.Uint16(portBuf[:]))

	upstream, err := i.dialMapped(context.Background(), host, port)
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0}) // connection refused
		return
	}
	defer func() { _ = upstream.Close() }()

	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	_ = upstream.SetDeadline(time.Time{})

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, conn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		done <- struct{}{}
	}()
	<-done
}

// dialMapped resolves a fake hostname and port to the listener that serves it.
// It is the one place that knows the fake internet's topology.
func (i *Internet) dialMapped(ctx context.Context, host string, port int) (net.Conn, error) {
	address, err := i.mapAddress(host, port)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", address)
}

// mapAddress is the dialer rewrite: every fake host is 127.0.0.1, and the
// requested port selects the HTTPS listener. A real address is passed through
// so a test can reach a local endpoint it started itself.
func (i *Internet) mapAddress(host string, port int) (string, error) {
	if _, known := i.byHost[host]; !known {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return net.JoinHostPort(host, strconv.Itoa(port)), nil
		}
		return "", fmt.Errorf("fakeinternet: unknown host %q", host)
	}
	switch port {
	case 443, 80:
		return i.httpsLn.Addr().String(), nil
	default:
		return "", errors.New("fakeinternet: only 80 and 443 are served")
	}
}
