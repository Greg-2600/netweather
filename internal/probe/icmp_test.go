package probe_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
)

// fakeEcho answers only the sequence numbers it is told to.
type fakeEcho struct {
	reply   map[int]bool
	seq     int
	sent    []int
	closed  bool
	sendErr error
	kind    string
}

func (f *fakeEcho) SendEcho(_ netip.Addr, _, seq int) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.seq = seq
	f.sent = append(f.sent, seq)
	return nil
}

func (f *fakeEcho) Recv(time.Time) (netip.Addr, probe.TraceMessage, error) {
	if !f.reply[f.seq] {
		return netip.Addr{}, probe.TraceMessage{}, errors.New("timeout")
	}
	kind := f.kind
	if kind == "" {
		kind = "echo_reply"
	}
	return netip.MustParseAddr("192.0.2.9"),
		probe.TraceMessage{Kind: kind, Seq: f.seq}, nil
}

func (f *fakeEcho) Close() error {
	f.closed = true
	return nil
}

func ping(t *testing.T, transport *fakeEcho, count int) *probe.ICMP {
	t.Helper()

	ping := probe.NewICMP()
	ping.Count = count
	ping.Timeout = 50 * time.Millisecond
	ping.Open = func() (probe.EchoTransport, error) { return transport, nil }
	ping.Resolve = func(context.Context, string) (netip.Addr, error) {
		return netip.MustParseAddr("192.0.2.9"), nil
	}
	return ping
}

func TestICMPCountsReplies(t *testing.T) {
	t.Parallel()

	transport := &fakeEcho{reply: map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true}}
	got := ping(t, transport, 5).Do(context.Background(), "anchor.example")

	if got.Failed() {
		t.Fatalf("class = %q: %v", got.ErrorClass, got.Err)
	}
	if got.Replied != 5 || got.Lost != 0 {
		t.Fatalf("replied/lost = %d/%d", got.Replied, got.Lost)
	}
	if !transport.closed {
		t.Error("transport was not closed")
	}
}

func TestICMPPartialLossIsNotAFailure(t *testing.T) {
	t.Parallel()

	transport := &fakeEcho{reply: map[int]bool{1: true, 3: true}}
	got := ping(t, transport, 5).Do(context.Background(), "anchor.example")

	if got.ErrorClass != "icmp_loss_partial" {
		t.Fatalf("class = %q, want icmp_loss_partial", got.ErrorClass)
	}
	if got.Replied != 2 || got.Lost != 3 {
		t.Fatalf("replied/lost = %d/%d", got.Replied, got.Lost)
	}
}

func TestICMPNoRepliesIsATimeout(t *testing.T) {
	t.Parallel()

	transport := &fakeEcho{reply: map[int]bool{}}
	got := ping(t, transport, 5).Do(context.Background(), "anchor.example")

	if got.ErrorClass != "icmp_timeout" {
		t.Fatalf("class = %q, want icmp_timeout", got.ErrorClass)
	}
	if got.Replied != 0 || got.Lost != 5 {
		t.Fatalf("replied/lost = %d/%d", got.Replied, got.Lost)
	}
	if got.RTT != 0 {
		t.Errorf("rtt with no replies = %v", got.RTT)
	}
}

func TestICMPIgnoresAReplyOfAnotherKind(t *testing.T) {
	t.Parallel()

	transport := &fakeEcho{reply: map[int]bool{1: true}, kind: "time_exceeded"}
	got := ping(t, transport, 1).Do(context.Background(), "anchor.example")
	if got.ErrorClass != "icmp_timeout" || got.Replied != 0 {
		t.Fatalf("result = %+v", got)
	}
}

func TestICMPFailuresAreVisible(t *testing.T) {
	t.Parallel()

	dst := netip.MustParseAddr("192.0.2.9")

	unavailable := probe.NewICMP()
	unavailable.Open = func() (probe.EchoTransport, error) {
		return nil, errors.New("CAP_NET_RAW is required")
	}
	unavailable.Resolve = func(context.Context, string) (netip.Addr, error) { return dst, nil }
	if got := unavailable.Do(context.Background(), "anchor.example"); got.ErrorClass != "icmp_unavailable" {
		t.Fatalf("open failure class = %q", got.ErrorClass)
	}

	badResolve := probe.NewICMP()
	badResolve.Resolve = func(context.Context, string) (netip.Addr, error) {
		return netip.Addr{}, errors.New("nxdomain")
	}
	if got := badResolve.Do(context.Background(), "missing.example"); got.ErrorClass != "icmp_config" {
		t.Fatalf("resolve failure class = %q", got.ErrorClass)
	}

	ipv6 := probe.NewICMP()
	ipv6.Resolve = func(context.Context, string) (netip.Addr, error) {
		return netip.MustParseAddr("2001:db8::1"), nil
	}
	if got := ipv6.Do(context.Background(), "anchor.example"); got.ErrorClass != "icmp_unsupported" {
		t.Fatalf("IPv6 class = %q", got.ErrorClass)
	}

	transport := &fakeEcho{reply: map[int]bool{}, sendErr: errors.New("network unreachable")}
	if got := ping(t, transport, 5).Do(context.Background(), "anchor.example"); got.ErrorClass != "icmp_unreachable" {
		t.Fatalf("send failure class = %q", got.ErrorClass)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	transport = &fakeEcho{reply: map[int]bool{1: true}}
	if got := ping(t, transport, 5).Do(ctx, "anchor.example"); got.ErrorClass != "icmp_cancelled" {
		t.Fatalf("cancelled class = %q", got.ErrorClass)
	}
}

func TestICMPDefaultsAndLiteralAddresses(t *testing.T) {
	t.Parallel()

	transport := &fakeEcho{reply: map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true}}

	// Count 0 means the documented five; a literal address needs no resolver.
	instrument := probe.NewICMP()
	instrument.Open = func() (probe.EchoTransport, error) { return transport, nil }
	instrument.Timeout = 0 // exercises the documented default deadline
	instrument.Resolve = func(context.Context, string) (netip.Addr, error) {
		t.Fatal("a literal address should not be resolved")
		return netip.Addr{}, nil
	}

	got := instrument.Do(context.Background(), "192.0.2.9")
	if got.Failed() || got.Replied != 5 {
		t.Fatalf("result = %+v", got)
	}
	if len(transport.sent) != 5 {
		t.Fatalf("sent %d echoes, want 5", len(transport.sent))
	}
}

func TestICMPFallsBackToTheSystemResolverForHostnames(t *testing.T) {
	t.Parallel()

	// localhost resolves without network access on every supported platform.
	transport := &fakeEcho{reply: map[int]bool{1: true}}
	instrument := probe.NewICMP()
	instrument.Count = 1
	instrument.Timeout = 50 * time.Millisecond
	instrument.Open = func() (probe.EchoTransport, error) { return transport, nil }
	// Resolve is deliberately nil: this exercises the default lookup path.

	got := instrument.Do(context.Background(), "localhost")
	if got.ErrorClass == "icmp_config" {
		t.Fatalf("localhost did not resolve: %v", got.Err)
	}
	if got.Replied != 1 {
		t.Fatalf("result = %+v", got)
	}
}

func TestICMPDefaultTransportOpenIsCallable(t *testing.T) {
	t.Parallel()

	// The production transport either opens a raw socket or reports why it
	// cannot. Either answer is fine here; the point is that the default closure
	// is wired, not that this environment grants CAP_NET_RAW.
	transport, err := probe.NewICMP().Open()
	if err == nil {
		_ = transport.Close()
	}
}
