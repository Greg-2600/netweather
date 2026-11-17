package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/config"
)

// writeDir lays out a config directory from a path -> body map.
func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLoadDirectory(t *testing.T) {
	t.Parallel()

	root := writeDir(t, map[string]string{
		"sites/financial/banking.txt": "# the big two\nchase.com\nbankofamerica.com\n",
		"sites/travel/airlines.txt":   "united.com\n",
		"core.txt":                    "ignored, not under sites\n",
		"overrides/chase.yaml": `target: chase.com
name: Chase
probe: tls
interval: 10m
`,
		"netweather.yaml": `version: 1
targets:
  - name: Cloudflare
    host: cloudflare.com
    port: 443
    probe: tls
    class: reference
    tier: 0
    interval_s: 30
`,
		"corridors.yaml": `version: 1
corridors:
  - id: nyc-lon
    from: NYC
    to: LON
    endpoints: ["198.51.100.1:443", "203.0.113.2:443"]
`,
	})

	f, err := config.Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	byHost := make(map[string]config.Target, len(f.Targets))
	for _, target := range f.Targets {
		byHost[target.Host] = target
	}
	if len(f.Targets) != 4 {
		t.Fatalf("targets = %d, want 4", len(f.Targets))
	}

	// The folder is the sector, with no tag to maintain.
	if got := byHost["chase.com"].Sector; got != "financial/banking" {
		t.Errorf("chase sector = %q", got)
	}
	if got := byHost["united.com"].Sector; got != "travel/airlines" {
		t.Errorf("united sector = %q", got)
	}
	// sites defaults: an "does it answer" target accepting any 2xx/3xx.
	united := byHost["united.com"]
	if united.Probe != "http" || united.Port != 443 || united.Tier != 2 || united.IntervalS != 300 {
		t.Errorf("sites defaults = %+v", united)
	}
	if united.Expect.StatusMin != 200 || united.Expect.StatusMax != 399 {
		t.Errorf("sites status range = %d-%d", united.Expect.StatusMin, united.Expect.StatusMax)
	}

	// The override changed the probe, name, and interval without changing the
	// host.
	chase := byHost["chase.com"]
	if chase.Name != "Chase" || chase.Probe != "tls" || chase.IntervalS != 600 {
		t.Errorf("chase override = %+v", chase)
	}
	if chase.ID != "chase-com-tls" {
		t.Errorf("chase id = %q", chase.ID)
	}

	// netweather.yaml contributes its own target and the corridor file lands.
	if ref := byHost["cloudflare.com"]; ref.Class != "reference" || ref.Tier != 0 {
		t.Errorf("global target = %+v", ref)
	}
	if len(f.Corridors) != 1 || f.Corridors[0].ID != "nyc-lon" {
		t.Errorf("corridors = %+v", f.Corridors)
	}
}

func TestLoadDirectoryRejects(t *testing.T) {
	t.Parallel()

	// An override with no matching target is a typo, not a new target.
	root := writeDir(t, map[string]string{
		"sites/banking.txt":    "chase.com\n",
		"overrides/other.yaml": "target: nope.example\nprobe: tls\n",
	})
	if _, err := config.Load(root); err == nil || !strings.Contains(err.Error(), "nope.example") {
		t.Fatalf("error = %v, want the unknown override target", err)
	}

	// A directory with no targets is a preflight error.
	empty := t.TempDir()
	if _, err := config.Load(empty); err == nil || !strings.Contains(err.Error(), "no targets") {
		t.Fatalf("error = %v, want a no-targets refusal", err)
	}

	// A bad line is still a startup error, and the sector file is named.
	bad := writeDir(t, map[string]string{"sites/banking.txt": "not a host\n"})
	if _, err := config.Load(bad); err == nil {
		t.Fatal("an invalid host line was accepted")
	}

	// An unknown override key is rejected rather than ignored.
	unknown := writeDir(t, map[string]string{
		"sites/banking.txt":    "chase.com\n",
		"overrides/chase.yaml": "target: chase.com\ntags: [hard]\n",
	})
	if _, err := config.Load(unknown); err == nil {
		t.Fatal("an unknown override field was accepted")
	}
}

