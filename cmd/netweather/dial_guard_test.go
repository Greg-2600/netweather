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

// TestOutboundDialsGoThroughNetx enforces THREAT_MODEL.md's first invariant:
// production code does not open an outbound connection with a direct net.Dial
// call. The guarded dialer resolves, checks the address and port, and dials the
// checked IP; a direct dial bypasses all of that.
//
// Test files are excluded because a test may make a raw connection to prove a
// fixture is reachable, and internal/netx is the one package that is allowed to
// dial.
func TestOutboundDialsGoThroughNetx(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	dialRE := regexp.MustCompile(`\bnet\.Dial(?:Timeout|IP|TCP|UDP|Unix)?\(`)
	var offenders []string

	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "netx" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, err := os.ReadFile(path) // #nosec G304 -- repo source chosen above.
			if err != nil {
				return err
			}
			if dialRE.Match(raw) {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				offenders = append(offenders, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Fatalf("direct net.Dial calls outside internal/netx: %s", strings.Join(offenders, ", "))
	}
}
