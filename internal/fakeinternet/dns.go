package fakeinternet

import (
	"net/netip"

	"golang.org/x/net/dns/dnsmessage"
)

// loopback is the only address the fake internet answers with, because every
// scenario listener is local.
var loopback = netip.MustParseAddr("127.0.0.1")

// serveDNS answers A queries from the scenario list. A name that is not in the
// list is NXDOMAIN; a scenario with aaaa_only (or no address at all) answers
// NOERROR with no records, which is NODATA.
//
// It is deliberately a real wire server rather than a net.Resolver fake: the
// DNS instrument speaks the protocol itself, and this is what proves the
// RCODE handling against a responder that is not the system resolver.
func (i *Internet) serveDNS() {
	buf := make([]byte, 1500)
	for {
		n, addr, err := i.dnsConn.ReadFrom(buf)
		if err != nil {
			return
		}
		reply := i.answerDNS(buf[:n])
		if reply == nil {
			continue
		}
		if _, err := i.dnsConn.WriteTo(reply, addr); err != nil {
			return
		}
	}
}

func (i *Internet) answerDNS(query []byte) []byte {
	var msg dnsmessage.Message
	if err := msg.Unpack(query); err != nil || len(msg.Questions) == 0 {
		return nil
	}
	question := msg.Questions[0]

	resp := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 msg.ID,
			Response:           true,
			RecursionAvailable: true,
		},
		Questions: msg.Questions,
	}

	host := trimDot(question.Name.String())
	scenario, ok := i.byHost[host]
	if !ok {
		resp.RCode = dnsmessage.RCodeNameError
		return packMessage(resp)
	}

	if question.Type == dnsmessage.TypeA && !scenario.AAAAOnly {
		addr := loopback
		if len(scenario.A) > 0 {
			parsed, err := netip.ParseAddr(scenario.A[0])
			if err != nil {
				resp.RCode = dnsmessage.RCodeServerFailure
				return packMessage(resp)
			}
			addr = parsed
		}
		resp.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name: question.Name, Type: dnsmessage.TypeA,
				Class: dnsmessage.ClassINET, TTL: 60,
			},
			Body: &dnsmessage.AResource{A: addr.As4()},
		}}
	}
	// NOERROR with no answers is NODATA, which is the honest answer here.
	return packMessage(resp)
}

func packMessage(msg dnsmessage.Message) []byte {
	packed, err := msg.Pack()
	if err != nil {
		return nil
	}
	return packed
}

func trimDot(name string) string {
	for len(name) > 0 && name[len(name)-1] == '.' {
		name = name[:len(name)-1]
	}
	return name
}
