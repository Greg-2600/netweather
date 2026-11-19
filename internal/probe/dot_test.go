package probe

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

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
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
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
					Header: dnsmessage.Header{ID: msg.ID, Response: true, RecursionAvailable: true},
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