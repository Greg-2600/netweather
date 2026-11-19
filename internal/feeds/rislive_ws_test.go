package feeds_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/feeds"
)

// scriptedWS replays a short frame script, then reports EOF. It records what
// the stream wrote so the subscribe and pong frames can be asserted.
type scriptedWS struct {
	frames [][]byte
	idx    int
	writes [][]byte
}

func (s *scriptedWS) Read(p []byte) (int, error) {
	if s.idx >= len(s.frames) {
		return 0, io.EOF
	}
	frame := s.frames[s.idx]
	s.idx++
	return copy(p, frame), nil
}

func (s *scriptedWS) Write(p []byte) (int, error) {
	s.writes = append(s.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (s *scriptedWS) Close() error { return nil }

func TestRISLiveWSStreamsMapsAndReconnects(t *testing.T) {
	t.Parallel()

	frames := [][]byte{
		[]byte(`{"type":"ping","data":null}`),
		[]byte(`{"type":"ris_message","data":{"type":"KEEPALIVE","host":"rrc00","id":"k1","timestamp":1763000000.1}}`),
		[]byte(`{"type":"ris_message","data":{"type":"UPDATE","host":"rrc00","id":"u1","timestamp":1763000000.25,"peer":"192.0.2.1","peer_asn":"64496","path":[15169,3356],"announcements":[{"next_hop":"192.0.2.1","prefixes":["198.51.100.0/24"]}],"withdrawals":["203.0.113.0/24"]}}`),
		[]byte(`not json`),
	}

	var dials atomic.Int32
	var connections []*scriptedWS
	sink := &fakeSink{}
	stream := feeds.NewRISLiveWS(sink, []int{15169})
	stream.FlushEvery = 10 * time.Millisecond
	stream.ReconnectBase = time.Millisecond
	stream.ReconnectMax = time.Millisecond
	stream.Dial = func(_ context.Context, _ string) (feeds.WSConn, error) {
		dials.Add(1)
		conn := &scriptedWS{frames: frames}
		connections = append(connections, conn)
		return conn, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	feeds.StreamLoop(ctx, stream, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if got := dials.Load(); got < 2 {
		t.Fatalf("dials = %d, want at least 2 (reconnect after EOF)", got)
	}
	if len(sink.events) == 0 {
		t.Fatal("no BGP events were stored")
	}

	var found bool
	for _, ev := range sink.events {
		if ev.ExternalID != "risws:rrc00:u1" {
			continue
		}
		found = true
		if ev.Feed != feeds.RISLiveWSFeed || ev.Kind != "bgp_withdrawal" || ev.Severity != 1 {
			t.Errorf("event = %+v", ev)
		}
		if ev.ASN != 15169 {
			t.Errorf("ASN = %d, want the configured ASN in the path (15169)", ev.ASN)
		}
		if !strings.Contains(ev.Payload, "203.0.113.0/24") {
			t.Errorf("payload missing the withdrawn prefix: %s", ev.Payload)
		}
	}
	if !found {
		t.Fatalf("the UPDATE event was not stored; got %+v", sink.events)
	}
	if len(sink.failures) == 0 || len(sink.successes) == 0 {
		t.Errorf("feed health not recorded: failures=%v successes=%d", sink.failures, len(sink.successes))
	}
	if len(connections) == 0 {
		t.Fatal("no connection was opened")
	}
	// A reconnect may return a fresh connection that never read a frame, so
	// assert across every connection rather than only the most recent one.
	var subscribed, ponged bool
	for _, conn := range connections {
		for _, w := range conn.writes {
			text := string(w)
			if strings.Contains(text, `"type":"ris_subscribe"`) && strings.Contains(text, `"path":15169`) {
				subscribed = true
			}
			if strings.Contains(text, `"type":"pong"`) {
				ponged = true
			}
		}
	}
	if !subscribed {
		t.Error("stream did not subscribe to the configured ASN")
	}
	if !ponged {
		t.Error("stream did not answer an application-level ping")
	}
}

func TestRISLiveWSNeedsAnASN(t *testing.T) {
	t.Parallel()

	stream := feeds.NewRISLiveWS(&fakeSink{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// With no ASN the stream records a failure and keeps retrying until ctx is
	// done; it must not dial or store an event.
	feeds.StreamLoop(ctx, stream, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sink := stream.Sink.(*fakeSink)
	if len(sink.events) != 0 || len(sink.failures) == 0 {
		t.Fatalf("events=%d failures=%d; want no events and a recorded failure",
			len(sink.events), len(sink.failures))
	}
}
