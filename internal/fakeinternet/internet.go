package fakeinternet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Scenario is one fake host's behaviour, mirroring testdata/internet.
type Scenario struct {
	Host     string     `yaml:"host"`
	TLS      string     `yaml:"tls"`
	A        []string   `yaml:"a"`
	AAAAOnly bool       `yaml:"aaaa_only"`
	Delay    string     `yaml:"delay"`
	HTTP     []Response `yaml:"http"`
}

// Response is one HTTP answer for a path. An empty Path is the default for
// every path the scenario does not name explicitly.
type Response struct {
	Path     string            `yaml:"path"`
	Status   int               `yaml:"status"`
	Headers  map[string]string `yaml:"headers"`
	Body     string            `yaml:"body"`
	Location string            `yaml:"location"`
}

// DelayDuration parses the scenario's delay, defaulting to zero.
func (s Scenario) DelayDuration() time.Duration {
	if strings.TrimSpace(s.Delay) == "" {
		return 0
	}
	d, err := time.ParseDuration(s.Delay)
	if err != nil {
		return 0
	}
	return d
}

// responseFor returns the response that applies to path.
func (s Scenario) responseFor(path string) Response {
	for _, r := range s.HTTP {
		if r.Path == path && r.Path != "" {
			return r
		}
	}
	for _, r := range s.HTTP {
		if r.Path == "" {
			return r
		}
	}
	if len(s.HTTP) > 0 {
		return s.HTTP[0]
	}
	return Response{Status: http.StatusOK}
}

// Internet is one running fake internet. Close it when the test ends.
type Internet struct {
	// Roots trusts only the test CA, so a real verification failure cannot be
	// mistaken for a fake one.
	Roots *x509.CertPool

	byHost  map[string]Scenario
	certs   map[string]tls.Certificate
	dnsConn net.PacketConn
	httpsLn net.Listener
	socksLn net.Listener

	clockLn  net.Listener
	egressLn net.Listener
	servers  []*http.Server

	mu       sync.Mutex
	clock    time.Time
	egressIP string

	closeOnce sync.Once
}

// LoadScenarios reads and validates a scenarios file.
func LoadScenarios(path string) ([]Scenario, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- test fixture path.
	if err != nil {
		return nil, fmt.Errorf("fakeinternet: %w", err)
	}
	var scenarios []Scenario
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&scenarios); err != nil {
		return nil, fmt.Errorf("fakeinternet: parse %s: %w", path, err)
	}
	if len(scenarios) == 0 {
		return nil, fmt.Errorf("fakeinternet: %s has no scenarios", path)
	}

	seen := make(map[string]struct{}, len(scenarios))
	for idx, s := range scenarios {
		if strings.TrimSpace(s.Host) == "" {
			return nil, fmt.Errorf("fakeinternet: scenario %d has no host", idx)
		}
		if _, dup := seen[s.Host]; dup {
			return nil, fmt.Errorf("fakeinternet: duplicate host %q", s.Host)
		}
		seen[s.Host] = struct{}{}
		switch strings.ToLower(strings.TrimSpace(s.TLS)) {
		case "", "valid", "expired", "mismatch", "untrusted":
		default:
			return nil, fmt.Errorf("fakeinternet: %s has unknown tls %q", s.Host, s.TLS)
		}
		if strings.TrimSpace(s.Delay) != "" {
			if _, err := time.ParseDuration(s.Delay); err != nil {
				return nil, fmt.Errorf("fakeinternet: %s delay: %w", s.Host, err)
			}
		}
		for _, a := range s.A {
			if net.ParseIP(a) == nil {
				return nil, fmt.Errorf("fakeinternet: %s has invalid address %q", s.Host, a)
			}
		}
	}
	return scenarios, nil
}

// Start builds one CA, one certificate per host, and the listeners. Every
// failure closes whatever was already started, so a caller only has to worry
// about one error.
func Start(scenarios []Scenario) (*Internet, error) {
	now := time.Now().UTC()
	ca, err := newAuthority(now)
	if err != nil {
		return nil, err
	}

	i := &Internet{
		Roots:    ca.rootPool(),
		byHost:   make(map[string]Scenario, len(scenarios)),
		certs:    make(map[string]tls.Certificate, len(scenarios)),
		clock:    now,
		egressIP: "203.0.113.9",
	}

	for _, s := range scenarios {
		i.byHost[s.Host] = s
		cert, err := ca.leaf(s.Host, tlsMode(s.TLS), now)
		if err != nil {
			return nil, err
		}
		i.certs[s.Host] = cert
	}

	if err := i.listen(); err != nil {
		_ = i.Close()
		return nil, err
	}

	go i.serveDNS()
	go i.serveSOCKS()
	go i.serveHTTPS()
	return i, nil
}

