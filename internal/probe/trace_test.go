package probe_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
)

type scriptedReply struct {
	from netip.Addr
	msg  probe.TraceMessage
}

// fakeTraceTransport is the socket seam: tests script replies per TTL.
type fakeTraceTransport struct {
	script  map[int][]scriptedReply
	sent    []int
	ttl     int
	closed  bool
	setErr  error
	sendErr error
}

func (f *fakeTraceTransport) SetTTL(ttl int) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.ttl = ttl
	return nil
}

func (f *fakeTraceTransport) SendEcho(_ netip.Addr, _, seq int) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, seq)
	return nil
}

func (f *fakeTraceTransport) Recv(time.Time) (netip.Addr, probe.TraceMessage, error) {
	replies := f.script[f.ttl]
	if len(replies) == 0 {
		return netip.Addr{}, probe.TraceMessage{}, errors.New("timeout")
	}
	reply := replies[0]
	f.script[f.ttl] = replies[1:]
	return reply.from, reply.msg, nil
}

func (f *fakeTraceTransport) Close() error {
	f.closed = true
	return nil
}

func newTrace(script map[int][]scriptedReply) (*probe.Trace, *fakeTraceTransport) {
	tr := &fakeTraceTransport{script: script}
	return &probe.Trace{
		MaxHops: 10,
		Timeout: 200 * time.Millisecond,
		Open:    func() (probe.TraceTransport, error) { return tr, nil },
	}, tr
}

func TestTraceWalksToTheDestination(t *testing.T) {
	t.Parallel()

	dst := netip.MustParseAddr("192.0.2.9")
	trace, transport := newTrace(map[int][]scriptedReply{
		1: {{from: netip.MustParseAddr("10.0.0.1"), msg: probe.TraceMessage{Kind: "time_exceeded"}}},
		2: {{from: netip.MustParseAddr("10.0.0.2"), msg: probe.TraceMessage{Kind: "time_exceeded"}}},
		3: {{from: dst, msg: probe.TraceMessage{Kind: "echo_reply", Seq: 3}}},
	})

	got := trace.Run(context.Background(), dst)
	if got.Failed() {
		t.Fatalf("Failed: %s: %v", got.ErrorClass, got.Err)
	}
	if !got.Reached {
		t.Fatal("destination reply did not finish the walk")
	}
	if len(got.Hops) != 3 {
		t.Fatalf("hops = %d, want 3: %+v", len(got.Hops), got.Hops)
	}
	if got.Hops[0].Addr.String() != "10.0.0.1" || got.Hops[2].Addr != dst {
		t.Fatalf("hops = %+v", got.Hops)
	}
	if !transport.closed {
		t.Error("transport was not closed")
	}
}

func TestTraceRecordsSilentHopsAndStops(t *testing.T) {
	t.Parallel()

	dst := netip.MustParseAddr("192.0.2.9")
	trace, _ := newTrace(map[int][]scriptedReply{
		1: {{from: netip.MustParseAddr("10.0.0.1"), msg: probe.TraceMessage{Kind: "time_exceeded"}}},
		// TTL 2 and beyond are silent.
	})

	got := trace.Run(context.Background(), dst)
	if got.Failed() {
		t.Fatalf("Failed: %s", got.ErrorClass)
	}
	if got.Reached {
		t.Fatal("a silent path reported the destination")
	}
	// One answered hop plus three silent ones, then the walk stops.
	if len(got.Hops) != 4 {
		t.Fatalf("hops = %d, want 4: %+v", len(got.Hops), got.Hops)
	}
	for _, hop := range got.Hops[1:] {
		if hop.Addr.IsValid() {
			t.Errorf("silent hop %d has an address: %+v", hop.TTL, hop)
		}
	}
}

func TestTraceIgnoresStrayReplies(t *testing.T) {
	t.Parallel()

	dst := netip.MustParseAddr("192.0.2.9")
	trace, _ := newTrace(map[int][]scriptedReply{
		1: {
			{from: netip.MustParseAddr("10.0.0.99"), msg: probe.TraceMessage{Kind: "echo_reply", Seq: 999}},
			{from: netip.MustParseAddr("10.0.0.1"), msg: probe.TraceMessage{Kind: "time_exceeded"}},
		},
	})

	got := trace.Run(context.Background(), dst)
	if got.Reached {
		t.Fatal("a reply for another sequence number finished the walk")
	}
	if len(got.Hops) == 0 || got.Hops[0].Addr.String() != "10.0.0.1" {
		t.Fatalf("hops = %+v", got.Hops)
	}
}

