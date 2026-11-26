package fakeinternet_test

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/fakeinternet"
	"golang.org/x/net/dns/dnsmessage"
)

func TestScenarioResponseFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		s    fakeinternet.Scenario
		path string
		want fakeinternet.Response
	}{
		{
			name: "exact path wins over default",
			s: fakeinternet.Scenario{HTTP: []fakeinternet.Response{
				{Path: "", Status: 200, Body: "default"},
				{Path: "/health", Status: 204, Body: "health"},
			}},
			path: "/health",
			want: fakeinternet.Response{Path: "/health", Status: 204, Body: "health"},
		},
		{
			name: "empty path is the default",
			s: fakeinternet.Scenario{HTTP: []fakeinternet.Response{
				{Path: "/health", Status: 204},
				{Path: "", Status: 503, Body: "down"},
			}},
			path: "/missing",
			want: fakeinternet.Response{Path: "", Status: 503, Body: "down"},
		},
		{
			name: "first response is the fallback",
			s:    fakeinternet.Scenario{HTTP: []fakeinternet.Response{{Path: "/health", Status: 204}}},
			path: "/missing",
			want: fakeinternet.Response{Path: "/health", Status: 204},
		},
		{
			name: "no responses means 200",
			s:    fakeinternet.Scenario{},
			path: "/",
			want: fakeinternet.Response{Status: 200},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := fakeinternet.ResponseFor(tc.s, tc.path); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("responseFor(%q) = %+v, want %+v", tc.path, got, tc.want)
			}
		})
	}
}

func TestScenarioDelayDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		delay string
		want  time.Duration
	}{
		{name: "empty", delay: "", want: 0},
		{name: "whitespace", delay: "  ", want: 0},
		{name: "milliseconds", delay: "250ms", want: 250 * time.Millisecond},
		{name: "seconds", delay: "2s", want: 2 * time.Second},
		{name: "negative", delay: "-1s", want: -time.Second},
		{name: "invalid", delay: "soon", want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := (fakeinternet.Scenario{Delay: tc.delay}).DelayDuration(); got != tc.want {
				t.Fatalf("DelayDuration(%q) = %v, want %v", tc.delay, got, tc.want)
			}
		})
	}
}

func TestTrimDot(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"ok.test.", "ok.test"},
		{"ok.test..", "ok.test"},
		{"ok.test", "ok.test"},
		{".", ""},
		{"", ""},
	}

	for _, tc := range tests {
		if got := fakeinternet.TrimDot(tc.in); got != tc.want {
			t.Errorf("trimDot(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAnswerDNS(t *testing.T) {
	t.Parallel()

	inet := fakeinternet.NewTestInternet(map[string]fakeinternet.Scenario{
		"ok.test":     {Host: "ok.test"},
		"custom.test": {Host: "custom.test", A: []string{"192.0.2.5"}},
		"bad.test":    {Host: "bad.test", A: []string{"not-an-ip"}},
		"v6.test":     {Host: "v6.test", AAAAOnly: true},
	}, nil)

	tests := []struct {
		name        string
		query       []byte
		wantNil     bool
		wantRCode   dnsmessage.RCode
		wantAnswers int
		wantA       netip.Addr
	}{
		{
			name:    "malformed query",
			query:   []byte{0x00},
			wantNil: true,
		},
		{
			name:    "query without questions",
			query:   packQuery(t, dnsmessage.Message{Header: dnsmessage.Header{ID: 9}}),
			wantNil: true,
		},
		{
			name:      "unknown host is NXDOMAIN",
			query:     dnsQuery(t, "absent.test.", dnsmessage.TypeA),
			wantRCode: dnsmessage.RCodeNameError,
		},
		{
			name:        "default A is loopback",
			query:       dnsQuery(t, "ok.test.", dnsmessage.TypeA),
			wantAnswers: 1,
			wantA:       netip.MustParseAddr("127.0.0.1"),
		},
		{
			name:        "explicit A is answered",
			query:       dnsQuery(t, "custom.test.", dnsmessage.TypeA),
			wantAnswers: 1,
			wantA:       netip.MustParseAddr("192.0.2.5"),
		},
		{
			name:      "invalid A is SERVFAIL",
			query:     dnsQuery(t, "bad.test.", dnsmessage.TypeA),
			wantRCode: dnsmessage.RCodeServerFailure,
		},
		{
			name:  "AAAA-only host is NODATA for A",
			query: dnsQuery(t, "v6.test.", dnsmessage.TypeA),
		},
		{
			name:  "non-A query is NODATA",
			query: dnsQuery(t, "ok.test.", dnsmessage.TypeAAAA),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reply := fakeinternet.AnswerDNS(inet, tc.query)
			if tc.wantNil {
				if reply != nil {
					t.Fatalf("answerDNS() = %x, want nil", reply)
				}
				return
			}
			if reply == nil {
				t.Fatal("answerDNS() = nil, want a reply")
			}

			var got dnsmessage.Message
			if err := got.Unpack(reply); err != nil {
				t.Fatalf("unpack reply: %v", err)
			}
			if got.RCode != tc.wantRCode {
				t.Fatalf("RCode = %v, want %v", got.RCode, tc.wantRCode)
			}
			if len(got.Answers) != tc.wantAnswers {
				t.Fatalf("answers = %d, want %d", len(got.Answers), tc.wantAnswers)
			}
			if tc.wantA.IsValid() {
				a, ok := got.Answers[0].Body.(*dnsmessage.AResource)
				if !ok {
					t.Fatalf("answer body = %T, want *dnsmessage.AResource", got.Answers[0].Body)
				}
				if addr := netip.AddrFrom4(a.A); addr != tc.wantA {
					t.Fatalf("answer = %v, want %v", addr, tc.wantA)
				}
			}
		})
	}
}

func TestPackMessageRejectsInvalidName(t *testing.T) {
	t.Parallel()

	if got := fakeinternet.PackMessage(dnsmessage.Message{}); got == nil {
		t.Fatal("packMessage rejected an empty message")
	}

	bad := dnsmessage.Message{Questions: []dnsmessage.Question{{
		Name:  dnsmessage.Name{Length: 255},
		Type:  dnsmessage.TypeA,
		Class: dnsmessage.ClassINET,
	}}}
	if got := fakeinternet.PackMessage(bad); got != nil {
		t.Fatalf("packMessage accepted an over-long name: %x", got)
	}
}

func TestMapAddress(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	inet := fakeinternet.NewTestInternet(
		map[string]fakeinternet.Scenario{"ok.test": {Host: "ok.test"}}, ln)

	tests := []struct {
		name    string
		host    string
		port    int
		want    string
		wantErr bool
	}{
		{name: "known host 443", host: "ok.test", port: 443, want: ln.Addr().String()},
		{name: "known host 80", host: "ok.test", port: 80, want: ln.Addr().String()},
		{name: "known host other port", host: "ok.test", port: 8080, wantErr: true},
		{name: "loopback passes through", host: "127.0.0.1", port: 9999, want: "127.0.0.1:9999"},
		{name: "ipv6 loopback passes through", host: "::1", port: 9999, want: "[::1]:9999"},
		{name: "unknown host is refused", host: "elsewhere.test", port: 443, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := fakeinternet.MapAddress(inet, tc.host, tc.port)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("mapAddress(%q, %d) = %q, want an error", tc.host, tc.port, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("mapAddress(%q, %d): %v", tc.host, tc.port, err)
			}
			if got != tc.want {
				t.Fatalf("mapAddress(%q, %d) = %q, want %q", tc.host, tc.port, got, tc.want)
			}
		})
	}
}

