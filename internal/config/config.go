// Package config loads and validates the target file. Validation is strict on
// purpose: a target list is untrusted input that decides what the process
// connects to, so a typo becomes a startup error rather than a probe.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/classify"
	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/schedule"
	"gopkg.in/yaml.v3"
)

const (
	// CurrentVersion is the only accepted schema version.
	CurrentVersion = 1
	// MaxTargets bounds the list (DESIGN.md §14).
	MaxTargets = 2000
	// MaxCorridors bounds the corridor list. Ten is the documented working
	// set; fifty leaves room for an operator who wants more without letting a
	// typo turn into thousands of probes.
	MaxCorridors = 50
	// MaxFileBytes bounds a config file before it is parsed.
	MaxFileBytes = 1 << 20
)

// File is the on-disk schema.
type File struct {
	Version   int        `yaml:"version"`
	Targets   []Target   `yaml:"targets"`
	Corridors []Corridor `yaml:"corridors"`
}

// Corridor is one operator-defined path pair (DESIGN.md §9). Endpoints are
// host:port strings; the corridor samples them with TCP, which works in a
// container without raw sockets.
type Corridor struct {
	ID        string   `yaml:"id"`
	From      string   `yaml:"from"`
	To        string   `yaml:"to"`
	Endpoints []string `yaml:"endpoints"`
}

// Target is one configured probe.
type Target struct {
	ID    string `yaml:"id"`
	Name  string `yaml:"name"`
	Host  string `yaml:"host"`
	Port  int    `yaml:"port"`
	Probe string `yaml:"probe"`
	QName string `yaml:"qname"`
	// Resolvers is the optional set of DNS servers a dns target asks. Empty
	// means "just the target's own address". Each entry is host:port on 53.
	Resolvers []string `yaml:"resolvers"`
	// StatusURL is the operator's status page for this target, in the same
	// shape the Statuspage feed accepts. Empty means "no provider mapping",
	// which the drawer states rather than guessing.
	StatusURL  string `yaml:"status_url"`
	Class      string `yaml:"class"`
	Sector     string `yaml:"sector"`
	Country    string `yaml:"country"`
	Tier       int    `yaml:"tier"`
	IntervalS  int    `yaml:"interval_s"`
	ICMPAnchor bool   `yaml:"icmp_anchor"`
	Expect     Expect `yaml:"expect"`
}

// Expect is the semantic contract, mirroring classify.Expected.
type Expect struct {
	Status []int `yaml:"status"`
	// StatusMin and StatusMax accept a range without listing every code. The
	// directory loader uses 200-399 for a sector list, where "any success or
	// redirect" is the honest default.
	StatusMin int `yaml:"status_min"`
	StatusMax int `yaml:"status_max"`
	// MaxRedirects is this target's redirect budget. The default (5) comes
	// from the instrument; a target that must not wander sets it lower.
	MaxRedirects    int    `yaml:"max_redirects"`
	BodyContains    string `yaml:"body_contains"`
	BodyNotContains string `yaml:"body_not_contains"`
	BodyRegex       string `yaml:"body_regex"`
	TitleContains   string `yaml:"title_contains"`
	HeaderPresent   string `yaml:"header_present"`
	HeaderName      string `yaml:"header_name"`
	HeaderValue     string `yaml:"header_value"`
	MinBodyBytes    int    `yaml:"min_body_bytes"`
	JSONPath        string `yaml:"json_path"`
	JSONValue       string `yaml:"json_value"`
}

// Interval returns the effective interval, falling back to the tier default.
func (t Target) Interval() time.Duration {
	if t.IntervalS > 0 {
		return time.Duration(t.IntervalS) * time.Second
	}
	return schedule.DefaultInterval(schedule.Tier(t.Tier))
}

