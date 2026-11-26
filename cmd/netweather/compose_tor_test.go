package main

import (
	"os"
	"path/filepath"
	"strings"
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
	if got := svc.Environment["NETWEATHER_TOR"]; got != "true" {
		t.Fatalf("NETWEATHER_TOR = %q, want the default install to enable Tor", got)
	}
}

// TestComposePinsRegionalTorExits pins the default multi-vantage wiring: every
// continent has its own client-only image and its own verified country, all six
// sidecars start without a profile, and the base file tells the app which SOCKS
// endpoints belong to which country.
func TestComposePinsRegionalTorExits(t *testing.T) {
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
			Build       any               `yaml:"build"`
			Ports       []string          `yaml:"ports"`
			Profiles    []string          `yaml:"profiles"`
			Environment map[string]string `yaml:"environment"`
			DependsOn   []string          `yaml:"depends_on"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatalf("compose.yaml: %v", err)
	}
	for _, cc := range []string{"DE", "JP", "AU", "SG", "CL", "MX"} {
		svc, ok := compose.Services["tor-"+strings.ToLower(cc)]
		if !ok {
			t.Fatalf("compose.yaml has no tor-%s service", strings.ToLower(cc))
		}
		build, ok := svc.Build.(map[string]any)
		if !ok {
			t.Fatalf("tor-%s build = %T, want a mapping", strings.ToLower(cc), svc.Build)
		}
		if build["dockerfile"] != "Dockerfile.tor" {
			t.Errorf("tor-%s does not build the client-only image", strings.ToLower(cc))
		}
		args, _ := build["args"].(map[string]any)
		if got := args["TOR_EXIT_COUNTRY"]; got != cc {
			t.Errorf("tor-%s exit = %v, want %s", strings.ToLower(cc), got, cc)
		}
		if len(svc.Ports) != 0 {
			t.Errorf("tor-%s publishes %v; the proxy must stay on egress", strings.ToLower(cc), svc.Ports)
		}
		if len(svc.Profiles) != 0 {
			t.Errorf("tor-%s profiles = %v; it must start by default", strings.ToLower(cc), svc.Profiles)
		}
	}

	app, ok := compose.Services["netweather"]
	if !ok {
		t.Fatal("compose.yaml has no netweather service")
	}
	want := "DE=tor-de:9050,JP=tor-jp:9050,AU=tor-au:9050,SG=tor-sg:9050,CL=tor-cl:9050,MX=tor-mx:9050"
	if got := app.Environment["NETWEATHER_TOR_VANTAGES"]; got != want {
		t.Fatalf("NETWEATHER_TOR_VANTAGES = %q, want %q", got, want)
	}
	if got := app.Environment["NETWEATHER_TOR"]; got != "true" {
		t.Fatalf("NETWEATHER_TOR = %q, want true", got)
	}
	if got := app.Environment["NETWEATHER_CONFIG"]; got != "/config/fieldtrial-targets.yaml" {
		t.Fatalf("NETWEATHER_CONFIG = %q, want the full field-trial list", got)
	}
	for _, name := range []string{"tor-de", "tor-jp", "tor-au", "tor-sg", "tor-cl", "tor-mx"} {
		found := false
		for _, dep := range app.DependsOn {
			if dep == name {
				found = true
			}
		}
		if !found {
			t.Errorf("default compose does not depend on %s", name)
		}
	}

	raw, err = os.ReadFile(filepath.Join(root, "compose.fieldtrial.yaml")) // #nosec G304 -- repo file.
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
			DependsOn   []string          `yaml:"depends_on"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &overlay); err != nil {
		t.Fatalf("compose.fieldtrial.yaml: %v", err)
	}
	overlayApp := overlay.Services["netweather"]
	if got := overlayApp.Environment["NETWEATHER_TOR_VANTAGES"]; got != want {
		t.Fatalf("overlay NETWEATHER_TOR_VANTAGES = %q, want %q", got, want)
	}
	for _, name := range []string{"tor-de", "tor-jp", "tor-au", "tor-sg", "tor-cl", "tor-mx"} {
		found := false
		for _, dep := range overlayApp.DependsOn {
			if dep == name {
				found = true
			}
		}
		if !found {
			t.Errorf("field trial does not depend on %s", name)
		}
	}
}
