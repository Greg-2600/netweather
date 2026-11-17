package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// TestCoverageFloorsMatchQualityDoc is the executable form of the promise in
// docs/QUALITY.md §7: "the floors live in internal/devtool/main.go ... this
// table is the readable copy, and the two are checked together when either
// changes". Without this test, changing one and forgetting the other is
// invisible until someone trusts the wrong number.
func TestCoverageFloorsMatchQualityDoc(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "QUALITY.md"))
	if err != nil {
		t.Fatalf("read QUALITY.md: %v", err)
	}

	pkgPattern := regexp.MustCompile("`((?:internal|cmd)/[a-z0-9/]+)`")
	floorPattern := regexp.MustCompile(`\*\*(\d+(?:\.\d+)?)%\*\*`)

	documented := make(map[string]float64)
	for _, line := range regexp.MustCompile(`\r?\n`).Split(string(raw), -1) {
		if len(line) == 0 || line[0] != '|' {
			continue
		}
		cells := splitTableRow(line)
		if len(cells) < 2 {
			continue
		}
		floor := floorPattern.FindStringSubmatch(cells[1])
		if floor == nil {
			continue
		}
		value, err := strconv.ParseFloat(floor[1], 64)
		if err != nil {
			t.Fatalf("parse floor %q: %v", floor[1], err)
		}
		for _, match := range pkgPattern.FindAllStringSubmatch(cells[0], -1) {
			documented["/"+match[1]] = value
		}
	}

	if len(documented) == 0 {
		t.Fatal("QUALITY.md §7 parsed as an empty floor table; the table shape changed")
	}
	if len(documented) != len(coverageThresholds) {
		t.Errorf("QUALITY.md documents %d packages, devtool enforces %d",
			len(documented), len(coverageThresholds))
	}
	for pkg, want := range coverageThresholds {
		got, ok := documented[pkg]
		if !ok {
			t.Errorf("%s is enforced at %.0f%% but missing from QUALITY.md §7", pkg, want)
			continue
		}
		if got != want {
			t.Errorf("%s: QUALITY.md says %.0f%%, devtool enforces %.0f%%", pkg, got, want)
		}
	}
	for pkg, got := range documented {
		if _, ok := coverageThresholds[pkg]; !ok {
			t.Errorf("%s is documented at %.0f%% but not enforced", pkg, got)
		}
	}
}

// splitTableRow returns the trimmed cells of a Markdown table line, without the
// empty leading and trailing cells produced by the outer pipes.
func splitTableRow(line string) []string {
	raw := regexp.MustCompile(`\s*\|\s*`).Split(line, -1)
	if len(raw) > 2 {
		raw = raw[1 : len(raw)-1]
	}
	for i := range raw {
		raw[i] = regexp.MustCompile(`^\s+|\s+$`).ReplaceAllString(raw[i], "")
	}
	return raw
}