// Classify converts the YAML expectation into the classifier's shape.
func (e Expect) Classify() (classify.Expected, error) {
	out := classify.Expected{
		Status:          e.Status,
		StatusMin:       e.StatusMin,
		StatusMax:       e.StatusMax,
		MaxRedirects:    e.MaxRedirects,
		BodyContains:    e.BodyContains,
		BodyNotContains: e.BodyNotContains,
		TitleContains:   e.TitleContains,
		HeaderPresent:   e.HeaderPresent,
		HeaderName:      e.HeaderName,
		HeaderValue:     e.HeaderValue,
		MinBodyBytes:    e.MinBodyBytes,
		JSONPath:        e.JSONPath,
		JSONValue:       e.JSONValue,
	}
	if e.BodyRegex != "" {
		re, err := regexp.Compile(e.BodyRegex)
		if err != nil {
			return out, fmt.Errorf("body_regex: %w", err)
		}
		out.BodyRegex = re
	}
	// Only a target with no content assertion can be probed for headers alone,
	// and only when its status contract admits the 206 that a ranged request
	// earns (DESIGN.md §5.2).
	out.PartialOK = !out.HasSemantic() && out.AllowsPartial()
	return out, nil
}

// Load reads a target file, or a documented config directory (DESIGN.md §13.2).
func Load(path string) (*File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if info.IsDir() {
		return loadDir(path)
	}
	if info.Size() > MaxFileBytes {
		return nil, fmt.Errorf("config: %s is %d bytes; limit is %d", path, info.Size(), MaxFileBytes)
	}

	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied config path.
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(raw)
}

// Parse validates a config from raw bytes. It exists so the parser can be
// fuzzed without the filesystem, and so callers that already hold the bytes do
// not have to write them out first.
func Parse(raw []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	var f File
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config: file is empty")
		}
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	// Reject a second YAML document rather than silently ignoring it.
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("config: multiple YAML documents are not supported")
	}

	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

var (
	hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	idCleanRE   = regexp.MustCompile(`[^a-z0-9]+`)
	// corridorIDRE is stricter than a derived target id: a corridor id is a
	// stable operator-chosen key, so it is never silently rewritten.
	corridorIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	validProbes  = map[string]map[int]struct{}{
		"http":    {80: {}, 443: {}},
		"tcp":     {80: {}, 443: {}},
		"tcpping": {80: {}, 443: {}},
		"tls":     {443: {}},
		"dns":     {53: {}},
		"ntp":     {123: {}},
		"icmp":    {},
	}
	validClasses = map[string]struct{}{
		"service": {}, "country": {}, "network": {}, "reference": {},
	}
)

// Validate normalises and checks the whole file. It mutates derived fields
// (ids, normalised hosts and intervals), which is why it takes a pointer.
func (f *File) Validate() error {
	if f.Version != CurrentVersion {
		return fmt.Errorf("config: version %d is not supported (want %d)", f.Version, CurrentVersion)
	}
	if len(f.Targets) > MaxTargets {
		return fmt.Errorf("config: %d targets exceeds the %d limit", len(f.Targets), MaxTargets)
	}

	seenKey := make(map[string]int, len(f.Targets))
	seenID := make(map[string]int, len(f.Targets))

	for i := range f.Targets {
		t := &f.Targets[i]
		if err := validateTarget(t); err != nil {
			return fmt.Errorf("config: target %d (%s): %w", i, t.Host, err)
		}

		key := fmt.Sprintf("%s|%d|%s", t.Host, t.Port, t.Probe)
		if other, dup := seenKey[key]; dup {
			return fmt.Errorf("config: target %d duplicates target %d (%s)", i, other, key)
		}
		seenKey[key] = i

		if other, dup := seenID[t.ID]; dup {
			return fmt.Errorf("config: target %d uses id %q already used by target %d", i, t.ID, other)
		}
		seenID[t.ID] = i
	}

	if len(f.Corridors) > MaxCorridors {
		return fmt.Errorf("config: %d corridors exceeds the %d limit", len(f.Corridors), MaxCorridors)
	}
	seenCorridor := make(map[string]int, len(f.Corridors))
	for i := range f.Corridors {
		c := &f.Corridors[i]
		if err := validateCorridor(c); err != nil {
			return fmt.Errorf("config: corridor %d (%s): %w", i, c.ID, err)
		}
		if other, dup := seenCorridor[c.ID]; dup {
			return fmt.Errorf("config: corridor %d uses id %q already used by corridor %d", i, c.ID, other)
		}
		seenCorridor[c.ID] = i
	}
	return nil
}