func TestStatusRange(t *testing.T) {
	t.Parallel()

	path := write(t, `version: 1
targets:
  - name: Range
    host: example.com
    port: 443
    probe: http
    class: service
    tier: 2
    interval_s: 300
    expect:
      status_min: 200
      status_max: 399
`)
	f, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	expect, err := f.Targets[0].Expect.Classify()
	if err != nil {
		t.Fatal(err)
	}
	if !expect.StatusAllowed(204) || !expect.StatusAllowed(301) || expect.StatusAllowed(404) {
		t.Fatalf("range = %+v did not accept 2xx/3xx and reject 404", expect)
	}

	for _, body := range []string{
		"      status_min: 99\n      status_max: 399\n",
		"      status_min: 400\n      status_max: 300\n",
	} {
		bad := write(t, `version: 1
targets:
  - name: Range
    host: example.com
    port: 443
    probe: http
    class: service
    tier: 2
    interval_s: 300
    expect:
`+body)
		if _, err := config.Load(bad); err == nil {
			t.Fatalf("range %q was accepted", body)
		}
	}
}

func TestLoadDirectoryOverrideAppliesAndValidates(t *testing.T) {
	t.Parallel()

	root := writeDir(t, map[string]string{
		"sites/financial/banking.txt": "chase.com\n",
		"overrides/chase.yaml": `target: chase.com
name: Chase (edge)
sector: financial/retail
status_url: https://www.chasestatus.com/
expect:
  status: [201]
`,
	})
	f, err := config.Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := f.Targets[0]
	if got.Name != "Chase (edge)" || got.Sector != "financial/retail" {
		t.Fatalf("override = %+v", got)
	}
	if got.StatusURL != "https://www.chasestatus.com/" {
		t.Fatalf("status_url = %q", got.StatusURL)
	}
	if len(got.Expect.Status) != 1 || got.Expect.Status[0] != 201 {
		t.Fatalf("expect = %+v, want the override to replace it", got.Expect)
	}

	// A malformed interval is a startup error with the file named.
	bad := writeDir(t, map[string]string{
		"sites/banking.txt":    "chase.com\n",
		"overrides/chase.yaml": "target: chase.com\ninterval: soon\n",
	})
	if _, err := config.Load(bad); err == nil || !strings.Contains(err.Error(), "positive duration") {
		t.Fatalf("error = %v, want an interval refusal", err)
	}

	// An override with no target key names nothing and is rejected.
	empty := writeDir(t, map[string]string{
		"sites/banking.txt":    "chase.com\n",
		"overrides/chase.yaml": "probe: tls\n",
	})
	if _, err := config.Load(empty); err == nil || !strings.Contains(err.Error(), "target is required") {
		t.Fatalf("error = %v, want a target refusal", err)
	}
}

func TestLoadDirectoryWithoutSites(t *testing.T) {
	t.Parallel()

	root := writeDir(t, map[string]string{
		"netweather.yaml": `version: 1
targets:
  - name: Cloudflare
    host: cloudflare.com
    port: 443
    probe: tls
    class: reference
    tier: 0
    interval_s: 30
`,
	})
	f, err := config.Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Targets) != 1 || f.Targets[0].Host != "cloudflare.com" {
		t.Fatalf("targets = %+v", f.Targets)
	}

	// A version in netweather.yaml that this build does not know is refused
	// rather than half-read.
	mismatch := writeDir(t, map[string]string{
		"netweather.yaml": "version: 9\ntargets: []\n",
	})
	if _, err := config.Load(mismatch); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error = %v, want a version refusal", err)
	}
}

func TestLoadDirectoryRejectsAnOversizedFile(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", config.MaxFileBytes+1)
	root := writeDir(t, map[string]string{
		"sites/banking.txt": big,
	})
	if _, err := config.Load(root); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error = %v, want a size refusal", err)
	}
}

func TestLoadDirectoryRejectsMultipleCorridorDocuments(t *testing.T) {
	t.Parallel()

	root := writeDir(t, map[string]string{
		"sites/banking.txt": "chase.com\n",
		"corridors.yaml": `version: 1
corridors: []
---
version: 1
corridors: []
`,
	})
	if _, err := config.Load(root); err == nil || !strings.Contains(err.Error(), "multiple YAML") {
		t.Fatalf("error = %v, want a multi-document refusal", err)
	}
}

func TestLoadDirectoryIgnoresNonYAMLOverrides(t *testing.T) {
	t.Parallel()

	root := writeDir(t, map[string]string{
		"sites/banking.txt":       "chase.com\n",
		"overrides/notes.txt":     "not an override\n",
		"overrides/nested/x.yaml": "target: nope.example\n",
	})
	f, err := config.Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Targets) != 1 {
		t.Fatalf("targets = %+v", f.Targets)
	}
}
