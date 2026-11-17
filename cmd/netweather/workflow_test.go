package main

import (
	"fmt"
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

// TestCICarriesTheDeclaredScans keeps THREAT_MODEL.md T62/T63 and QUALITY.md
// §8 honest: the docs say a secret scan and a Dockerfile lint run in CI, so the
// jobs must exist and be pinned rather than floating.
func TestCICarriesTheDeclaredScans(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml")) // #nosec G304 -- repo file.
	if err != nil {
		t.Fatalf("ci.yml: %v", err)
	}

	var ci struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string         `yaml:"name"`
				Run  string         `yaml:"run"`
				Uses string         `yaml:"uses"`
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &ci); err != nil {
		t.Fatalf("ci.yml: %v", err)
	}

	secrets, ok := ci.Jobs["secrets"]
	if !ok {
		t.Fatal("ci.yml has no secrets job")
	}
	var gitleaks, fullHistory bool
	for _, step := range secrets.Steps {
		if strings.Contains(step.Run, "gitleaks") {
			gitleaks = true
			// A scanner pinned to @vX.Y.Z, not @latest and not an unpinned go run.
			if !strings.Contains(step.Run, "gitleaks/v8@v") {
				t.Errorf("gitleaks step is not version-pinned: %q", step.Run)
			}
		}
		if strings.Contains(step.Uses, "actions/checkout") {
			if depth, ok := step.With["fetch-depth"]; ok && fmt.Sprint(depth) == "0" {
				fullHistory = true
			}
		}
	}
	if !gitleaks {
		t.Error("the secrets job does not run gitleaks")
	}
	if !fullHistory {
		t.Error("the secrets job checks out a shallow history; a removed secret is still leaked")
	}

	dockerfile, ok := ci.Jobs["dockerfile"]
	if !ok {
		t.Fatal("ci.yml has no dockerfile job")
	}
	scanned := 0
	for _, step := range dockerfile.Steps {
		if !strings.Contains(step.Run, "hadolint") {
			continue
		}
		scanned++
		if strings.Contains(step.Run, ":latest") {
			t.Errorf("hadolint image floats on :latest: %q", step.Run)
		}
	}
	if scanned != 2 {
		t.Fatalf("hadolint steps = %d, want one per Dockerfile", scanned)
	}
}

// TestReleaseBuildsTheDebugVariant keeps IMAGE.md's debug image honest. The
// docs promise a signed <version>-debug tag built from the debug target; a job
// that silently disappears or stops targeting debug would leave an operator
// with a documented image that does not exist.
func TestReleaseBuildsTheDebugVariant(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml")) // #nosec G304 -- repo file.
	if err != nil {
		t.Fatalf("release.yml: %v", err)
	}

	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string         `yaml:"name"`
				Uses string         `yaml:"uses"`
				Run  string         `yaml:"run"`
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("release.yml: %v", err)
	}

	debug, ok := wf.Jobs["debug-image"]
	if !ok {
		t.Fatal("release.yml has no debug-image job")
	}
	var built, tagged, signed bool
	for _, step := range debug.Steps {
		if strings.Contains(step.Uses, "docker/build-push-action") {
			if fmt.Sprint(step.With["target"]) == "debug" {
				built = true
			}
			if tags, ok := step.With["tags"].(string); ok && strings.Contains(tags, "-debug") {
				tagged = true
			}
		}
		if strings.Contains(step.Run, "cosign sign") {
			signed = true
		}
	}
	if !built {
		t.Error("the debug job does not build the debug target")
	}
	if !tagged {
		t.Error("the debug job does not push a -debug tag")
	}
	if !signed {
		t.Error("the debug image is not signed")
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
	for _, want := range []string{
		"ClientOnly 1", "ExitPolicy reject *:*",
		"BridgeRelay 0", "PublishServerDescriptor 0",
		"SocksPort 0.0.0.0:9050",
	} {
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
