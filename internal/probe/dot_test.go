package probe //nolint:testpackage // white-box tests cover the DoT framing and cert helpers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/netx"
	"golang.org/x/net/dns/dnsmessage"
)

func TestDoTConfigErrors(t *testing.T) {
	d := NewDoT(nil)
	r := d.Do(t.Context(), "", "example.com")
	if r.ErrorClass != "dot_config" {
		t.Fatalf("empty server: %s", r.ErrorClass)
	}
	r = d.Do(t.Context(), "127.0.0.1:853", "")
	if r.ErrorClass != "dot_config" {
		t.Fatalf("empty name: %s", r.ErrorClass)
	}
}

func TestDoTQuery(t *testing.T) {
	certPEM, keyPEM, err := makeCert()
	if err != nil {
		t.Fatal(err)
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{tlsCert}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				var size [2]byte
				if _, err := readFull(c, size[:]); err != nil {
					return
				}
				n := int(binary.BigEndian.Uint16(size[:]))
				buf := make([]byte, n)
				if _, err := readFull(c, buf); err != nil {
					return
				}
				var msg dnsmessage.Message
				if err := msg.Unpack(buf); err != nil {
					return
				}
				resp := dnsmessage.Message{
					Header:    dnsmessage.Header{ID: msg.ID, Response: true, RecursionAvailable: true},
					Questions: msg.Questions,
					Answers: []dnsmessage.Resource{{
						Header: dnsmessage.ResourceHeader{Name: msg.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
						Body:   &dnsmessage.AResource{A: [4]byte{1, 2, 3, 4}},
					}},
				}
				packed, err := resp.Pack()
				if err != nil {
					return
				}
				framed := make([]byte, 2+len(packed))
				binary.BigEndian.PutUint16(framed[:2], uint16(len(packed)))
				copy(framed[2:], packed)
				_, _ = c.Write(framed)
			}(conn)
		}
	}()

	d := NewDoT(nil)
	d.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	r := d.Do(t.Context(), ln.Addr().String(), "example.com")
	if r.Err != nil {
		t.Fatalf("dot query failed: %v (%s)", r.Err, r.ErrorClass)
	}
	if len(r.Addresses) != 1 {
		t.Fatalf("expected 1 address, got %d", len(r.Addresses))
	}
}

func makeCert() ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}

// dotAnswer builds a framed response to a parsed query. A nil return means the
// query was unusable, which leaves the client waiting for its deadline.
func dotAnswer(query []byte, qtype dnsmessage.Type, rcode dnsmessage.RCode, withAnswer bool) []byte {
	var msg dnsmessage.Message
	if err := msg.Unpack(query); err != nil || len(msg.Questions) == 0 {
		return nil
	}
	msg.Response = true
	msg.RecursionAvailable = true
	msg.RCode = rcode
	if withAnswer {
		header := dnsmessage.ResourceHeader{
			Name: msg.Questions[0].Name, Type: qtype, Class: dnsmessage.ClassINET, TTL: 60,
		}
		switch qtype {
		case dnsmessage.TypeA:
			msg.Answers = []dnsmessage.Resource{{
				Header: header,
				Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 7}},
			}}
		case dnsmessage.TypeAAAA:
			msg.Answers = []dnsmessage.Resource{{
				Header: header,
				Body:   &dnsmessage.AAAAResource{AAAA: [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}},
			}}
		default:
			// Other question types are not part of these fixtures.
		}
	}
	packed, err := msg.Pack()
	if err != nil {
		return nil
	}
	framed := make([]byte, 2+len(packed))
	binary.BigEndian.PutUint16(framed[:2], uint16(len(packed)))
	copy(framed[2:], packed)
	return framed
}

