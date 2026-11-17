package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/config"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "targets.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValidFile(t *testing.T) {
	t.Parallel()

	path := write(t, `version: 1
targets:
  - name: Chase
    host: WWW.Chase.com
    port: 443
    probe: http
    class: service
    sector: financial/banking
    tier: 2
    expect:
      status: [200]
      body_contains: "chase"
  - name: root server
    host: 198.41.0.4
    port: 53
    probe: dns
    qname: example.com
    class: network
    tier: 3
    interval_s: 120
`)

	f, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Targets) != 2 {
		t.Fatalf("targets = %d", len(f.Targets))
	}

	first := f.Targets[0]
	if first.Host != "www.chase.com" {
		t.Errorf("host not normalised: %q", first.Host)
	}
	if first.ID != "www-chase-com-http" {
		t.Errorf("derived id = %q", first.ID)
	}
	if first.IntervalS != 300 {
		t.Errorf("interval = %d, want the tier default 300", first.IntervalS)
	}

	exp, err := first.Expect.Classify()
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Status) != 1 || exp.Status[0] != 200 || exp.BodyContains != "chase" {
		t.Fatalf("expectation = %+v", exp)
	}

	// Anchors may be IP literals.
	if f.Targets[1].Host != "198.41.0.4" || f.Targets[1].IntervalS != 120 {
		t.Fatalf("anchor = %+v", f.Targets[1])
	}
}

func TestLoadRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{"wrong version", "version: 2\ntargets: []\n", "not supported"},
		{"unknown field", "version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 443\n    probe: http\n    class: service\n    bogus: 1\n", "field bogus"},
		{"bad yaml", "version: 1\ntargets: [\n", "parse"},
		{"two documents", "version: 1\ntargets: []\n---\nversion: 1\n", "multiple YAML documents"},
		{"missing name", "version: 1\ntargets:\n  - host: x.com\n    port: 443\n    probe: http\n    class: service\n", "name is required"},
		{"ip service", "version: 1\ntargets:\n  - name: x\n    host: 1.1.1.1\n    port: 443\n    probe: http\n    class: service\n", "hostname"},
		{"bad host", "version: 1\ntargets:\n  - name: x\n    host: http://x.com/path\n    port: 443\n    probe: http\n    class: service\n", "invalid host"},
		{"port for probe", "version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 53\n    probe: http\n    class: service\n", "not valid for probe"},
		{"blocked port", "version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 22\n    probe: tcp\n    class: service\n", "not valid for probe"},
		{"unknown probe", "version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 443\n    probe: ftp\n    class: service\n", "not supported"},
		{"unknown class", "version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 443\n    probe: http\n    class: ads\n", "class"},
		{"bad tier", "version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 443\n    probe: http\n    class: service\n    tier: 9\n", "tier"},
		{"short interval", "version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 443\n    probe: http\n    class: service\n    interval_s: 10\n", "floor"},
		{"bad status", "version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 443\n    probe: http\n    class: service\n    expect:\n      status: [99]\n", "HTTP status"},
		{"header value without name", "version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 443\n    probe: http\n    class: service\n    expect:\n      header_value: green\n", "header_name"},
		{"bad regex", "version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 443\n    probe: http\n    class: service\n    expect:\n      body_regex: \"(\"\n", "body_regex"},
		{"icmp with a port", "version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 443\n    probe: icmp\n    class: service\n", "port 0"},
		{"dns without qname", "version: 1\ntargets:\n  - name: anchor\n    host: 198.41.0.4\n    port: 53\n    probe: dns\n    class: network\n    tier: 3\n", "qname"},
		{"dns with a bad qname", "version: 1\ntargets:\n  - name: anchor\n    host: 198.41.0.4\n    port: 53\n    probe: dns\n    qname: \"not a name\"\n    class: network\n    tier: 3\n", "qname"},
		{"qname on a non-dns target", "version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 443\n    probe: http\n    qname: example.com\n    class: service\n", "only valid for dns"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(write(t, tc.body))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestDuplicatesAreRejected(t *testing.T) {
	t.Parallel()

	path := write(t, `version: 1
targets:
  - name: a
    host: x.com
    port: 443
    probe: http
    class: service
  - name: b
    host: X.com
    port: 443
    probe: http
    class: service
`)
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("error = %v, want a duplicate rejection", err)
	}
}

func TestIDCollisionIsRejected(t *testing.T) {
	t.Parallel()

	path := write(t, `version: 1
targets:
  - id: same
    name: a
    host: a.com
    port: 443
    probe: http
    class: service
  - id: same
    name: b
    host: b.com
    port: 443
    probe: http
    class: service
`)
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("error = %v", err)
	}
}

func TestTargetCap(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString("version: 1\ntargets:\n")
	for i := 0; i <= config.MaxTargets; i++ {
		fmt.Fprintf(&b, "  - name: t%d\n    host: h%d.example\n    port: 443\n    probe: http\n    class: service\n", i, i)
	}

	_, err := config.Load(write(t, b.String()))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want the cap", err)
	}
}