func TestHandleSOCKSProtocol(t *testing.T) {
	t.Parallel()

	scenarios, err := fakeinternet.LoadScenarios("../../testdata/internet/scenarios.yaml")
	if err != nil {
		t.Fatalf("load scenarios: %v", err)
	}
	inet, err := fakeinternet.Start(scenarios)
	if err != nil {
		t.Fatalf("start fake internet: %v", err)
	}
	t.Cleanup(func() { _ = inet.Close() })

	greeting := []byte{5, 1, 0}
	greetingReply := []byte{5, 0}
	reject := func(code byte) []byte { return []byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0} }
	success := []byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	domain := func(host string, port uint16) []byte {
		b := []byte{5, 1, 0, 3, byte(len(host))}
		b = append(b, host...)
		return append(b, byte(port>>8), byte(port))
	}

	tests := []struct {
		name    string
		request []byte
		want    []byte
		wantEOF bool
	}{
		{
			name:    "bad greeting version",
			request: []byte{4, 0},
			wantEOF: true,
		},
		{
			name:    "unsupported command",
			request: append(append([]byte{}, greeting...), 5, 2, 0, 1),
			want:    append(append([]byte{}, greetingReply...), reject(7)...),
		},
		{
			name:    "unsupported address type",
			request: append(append([]byte{}, greeting...), 5, 1, 0, 9),
			want:    append(append([]byte{}, greetingReply...), reject(8)...),
		},
		{
			name:    "unknown domain",
			request: append(append([]byte{}, greeting...), domain("elsewhere.test", 443)...),
			want:    append(append([]byte{}, greetingReply...), reject(5)...),
		},
		{
			name: "non-loopback IPv4 is refused",
			request: append(append([]byte{}, greeting...),
				5, 1, 0, 1, 192, 0, 2, 1, 0, 80),
			want: append(append([]byte{}, greetingReply...), reject(5)...),
		},
		{
			name: "non-loopback IPv6 is refused",
			request: append(append(append([]byte{}, greeting...),
				5, 1, 0, 4), append(net.ParseIP("2001:db8::1").To16(), 0, 80)...),
			want: append(append([]byte{}, greetingReply...), reject(5)...),
		},
		{
			name:    "known domain connects",
			request: append(append([]byte{}, greeting...), domain("ok.test", 443)...),
			want:    append(append([]byte{}, greetingReply...), success...),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", inet.SOCKSAddr())
			if err != nil {
				t.Fatalf("dial socks: %v", err)
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

			if _, err := conn.Write(tc.request); err != nil && !tc.wantEOF {
				t.Fatalf("write request: %v", err)
			}

			if tc.wantEOF {
				var b [1]byte
				if _, err := conn.Read(b[:]); err == nil {
					t.Fatal("connection stayed open, want it closed")
				} else {
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						t.Fatalf("read timed out waiting for close: %v", err)
					}
				}
				return
			}

			got := make([]byte, len(tc.want))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatalf("read reply: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("reply = % x, want % x", got, tc.want)
			}
		})
	}
}

func dnsQuery(t *testing.T, name string, qtype dnsmessage.Type) []byte {
	t.Helper()

	n, err := dnsmessage.NewName(name)
	if err != nil {
		t.Fatalf("new name %q: %v", name, err)
	}
	return packQuery(t, dnsmessage.Message{
		Header: dnsmessage.Header{ID: 7, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name: n, Type: qtype, Class: dnsmessage.ClassINET,
		}},
	})
}

func packQuery(t *testing.T, msg dnsmessage.Message) []byte {
	t.Helper()

	raw, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack query: %v", err)
	}
	return raw
}