func (i *Internet) listen() error {
	var lc net.ListenConfig
	dnsConn, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("fakeinternet: dns: %w", err)
	}
	i.dnsConn = dnsConn

	raw, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("fakeinternet: https: %w", err)
	}
	i.httpsLn = tls.NewListener(raw, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, ok := i.certs[hello.ServerName]
			if !ok {
				return nil, fmt.Errorf("fakeinternet: no certificate for %q", hello.ServerName)
			}
			return &cert, nil
		},
	})

	socksLn, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("fakeinternet: socks: %w", err)
	}
	i.socksLn = socksLn

	clockLn, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("fakeinternet: clock: %w", err)
	}
	i.clockLn = clockLn
	i.serveEndpoint(clockLn, i.handleClock)

	egressLn, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("fakeinternet: egress: %w", err)
	}
	i.egressLn = egressLn
	i.serveEndpoint(egressLn, i.handleEgress)
	return nil
}

// serveEndpoint starts a plain-HTTP listener for one control endpoint.
func (i *Internet) serveEndpoint(ln net.Listener, handler http.HandlerFunc) {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	i.servers = append(i.servers, srv)
	go func() { _ = srv.Serve(ln) }()
}

// DialContext is the dialer seam. Every fake hostname is 127.0.0.1; the port
// selects the HTTPS listener. A real loopback address is passed through, so a
// test can also reach an endpoint it started itself.
func (i *Internet) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("fakeinternet: port %q: %w", portText, err)
	}
	mapped, err := i.mapAddress(host, port)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	return d.DialContext(ctx, network, mapped)
}

// Transport is an HTTP transport that reaches the fake hosts and trusts only
// the test CA. It is what the real probe.HTTP instrument runs over.
func (i *Internet) Transport() *http.Transport {
	return &http.Transport{
		Proxy:             nil,
		DialContext:       i.DialContext,
		TLSClientConfig:   &tls.Config{RootCAs: i.Roots, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: false,
	}
}

// TLSConfig trusts only the test CA, for probe.TLS.
func (i *Internet) TLSConfig() *tls.Config {
	return &tls.Config{RootCAs: i.Roots, MinVersion: tls.VersionTLS12}
}

// DNSAddr is the fake resolver's UDP address.
func (i *Internet) DNSAddr() string { return i.dnsConn.LocalAddr().String() }

// HTTPSAddr is the fake HTTPS listener's address.
func (i *Internet) HTTPSAddr() string { return i.httpsLn.Addr().String() }

// SOCKSAddr is the fake SOCKS5 forwarder's address.
func (i *Internet) SOCKSAddr() string { return i.socksLn.Addr().String() }

// ClockURL is the controllable date endpoint, for selfcheck.ClockCheck.
func (i *Internet) ClockURL() string {
	return "http://" + i.clockLn.Addr().String() + "/clock"
}

// EgressURL is the controllable trace endpoint, for selfcheck.EgressIPCheck.
func (i *Internet) EgressURL() string {
	return "http://" + i.egressLn.Addr().String() + "/cdn-cgi/trace"
}

// SetClock sets the time the clock endpoint reports.
func (i *Internet) SetClock(at time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.clock = at
}

// SetEgressIP sets the address the trace endpoint reports.
func (i *Internet) SetEgressIP(addr string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.egressIP = addr
}

func (i *Internet) handleClock(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	at := i.clock
	i.mu.Unlock()
	w.Header().Set("Date", at.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("clock"))
}

func (i *Internet) handleEgress(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	addr := i.egressIP
	i.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintf(w, "ip=%s\nts=%d\n", addr, time.Now().Unix())
}

// serveHTTPS answers every fake host from one listener: SNI selects the
// certificate and the Host header selects the scenario.
func (i *Internet) serveHTTPS() {
	srv := &http.Server{
		Handler:           http.HandlerFunc(i.handleHTTPS),
		ReadHeaderTimeout: 5 * time.Second,
	}
	i.servers = append(i.servers, srv)
	_ = srv.Serve(i.httpsLn)
}

func (i *Internet) handleHTTPS(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	scenario, ok := i.byHost[host]
	if !ok {
		http.Error(w, "no such fake host", http.StatusNotFound)
		return
	}

	if delay := scenario.DelayDuration(); delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	resp := scenario.responseFor(r.URL.Path)
	for name, value := range resp.Headers {
		w.Header().Set(name, value)
	}
	if resp.Location != "" {
		w.Header().Set("Location", resp.Location)
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(resp.Body))
}

// Close stops every listener and server. It is safe to call twice.
func (i *Internet) Close() error {
	i.closeOnce.Do(func() {
		for _, srv := range i.servers {
			_ = srv.Close()
		}
		for _, c := range []interface{ Close() error }{i.dnsConn, i.httpsLn, i.socksLn, i.clockLn, i.egressLn} {
			if c != nil {
				_ = c.Close()
			}
		}
	})
	return nil
}
