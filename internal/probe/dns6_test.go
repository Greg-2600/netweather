package probe_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/Greg-2600/netweather/internal/probe"
	"golang.org/x/net/dns/dnsmessage"
)

func TestDNSDo6QueriesAAAARecords(t *testing.T) {
	t.Parallel()

	server := startDNSResponder(t, func(query []byte) []byte {
		msg, ok := decodeQuery(query)
		if !ok || len(msg.Questions) != 1 {
			return nil
		}
		if msg.Questions[0].Type != dnsmessage.TypeAAAA {
			return nil
		}
		msg.Response = true
		msg.RecursionAvailable = true
		msg.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name: msg.Questions[0].Name, Type: dnsmessage.TypeAAAA,
				Class: dnsmessage.ClassINET, TTL: 60,
			},
			Body: &dnsmessage.AAAAResource{AAAA: [16]byte{
				0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1,
			}},
		}}
		return packMessage(msg)
	})

	got := probe.NewDNS(nil).Do6(context.Background(), server, "v6.example")
	if got.Failed() {
		t.Fatalf("Do6 failed: %s: %v", got.ErrorClass, got.Err)
	}
	if len(got.Addresses) != 1 || got.Addresses[0].String() != "2001:db8::1" {
		t.Fatalf("addresses = %v", got.Addresses)
	}
}

func TestDNSDo6ReportsFailures(t *testing.T) {
	t.Parallel()

	d := probe.NewDNS(nil)
	if got := d.Do6(context.Background(), "", "v6.example"); got.ErrorClass != "dns_config" {
		t.Fatalf("empty server class = %q", got.ErrorClass)
	}
	if got := d.Do6(context.Background(), "127.0.0.1:53", ""); got.ErrorClass != "dns_config" {
		t.Fatalf("empty name class = %q", got.ErrorClass)
	}

	unreachable := probe.NewDNS(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unreachable")
	})
	if got := unreachable.Do6(context.Background(), "127.0.0.1:53", "v6.example"); got.ErrorClass != "dns_error" {
		t.Fatalf("dial failure class = %q", got.ErrorClass)
	}

	nxdomain := startDNSResponder(t, func(query []byte) []byte {
		return dnsResponse(query, 3, "")
	})
	if got := probe.NewDNS(nil).Do6(context.Background(), nxdomain, "missing.example"); got.ErrorClass != "dns_nxdomain" {
		t.Fatalf("nxdomain class = %q", got.ErrorClass)
	}
}
