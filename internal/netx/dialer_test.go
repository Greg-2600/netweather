package netx_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/Greg-2600/netweather/internal/netx"
)

type fakeResolver struct {
	mu     sync.Mutex
	addrs  map[string][]netip.Addr
	err    error
	calls  int
	lookup []string
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.lookup = append(f.lookup, host)
	if f.err != nil {
		return nil, f.err
	}
	return f.addrs[host], nil
}

type recorder struct {
	mu        sync.Mutex
	addresses []string
	err       error
}

func (r *recorder) dial(_ context.Context, _, address string) (net.Conn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.addresses = append(r.addresses, address)
	return nil, r.err
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.addresses...)
}

func newDialer(addrs map[string][]netip.Addr, rec *recorder) (*netx.Dialer, *fakeResolver) {
	res := &fakeResolver{addrs: addrs}
	return &netx.Dialer{Policy: netx.DefaultPolicy(), Resolver: res, Dial: rec.dial}, res
}

func TestDialRejectsBlockedPortBeforeResolving(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	d, res := newDialer(map[string][]netip.Addr{
		"example.com": {netip.MustParseAddr("93.184.216.34")},
	}, rec)

	_, err := d.DialContext(context.Background(), "tcp", "example.com:22")
	if !errors.Is(err, netx.ErrBlockedPort) {
		t.Fatalf("error = %v, want ErrBlockedPort", err)
	}
	if res.calls != 0 {
		t.Fatal("resolver must not run for a blocked port")
	}
	if len(rec.seen()) != 0 {
		t.Fatal("dial happened for a blocked port")
	}
}

func TestDialBlocksPrivateLiterals(t *testing.T) {
	t.Parallel()

	for _, address := range []string{
		"127.0.0.1:80",
		"10.0.0.1:443",
		"169.254.169.254:80",
		"[::1]:443",
		"[::ffff:127.0.0.1]:443",
		"[fd00::1]:443",
	} {
		rec := &recorder{}
		d, _ := newDialer(nil, rec)

		_, err := d.DialContext(context.Background(), "tcp", address)
		if !errors.Is(err, netx.ErrBlocked) {
			t.Errorf("%s: error = %v, want ErrBlocked", address, err)
		}
		if len(rec.seen()) != 0 {
			t.Errorf("%s: dial happened despite the block", address)
		}
	}
}

func TestDialDialsTheValidatedIPNotTheHostname(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	d, res := newDialer(map[string][]netip.Addr{
		"example.com": {netip.MustParseAddr("93.184.216.34")},
	}, rec)

	conn, err := d.DialContext(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	if conn != nil {
		t.Fatalf("expected the stub dialer's nil conn, got %v", conn)
	}

	seen := rec.seen()
	if len(seen) != 1 || seen[0] != "93.184.216.34:443" {
		t.Fatalf("dialed %v, want the IP literal 93.184.216.34:443", seen)
	}
	if res.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", res.calls)
	}
}

func TestDialSkipsBlockedAnswers(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	d, _ := newDialer(map[string][]netip.Addr{
		"mixed.example": {
			netip.MustParseAddr("10.0.0.1"),
			netip.MustParseAddr("93.184.216.34"),
		},
	}, rec)

	if _, err := d.DialContext(context.Background(), "tcp", "mixed.example:443"); err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	seen := rec.seen()
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "93.184.216.34:") {
		t.Fatalf("dialed %v; a blocked answer must be skipped, not fatal", seen)
	}
}

func TestDialFailsWhenEveryAnswerIsBlocked(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	d, _ := newDialer(map[string][]netip.Addr{
		"internal.example": {
			netip.MustParseAddr("10.0.0.1"),
			netip.MustParseAddr("192.168.0.1"),
		},
	}, rec)

	_, err := d.DialContext(context.Background(), "tcp", "internal.example:443")
	if !errors.Is(err, netx.ErrBlocked) {
		t.Fatalf("error = %v, want ErrBlocked", err)
	}
	if len(rec.seen()) != 0 {
		t.Fatalf("dialed %v despite every answer being blocked", rec.seen())
	}
}

func TestDialPropagatesResolverFailure(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	d, res := newDialer(nil, rec)
	res.err = errors.New("servfail")

	_, err := d.DialContext(context.Background(), "tcp", "broken.example:443")
	if err == nil || !strings.Contains(err.Error(), "servfail") {
		t.Fatalf("error = %v, want the resolver failure", err)
	}
}

func TestDialRejectsEmptyResolution(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	d, _ := newDialer(map[string][]netip.Addr{"empty.example": nil}, rec)

	_, err := d.DialContext(context.Background(), "tcp", "empty.example:443")
	if err == nil || !strings.Contains(err.Error(), "no addresses") {
		t.Fatalf("error = %v, want a no-addresses error", err)
	}
}

