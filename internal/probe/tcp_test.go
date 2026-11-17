package probe_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
)

func TestClassifyDialError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"deadline", context.DeadlineExceeded, "tcp_timeout"},
		{"nxdomain", &net.DNSError{IsNotFound: true}, "dns_nxdomain"},
		{"dns timeout", &net.DNSError{IsTimeout: true}, "dns_timeout"},
		{"dns other", &net.DNSError{Err: "servfail"}, "dns_error"},
		{"refused", &net.OpError{Op: "dial", Err: errors.New("refused")}, "tcp_refused"},
		{"reset", &net.OpError{Op: "read", Err: errors.New("reset")}, "tcp_reset"},
		{"other", errors.New("boom"), "tcp_error"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := probe.ClassifyDialError(tc.err); got != tc.want {
				t.Fatalf("ClassifyDialError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestTCPSucceedsOnConnect(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	defer func() { _ = server.Close() }()

	p := &probe.TCP{Dial: func(context.Context, string, string) (net.Conn, error) {
		time.Sleep(time.Millisecond) // make the measured latency meaningful
		return client, nil
	}}
	res := p.Do(context.Background(), "example.com:443")
	if res.Failed() {
		t.Fatalf("Failed: %s", res.Err)
	}
	if res.Latency <= 0 {
		t.Fatalf("latency = %v", res.Latency)
	}
}

func TestTCPClassifiesDialFailure(t *testing.T) {
	t.Parallel()

	p := &probe.TCP{Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: errors.New("refused")}
	}}

	res := p.Do(context.Background(), "example.com:443")
	if res.ErrorClass != "tcp_refused" {
		t.Fatalf("class = %q", res.ErrorClass)
	}
}

func TestTCPConstructorUsesDefaults(t *testing.T) {
	t.Parallel()

	p := probe.NewTCP(nil)
	if p.Dial == nil {
		t.Fatal("NewTCP left Dial nil without installing a default")
	}

	withDial := probe.NewTCPWithDial(nil)
	if withDial.Dial == nil {
		t.Fatal("NewTCPWithDial left Dial nil without installing a default")
	}
}