func TestEmptyTargetListIsValid(t *testing.T) {
	t.Parallel()

	f, err := config.Load(write(t, "version: 1\ntargets: []\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Targets) != 0 {
		t.Fatalf("targets = %d", len(f.Targets))
	}
}

func TestFileSizeCap(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "big.yaml")
	big := strings.Repeat("# padding\n", config.MaxFileBytes/8)
	if err := os.WriteFile(path, []byte("version: 1\ntargets: []\n"+big), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error = %v, want the size limit", err)
	}
}

func TestMissingFile(t *testing.T) {
	t.Parallel()

	if _, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("missing file should be an error")
	}
}

func TestLoadAcceptsADNSAnchor(t *testing.T) {
	t.Parallel()

	path := write(t, `version: 1
targets:
  - name: Root server A
    host: 198.41.0.4
    port: 53
    probe: dns
    qname: Example.COM.
    class: network
    tier: 3
    interval_s: 120
`)

	f, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := f.Targets[0]
	if got.Host != "198.41.0.4" || got.Port != 53 || got.Probe != "dns" {
		t.Fatalf("target = %+v", got)
	}
	if got.QName != "example.com" {
		t.Fatalf("qname = %q, want the normalised name", got.QName)
	}
	if got.IntervalS != 120 {
		t.Fatalf("interval = %d", got.IntervalS)
	}
}

func TestLoadAcceptsTCPPing(t *testing.T) {
	t.Parallel()

	path := write(t, `version: 1
targets:
  - name: Portable RTT
    host: example.com
    port: 443
    probe: tcpping
    class: network
    tier: 3
    interval_s: 120
`)

	f, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := f.Targets[0]
	if got.Probe != "tcpping" || got.Port != 443 {
		t.Fatalf("target = %+v", got)
	}
	// tcpping is only meaningful on the probe ports in the allowlist.
	if _, err := config.Load(write(t, `version: 1
targets:
  - name: bad
    host: example.com
    port: 22
    probe: tcpping
    class: network
    tier: 3
`)); err == nil {
		t.Fatal("tcpping on port 22 should be rejected")
	}
}

func TestIntervalFloorAndDefault(t *testing.T) {
	t.Parallel()

	f, err := config.Load(write(t, `version: 1
targets:
  - name: a
    host: a.com
    port: 443
    probe: http
    class: service
    tier: 0
  - name: b
    host: b.com
    port: 443
    probe: http
    class: service
    tier: 2
    interval_s: 600
`))
	if err != nil {
		t.Fatal(err)
	}

	if got := f.Targets[0].Interval(); got != 30*time.Second {
		t.Fatalf("tier 0 interval = %v", got)
	}
	if got := f.Targets[1].Interval(); got != 10*time.Minute {
		t.Fatalf("explicit interval = %v", got)
	}
}

func TestClassifyCompilesRegex(t *testing.T) {
	t.Parallel()

	exp, err := (config.Expect{BodyRegex: `v\d+`}).Classify()
	if err != nil {
		t.Fatal(err)
	}
	if exp.BodyRegex == nil || !exp.BodyRegex.MatchString("v42") {
		t.Fatal("regex was not compiled")
	}
}

