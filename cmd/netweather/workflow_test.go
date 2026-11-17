package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWorkflowFilesParse is the guard against shipping a workflow that GitHub
// rejects after the merge. It parses every workflow and the Dependabot config
// with the same YAML library the project already uses.
func TestWorkflowFilesParse(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	files := []string{filepath.Join(root, ".github", "dependabot.yml")}
	workflows, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) == 0 {
		t.Fatal("no workflow files found; the glob is wrong")
	}
	files = append(files, workflows...)

	for _, path := range files {
		raw, err := os.ReadFile(path) // #nosec G304 -- repo files chosen above.
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Errorf("%s is not valid YAML: %v", path, err)
			continue
		}
		if len(doc) == 0 {
			t.Errorf("%s is empty", path)
		}
		if filepath.Base(filepath.Dir(path)) == "workflows" {
			if _, ok := doc["on"]; !ok {
				t.Errorf("%s has no on: trigger", path)
			}
			if _, ok := doc["jobs"]; !ok {
				t.Errorf("%s has no jobs:", path)
			}
		}
	}
}

// TestTorSidecarIsBakedIn pins the two properties that make the sidecar safe:
// the torrc is client-only with no exit traffic, and compose builds that image
// rather than pulling a third-party proxy.
func TestTorSidecarIsBakedIn(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	torrc, err := os.ReadFile(filepath.Join(root, "torrc")) // #nosec G304 -- repo file.
	if err != nil {
		t.Fatalf("torrc: %v", err)
	}
	for _, want := range []string{"ClientOnly 1", "ExitPolicy reject *:*", "SocksPort 0.0.0.0:9050"} {
		if !strings.Contains(string(torrc), want) {
			t.Errorf("torrc is missing %q", want)
		}
	}

	raw, err := os.ReadFile(filepath.Join(root, "compose.yaml")) // #nosec G304 -- repo file.
	if err != nil {
		t.Fatalf("compose.yaml: %v", err)
	}
	var compose struct {
		Services map[string]struct {
			// build is a mapping for the tor service and the short string form
			// for the main image, so it cannot be decoded into a struct.
			Build any    `yaml:"build"`
			Ports []any  `yaml:"ports"`
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatalf("compose.yaml: %v", err)
	}
	tor, ok := compose.Services["tor"]
	if !ok {
		t.Fatal("compose has no tor service")
	}
	build, ok := tor.Build.(map[string]any)
	if !ok {
		t.Fatalf("tor build = %T, want a mapping with dockerfile", tor.Build)
	}
	if build["dockerfile"] != "Dockerfile.tor" {
		t.Fatalf("tor builds with %v, want Dockerfile.tor", build["dockerfile"])
	}
	if len(tor.Ports) != 0 {
		t.Fatalf("tor publishes %v; the proxy must stay on the egress network", tor.Ports)
	}
}
