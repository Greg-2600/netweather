package netx_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/netx"
)

type staticResolver struct{ addrs []netip.Addr }

func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r.addrs, nil
}

// pipeDial returns one end of an in-memory connection so a test never touches
// the network, while still producing a real net.Conn for the dialer.
func pipeDial(context.Context, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	_ = server.Close()
	return client, nil
}

// TestDialerEmitsTraceStages pins LOGGING.md §4: the instrument reports one
// TRACE line per dial stage (dns, then tcp), so a scoped debug session can see
// where a probe spent its time. Nothing is emitted at the normal level.
func TestDialerEmitsTraceStages(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: logging.LevelTrace}))
	d := &netx.Dialer{
		Policy:   netx.DefaultPolicy(),
		Resolver: staticResolver{addrs: []netip.Addr{netip.MustParseAddr("1.1.1.1")}},
		Dial:     pipeDial,
	}

	ctx := logging.WithLogger(context.Background(), logger)
	conn, err := d.DialContext(ctx, "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	_ = conn.Close()

	out := buf.String()
	for _, want := range []string{"stage=dns", "stage=tcp", "host=example.com", "addr=1.1.1.1"} {
		if !strings.Contains(out, want) {
			t.Errorf("trace output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "event=probe_stage") {
		t.Errorf("stage lines lack the event key:\n%s", out)
	}
}

func TestDialerIsSilentAtInfo(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil)) // INFO
	d := &netx.Dialer{
		Policy:   netx.DefaultPolicy(),
		Resolver: staticResolver{addrs: []netip.Addr{netip.MustParseAddr("1.1.1.1")}},
		Dial:     pipeDial,
	}

	conn, err := d.DialContext(logging.WithLogger(context.Background(), logger), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	_ = conn.Close()

	if buf.Len() != 0 {
		t.Fatalf("a normal level emitted trace output:\n%s", buf.String())
	}
}
