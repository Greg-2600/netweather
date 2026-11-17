package main

import (
	"os"
	"path/filepath"
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