// startDoTServer runs a TLS listener that answers one framed query per
// connection. Returning nil bytes closes the connection without an answer.
func startDoTServer(t *testing.T, reply func([]byte) []byte) string {
	t.Helper()

	certPEM, keyPEM, err := makeCert()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
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
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				var size [2]byte
				if _, err := readFull(c, size[:]); err != nil {
					return
				}
				query := make([]byte, int(binary.BigEndian.Uint16(size[:])))
				if _, err := readFull(c, query); err != nil {
					return
				}
				if out := reply(query); len(out) > 0 {
					_, _ = c.Write(out)
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func dotInstrument() *DoT {
	d := NewDoT((&net.Dialer{Timeout: time.Second}).DialContext)
	d.TLSConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- local test certificate.
	d.Timeout = 2 * time.Second
	return d
}

func TestDoTDialErrorsAreClassified(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, "dot_timeout"},
		{"plain", errors.New("connection refused"), "dot_error"},
		{"blocked port", netx.ErrBlockedPort, "policy_blocked_port"},
		{"blocked", netx.ErrBlocked, "policy_blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := NewDoT(func(context.Context, string, string) (net.Conn, error) {
				return nil, tc.err
			})
			got := d.Do(t.Context(), "127.0.0.1:853", "example.com")
			if got.ErrorClass != tc.want {
				t.Fatalf("class = %q, want %q (%v)", got.ErrorClass, tc.want, got.Err)
			}
			if got.Err == nil {
				t.Fatal("a classified failure has no error detail")
			}
		})
	}
}

func TestDoTClassifiesServerAnswers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		reply func([]byte) []byte
		want  string
		addr  string
	}{
		{
			name:  "nxdomain",
			reply: func(q []byte) []byte { return dotAnswer(q, dnsmessage.TypeA, dnsmessage.RCodeNameError, false) },
			want:  "dot_nxdomain",
		},
		{
			name:  "servfail",
			reply: func(q []byte) []byte { return dotAnswer(q, dnsmessage.TypeA, dnsmessage.RCodeServerFailure, false) },
			want:  "dot_servfail",
		},
		{
			name:  "no records",
			reply: func(q []byte) []byte { return dotAnswer(q, dnsmessage.TypeA, dnsmessage.RCodeSuccess, false) },
			want:  "dot_no_answer",
		},
		{
			name:  "malformed response",
			reply: func([]byte) []byte { return []byte{0, 3, 'a', 'b', 'c'} },
			want:  "dot_error",
		},
		{
			name:  "truncated size frame",
			reply: func([]byte) []byte { return nil },
			want:  "dot_error",
		},
		{
			name:  "truncated message body",
			reply: func([]byte) []byte { return []byte{0, 40} },
			want:  "dot_error",
		},
		{
			name:  "a record",
			reply: func(q []byte) []byte { return dotAnswer(q, dnsmessage.TypeA, dnsmessage.RCodeSuccess, true) },
			addr:  "192.0.2.7",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := startDoTServer(t, tc.reply)
			got := dotInstrument().Do(t.Context(), server, "example.com")
			if got.ErrorClass != tc.want {
				t.Fatalf("class = %q, want %q (%v)", got.ErrorClass, tc.want, got.Err)
			}
			if tc.want == "" && (len(got.Addresses) != 1 || got.Addresses[0].String() != tc.addr) {
				t.Fatalf("addresses = %v, want %s", got.Addresses, tc.addr)
			}
			if tc.want != "" && got.Err == nil {
				t.Fatal("a classified failure has no error detail")
			}
		})
	}
}

func TestDoTDo6AsksForAAAA(t *testing.T) {
	t.Parallel()

	server := startDoTServer(t, func(query []byte) []byte {
		var msg dnsmessage.Message
		if err := msg.Unpack(query); err != nil || len(msg.Questions) != 1 {
			return nil
		}
		if msg.Questions[0].Type != dnsmessage.TypeAAAA {
			return nil
		}
		return dotAnswer(query, dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, true)
	})

	got := dotInstrument().Do6(t.Context(), server, "v6.example")
	if got.Failed() {
		t.Fatalf("Do6 failed: %s: %v", got.ErrorClass, got.Err)
	}
	if len(got.Addresses) != 1 || got.Addresses[0].String() != "2001:db8::1" {
		t.Fatalf("addresses = %v", got.Addresses)
	}
}

// startHangingDoTServer reads one framed query and then holds the connection
// open. With sendSize it writes only the length frame, so the client times out
// while reading the body instead of while reading the size.
func startHangingDoTServer(t *testing.T, sendSize bool) string {
	t.Helper()

	certPEM, keyPEM, err := makeCert()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = ln.Close()
		close(done)
	})

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		var size [2]byte
		if _, err := readFull(conn, size[:]); err != nil {
			return
		}
		query := make([]byte, int(binary.BigEndian.Uint16(size[:])))
		if _, err := readFull(conn, query); err != nil {
			return
		}
		if sendSize {
			_, _ = conn.Write([]byte{0, 10})
		}
		<-done
	}()
	return ln.Addr().String()
}

