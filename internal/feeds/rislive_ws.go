package feeds

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/store"
	"golang.org/x/net/websocket"
)

// RISLiveWSFeed is the feed name used for health and audit records. It is kept
// separate from the polling adapter's "ris-live" so both can run at once and a
// stale poller cannot mask a dead stream.
const RISLiveWSFeed = "ris-live-ws"

// DefaultRISLiveWSURL is RIPE's public real-time BGP WebSocket endpoint. The
// client query parameter is RIPE's recommendation for identifying an app.
const DefaultRISLiveWSURL = "wss://ris-live.ripe.net/v1/ws/?client=netweather"

const (
	defaultWSFlushEvery  = 10 * time.Second
	defaultWSDialBackoff = time.Second
	defaultWSMaxBackoff  = time.Minute
	defaultWSReadBuffer  = 1 << 20
	defaultWSBatchSize   = 128
)

// WSConn is the narrow part of a WebSocket connection the stream needs. Both
// *websocket.Conn and test doubles satisfy it, so a reconnect can be tested
// without RIPE or a real socket.
type WSConn interface {
	io.Reader
	io.Writer
	io.Closer
}

// RISLiveWS is a real-time RIS Live client. It is a stream, not a Fetcher: it
// opens one connection, subscribes to the operator's ASNs, and batches the BGP
// updates RIS pushes into the same external_event table the polling adapter
// uses. A lost connection is re-established with bounded exponential backoff.
//
// This is an addition to, not a replacement for, the RIPEstat polling adapter:
// the poller still works when a WebSocket cannot traverse a network, and the
// stream is opt-in because it holds a persistent connection to a third party.
type RISLiveWS struct {
	URL    string
	ASNs   []int
	Sink   Sink
	Dialer *netx.Dialer
	Now    func() time.Time

	// Dial is an optional seam for tests. When nil, the stream dials through
	// netx so this path obeys the same egress policy as every other probe.
	Dial func(ctx context.Context, rawURL string) (WSConn, error)

	// FlushEvery batches pushed events before a single store write.
	FlushEvery time.Duration
	// ReconnectBase and ReconnectMax bound the backoff after a disconnect.
	ReconnectBase time.Duration
	ReconnectMax  time.Duration
	// MaxBatch caps one write, so a busy firehose cannot grow memory without
	// bound while the store is slow.
	MaxBatch int
}

// NewRISLiveWS returns a stream with production defaults. An empty ASN list
// disables it: subscribing to every ASN on the internet is not a configured
// source.
func NewRISLiveWS(sink Sink, asns []int) *RISLiveWS {
	cp := make([]int, len(asns))
	copy(cp, asns)
	return &RISLiveWS{
		URL:    DefaultRISLiveWSURL,
		ASNs:   cp,
		Sink:   sink,
		Dialer: netx.New(),
		Now:    time.Now,
	}
}

func (r *RISLiveWS) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func (r *RISLiveWS) flushEvery() time.Duration {
	if r.FlushEvery <= 0 {
		return defaultWSFlushEvery
	}
	return r.FlushEvery
}

func (r *RISLiveWS) reconnectBase() time.Duration {
	if r.ReconnectBase <= 0 {
		return defaultWSDialBackoff
	}
	return r.ReconnectBase
}

func (r *RISLiveWS) reconnectMax() time.Duration {
	if r.ReconnectMax <= 0 {
		return defaultWSMaxBackoff
	}
	return r.ReconnectMax
}

func (r *RISLiveWS) maxBatch() int {
	if r.MaxBatch <= 0 {
		return defaultWSBatchSize
	}
	return r.MaxBatch
}