func TestLoadAcceptsCorridors(t *testing.T) {
	t.Parallel()

	f, err := config.Load(write(t, `version: 1
targets:
  - name: A
    host: a.example
    port: 443
    probe: tls
    class: service
corridors:
  - id: nyc-lon
    from: NYC
    to: LON
    endpoints: ["Anchor.One.Example:443", "anchor.two.example:80"]
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := f.Corridors[0]
	if got.ID != "nyc-lon" || got.From != "NYC" || got.To != "LON" {
		t.Fatalf("corridor = %+v", got)
	}
	if len(got.Endpoints) != 2 {
		t.Fatalf("endpoints = %+v", got.Endpoints)
	}
	// Hosts are normalised, so the same endpoint cannot be spelled twice.
	if got.Endpoints[0] != "anchor.one.example:443" || got.Endpoints[1] != "anchor.two.example:80" {
		t.Fatalf("endpoints = %+v", got.Endpoints)
	}
}

func TestCorridorValidation(t *testing.T) {
	t.Parallel()

	base := `version: 1
targets:
  - name: A
    host: a.example
    port: 443
    probe: tls
    class: service
corridors:
`
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing id", base + "  - from: A\n    to: B\n    endpoints: [\"a.example:443\", \"b.example:443\"]\n", "id is required"},
		{"bad id", base + "  - id: Bad_ID\n    from: A\n    to: B\n    endpoints: [\"a.example:443\", \"b.example:443\"]\n", "lower-case"},
		{"missing from", base + "  - id: x\n    to: B\n    endpoints: [\"a.example:443\", \"b.example:443\"]\n", "from and to"},
		{"one endpoint", base + "  - id: x\n    from: A\n    to: B\n    endpoints: [\"a.example:443\"]\n", "exactly two"},
		{"bad port", base + "  - id: x\n    from: A\n    to: B\n    endpoints: [\"a.example:22\", \"b.example:443\"]\n", "must be 80 or 443"},
		{"not a port", base + "  - id: x\n    from: A\n    to: B\n    endpoints: [\"a.example:https\", \"b.example:443\"]\n", "not a number"},
		{"duplicate endpoint", base + "  - id: x\n    from: A\n    to: B\n    endpoints: [\"a.example:443\", \"A.Example:443\"]\n", "duplicates"},
		{"duplicate id", base + "  - id: x\n    from: A\n    to: B\n    endpoints: [\"a.example:443\", \"b.example:443\"]\n  - id: x\n    from: C\n    to: D\n    endpoints: [\"c.example:443\", \"d.example:443\"]\n", "already used"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(write(t, tc.body))
			if err == nil {
				t.Fatalf("Load accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadAcceptsDNSResolvers(t *testing.T) {
	t.Parallel()

	f, err := config.Load(write(t, `version: 1
targets:
  - name: Resolver pair
    host: 1.1.1.1
    port: 53
    probe: dns
    qname: example.com
    class: network
    resolvers: ["9.9.9.9:53", "DNS.Google:53"]
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := f.Targets[0].Resolvers
	if len(got) != 2 {
		t.Fatalf("resolvers = %+v", got)
	}
	// Hosts are normalised, so the same resolver cannot be spelled twice.
	if got[0] != "9.9.9.9:53" || got[1] != "dns.google:53" {
		t.Fatalf("resolvers = %+v", got)
	}
}

func TestResolverValidation(t *testing.T) {
	t.Parallel()

	base := `version: 1
targets:
  - name: Resolver pair
    host: 1.1.1.1
    port: 53
    probe: dns
    qname: example.com
    class: network
`
	cases := []struct {
		name string
		body string
		want string
	}{
		{"wrong port", base + "    resolvers: [\"9.9.9.9:5353\"]\n", "must be 53"},
		{"bad host", base + "    resolvers: [\"not a host:53\"]\n", "invalid host"},
		{"no port", base + "    resolvers: [\"9.9.9.9\"]\n", "missing port"},
		{"duplicate", base + "    resolvers: [\"9.9.9.9:53\", \"9.9.9.9:53\"]\n", "duplicates"},
		{"too many", base + "    resolvers: [\"1.0.0.1:53\",\"8.8.8.8:53\",\"8.8.4.4:53\",\"208.67.222.222:53\",\"208.67.220.220:53\"]\n", "exceeds the limit"},
		{
			"non-dns probe",
			`version: 1
targets:
  - name: Web
    host: a.example
    port: 443
    probe: tls
    class: service
    resolvers: ["9.9.9.9:53"]
`,
			"only valid for dns",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(write(t, tc.body))
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestStatusURLAcceptedAndNormalised(t *testing.T) {
	t.Parallel()

	f, err := config.Load(write(t, `version: 1
targets:
  - name: Chase
    host: chase.com
    port: 443
    probe: tls
    class: service
    status_url: "https://status.chase.com/"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := f.Targets[0].StatusURL; got != "https://status.chase.com/" {
		t.Fatalf("status_url = %q", got)
	}
}

func TestStatusURLValidation(t *testing.T) {
	t.Parallel()

	base := `version: 1
targets:
  - name: Chase
    host: chase.com
    port: 443
    probe: tls
    class: service
`
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"not a url", "status.chase.com", "not an http"},
		{"wrong scheme", "ftp://status.chase.com", "not an http"},
		{"no host", "https://", "not an http"},
		{"credentials", "https://user:pass@status.chase.com", "credentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(write(t, base+"    status_url: \""+tc.url+"\"\n"))
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestNTPTargetsAreLimitedToTheClockAllowlist(t *testing.T) {
	t.Parallel()

	for _, host := range []string{
		"time.cloudflare.com",
		"pool.ntp.org",
		"0.pool.ntp.org",
		"time.google.com",
		"TIME.APPLE.COM.",
	} {
		path := write(t, `version: 1
targets:
  - name: Clock
    host: `+host+`
    port: 123
    probe: ntp
    class: reference
    tier: 0
`)
		if _, err := config.Load(path); err != nil {
			t.Errorf("%q was rejected: %v", host, err)
		}
	}

	// NTP is self-health, not a target monitor: an arbitrary UDP/123 host
	// would make the tool a clock-port scanner (DESIGN.md §14).
	for _, host := range []string{
		"example.com",
		"ntp.attacker.example",
		"pool.example.org",
	} {
		path := write(t, `version: 1
targets:
  - name: Clock
    host: `+host+`
    port: 123
    probe: ntp
    class: reference
    tier: 0
`)
		_, err := config.Load(path)
		if err == nil {
			t.Errorf("%q was accepted; ntp is allowlisted", host)
			continue
		}
		if !strings.Contains(err.Error(), "fixed allowlist") {
			t.Errorf("%q error = %v, want the allowlist named", host, err)
		}
	}
}