func TestDialRejectsMalformedAddress(t *testing.T) {
	t.Parallel()

	d, _ := newDialer(nil, &recorder{})
	if _, err := d.DialContext(context.Background(), "tcp", "not-a-host-port"); err == nil {
		t.Fatal("malformed address should be rejected")
	}
	if _, err := d.DialContext(context.Background(), "tcp", "example.com:https"); err == nil {
		t.Fatal("non-numeric port should be rejected")
	}
}

// With no resolver injected, netx must use the system resolver rather than
// panicking. A cancelled context keeps the test offline.
func TestDialDefaultsToSystemResolver(t *testing.T) {
	t.Parallel()

	d := &netx.Dialer{Policy: netx.DefaultPolicy(), Dial: (&recorder{}).dial}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := d.DialContext(ctx, "tcp", "example.com:443"); err == nil {
		t.Fatal("a cancelled context should fail resolution")
	}
}

func TestDialDoesNotCacheResolution(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	d, res := newDialer(map[string][]netip.Addr{
		"rotating.example": {netip.MustParseAddr("93.184.216.34")},
	}, rec)

	for i := 0; i < 2; i++ {
		if _, err := d.DialContext(context.Background(), "tcp", "rotating.example:443"); err != nil {
			t.Fatal(err)
		}
	}
	if res.calls != 2 {
		t.Fatalf("resolver calls = %d, want 2 (a cache is a rebinding vector)", res.calls)
	}
}

func TestDialReturnsPermittedConnectionFailure(t *testing.T) {
	t.Parallel()

	blockedOrNot := errors.New("connection refused")
	rec := &recorder{err: blockedOrNot}
	d, _ := newDialer(map[string][]netip.Addr{
		"down.example": {netip.MustParseAddr("93.184.216.34")},
	}, rec)

	_, err := d.DialContext(context.Background(), "tcp", "down.example:443")
	if !errors.Is(err, blockedOrNot) {
		t.Fatalf("error = %v, want the dial failure (not a policy refusal)", err)
	}
}

func TestCheckHostReturnsOnlyAllowed(t *testing.T) {
	t.Parallel()

	d, _ := newDialer(map[string][]netip.Addr{
		"mixed.example": {
			netip.MustParseAddr("10.0.0.1"),
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("::1"),
		},
	}, &recorder{})

	allowed, err := d.CheckHost(context.Background(), "mixed.example")
	if err != nil {
		t.Fatalf("CheckHost: %v", err)
	}
	if len(allowed) != 1 || allowed[0].String() != "93.184.216.34" {
		t.Fatalf("allowed = %v, want just the public address", allowed)
	}
}

func TestCheckHostAllBlocked(t *testing.T) {
	t.Parallel()

	d, _ := newDialer(map[string][]netip.Addr{
		"internal.example": {netip.MustParseAddr("127.0.0.1")},
	}, &recorder{})

	if _, err := d.CheckHost(context.Background(), "internal.example"); !errors.Is(err, netx.ErrBlocked) {
		t.Fatalf("error = %v, want ErrBlocked", err)
	}
}

func TestCheckHostLiteral(t *testing.T) {
	t.Parallel()

	d, res := newDialer(nil, &recorder{})
	allowed, err := d.CheckHost(context.Background(), "93.184.216.34")
	if err != nil {
		t.Fatalf("CheckHost: %v", err)
	}
	if len(allowed) != 1 {
		t.Fatalf("allowed = %v", allowed)
	}
	if res.calls != 0 {
		t.Fatal("a literal address must not hit the resolver")
	}
}

func TestCheckHostEmptyResolution(t *testing.T) {
	t.Parallel()

	d, _ := newDialer(map[string][]netip.Addr{"empty.example": nil}, &recorder{})
	if _, err := d.CheckHost(context.Background(), "empty.example"); err == nil {
		t.Fatal("empty resolution should be an error")
	}
}

func TestNewHasProductionDefaults(t *testing.T) {
	t.Parallel()

	d := netx.New()
	if d.Resolver == nil || d.Dial == nil {
		t.Fatalf("New left a seam nil: %+v", d)
	}
	if !d.Policy.PortAllowed(443) || d.Policy.PortAllowed(22) {
		t.Fatalf("unexpected default policy: %+v", d.Policy)
	}
}

// A nil Dial must fall back to the standard dialer rather than panicking. A
// cancelled context keeps this offline while still exercising the path.
func TestDialFallsBackToStandardDialer(t *testing.T) {
	t.Parallel()

	d := &netx.Dialer{
		Policy:   netx.DefaultPolicy(),
		Resolver: &fakeResolver{addrs: map[string][]netip.Addr{"example.com": {netip.MustParseAddr("93.184.216.34")}}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := d.DialContext(ctx, "tcp", "example.com:443"); err == nil {
		t.Fatal("a cancelled context should fail the dial")
	}
}

func TestBlockedPortErrorMentionsPort(t *testing.T) {
	t.Parallel()

	d, _ := newDialer(nil, &recorder{})
	_, err := d.DialContext(context.Background(), "tcp", "example.com:3306")
	if !errors.Is(err, netx.ErrBlockedPort) {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "3306") {
		t.Fatalf("message %q omits the port", err.Error())
	}
}
