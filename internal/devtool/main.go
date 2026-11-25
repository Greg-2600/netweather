// Command devtool is the repository's cross-platform task runner. It exists so
// "how do I build/test/lint this" has one answer on Windows, macOS, Linux and
// CI, without requiring make, just, or a shell-specific script.
//
//	go run ./internal/devtool <task>
//
// Tasks: fmt, vet, lint, lint-all, test, cover, ui, ui-all, compose, mutate, build, snapshot, verify
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	modulePath   = "github.com/Greg-2600/netweather"
	versionPkg   = modulePath + "/internal/version"
	coverageFile = "coverage.out"
	distDir      = "dist"
)

const totalCoverageThreshold = 87.0

// coverageThresholds is the enforced per-package coverage floor. docs/QUALITY.md
// §7 is the readable copy of this table; TestCoverageFloorsMatchQualityDoc fails
// if the two drift, because a floor that only exists in prose is not a gate.
var coverageThresholds = map[string]float64{
	"/internal/agent":        85,
	"/internal/anomaly":      90,
	"/internal/api":          90,
	"/internal/baseline":     95,
	"/internal/board":        90,
	"/internal/classify":     95,
	"/internal/config":       90,
	"/internal/confirm":      95,
	"/internal/correlate":    90,
	"/internal/corridor":     90,
	"/internal/dnsdiv":       95,
	"/internal/doctor":       85,
	"/internal/enrich":       85,
	"/internal/fakeinternet": 75,
	"/internal/feeds":        85,
	"/internal/ihi":          95,
	"/internal/incidents":    85,
	"/internal/logging":      80,
	"/internal/metrics":      90,
	"/internal/netx":         95,
	"/internal/probe":        90,
	"/internal/schedule":     85,
	"/internal/seed":         85,
	"/internal/selfcheck":    85,
	"/internal/tracebudget":  85,
	"/internal/startup":      75,
	"/internal/state":        90,
	"/internal/store":        80,
	"/internal/streamlimit":  90,
	"/internal/bytecap":      90,
	"/internal/web":          72,
	"/cmd/netweather":        30, // serve is wiring; the container smoke test owns it
	"/internal/verdict":      95,
	"/internal/version":      70,
	"/internal/devtool":      15, // profile parser + ldflags tests
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	task := os.Args[1]
	args := os.Args[2:]

	var err error
	switch task {
	case "fmt":
		err = taskFmt(args)
	case "vet":
		err = runGo("vet", "./...")
	case "lint":
		err = runTool("golangci-lint", "run")
	case "lint-all":
		err = taskLintAll()
	case "test":
		err = taskTest()
	case "cover":
		err = taskCover()
	case "ui":
		err = runGo("test", "-tags=uitest", "-count=1", "./internal/web/uitest")
	case "ui-all":
		// One run per browser, so a WebKit failure names WebKit. The per-PR job
		// stays chromium-only; the weekly job runs all three (QUALITY.md §5.8).
		err = taskUIBrowsers()
	case "e2e":
		err = runGo("test", "-tags=e2e", "-count=1", "./internal/e2e")
	case "compose":
		// The suite builds and tears down its own image and networks, so this is
		// the whole harness: it needs Docker and nothing else.
		err = runGo("test", "-tags=compose", "-count=1", "-timeout", "15m", "./internal/e2e")
	case "mutate":
		// Advisory mutation testing over the classifier (QUALITY.md §5.7). The
		// version is pinned in the task so a run is reproducible and no tool
		// directive is needed for something this slow.
		err = runGo("run", "github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0",
			"unleash", "./internal/classify",
			"--workers=2", "--timeout-coefficient=10",
			"-o", "mutation.json")
	case "build":
		err = taskBuild()
	case "repro":
		err = taskRepro()
	case "snapshot":
		err = taskSnapshot()
	case "verify":
		err = taskVerify()
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown task %q", task)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "\ndevtool:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `netweather devtool

  go run ./internal/devtool fmt [-check]   gofumpt + gci (write, or only check)
  go run ./internal/devtool vet            go vet ./...
  go run ./internal/devtool lint           golangci-lint (pinned in go.mod)
  go run ./internal/devtool lint-all       golangci-lint + Dockerfiles + gitleaks
  go run ./internal/devtool test           race (when available) + shuffle + count=1
  go run ./internal/devtool cover          coverage profile + per-package gates
  go run ./internal/devtool ui             Playwright suite (needs a browser)
  go run ./internal/devtool ui-all         Playwright suite on chromium, firefox, and webkit
  go run ./internal/devtool e2e            real-endpoint E2E (needs NETWEATHER_E2E=1)
  go run ./internal/devtool compose        container network-plane suite (needs Docker)
  go run ./internal/devtool mutate         mutation testing on internal/classify (advisory, slow)
  go run ./internal/devtool build          static binary with version metadata
  go run ./internal/devtool repro          build twice, compare hashes
  go run ./internal/devtool snapshot       cross-platform build matrix + SHA256SUMS
  go run ./internal/devtool verify         fmt -check, vet, test, cover, build
`)
}