// StreamLoop keeps one RIS Live connection alive until ctx is done. A disconnect
// is recorded as feed failure and retried with exponential backoff; a long-lived
// connection resets the backoff because it was healthy.
func StreamLoop(ctx context.Context, s *RISLiveWS, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	delay := s.reconnectBase()
	for {
		if ctx.Err() != nil {
			return
		}
		connectedFor, err := s.runOnce(ctx, logger)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = fmt.Errorf("ris-live-ws connection ended")
		}
		detail := err.Error()
		if len(detail) > 200 {
			detail = detail[:200]
		}
		if recErr := s.Sink.RecordFeedFailure(ctx, RISLiveWSFeed, detail, s.now()); recErr != nil {
			logger.Warn("ris live stream failure not recorded",
				"event", "feed_failure_unrecorded", "component", "feeds",
				"feed", RISLiveWSFeed, "error", recErr)
		}
		logger.Warn("ris live stream disconnected; reconnecting",
			"event", "feed_disconnected", "component", "feeds",
			"feed", RISLiveWSFeed, "connected_for", connectedFor.String(),
			"backoff", delay.String(), "error", err)

		if connectedFor > s.reconnectBase() {
			delay = s.reconnectBase()
		} else {
			delay *= 2
			if delay > s.reconnectMax() {
				delay = s.reconnectMax()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// runOnce owns one connection lifetime. connectedFor lets the caller distinguish
// a flaky dial from a stream that ran for a while and then dropped.
func (r *RISLiveWS) runOnce(ctx context.Context, logger *slog.Logger) (time.Duration, error) {
	start := r.now()
	if len(r.ASNs) == 0 {
		return 0, fmt.Errorf("feeds: ris-live-ws needs at least one ASN")
	}

	conn, err := r.open(ctx)
	if err != nil {
		return r.now().Sub(start), err
	}
	defer func() { _ = conn.Close() }()

	for _, asn := range r.ASNs {
		sub, err := json.Marshal(map[string]any{
			"type": "ris_subscribe",
			"data": map[string]any{
				"type": "UPDATE",
				"path": asn,
				"socketOptions": map[string]any{
					"acknowledge": true,
				},
			},
		})
		if err != nil {
			return r.now().Sub(start), fmt.Errorf("feeds: ris-live-ws subscribe: %w", err)
		}
		if _, err := conn.Write(sub); err != nil {
			return r.now().Sub(start), fmt.Errorf("feeds: ris-live-ws subscribe write: %w", err)
		}
	}

	// A reader goroutine keeps the store flush on the ticker even during a quiet
	// stream. Closing the connection below unblocks a Read stuck on the wire.
	frames := make(chan []byte, 8)
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, defaultWSReadBuffer)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				readErr <- err
				return
			}
			frame := append([]byte(nil), buf[:n]...)
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()

	batch := make([]store.ExternalEvent, 0, r.maxBatch())
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := r.Sink.UpsertExternalEvents(ctx, batch); err != nil {
			return err
		}
		if err := r.Sink.RecordFeedSuccess(ctx, RISLiveWSFeed, r.now()); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}

	ticker := time.NewTicker(r.flushEvery())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = flush()
			return r.now().Sub(start), ctx.Err()
		case err := <-readErr:
			_ = flush()
			return r.now().Sub(start), fmt.Errorf("feeds: ris-live-ws read: %w", err)
		case <-ticker.C:
			if err := flush(); err != nil {
				return r.now().Sub(start), err
			}
		case frame := <-frames:
			// RIS can send an application-level ping; answer it so the server
			// does not drop an otherwise quiet subscription.
			var envelope struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(frame, &envelope) == nil && envelope.Type == "ping" {
				if _, err := conn.Write([]byte(`{"type":"pong","data":null}`)); err != nil {
					return r.now().Sub(start), fmt.Errorf("feeds: ris-live-ws pong: %w", err)
				}
				continue
			}
			event, ok := r.parseFrame(frame)
			if ok {
				batch = append(batch, event)
				if len(batch) >= r.maxBatch() {
					if err := flush(); err != nil {
						return r.now().Sub(start), err
					}
				}
			}
		}
	}
}

