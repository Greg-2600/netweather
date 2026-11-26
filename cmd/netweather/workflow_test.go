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
		"SocksPort 0.0.0.0:9050 IsolateSOCKSAuth",
		"ControlPort 0.0.0.0:9051",
		"HashedControlPassword 16:",
		"MaxCircuitDirtiness 3600",
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
			// build is a mapping for the pinned regional services and the
			// short string form for the main image.
			Build    any      `yaml:"build"`
			Ports    []any    `yaml:"ports"`
			Image    string   `yaml:"image"`
			Profiles []string `yaml:"profiles"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatalf("compose.yaml: %v", err)
	}
	tor, ok := compose.Services["tor"]
	if !ok {
		t.Fatal("compose has no tor service")
	}
	if tor.Build == nil {
		t.Fatal("tor does not build Dockerfile.tor")
	}
	if tor.Image != "netweather-tor:local" {
		t.Fatalf("tor image = %q, want the locally built client-only image", tor.Image)
	}
	if len(tor.Profiles) != 1 || tor.Profiles[0] != "tor" {
		t.Fatalf("tor profiles = %v, want the optional unpinned profile", tor.Profiles)
	}
	if len(tor.Ports) != 0 {
		t.Fatalf("tor publishes %v; the proxy must stay on the egress network", tor.Ports)
	}
}

// TestCIImagePublishRules keeps issue #8 and IMAGE.md §6.1 honest: pull
// requests build the CI image and never push, only a push to main publishes,
// only the publishing job can write a package, and GITHUB_TOKEN is the only
// credential the workflow uses.
func TestCIImagePublishRules(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml")) // #nosec G304 -- repo file.
	if err != nil {
		t.Fatalf("ci.yml: %v", err)
	}

	type step struct {
		Uses string         `yaml:"uses"`
		With map[string]any `yaml:"with"`
	}
	var ci struct {
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			If          string            `yaml:"if"`
			Needs       []string          `yaml:"needs"`
			Permissions map[string]string `yaml:"permissions"`
			Concurrency struct {
				Group            string `yaml:"group"`
				CancelInProgress bool   `yaml:"cancel-in-progress"`
			} `yaml:"concurrency"`
			Steps []step `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &ci); err != nil {
		t.Fatalf("ci.yml: %v", err)
	}

	if got := ci.Permissions["packages"]; got != "" {
		t.Errorf("workflow-level packages permission = %q; grant it only on image-publish", got)
	}
	for name, job := range ci.Jobs {
		if name != "image-publish" && job.Permissions["packages"] != "" {
			t.Errorf("job %s has packages: %s; only image-publish may write", name, job.Permissions["packages"])
		}
	}

	buildPush := func(steps []step) step {
		t.Helper()
		for _, s := range steps {
			if strings.Contains(s.Uses, "docker/build-push-action@") {
				return s
			}
		}
		t.Fatal("no docker/build-push-action step")
		return step{}
	}
	gates := []string{"quality", "lint", "vuln", "secrets", "dockerfile", "build", "uitest", "container", "compose"}
	needsGates := func(name string, needs []string) {
		t.Helper()
		for _, gate := range gates {
			found := false
			for _, n := range needs {
				found = found || n == gate
			}
			if !found {
				t.Errorf("%s does not wait for the %s gate", name, gate)
			}
		}
	}

	pr, ok := ci.Jobs["image-build"]
	if !ok {
		t.Fatal("ci.yml has no image-build job")
	}
	if !strings.Contains(pr.If, "pull_request") {
		t.Errorf("image-build if = %q, want pull requests only", pr.If)
	}
	needsGates("image-build", pr.Needs)
	if s := buildPush(pr.Steps); fmt.Sprint(s.With["push"]) != "false" {
		t.Errorf("image-build push = %v; a pull request must not push", s.With["push"])
	}

	pub, ok := ci.Jobs["image-publish"]
	if !ok {
		t.Fatal("ci.yml has no image-publish job")
	}
	if !strings.Contains(pub.If, "github.event_name == 'push'") || !strings.Contains(pub.If, "refs/heads/main") {
		t.Errorf("image-publish if = %q, want push to main only", pub.If)
	}
	needsGates("image-publish", pub.Needs)
	if pub.Permissions["contents"] != "read" || pub.Permissions["packages"] != "write" || len(pub.Permissions) != 2 {
		t.Errorf("image-publish permissions = %v, want exactly contents: read and packages: write", pub.Permissions)
	}
	if pub.Concurrency.Group == "" || pub.Concurrency.CancelInProgress {
		t.Errorf("image-publish concurrency = %+v, want a group that queues rather than cancels", pub.Concurrency)
	}
	s := buildPush(pub.Steps)
	if fmt.Sprint(s.With["push"]) != "true" {
		t.Errorf("image-publish push = %v, want true", s.With["push"])
	}
	tags := fmt.Sprint(s.With["tags"])
	for _, want := range []string{":sha-${{ github.sha }}", ":main"} {
		if !strings.Contains(tags, want) {
			t.Errorf("image-publish tags %q are missing %q", tags, want)
		}
	}
	if strings.Contains(tags, ":latest") {
		t.Error("image-publish pushes :latest; that tag belongs to the signed release")
	}
	args := fmt.Sprint(s.With["build-args"])
	for _, want := range []string{"VERSION=", "COMMIT=", "DATE="} {
		if !strings.Contains(args, want) {
			t.Errorf("image-publish build-args are missing %s", want)
		}
	}

	if all, token := strings.Count(string(raw), "secrets."), strings.Count(string(raw), "secrets.GITHUB_TOKEN"); all != token {
		t.Errorf("ci.yml reads %d secrets, %d of them GITHUB_TOKEN; no other credential is allowed", all, token)
	}
}