// --- process helpers -------------------------------------------------------

func goBinary() (string, error) {
	p, err := exec.LookPath("go")
	if err != nil {
		return "", errors.New("cannot find the go toolchain; add it to PATH")
	}
	return p, nil
}

func command(name string, args ...string) *exec.Cmd {
	// #nosec G204 -- name is the Go toolchain or git; args are build flags we build ourselves.
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}

func runGo(args ...string) error {
	goBin, err := goBinary()
	if err != nil {
		return err
	}
	return command(goBin, args...).Run()
}

func runTool(tool string, args ...string) error {
	return runGo(append([]string{"tool", tool}, args...)...)
}

func taskLintAll() error {
	fmt.Println("==> golangci-lint")
	if err := runTool("golangci-lint", "run"); err != nil {
		return err
	}

	docker, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("docker is required for Dockerfile and Compose lint; install Docker first")
	}
	if err := lintDockerfiles(docker); err != nil {
		return err
	}

	fmt.Println("==> gitleaks")
	return runGo("run",
		"github.com/zricethezav/gitleaks/v8@v8.24.3",
		"detect",
		"--source", ".",
		"--config", ".gitleaks.toml",
		"--verbose",
		"--redact",
		"--exit-code", "1")
}

func lintDockerfiles(docker string) error {
	for _, file := range []string{"Dockerfile", "Dockerfile.tor"} {
		fmt.Println("==> hadolint", file)
		// #nosec G304 -- file is one of the two fixed repository Dockerfiles.
		input, err := os.Open(file)
		if err != nil {
			return fmt.Errorf("open %s: %w", file, err)
		}
		// #nosec G204 -- docker comes from PATH and args are fixed by this task.
		cmd := exec.CommandContext(context.Background(), docker, "run", "--rm", "-i", "hadolint/hadolint:v2.12.0")
		cmd.Stdin = input
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Run()
		_ = input.Close()
		if err != nil {
			return fmt.Errorf("hadolint %s: %w", file, err)
		}
	}
	return nil
}

