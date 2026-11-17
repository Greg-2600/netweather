package probe_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
)

func ntpStamp(t time.Time) []byte {
	b := make([]byte, 8)
	secs := uint32(t.Unix() + 2208988800)
	frac := uint64(t.Nanosecond()) << 32 / 1e9
	binary.BigEndian.PutUint32(b[0:4], secs)
	binary.BigEndian.PutUint32(b[4:8], uint32(frac))
	return b
}

// startNTP runs a minimal NTP server that answers with a fixed stratum and a
// transmit timestamp offset from the request.
func startNTP(t *testing.T, stratum int, serverTime func() time.Time) string {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 48)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 48 {
				continue
			}

			reply := make([]byte, 48)
			reply[0] = 0x24 // LI=0, VN=4, Mode=4 (server)
			reply[1] = byte(stratum)
			// Originate = the client's transmit timestamp, as expected.
			copy(reply[24:32], buf[40:48])

			now := serverTime()
			copy(reply[32:40], ntpStamp(now))                        // receive
			copy(reply[40:48], ntpStamp(now.Add(-time.Millisecond))) // transmit
			_, _ = pc.WriteTo(reply, addr)
		}
	}()

	return pc.LocalAddr().String()
}

func TestNTPReadsOffsetAndStratum(t *testing.T) {
	t.Parallel()

	server := startNTP(t, 2, time.Now)
	// The production dialer allows UDP/123 only; the fake listens on a random
	// port, so the tests inject a plain dialer and exercise the protocol instead.
	p := probe.NewNTPWithDial((&net.Dialer{}).DialContext)

	res := p.Do(context.Background(), server)
	if res.Failed() {
		t.Fatalf("Failed: %s: %v", res.ErrorClass, res.Err)
	}
	if res.Stratum != 2 {
		t.Fatalf("stratum = %d", res.Stratum)
	}
	if res.RTT <= 0 {
		t.Fatalf("rtt = %v", res.RTT)
	}
	// The fake's transmit timestamp is 1ms behind its receive stamp, so the
	// measured offset is small but the sign must be sane either way.
	if res.Offset > time.Minute || res.Offset < -time.Minute {
		t.Fatalf("offset = %v, want a small value", res.Offset)
	}
}

func TestNTPRejectsKissOfDeath(t *testing.T) {
	t.Parallel()

	server := startNTP(t, 0, time.Now)
	res := probe.NewNTPWithDial((&net.Dialer{}).DialContext).Do(context.Background(), server)

	if res.ErrorClass != "ntp_denied" {
		t.Fatalf("class = %q, want ntp_denied", res.ErrorClass)
	}
}

func TestNTPRejectsUnsynchronisedServer(t *testing.T) {
	t.Parallel()

	server := startNTP(t, 16, time.Now)
	res := probe.NewNTPWithDial((&net.Dialer{}).DialContext).Do(context.Background(), server)

	if res.ErrorClass != "ntp_bad_response" {
		t.Fatalf("class = %q, want ntp_bad_response", res.ErrorClass)
	}
}

func TestNTPTimesOutOnASilentServer(t *testing.T) {
	t.Parallel()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()

	p := probe.NewNTPWithDial((&net.Dialer{}).DialContext)
	p.Timeout = 80 * time.Millisecond

	start := time.Now()
	res := p.Do(context.Background(), pc.LocalAddr().String())
	if res.ErrorClass != "ntp_timeout" {
		t.Fatalf("class = %q, want ntp_timeout (%v)", res.ErrorClass, res.Err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("exchange took %v despite the timeout", elapsed)
	}
}

func TestNTPClassifiesDialFailure(t *testing.T) {
	t.Parallel()

	p := probe.NewNTPWithDial(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("network unreachable")
	})
	res := p.Do(context.Background(), "127.0.0.1:123")

	if res.ErrorClass != "ntp_unreachable" {
		t.Fatalf("class = %q, want ntp_unreachable", res.ErrorClass)
	}
}

func TestClassifyNTPError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"deadline", context.DeadlineExceeded, "ntp_timeout"},
		{"net timeout", &net.DNSError{IsTimeout: true}, "ntp_timeout"},
		{"other", errors.New("boom"), "ntp_unreachable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := probe.ClassifyNTPError(tc.err); got != tc.want {
				t.Fatalf("ClassifyNTPError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestNTPConstructorsUseDefaults(t *testing.T) {
	t.Parallel()

	if probe.NewNTP(nil).Dial == nil {
		t.Fatal("NewNTP left Dial nil")
	}
	if probe.NewNTPWithDial(nil).Dial == nil {
		t.Fatal("NewNTPWithDial left Dial nil")
	}
}
