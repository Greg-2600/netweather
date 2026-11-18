package main

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestComposePointsTorAtTheSidecarService pins the shipped wiring: the app's
// Tor client must reach the sidecar by service name, not loopback, or enabling
// the profile would make startup fail with a refused connection. The variable
// is inert until NETWEATHER_TOR is true, so setting it unconditionally is safe
// and removes a two-line footgun.
func TestComposePointsTorAtTheSidecarService(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "compose.yaml")) // #nosec G304 -- repo file.
	if err != nil {
		t.Fatal(err)
	}

	var compose struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatalf("compose.yaml: %v", err)
	}

	svc, ok := compose.Services["netweather"]
	if !ok {
		t.Fatal("compose.yaml has no netweather service")
	}
	if got := svc.Environment["NETWEATHER_TOR_SOCKS"]; got != "tor:9050" {
		t.Fatalf("NETWEATHER_TOR_SOCKS = %q, want the sidecar service name", got)
	}
}