func capture(name string, args ...string) (string, error) {
	// #nosec G204 -- fixed tool names and literal git subcommands.
	out, err := exec.CommandContext(context.Background(), name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

func goEnv(key string) string {
	goBin, err := goBinary()
	if err != nil {
		return ""
	}
	// #nosec G204 -- goBin comes from LookPath("go"), and key is an internal constant.
	out, err := exec.CommandContext(context.Background(), goBin, "env", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// --- fmt -------------------------------------------------------------------

func taskFmt(args []string) error {
	check := false
	for _, a := range args {
		if a == "-check" || a == "--check" {
			check = true
		}
	}

	if check {
		// #nosec G204 -- pinned go tool, literal arguments.
		out, err := exec.CommandContext(context.Background(), mustTool(), "tool", "gofumpt", "-l", ".").Output()
		if err != nil {
			return fmt.Errorf("gofumpt: %w", err)
		}
		if files := strings.TrimSpace(string(out)); files != "" {
			return fmt.Errorf("gofumpt would reformat:\n%s", files)
		}
		// #nosec G204 -- pinned go tool, literal arguments.
		diff, err := exec.CommandContext(context.Background(), mustTool(), "tool", "gci", "diff", "-s", "standard", "-s", "default", ".").Output()
		if err != nil {
			return fmt.Errorf("gci: %w", err)
		}
		if d := strings.TrimSpace(string(diff)); d != "" {
			return fmt.Errorf("import order differs (gci):\n%s", d)
		}
		return nil
	}

	if err := runTool("gofumpt", "-w", "."); err != nil {
		return err
	}
	return runTool("gci", "write", "-s", "standard", "-s", "default", ".")
}

func mustTool() string {
	b, err := goBinary()
	if err != nil {
		return "go"
	}
	return b
}

// --- test / cover ----------------------------------------------------------

func taskTest() error {
	if !raceEnv() {
		return runGo("test", "-count=1", "-shuffle=on", "./...")
	}

	out, err := capture("go", "list", "./...")
	if err != nil {
		return fmt.Errorf("list packages: %w", err)
	}
	packages := racePackages(out)
	args := []string{"test", "-count=1", "-shuffle=on", "-race"}
	args = append(args, packages...)
	if err := runGo(args...); err != nil {
		return err
	}
	return runGo("test", "-count=1", "-shuffle=on", "./internal/web")
}

func racePackages(packageList string) []string {
	packages := make([]string, 0)
	for _, pkg := range strings.Split(strings.TrimSpace(packageList), "\n") {
		pkg = strings.TrimSpace(pkg)
		if pkg != "" && !strings.HasSuffix(pkg, "/internal/web") {
			packages = append(packages, pkg)
		}
	}
	return packages
}

// raceEnv reports whether the race detector can run here. It needs cgo, which on
// Windows needs a C compiler most contributors do not have.
func raceEnv() bool {
	return goEnv("CGO_ENABLED") == "1"
}

type pkgCoverage struct {
	pkg     string
	total   int
	covered int
}

func taskCover() error {
	profile := filepath.Join(os.TempDir(), coverageFile)
	err := runGo("test", "-count=1", "-covermode=atomic", "-coverprofile="+profile, "./...")
	if err != nil {
		return fmt.Errorf("tests failed; coverage is meaningless: %w", err)
	}
	// The profile lives in the temp dir; nothing else needs a copy, and writing
	// one into the repo would be an unowned artefact.

	stats, total, covered, err := parseCoverage(profile)
	if err != nil {
		return err
	}

	sort.Slice(stats, func(i, j int) bool { return stats[i].pkg < stats[j].pkg })

	fmt.Printf("%-58s %6s %6s\n", "package", "cover", "min")
	var failures []string
	for _, s := range stats {
		if s.total == 0 {
			continue
		}
		pct := 100 * float64(s.covered) / float64(s.total)
		min := coverageFloor(s.pkg)
		status := "ok"
		if pct+0.05 < min {
			status = "FAIL"
			failures = append(failures, fmt.Sprintf("%s: %.1f%% < %.0f%%", s.pkg, pct, min))
		}
		fmt.Printf("%-58s %5.1f%% %5.0f%% %s\n", s.pkg, pct, min, status)
	}

	if total > 0 {
		overall := 100 * float64(covered) / float64(total)
		fmt.Printf("%-58s %5.1f%% %5.0f%%\n", "TOTAL (library)", overall, totalCoverageThreshold)
		if overall+0.05 < totalCoverageThreshold {
			failures = append(failures, fmt.Sprintf("total library coverage %.1f%% < %.0f%%", overall, totalCoverageThreshold))
		}
	}

	if err := writeCoverageSummary(stats, total, covered, failures); err != nil {
		return err
	}

	if len(failures) > 0 {
		return fmt.Errorf("coverage gates failed:\n  %s", strings.Join(failures, "\n  "))
	}
	return nil
}

func coverageFloor(pkg string) float64 {
	min := 70.0
	for suffix, m := range coverageThresholds {
		if strings.HasSuffix(pkg, suffix) {
			min = m
		}
	}
	return min
}

// writeCoverageSummary appends the same table to the GitHub Actions job summary
// when GITHUB_STEP_SUMMARY is set. It is a no op locally.
func writeCoverageSummary(stats []pkgCoverage, total, covered int, failures []string) error {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return nil
	}

	var b strings.Builder
	b.WriteString("## Coverage\n\n")
	b.WriteString("| Package | Coverage | Floor | Status |\n")
	b.WriteString("| --- | ---: | ---: | --- |\n")
	for _, s := range stats {
		if s.total == 0 {
			continue
		}
		pct := 100 * float64(s.covered) / float64(s.total)
		min := coverageFloor(s.pkg)
		status := "ok"
		if pct+0.05 < min {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "| `%s` | %.1f%% | %.0f%% | %s |\n", s.pkg, pct, min, status)
	}
	if total > 0 {
		overall := 100 * float64(covered) / float64(total)
		status := "ok"
		if overall+0.05 < totalCoverageThreshold {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "| **Total (library)** | **%.1f%%** | **%.0f%%** | **%s** |\n", overall, totalCoverageThreshold, status)
	}
	if len(failures) > 0 {
		b.WriteString("\nCoverage gates failed:\n\n")
		for _, failure := range failures {
			fmt.Fprintf(&b, "* %s\n", failure)
		}
	}

	// #nosec G304 G703 -- GITHUB_STEP_SUMMARY is a runner provided path.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open job summary: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(b.String()); err != nil {
		return fmt.Errorf("write job summary: %w", err)
	}
	return nil
}

func parseCoverage(path string) ([]pkgCoverage, int, int, error) {
	// #nosec G304 -- the path is either a temp file we created or ./coverage.out.
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("open coverage profile: %w", err)
	}
	defer func() { _ = f.Close() }()

	byPkg := make(map[string]*pkgCoverage)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		// file.go:start.col,end.col numStmt count
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			continue
		}
		file := line[:colon]
		fields := strings.Fields(line[colon+1:])
		if len(fields) != 3 {
			continue
		}
		stmts, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}

		pkg := filepath.ToSlash(filepath.Dir(file))
		entry, ok := byPkg[pkg]
		if !ok {
			entry = &pkgCoverage{pkg: pkg}
			byPkg[pkg] = entry
		}
		entry.total += stmts
		if count > 0 {
			entry.covered += stmts
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, 0, err
	}

	out := make([]pkgCoverage, 0, len(byPkg))
	var total, covered int
	for _, e := range byPkg {
		out = append(out, *e)
		// Entry points and the task runner are gated per package, but they would
		// distort a "library" total: main() is mostly process wiring.
		if strings.Contains(e.pkg, "/cmd/") || strings.HasSuffix(e.pkg, "/internal/devtool") {
			continue
		}
		total += e.total
		covered += e.covered
	}
	return out, total, covered, nil
}

