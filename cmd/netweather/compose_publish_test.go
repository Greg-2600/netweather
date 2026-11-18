package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestComposePublishesOnlyTheDashboard enforces THREAT_MODEL.md invariant 12:
// no shipped compose file mounts the Docker socket or publishes a port other
// than the dashboard's. The runtime compose suite checks the same property on a
// live container; this test makes a mistake fail before the image is built.
func TestComposePublishesOnlyTheDashboard(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(root, "compose*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no compose files found; the glob is wrong")
	}

	for _, path := range files {
		raw, err := os.ReadFile(path) // #nosec G304 -- repo file chosen above.
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Services map[string]struct {
				Ports   []string `yaml:"ports"`
				Volumes []string `yaml:"volumes"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(doc.Services) == 0 {
			t.Fatalf("%s has no services", path)
		}

		for name, svc := range doc.Services {
			for _, port := range svc.Ports {
				if !strings.HasSuffix(port, ":2600") {
					t.Errorf("%s: service %s publishes %q; only 2600 may leave the container",
						filepath.Base(path), name, port)
				}
			}
			for _, volume := range svc.Volumes {
				if strings.Contains(volume, "docker.sock") {
					t.Errorf("%s: service %s mounts the Docker socket", filepath.Base(path), name)
				}
			}
		}
	}
}