// parseFrame maps one ris_message frame into the neutral event shape. Only
// UPDATE messages are evidence: keepalives and peer-state changes are routing
// noise, not an observable outage.
func (r *RISLiveWS) parseFrame(frame []byte) (store.ExternalEvent, bool) {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil || envelope.Type != "ris_message" {
		return store.ExternalEvent{}, false
	}

	var msg struct {
		Type          string            `json:"type"`
		Host          string            `json:"host"`
		ID            string            `json:"id"`
		Timestamp     float64           `json:"timestamp"`
		Peer          string            `json:"peer"`
		PeerASN       string            `json:"peer_asn"`
		Path          []json.RawMessage `json:"path"`
		Announcements []struct {
			NextHop  string   `json:"next_hop"`
			Prefixes []string `json:"prefixes"`
		} `json:"announcements"`
		Withdrawals []string `json:"withdrawals"`
	}
	if err := json.Unmarshal(envelope.Data, &msg); err != nil {
		return store.ExternalEvent{}, false
	}
	if !strings.EqualFold(msg.Type, "UPDATE") || msg.ID == "" {
		return store.ExternalEvent{}, false
	}

	kind := "bgp_update"
	severity := 0.0
	if len(msg.Withdrawals) > 0 {
		kind = "bgp_withdrawal"
		severity = 1.0
	}

	started := r.now()
	if msg.Timestamp > 0 {
		started = time.Unix(0, int64(msg.Timestamp*float64(time.Second))).UTC()
	}
	prefixes := make([]string, 0, len(msg.Withdrawals))
	prefixes = append(prefixes, msg.Withdrawals...)
	for _, announcement := range msg.Announcements {
		prefixes = append(prefixes, announcement.Prefixes...)
	}
	payload, _ := json.Marshal(map[string]any{
		"host": msg.Host, "id": msg.ID, "peer": msg.Peer,
		"peer_asn": msg.PeerASN, "path": msg.Path,
		"prefixes": prefixes, "withdrawals": msg.Withdrawals,
	})

	return store.ExternalEvent{
		Feed:       RISLiveWSFeed,
		ExternalID: fmt.Sprintf("risws:%s:%s", msg.Host, msg.ID),
		Kind:       kind, Severity: severity,
		ASN:       asnForMessage(msg.Path, r.ASNs, msg.PeerASN),
		StartedAt: started, ObservedAt: r.now(),
		Payload: capPayload(string(payload), 8<<10),
	}, true
}

// asnForMessage prefers the configured ASN that actually appears in the AS
// path, then the peering ASN, and finally zero when neither can be attributed.
// We subscribe per ASN, so a configured match is the normal case.
func asnForMessage(path []json.RawMessage, configured []int, peerASN string) int {
	seen := make(map[int]bool, len(path))
	for _, raw := range path {
		var asn int
		if err := json.Unmarshal(raw, &asn); err == nil {
			seen[asn] = true
			continue
		}
		var set []int
		if err := json.Unmarshal(raw, &set); err == nil {
			for _, member := range set {
				seen[member] = true
			}
		}
	}
	for _, asn := range configured {
		if seen[asn] {
			return asn
		}
	}
	if n, err := strconv.Atoi(strings.TrimSpace(peerASN)); err == nil && n > 0 {
		return n
	}
	return 0
}

// open dials through netx (or the injected seam), upgrades the connection, and
// returns the WebSocket. The raw TCP dial is done by netx so special-use and
// private addresses stay blocked, as they are for every other probe.
func (r *RISLiveWS) open(ctx context.Context) (WSConn, error) {
	rawURL := r.URL
	if strings.TrimSpace(rawURL) == "" {
		rawURL = DefaultRISLiveWSURL
	}
	if r.Dial != nil {
		return r.Dial(ctx, rawURL)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("feeds: ris-live-ws url: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("feeds: ris-live-ws url %q has no host", rawURL)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("feeds: ris-live-ws url %q must be ws(s)", rawURL)
	}

	dialer := r.Dialer
	if dialer == nil {
		dialer = netx.New()
	}
	netConn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("feeds: ris-live-ws dial: %w", err)
	}

	var rwc io.ReadWriteCloser = netConn
	if u.Scheme == "wss" {
		tlsConn := tls.Client(netConn, &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = netConn.Close()
			return nil, fmt.Errorf("feeds: ris-live-ws tls: %w", err)
		}
		rwc = tlsConn
	}

	config := &websocket.Config{
		Location: u,
		Origin:   u,
		Version:  13,
	}
	ws, err := websocket.NewClient(config, rwc)
	if err != nil {
		_ = rwc.Close()
		return nil, fmt.Errorf("feeds: ris-live-ws handshake: %w", err)
	}
	return ws, nil
}