func validateTarget(t *Target) error {
	t.Host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(t.Host), "."))
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return errors.New("name is required")
	}

	ip := net.ParseIP(t.Host)
	if ip != nil {
		if t.Class != "country" && t.Class != "network" {
			return errors.New("service targets must use a hostname, not an IP literal")
		}
	} else if !validHostname(t.Host) {
		return fmt.Errorf("invalid host %q", t.Host)
	}

	if _, ok := validClasses[t.Class]; !ok {
		return fmt.Errorf("class %q is not one of service, country, network, reference", t.Class)
	}

	allowedPorts, ok := validProbes[t.Probe]
	if !ok {
		return fmt.Errorf("probe %q is not supported", t.Probe)
	}
	if t.Probe == "icmp" {
		if t.Port != 0 {
			return errors.New("icmp targets must use port 0")
		}
	} else {
		if _, ok := allowedPorts[t.Port]; !ok {
			return fmt.Errorf("port %d is not valid for probe %q", t.Port, t.Probe)
		}
		if !netx.DefaultPolicy().PortAllowed(t.Port) {
			return fmt.Errorf("port %d is not in the outbound allowlist", t.Port)
		}
	}

	if t.Tier < 0 || t.Tier > 3 {
		return fmt.Errorf("tier %d must be 0-3", t.Tier)
	}

	// NTP is self-health, not a target monitor (DESIGN.md §14): a fixed clock
	// source keep the tool from becoming a UDP/123 scanner and from a typo
	// pointing the clock check at a stranger's host.
	if t.Probe == "ntp" && !ntpHostAllowed(t.Host) {
		return fmt.Errorf("ntp host %q is not in the fixed allowlist (%s)",
			t.Host, strings.Join(ntpAllowlistNames(), ", "))
	}

	// qname is the name a DNS anchor asks its server about. It is required for
	// dns targets and meaningless for every other instrument, so a stray value
	// is a config mistake rather than something to ignore.
	t.QName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(t.QName), "."))
	if t.Probe == "dns" {
		if t.QName == "" {
			return errors.New("dns targets require qname")
		}
		if !validHostname(t.QName) {
			return fmt.Errorf("invalid qname %q", t.QName)
		}
	} else if t.QName != "" {
		return fmt.Errorf("qname is only valid for dns targets, not %q", t.Probe)
	}

	// resolvers are a DNS-only list: each is a resolver the target asks, and
	// divergence compares each one against its own history (CLASSIFIER.md 3.2).
	if len(t.Resolvers) > 0 {
		if t.Probe != "dns" {
			return fmt.Errorf("resolvers are only valid for dns targets, not %q", t.Probe)
		}
		if len(t.Resolvers) > 4 {
			return fmt.Errorf("%d resolvers exceeds the limit of 4", len(t.Resolvers))
		}
		seen := make(map[string]struct{}, len(t.Resolvers))
		for i, raw := range t.Resolvers {
			host, portText, err := net.SplitHostPort(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("resolver %d: %w", i+1, err)
			}
			host = strings.ToLower(strings.TrimSpace(host))
			if net.ParseIP(host) == nil && !validHostname(host) {
				return fmt.Errorf("resolver %d: invalid host %q", i+1, host)
			}
			if portText != "53" {
				return fmt.Errorf("resolver %d: port %q must be 53", i+1, portText)
			}
			addr := net.JoinHostPort(host, "53")
			if _, dup := seen[addr]; dup {
				return fmt.Errorf("resolver %d duplicates %q", i+1, addr)
			}
			seen[addr] = struct{}{}
			t.Resolvers[i] = addr
		}
	}

	// status_url is the operator's status page for this target. Its host is the
	// match key against stored statuspage events, so it is validated and
	// normalised here rather than parsed again in the query.
	if raw := strings.TrimSpace(t.StatusURL); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("status_url %q is not an http(s) URL", t.StatusURL)
		}
		if u.User != nil {
			return fmt.Errorf("status_url %q must not carry credentials", t.StatusURL)
		}
		t.StatusURL = u.String()
	}

	interval := t.Interval()
	if interval < schedule.Floor {
		return fmt.Errorf("interval_s %d is below the %s floor", t.IntervalS, schedule.Floor)
	}
	t.IntervalS = int(interval / time.Second)

	for _, status := range t.Expect.Status {
		if status < 100 || status > 599 {
			return fmt.Errorf("expect.status %d is not a valid HTTP status", status)
		}
	}
	if t.Expect.MaxRedirects < 0 || t.Expect.MaxRedirects > 20 {
		return fmt.Errorf("expect.max_redirects %d must be between 0 and 20", t.Expect.MaxRedirects)
	}
	if t.Expect.StatusMin != 0 || t.Expect.StatusMax != 0 {
		if t.Expect.StatusMin < 100 || t.Expect.StatusMin > 599 {
			return fmt.Errorf("expect.status_min %d is not a valid HTTP status", t.Expect.StatusMin)
		}
		if t.Expect.StatusMax < t.Expect.StatusMin || t.Expect.StatusMax > 599 {
			return fmt.Errorf("expect.status_max %d must be between status_min and 599", t.Expect.StatusMax)
		}
	}
	if t.Expect.HeaderValue != "" && t.Expect.HeaderName == "" {
		return errors.New("expect.header_value requires expect.header_name")
	}
	if t.Expect.BodyRegex != "" {
		if _, err := regexp.Compile(t.Expect.BodyRegex); err != nil {
			return fmt.Errorf("expect.body_regex: %w", err)
		}
	}

	if t.ID == "" {
		t.ID = deriveID(t.Host, t.Probe)
	}
	if t.ID == "" {
		return errors.New("could not derive an id")
	}
	return nil
}

