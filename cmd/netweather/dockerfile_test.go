package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDockerfilesPinBaseImages is the executable form of THREAT_MODEL.md
// T61/T63: a base image referenced by a moving tag is a supply-chain
// dependency the release cannot reproduce or verify. Dependabot updates the
// digest, so pinning does not freeze the base forever.
func TestDockerfilesPinBaseImages(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"Dockerfile", "Dockerfile.tor"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join("..", "..", name))
			if err != nil {
				t.Fatal(err)
			}
			found := 0
			for i, line := range strings.Split(string(raw), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 0 || !strings.EqualFold(fields[0], "FROM") {
					continue
				}
				if len(fields) < 2 {
					t.Fatalf("%s:%d has a FROM with no image", name, i+1)
				}
				found++
				image := fields[1]
				if !strings.Contains(image, "@sha256:") {
					t.Errorf("%s:%d base image %q is not digest-pinned", name, i+1, image)
				}
				if strings.Contains(image, ":latest") && !strings.Contains(image, "@sha256:") {
					t.Errorf("%s:%d base image %q uses a moving latest tag", name, i+1, image)
				}
			}
			if found == 0 {
				t.Fatalf("%s has no FROM line", name)
			}
		})
	}
}
