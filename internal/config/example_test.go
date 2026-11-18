package config_test

import (
	"path/filepath"
	"testing"

	"github.com/Greg-2600/netweather/internal/config"
)

// TestShippedExampleLoads keeps config/targets.example.yaml executable
// documentation. An example that no longer parses is worse than none: it is
// the file a new operator copies first.
func TestShippedExampleLoads(t *testing.T) {
	t.Parallel()

	f, err := config.Load(filepath.Join("..", "..", "config", "targets.example.yaml"))
	if err != nil {
		t.Fatalf("the shipped example does not load: %v", err)
	}
	if len(f.Targets) == 0 {
		t.Fatal("the shipped example has no targets")
	}
}