// --- build -----------------------------------------------------------------

func buildMetadata() (version, commit, date string) {
	version, _ = capture("git", "describe", "--tags", "--always", "--dirty")
	if version == "" {
		version = "dev"
	}
	commit, _ = capture("git", "rev-parse", "--short", "HEAD")
	if commit == "" {
		commit = "none"
	}
	return version, commit, commitDate()
}

// commitDate prefers the commit timestamp so two builds of the same revision
// produce byte-identical binaries. SOURCE_DATE_EPOCH is honoured for builds
// from an export with no git history; wall-clock time is the last resort and is
// the only case that is not reproducible.
func commitDate() string {
	if out, err := capture("git", "show", "-s", "--format=%cI", "HEAD"); err == nil && out != "" {
		return out
	}
	if epoch := os.Getenv("SOURCE_DATE_EPOCH"); epoch != "" {
		if sec, err := strconv.ParseInt(epoch, 10, 64); err == nil {
			return time.Unix(sec, 0).UTC().Format(time.RFC3339)
		}
	}
	return time.Now().UTC().Format(time.RFC3339)
}

func ldflags(version, commit, date string) string {
	return strings.Join([]string{
		"-s", "-w",
		"-X", versionPkg + ".Version=" + version,
		"-X", versionPkg + ".Commit=" + commit,
		"-X", versionPkg + ".Date=" + date,
	}, " ")
}

