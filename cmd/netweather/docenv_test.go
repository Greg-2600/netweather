package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// envVarPattern matches both ways a NETWEATHER_ variable is read: directly
// with os.Getenv, and through the injected getenv function the logging and
// startup config loaders use so their tests never touch the process
// environment. The prefix filter is deliberate: SOURCE_DATE_EPOCH is a build
// knob, not a runtime setting an operator has to know about.
var envVarPattern = regexp.MustCompile(`(?:os\.Getenv|getenv)\("(NETWEATHER_[A-Z0-9_]+)"\)`)

// TestEveryEnvironmentVariableIsDocumented is the guard against configuration
// drift: every NETWEATHER_ variable the binary reads must appear in the
// README's configuration table. It found eight undocumented variables when it
// was written, and it fails before a new one can ship silently.
func TestEveryEnvironmentVariableIsDocumented(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	read := make(map[string]string) // variable -> first file that reads it
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path) // #nosec G304 -- the test walks its own module.
		if err != nil {
			return err
		}
		for _, match := range envVarPattern.FindAllStringSubmatch(string(raw), -1) {
			name := match[1]
			if _, seen := read[name]; !seen {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					rel = path
				}
				read[name] = filepath.ToSlash(rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(read) < 10 {
		t.Fatalf("found only %d environment variables; the walk is not seeing the source", len(read))
	}

	readme, err := os.ReadFile(filepath.Join(root, "README.md")) // #nosec G304 -- repo file.
	if err != nil {
		t.Fatal(err)
	}
	documented := string(readme)

	missing := make([]string, 0, len(read))
	for name, file := range read {
		if !strings.Contains(documented, name) {
			missing = append(missing, name+" (read by "+file+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("undocumented environment variables in README.md:\n  %s", strings.Join(missing, "\n  "))
	}
}