// validateCorridor normalises and checks one corridor definition. Endpoints are
// normalised to lowercase host:port so the same endpoint cannot be listed twice
// in different spellings.
func validateCorridor(c *Corridor) error {
	c.ID = strings.ToLower(strings.TrimSpace(c.ID))
	c.From = strings.TrimSpace(c.From)
	c.To = strings.TrimSpace(c.To)

	if c.ID == "" {
		return errors.New("id is required")
	}
	if !corridorIDRE.MatchString(c.ID) {
		return fmt.Errorf("id %q must be lower-case letters, digits, or dashes", c.ID)
	}
	if c.From == "" || c.To == "" {
		return errors.New("from and to are required")
	}
	if len(c.Endpoints) != 2 {
		return fmt.Errorf("needs exactly two endpoints, got %d", len(c.Endpoints))
	}

	seen := make(map[string]struct{}, len(c.Endpoints))
	for i, raw := range c.Endpoints {
		host, portText, err := net.SplitHostPort(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("endpoint %d: %w", i+1, err)
		}
		host = strings.ToLower(strings.TrimSpace(host))
		if net.ParseIP(host) == nil && !validHostname(host) {
			return fmt.Errorf("endpoint %d: invalid host %q", i+1, host)
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return fmt.Errorf("endpoint %d: port %q is not a number", i+1, portText)
		}
		if port != 80 && port != 443 {
			return fmt.Errorf("endpoint %d: port %d must be 80 or 443", i+1, port)
		}
		if !netx.DefaultPolicy().PortAllowed(port) {
			return fmt.Errorf("endpoint %d: port %d is not in the outbound allowlist", i+1, port)
		}

		addr := net.JoinHostPort(host, strconv.Itoa(port))
		if _, dup := seen[addr]; dup {
			return fmt.Errorf("endpoint %d duplicates %q", i+1, addr)
		}
		seen[addr] = struct{}{}
		c.Endpoints[i] = addr
	}
	return nil
}

func validHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !hostLabelRE.MatchString(label) {
			return false
		}
	}
	return true
}

// ntpHosts is the fixed clock-source allowlist from DESIGN.md §14. NTP answers
// "is my clock right?", so the destination is a constant, not a monitored
// target: an arbitrary host would turn the monitor into a UDP/123 scanner.
var ntpHosts = []string{
	"pool.ntp.org",
	"time.cloudflare.com",
	"time.google.com",
	"time.apple.com",
	"time.windows.com",
}

// ntpHostAllowed accepts a listed clock source or a numbered pool member such
// as 0.pool.ntp.org. The numbered form is what the pool documents and what an
// operator is likely to paste, so it is accepted rather than second-guessed.
func ntpHostAllowed(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, allowed := range ntpHosts {
		if h == allowed {
			return true
		}
	}
	if strings.HasSuffix(h, ".pool.ntp.org") {
		return validHostname(h)
	}
	return false
}

func ntpAllowlistNames() []string {
	return ntpHosts
}

func deriveID(host, probe string) string {
	return strings.Trim(idCleanRE.ReplaceAllString(strings.ToLower(host), "-"), "-") + "-" + probe
}