func taskBuild() error {
	version, commit, date := buildMetadata()
	if err := os.MkdirAll(distDir, 0o750); err != nil {
		return err
	}
	out := filepath.Join(distDir, "netweather")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	return runGo("build",
		"-trimpath",
		"-ldflags", ldflags(version, commit, date),
		"-o", out,
		"./cmd/netweather",
	)
}

type target struct{ goos, goarch string }

// taskRepro builds the same source twice and compares the digests. If this fails,
// a release binary cannot be verified by rebuilding it, which is the whole point
// of publishing checksums.
// taskUIBrowsers runs the browser suite once per engine. The harness picks the
// engine from NETWEATHER_UI_BROWSER, so a failure names the browser instead of
// leaving the operator to guess which one broke.
func taskUIBrowsers() error {
	goBin, err := goBinary()
	if err != nil {
		return err
	}
	for _, name := range []string{"chromium", "firefox", "webkit"} {
		fmt.Printf("==> ui %s\n", name)
		cmd := command(goBin, "test", "-tags=uitest", "-count=1", "./internal/web/uitest")
		cmd.Env = append(os.Environ(), "NETWEATHER_UI_BROWSER="+name)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ui %s: %w", name, err)
		}
	}
	return nil
}

func taskRepro() error {
	version, commit, date := buildMetadata()

	dir, err := os.MkdirTemp("", "netweather-repro")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	digests := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		out := filepath.Join(dir, "build"+strconv.Itoa(i))
		if err := runGo("build",
			"-trimpath",
			"-ldflags", ldflags(version, commit, date),
			"-o", out,
			"./cmd/netweather",
		); err != nil {
			return err
		}
		sum, err := fileSHA256(out)
		if err != nil {
			return err
		}
		digests = append(digests, sum)
	}

	if digests[0] != digests[1] {
		return fmt.Errorf("builds are not reproducible:\n  %s\n  %s", digests[0], digests[1])
	}
	fmt.Printf("reproducible: %s (version %s, date %s)\n", digests[0][:16], version, date)
	return nil
}

func taskSnapshot() error {
	version, commit, date := buildMetadata()
	if err := os.MkdirAll(distDir, 0o750); err != nil {
		return err
	}

	targets := []target{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "amd64"},
		{"darwin", "arm64"},
		{"windows", "amd64"},
	}

	sums := make([]string, 0, len(targets))
	for _, t := range targets {
		name := fmt.Sprintf("netweather-%s-%s", t.goos, t.goarch)
		if t.goos == "windows" {
			name += ".exe"
		}
		out := filepath.Join(distDir, name)

		goBin, err := goBinary()
		if err != nil {
			return err
		}
		cmd := command(goBin, "build",
			"-trimpath",
			"-ldflags", ldflags(version, commit, date),
			"-o", out,
			"./cmd/netweather",
		)
		cmd.Env = append(os.Environ(), "GOOS="+t.goos, "GOARCH="+t.goarch, "CGO_ENABLED=0")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build %s/%s: %w", t.goos, t.goarch, err)
		}

		sum, err := fileSHA256(out)
		if err != nil {
			return err
		}
		sums = append(sums, fmt.Sprintf("%s  %s", sum, name))
		fmt.Printf("%-32s %s\n", name, sum[:16])
	}

	sumPath := filepath.Join(distDir, "SHA256SUMS")
	if err := os.WriteFile(sumPath, []byte(strings.Join(sums, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Println("wrote", sumPath, "for version", version)
	return nil
}

func fileSHA256(path string) (string, error) {
	// #nosec G304 -- path is a build output this process just wrote.
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func taskVerify() error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{"fmt -check", func() error { return taskFmt([]string{"-check"}) }},
		{"modules", func() error { return runGo("mod", "verify") }},
		{"tidy", func() error { return runGo("mod", "tidy", "-diff") }},
		{"vet", func() error { return runGo("vet", "./...") }},
		{"lint", func() error { return runTool("golangci-lint", "run") }},
		{"test", taskTest},
		{"cover", taskCover},
		{"build", taskBuild},
	}
	for _, s := range steps {
		fmt.Println("==>", s.name)
		if err := s.fn(); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	fmt.Println("verify: ok")
	return nil
}
