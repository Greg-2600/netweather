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

// TestEnvironmentVariablesAreDocumented keeps the README and docs honest about
// the configuration surface. A new NETWEATHER_* variable in production code
// that no document mentions is a setting an operator cannot discover.
//
// Test files are excluded: NETWEATHER_E2E and the browser switch exist only to
// select a test path, and documenting them as deployment knobs would be worse
// than leaving them out.
func TestEnvironmentVariablesAreDocumented(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	codeRE := regexp.MustCompile(`"NETWEATHER_[A-Z0-9_]+"`)
	docRE := regexp.MustCompile(`NETWEATHER_[A-Z0-9_]+`)

	code := make(map[string]struct{})
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, err := os.ReadFile(path) // #nosec G304 -- repo source chosen above.
			if err != nil {
				return err
			}
			for _, match := range codeRE.FindAllString(string(raw), -1) {
				code[strings.Trim(match, `"`)] = struct{}{}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(code) == 0 {
		t.Fatal("the source scan found no environment variables; the pattern is wrong")
	}

	docs := make(map[string]struct{})
	docFiles := []string{filepath.Join(root, "README.md")}
	entries, err := os.ReadDir(filepath.Join(root, "docs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			docFiles = append(docFiles, filepath.Join(root, "docs", entry.Name()))
		}
	}
	for _, path := range docFiles {
		raw, err := os.ReadFile(path) // #nosec G304 -- repo docs chosen above.
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range docRE.FindAllString(string(raw), -1) {
			docs[name] = struct{}{}
		}
	}

	var missing []string
	for name := range code {
		if _, ok := docs[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("production environment variables with no documentation: %s",
			strings.Join(missing, ", "))
	}
}
