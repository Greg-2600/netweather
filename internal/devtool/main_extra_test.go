package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoverageFloor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pkg  string
		want float64
	}{
		{"/internal/classify", 95},
		{"/internal/devtool", 15},
		{"/cmd/netweather", 30},
		{"/internal/not-in-the-table", 70},
	}
	for _, tc := range cases {
		if got := coverageFloor(tc.pkg); got != tc.want {
			t.Errorf("coverageFloor(%q) = %.0f, want %.0f", tc.pkg, got, tc.want)
		}
	}
}

func TestRacePackagesIgnoresBlankLines(t *testing.T) {
	t.Parallel()

	got := racePackages("\n  \n" + modulePath + "/internal/api\n\n" + modulePath + "/internal/web\n")
	if len(got) != 1 || got[0] != modulePath+"/internal/api" {
		t.Fatalf("race packages = %v, want [%s/internal/api]", got, modulePath)
	}
}

func TestParseCoverageMalformedLines(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "coverage.out")
	profile := strings.Join([]string{
		"mode: atomic",
		"",
		"not a coverage line",
		"github.com/Greg-2600/netweather/internal/api/a.go:1.1,2.2 3",
		"github.com/Greg-2600/netweather/internal/api/a.go:1.1,2.2 x 1",
		"github.com/Greg-2600/netweather/internal/api/a.go:1.1,2.2 3 y",
		"github.com/Greg-2600/netweather/internal/api/a.go:1.1,2.2 3 1",
		"github.com/Greg-2600/netweather/internal/devtool/main.go:1.1,2.2 4 1",
	}, "\n")

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

	var api, devtool pkgCoverage
	for _, s := range stats {
		switch {
		case strings.HasSuffix(s.pkg, "/internal/api"):
			api = s
		case strings.HasSuffix(s.pkg, "/internal/devtool"):
			devtool = s
		}
	}
	if api.total != 3 || api.covered != 3 {
		t.Fatalf("api coverage = %d/%d, want 3/3", api.covered, api.total)
	}
	if devtool.total != 4 || devtool.covered != 4 {
		t.Fatalf("devtool coverage = %d/%d, want 4/4", devtool.covered, devtool.total)
	}
	// The task runner is reported per package but excluded from the library total.
	if total != 3 || covered != 3 {
		t.Fatalf("totals = %d/%d, want 3/3 (devtool must be excluded)", covered, total)
	}
}

func TestParseCoverageScannerError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "oversized.out")
	profile := "mode: set\n" + strings.Repeat("a", 2*1024*1024)
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := parseCoverage(path); err == nil {
		t.Fatal("an oversized profile line should surface the scanner error")
	}
}

func TestWriteCoverageSummaryDisabled(t *testing.T) {
	t.Setenv("GITHUB_STEP_SUMMARY", "")

	if err := writeCoverageSummary(nil, 0, 0, nil); err != nil {
		t.Fatalf("writeCoverageSummary without a summary path: %v", err)
	}
}

func TestWriteCoverageSummaryFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", path)

	stats := []pkgCoverage{
		{pkg: "github.com/Greg-2600/netweather/internal/api", total: 10, covered: 9},
		{pkg: "github.com/Greg-2600/netweather/internal/empty", total: 0, covered: 0},
		{pkg: "github.com/Greg-2600/netweather/internal/store", total: 10, covered: 1},
	}
	failures := []string{"internal/store: 10.0% < 80%"}
	if err := writeCoverageSummary(stats, 20, 10, failures); err != nil {
		t.Fatalf("writeCoverageSummary: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| `github.com/Greg-2600/netweather/internal/store` | 10.0% | 80% | FAIL |",
		"| **Total (library)** | **50.0%** | **87%** | **FAIL** |",
		"Coverage gates failed:",
		"* internal/store: 10.0% < 80%",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("summary missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "internal/empty") {
		t.Errorf("zero-statement package should be skipped:\n%s", raw)
	}
}

func TestWriteCoverageSummaryZeroTotal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", path)

	stats := []pkgCoverage{{pkg: "github.com/Greg-2600/netweather/internal/api", total: 10, covered: 10}}
	if err := writeCoverageSummary(stats, 0, 0, nil); err != nil {
		t.Fatalf("writeCoverageSummary: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Total (library)") {
		t.Errorf("zero total should not write a library row:\n%s", raw)
	}
}

func TestWriteCoverageSummaryOpenError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", path)

	if err := writeCoverageSummary(nil, 0, 0, nil); err == nil {
		t.Fatal("an unwritable summary path should fail")
	}
}