func TestDoTReportsAReadTimeoutSeparatelyFromARefusal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		sendSize bool
	}{
		{"before size frame", false},
		{"before message body", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := startHangingDoTServer(t, tc.sendSize)
			d := dotInstrument()
			d.Timeout = 200 * time.Millisecond
			got := d.Do(t.Context(), server, "example.com")
			if got.ErrorClass != "dot_timeout" || got.Err == nil {
				t.Fatalf("class = %q, want dot_timeout (%v)", got.ErrorClass, got.Err)
			}
		})
	}
}

func TestDoTDefaultTLSConfigIsBuilt(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	// A nil TLSConfig makes do() build the secure default before the
	// handshake fails against this plain listener.
	d := &DoT{Dial: func(context.Context, string, string) (net.Conn, error) {
		return net.Dial("tcp", ln.Addr().String())
	}}
	got := d.Do(t.Context(), "127.0.0.1:853", "example.com")
	if got.ErrorClass != "dot_tls" || got.Err == nil {
		t.Fatalf("class = %q, want dot_tls (%v)", got.ErrorClass, got.Err)
	}
}

func TestDoTHandshakeFailure(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	got := dotInstrument().Do(t.Context(), ln.Addr().String(), "example.com")
	if got.ErrorClass != "dot_tls" || got.Err == nil {
		t.Fatalf("class = %q, want dot_tls (%v)", got.ErrorClass, got.Err)
	}
}

func TestDoTAcceptsAServerWithoutAPort(t *testing.T) {
	t.Parallel()

	server := startDoTServer(t, func(q []byte) []byte {
		return dotAnswer(q, dnsmessage.TypeA, dnsmessage.RCodeSuccess, true)
	})

	// A bare host makes SplitHostPort fail; the DoT code then uses the whole
	// string as the TLS ServerName. The injected dialer still reaches the test
	// listener, so this must not turn into a configuration failure.
	d := NewDoT(func(context.Context, string, string) (net.Conn, error) {
		return net.Dial("tcp", server)
	})
	d.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	got := d.Do(t.Context(), "127.0.0.1", "example.com")
	if got.Failed() {
		t.Fatalf("bare host failed: %s: %v", got.ErrorClass, got.Err)
	}
}

func TestDoTWriteFailure(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM, err := makeCert()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	go func() {
		s := tls.Server(server, &tls.Config{Certificates: []tls.Certificate{cert}})
		_ = s.Handshake()
		// Once the handshake is over, dropping the socket makes the client's
		// framed query fail in tlsConn.Write rather than during the handshake.
		_ = s.Close()
	}()

	d := NewDoT(func(context.Context, string, string) (net.Conn, error) { return client, nil })
	d.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	got := d.Do(t.Context(), "127.0.0.1:853", "example.com")
	if got.ErrorClass != "dot_error" || got.Err == nil {
		t.Fatalf("class = %q, want dot_error (%v)", got.ErrorClass, got.Err)
	}
}

func TestDoTZeroValueUsesDefaults(t *testing.T) {
	t.Parallel()

	server := startDoTServer(t, func(q []byte) []byte {
		return dotAnswer(q, dnsmessage.TypeA, dnsmessage.RCodeSuccess, true)
	})

	// No Dial, no Timeout and a TLSConfig with no ServerName or MinVersion:
	// every default branch in do() must be taken on the way to a valid answer.
	d := &DoT{TLSConfig: &tls.Config{InsecureSkipVerify: true}}
	got := d.Do(t.Context(), server, "example.com")
	if got.Failed() {
		t.Fatalf("zero-value DoT failed: %s: %v", got.ErrorClass, got.Err)
	}
	if d.TLSConfig.ServerName != "127.0.0.1" || d.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS defaults not applied: %+v", d.TLSConfig)
	}
}

func TestDoTRejectsAnUnusableQName(t *testing.T) {
	t.Parallel()

	d := NewDoT(nil)
	got := d.Do(t.Context(), "127.0.0.1:853", "")
	if got.ErrorClass != "dot_config" {
		t.Fatalf("empty qname class = %q", got.ErrorClass)
	}
	got = d.Do(t.Context(), "127.0.0.1:853", strings.Repeat("a", 300)+".example")
	if got.ErrorClass != "dot_config" {
		t.Fatalf("long qname class = %q", got.ErrorClass)
	}
}
