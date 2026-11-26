package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A profile line is "file.go:start.col,end.col numStmt count". Parsing it wrong
// silently reports 0% coverage, which is exactly the bug this test exists for.
func TestParseCoverage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "coverage.out")
	profile := "mode: atomic\n" +
		"github.com/Greg-2600/netweather/internal/api/a.go:1.1,2.2 3 1\n" +
		"github.com/Greg-2600/netweather/internal/api/a.go:3.3,4.4 2 0\n" +
		"github.com/Greg-2600/netweather/cmd/netweather/main.go:1.1,2.2 5 1\n"

	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, total, covered, err := parseCoverage(path)
	if err != nil {
		t.Fatalf("parseCoverage: %v", err)
	}

	if len(stats) != 2 {
		t.Fatalf("packages = %d, want 2: %#v", len(stats), stats)
	}

	var api pkgCoverage
	for _, s := range stats {
		if s.pkg == "github.com/Greg-2600/netweather/internal/api" {
			api = s
		}
	}
	if api.total != 5 || api.covered != 3 {
		t.Fatalf("api coverage = %d/%d, want 3/5", api.covered, api.total)
	}

	// cmd/* is excluded from the library total but still reported per package.
	if total != 5 || covered != 3 {
		t.Fatalf("totals = %d/%d, want 3/5 (cmd must be excluded)", covered, total)
	}
}

func TestParseCoverageEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "empty.out")
	if err := os.WriteFile(path, []byte("mode: set\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, total, covered, err := parseCoverage(path)
	if err != nil {
		t.Fatalf("parseCoverage: %v", err)
	}
	if len(stats) != 0 || total != 0 || covered != 0 {
		t.Fatalf("empty profile produced %d stats, %d/%d", len(stats), covered, total)
	}
}

func TestParseCoverageMissingFile(t *testing.T) {
	t.Parallel()

	if _, _, _, err := parseCoverage(filepath.Join(t.TempDir(), "absent.out")); err == nil {
		t.Fatal("missing profile should be an error, not silently 0%")
	}
}

func TestLdflagsCarryEveryField(t *testing.T) {
	t.Parallel()

	got := ldflags("v1.2.3", "abc123", "2026-11-16T00:00:00Z")
	for _, want := range []string{
		"-s",
		versionPkg + ".Version=v1.2.3",
		versionPkg + ".Commit=abc123",
		versionPkg + ".Date=2026-11-16T00:00:00Z",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ldflags missing %q: %s", want, got)
		}
	}
}

func TestLintDockerfilesMissingFile(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	if err := lintDockerfiles("/fixed/docker"); err == nil {
		t.Fatal("missing Dockerfile should fail lint")
	}
}

func TestRacePackagesExcludesWeb(t *testing.T) {
	t.Parallel()

	got := racePackages("github.com/Greg-2600/netweather/internal/api\ngithub.com/Greg-2600/netweather/internal/web\ngithub.com/Greg-2600/netweather/internal/webhook")
	if len(got) != 2 || got[0] != "github.com/Greg-2600/netweather/internal/api" || got[1] != "github.com/Greg-2600/netweather/internal/webhook" {
		t.Fatalf("race packages = %v", got)
	}
}

func TestWriteCoverageSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", path)

	stats := []pkgCoverage{{
		pkg:     "github.com/Greg-2600/netweather/internal/api",
		total:   10,
		covered: 9,
	}}
	if err := writeCoverageSummary(stats, 10, 9, nil); err != nil {
		t.Fatalf("writeCoverageSummary: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Coverage",
		"internal/api",
		"90.0%",
		"Total (library)",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("summary missing %q:\n%s", want, raw)
		}
	}
}