func TestFileSHA256(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "artifact")
	content := []byte("netweather\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	want := sha256.Sum256(content)
	got, err := fileSHA256(path)
	if err != nil {
		t.Fatalf("fileSHA256: %v", err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("digest = %s, want %s", got, hex.EncodeToString(want[:]))
	}

	if _, err := fileSHA256(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("missing file should be an error")
	}
}

func TestGoBinaryAndMustTool(t *testing.T) {
	if _, err := goBinary(); err != nil {
		t.Fatalf("goBinary with go on PATH: %v", err)
	}

	t.Setenv("PATH", "")
	if _, err := goBinary(); err == nil {
		t.Fatal("goBinary without go on PATH should fail")
	}
	if got := mustTool(); got != "go" {
		t.Fatalf("mustTool fallback = %q, want go", got)
	}
}

func TestCaptureAndGoEnv(t *testing.T) {
	goBin, err := goBinary()
	if err != nil {
		t.Fatal(err)
	}

	out, err := capture(goBin, "version")
	if err != nil {
		t.Fatalf("capture go version: %v", err)
	}
	if !strings.HasPrefix(out, "go version ") {
		t.Fatalf("capture output = %q, want a go version string", out)
	}
	if _, err := capture(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("capture of a missing command should fail")
	}

	if got := goEnv("GOROOT"); got == "" {
		t.Fatal("goEnv(GOROOT) should not be empty")
	}
	t.Setenv("PATH", "")
	if got := goEnv("GOROOT"); got != "" {
		t.Fatalf("goEnv without go on PATH = %q, want empty", got)
	}
	if raceEnv() {
		t.Fatal("raceEnv should be false without the go toolchain")
	}
}

func TestCommitDateSourceDateEpoch(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")

	if got, want := commitDate(), "2023-11-14T22:13:20Z"; got != want {
		t.Fatalf("commitDate = %q, want %q", got, want)
	}

	t.Setenv("SOURCE_DATE_EPOCH", "not-a-number")
	got := commitDate()
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("commitDate fallback %q is not RFC3339: %v", got, err)
	}
	if delta := time.Since(parsed); delta < -time.Minute || delta > time.Minute {
		t.Fatalf("commitDate fallback %q is not near now", got)
	}
}

func TestCommandRunGoAndRunTool(t *testing.T) {
	goBin, err := goBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := command(goBin, "version").Run(); err != nil {
		t.Fatalf("command go version: %v", err)
	}
	if err := runGo("version"); err != nil {
		t.Fatalf("runGo version: %v", err)
	}
	if err := runGo("definitely-not-a-subcommand"); err == nil {
		t.Fatal("an unknown go subcommand should fail")
	}
	if err := runTool("definitely-not-a-tool"); err == nil {
		t.Fatal("an unknown go tool should fail")
	}
}

func TestBuildMetadata(t *testing.T) {
	version, commit, date := buildMetadata()
	if version == "" {
		t.Error("version should never be empty")
	}
	if commit == "" {
		t.Error("commit should never be empty")
	}
	if _, err := time.Parse(time.RFC3339, date); err != nil {
		t.Fatalf("build date %q is not RFC3339: %v", date, err)
	}
}