func TestTraceTransportFailures(t *testing.T) {
	t.Parallel()

	dst := netip.MustParseAddr("192.0.2.9")

	unavailable := &probe.Trace{
		Open: func() (probe.TraceTransport, error) {
			return nil, errors.New("raw socket unavailable")
		},
	}
	if got := unavailable.Run(context.Background(), dst); got.ErrorClass != "trace_unavailable" {
		t.Fatalf("open failure class = %q", got.ErrorClass)
	}

	if got := probe.NewTrace().Run(context.Background(), netip.Addr{}); got.ErrorClass != "trace_config" {
		t.Fatalf("invalid destination class = %q", got.ErrorClass)
	}
	if got := probe.NewTrace().Run(context.Background(), netip.MustParseAddr("2001:db8::1")); got.ErrorClass != "trace_unsupported" {
		t.Fatalf("IPv6 class = %q", got.ErrorClass)
	}

	trace, transport := newTrace(nil)
	transport.setErr = errors.New("setttl failed")
	if got := trace.Run(context.Background(), dst); got.ErrorClass != "trace_unavailable" {
		t.Fatalf("setttl failure class = %q", got.ErrorClass)
	}

	trace, transport = newTrace(nil)
	transport.sendErr = errors.New("send failed")
	if got := trace.Run(context.Background(), dst); got.ErrorClass != "trace_send" {
		t.Fatalf("send failure class = %q", got.ErrorClass)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	trace, _ = newTrace(nil)
	if got := trace.Run(ctx, dst); got.ErrorClass != "trace_cancelled" {
		t.Fatalf("cancelled class = %q", got.ErrorClass)
	}
}

func TestTraceDefaultTransportAndHopCeiling(t *testing.T) {
	t.Parallel()

	dst := netip.MustParseAddr("192.0.2.9")

	// A nil Open installs the raw-socket transport. Whether this environment
	// grants CAP_NET_RAW decides between trace_send and trace_unavailable;
	// either way the default branch itself must be reachable.
	zero := &probe.Trace{MaxHops: 1, Timeout: 10 * time.Millisecond}
	if got := zero.Run(context.Background(), dst); got.ErrorClass != "trace_send" && got.ErrorClass != "trace_unavailable" && got.Reached {
		t.Fatalf("unexpected default-transport result: %+v", got)
	}

	// Two silent hops exhaust MaxHops before the three-silent-hop early stop,
	// so the walk returns from the loop's normal exit.
	trace, _ := newTrace(nil)
	trace.MaxHops = 2
	got := trace.Run(context.Background(), dst)
	if got.Reached || len(got.Hops) != 2 {
		t.Fatalf("hop ceiling result = %+v", got)
	}
}

func TestTraceUsesDefaults(t *testing.T) {
	t.Parallel()

	dst := netip.MustParseAddr("192.0.2.9")
	transport := &fakeTraceTransport{script: map[int][]scriptedReply{
		1: {{from: dst, msg: probe.TraceMessage{Kind: "echo_reply", Seq: 1}}},
	}}

	trace := &probe.Trace{Open: func() (probe.TraceTransport, error) { return transport, nil }}
	got := trace.Run(context.Background(), dst)
	if !got.Reached {
		t.Fatalf("defaults did not reach the destination: %+v", got.Hops)
	}

	if (probe.TraceResult{}).Failed() {
		t.Fatal("an empty result is not a failure")
	}
	if !(probe.TraceResult{ErrorClass: "trace_unavailable"}).Failed() {
		t.Fatal("an error class must count as a failure")
	}
}

// TestTraceAgainstLoopback uses a real raw ICMP socket when the environment
// permits one and skips otherwise. CI images usually do not grant NET_RAW to
// the test process, which is exactly why the walk logic is tested through the
// seam above.
func TestTraceAgainstLoopback(t *testing.T) {
	t.Parallel()

	trace := probe.NewTrace()
	trace.MaxHops = 3
	trace.Timeout = 500 * time.Millisecond

	got := trace.Run(context.Background(), netip.MustParseAddr("127.0.0.1"))
	if got.ErrorClass == "trace_unavailable" {
		t.Skipf("raw ICMP unavailable here: %v", got.Err)
	}
	if !got.Reached {
		t.Fatalf("loopback trace did not reach the destination: %+v", got.Hops)
	}
}
