package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestMain doubles as a tiny, scriptable stand-in for the external tools the
// task runner shells out to. A test copies the test binary into a temp dir as
// "go" and "docker", prepends that dir to PATH, and then the child process
// lands here instead of running the test suite. Nothing leaves the machine.
func TestMain(m *testing.M) {
	if os.Getenv("NETWEATHER_FAKE_TOOL") == "1" {
		os.Exit(fakeToolMain(os.Args))
	}
	os.Exit(m.Run())
}

type fakeCall struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Browser string   `json:"browser,omitempty"`
	GOOS    string   `json:"goos,omitempty"`
	GOARCH  string   `json:"goarch,omitempty"`
	CGO     string   `json:"cgo,omitempty"`
}

func fakeToolMain(argv []string) int {
	logPath := os.Getenv("NETWEATHER_FAKE_LOG")
	var previous []fakeCall
	if logPath != "" {
		if raw, err := os.ReadFile(logPath); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				if line == "" {
					continue
				}
				var call fakeCall
				if json.Unmarshal([]byte(line), &call) == nil {
					previous = append(previous, call)
				}
			}
		}
	}

	call := fakeCall{
		Command: strings.TrimSuffix(filepath.Base(argv[0]), ".exe"),
		Args:    argv[1:],
		Browser: os.Getenv("NETWEATHER_UI_BROWSER"),
		GOOS:    os.Getenv("GOOS"),
		GOARCH:  os.Getenv("GOARCH"),
		CGO:     os.Getenv("CGO_ENABLED"),
	}
	if logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_ = json.NewEncoder(f).Encode(call)
			_ = f.Close()
		}
	}

	current := len(previous) + 1
	if want, _ := strconv.Atoi(os.Getenv("NETWEATHER_FAKE_FAIL_AT")); want > 0 && current == want {
		_, _ = fmt.Fprintln(os.Stderr, "fake tool: forced failure")
		return 1
	}
	if match := os.Getenv("NETWEATHER_FAKE_FAIL_MATCH"); match != "" && strings.Contains(strings.Join(call.Args, " "), match) {
		_, _ = fmt.Fprintln(os.Stderr, "fake tool: forced failure")
		return 1
	}
	if os.Getenv("NETWEATHER_FAKE_FAIL") == "1" {
		_, _ = fmt.Fprintln(os.Stderr, "fake tool: forced failure")
		return 1
	}

	if len(call.Args) >= 2 && call.Args[0] == "env" {
		key := call.Args[1]
		value := os.Getenv("NETWEATHER_FAKE_ENV_" + key)
		if value == "" && key == "CGO_ENABLED" {
			value = "0"
		}
		if value != "" {
			_, _ = fmt.Fprintln(os.Stdout, value)
		}
		return 0
	}
	if len(call.Args) >= 1 && call.Args[0] == "list" {
		value := os.Getenv("NETWEATHER_FAKE_LIST")
		if value == "" {
			value = modulePath + "/internal/api\n" + modulePath + "/internal/web"
		}
		_, _ = fmt.Fprintln(os.Stdout, value)
		return 0
	}
	if len(call.Args) >= 2 && call.Args[0] == "tool" {
		switch call.Args[1] {
		case "gofumpt":
			if value := os.Getenv("NETWEATHER_FAKE_GOFMT"); value != "" {
				_, _ = fmt.Fprintln(os.Stdout, value)
			}
			return 0
		case "gci":
			if value := os.Getenv("NETWEATHER_FAKE_GCI"); value != "" {
				_, _ = fmt.Fprintln(os.Stdout, value)
			}
			return 0
		}
	}

	for _, arg := range call.Args {
		if !strings.HasPrefix(arg, "-coverprofile=") {
			continue
		}
		if os.Getenv("NETWEATHER_FAKE_NO_PROFILE") == "1" {
			return 0
		}
		content := os.Getenv("NETWEATHER_FAKE_PROFILE")
		if content == "" {
			content = "mode: atomic\n" + modulePath + "/internal/api/a.go:1.1,2.2 10 10\n"
		}
		if err := os.WriteFile(strings.TrimPrefix(arg, "-coverprofile="), []byte(content), 0o600); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	for i, arg := range call.Args {
		if arg != "-o" || i+1 >= len(call.Args) {
			continue
		}
		if os.Getenv("NETWEATHER_FAKE_NO_OUTPUT") == "1" {
			continue
		}
		content := "fake artifact\n"
		if os.Getenv("NETWEATHER_FAKE_OUTPUT_SEQ") == "1" {
			content = fmt.Sprintf("fake artifact %d\n", current)
		}
		if err := os.WriteFile(call.Args[i+1], []byte(content), 0o600); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}

func installFakeTools(t *testing.T) string {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	dir := t.TempDir()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	for _, name := range []string{"go", "docker"} {
		if err := os.WriteFile(filepath.Join(dir, name+suffix), raw, 0o755); err != nil {
			t.Fatalf("install fake %s: %v", name, err)
		}
	}

	logPath := filepath.Join(dir, "calls.jsonl")
	t.Setenv("NETWEATHER_FAKE_TOOL", "1")
	t.Setenv("NETWEATHER_FAKE_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	return logPath
}

func readFakeCalls(t *testing.T, logPath string) []fakeCall {
	t.Helper()

	raw, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read fake calls: %v", err)
	}
	var calls []fakeCall
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var call fakeCall
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatalf("parse fake call %q: %v", line, err)
		}
		calls = append(calls, call)
	}
	return calls
}

func fakeCommand(calls []fakeCall, command string, contains ...string) bool {
	for _, call := range calls {
		if call.Command != command {
			continue
		}
		joined := strings.Join(call.Args, " ")
		matched := true
		for _, want := range contains {
			if !strings.Contains(joined, want) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func TestTaskFmtWithFakeToolchain(t *testing.T) {
	logPath := installFakeTools(t)

	if err := taskFmt(nil); err != nil {
		t.Fatalf("taskFmt write: %v", err)
	}
	calls := readFakeCalls(t, logPath)
	if !fakeCommand(calls, "go", "tool gofumpt -w .") {
		t.Errorf("write did not run gofumpt: %+v", calls)
	}
	if !fakeCommand(calls, "go", "tool gci write") {
		t.Errorf("write did not run gci: %+v", calls)
	}

	t.Setenv("NETWEATHER_FAKE_GOFMT", "main.go")
	if err := taskFmt([]string{"-check"}); err == nil || !strings.Contains(err.Error(), "gofumpt would reformat") {
		t.Fatalf("taskFmt -check with unformatted file = %v", err)
	}

	t.Setenv("NETWEATHER_FAKE_GOFMT", "")
	t.Setenv("NETWEATHER_FAKE_GCI", "diff --git a/main.go b/main.go")
	if err := taskFmt([]string{"--check"}); err == nil || !strings.Contains(err.Error(), "import order differs") {
		t.Fatalf("taskFmt --check with import diff = %v", err)
	}

	t.Setenv("NETWEATHER_FAKE_GCI", "")
	t.Setenv("NETWEATHER_FAKE_FAIL_MATCH", "gofumpt")
	if err := taskFmt(nil); err == nil {
		t.Fatal("taskFmt should surface a gofumpt failure")
	}
}

func TestTaskLintAllWithFakeToolchain(t *testing.T) {
	logPath := installFakeTools(t)

	dir := t.TempDir()
	for _, name := range []string{"Dockerfile", "Dockerfile.tor"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("FROM scratch\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)

	if err := taskLintAll(); err != nil {
		t.Fatalf("taskLintAll: %v", err)
	}
	calls := readFakeCalls(t, logPath)
	if !fakeCommand(calls, "go", "tool golangci-lint run") {
		t.Errorf("lint-all did not run golangci-lint: %+v", calls)
	}
	if !fakeCommand(calls, "go", "run github.com/zricethezav/gitleaks") {
		t.Errorf("lint-all did not run gitleaks: %+v", calls)
	}
	if got := countFakeCommand(calls, "docker", "run --rm -i hadolint/hadolint"); got != 2 {
		t.Errorf("hadolint runs = %d, want 2", got)
	}

	t.Setenv("NETWEATHER_FAKE_FAIL_MATCH", "hadolint")
	if err := taskLintAll(); err == nil || !strings.Contains(err.Error(), "hadolint Dockerfile") {
		t.Fatalf("taskLintAll with failing hadolint = %v", err)
	}

	t.Setenv("NETWEATHER_FAKE_FAIL_MATCH", "")
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	if err := os.Remove(filepath.Join(filepath.Dir(logPath), "docker"+suffix)); err != nil {
		t.Fatal(err)
	}
	if err := taskLintAll(); err == nil || !strings.Contains(err.Error(), "docker is required") {
		t.Fatalf("taskLintAll without docker = %v", err)
	}
}

func countFakeCommand(calls []fakeCall, command string, contains string) int {
	count := 0
	for _, call := range calls {
		if call.Command == command && strings.Contains(strings.Join(call.Args, " "), contains) {
			count++
		}
	}
	return count
}

func TestTaskTestWithFakeToolchain(t *testing.T) {
	logPath := installFakeTools(t)

	t.Setenv("NETWEATHER_FAKE_ENV_CGO_ENABLED", "0")
	if err := taskTest(); err != nil {
		t.Fatalf("taskTest without race: %v", err)
	}
	if calls := readFakeCalls(t, logPath); !fakeCommand(calls, "go", "test -count=1 -shuffle=on ./...") {
		t.Errorf("non-race test command missing: %+v", calls)
	}

	t.Setenv("NETWEATHER_FAKE_ENV_CGO_ENABLED", "1")
	if err := taskTest(); err != nil {
		t.Fatalf("taskTest with race: %v", err)
	}
	calls := readFakeCalls(t, logPath)
	if !fakeCommand(calls, "go", "test", "-race", modulePath+"/internal/api") {
		t.Errorf("race test command missing: %+v", calls)
	}
	if !fakeCommand(calls, "go", "test", "-shuffle=on", "./internal/web") {
		t.Errorf("separate web test command missing: %+v", calls)
	}
	if fakeCommand(calls, "go", "test", "-race", modulePath+"/internal/web") {
		t.Errorf("web should not run under the race pass: %+v", calls)
	}

	t.Setenv("NETWEATHER_FAKE_FAIL_MATCH", "-shuffle")
	if err := taskTest(); err == nil {
		t.Fatal("taskTest should surface a failing test command")
	}
}

func TestTaskCoverWithFakeToolchain(t *testing.T) {
	logPath := installFakeTools(t)
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "summary.md"))

	if err := taskCover(); err != nil {
		t.Fatalf("taskCover: %v", err)
	}
	if calls := readFakeCalls(t, logPath); !fakeCommand(calls, "go", "-covermode=atomic", "-coverprofile=") {
		t.Errorf("coverage test command missing: %+v", calls)
	}
	summary, err := os.ReadFile(os.Getenv("GITHUB_STEP_SUMMARY"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summary), "## Coverage") {
		t.Errorf("summary missing heading:\n%s", summary)
	}

	t.Setenv("NETWEATHER_FAKE_PROFILE", "mode: atomic\n"+modulePath+"/internal/store/a.go:1.1,2.2 10 0\n")
	if err := taskCover(); err == nil || !strings.Contains(err.Error(), "coverage gates failed") {
		t.Fatalf("taskCover with a low-coverage profile = %v", err)
	}

	t.Setenv("NETWEATHER_FAKE_PROFILE", "")
	t.Setenv("NETWEATHER_FAKE_FAIL_MATCH", "-coverprofile")
	if err := taskCover(); err == nil || !strings.Contains(err.Error(), "tests failed; coverage is meaningless") {
		t.Fatalf("taskCover with failing tests = %v", err)
	}

	t.Setenv("NETWEATHER_FAKE_FAIL_MATCH", "")
	t.Setenv("NETWEATHER_FAKE_NO_PROFILE", "1")
	if err := taskCover(); err == nil || !strings.Contains(err.Error(), "open coverage profile") {
		t.Fatalf("taskCover without a profile = %v", err)
	}
}

func TestTaskBuildWithFakeToolchain(t *testing.T) {
	logPath := installFakeTools(t)
	t.Chdir(t.TempDir())

	if err := taskBuild(); err != nil {
		t.Fatalf("taskBuild: %v", err)
	}
	out := filepath.Join(distDir, "netweather")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("build output: %v", err)
	}
	if calls := readFakeCalls(t, logPath); !fakeCommand(calls, "go", "build", "-trimpath", "-ldflags") {
		t.Errorf("build command missing flags: %+v", calls)
	}

	t.Setenv("NETWEATHER_FAKE_FAIL_MATCH", "-trimpath")
	if err := taskBuild(); err == nil {
		t.Fatal("taskBuild should surface a failing build")
	}
}

func TestTaskReproWithFakeToolchain(t *testing.T) {
	logPath := installFakeTools(t)
	t.Chdir(t.TempDir())

	if err := taskRepro(); err != nil {
		t.Fatalf("taskRepro: %v", err)
	}
	if got := countFakeCommand(readFakeCalls(t, logPath), "go", "build"); got != 2 {
		t.Fatalf("repro builds = %d, want 2", got)
	}

	t.Setenv("NETWEATHER_FAKE_OUTPUT_SEQ", "1")
	if err := taskRepro(); err == nil || !strings.Contains(err.Error(), "not reproducible") {
		t.Fatalf("taskRepro with differing artifacts = %v", err)
	}

	t.Setenv("NETWEATHER_FAKE_OUTPUT_SEQ", "")
	t.Setenv("NETWEATHER_FAKE_FAIL_AT", strconv.Itoa(len(readFakeCalls(t, logPath))+1))
	if err := taskRepro(); err == nil {
		t.Fatal("taskRepro should surface a failing build")
	}
}

func TestTaskSnapshotWithFakeToolchain(t *testing.T) {
	logPath := installFakeTools(t)
	t.Chdir(t.TempDir())

	if err := taskSnapshot(); err != nil {
		t.Fatalf("taskSnapshot: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(distDir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(raw)), "\n"); len(lines) != 5 {
		t.Fatalf("SHA256SUMS lines = %d, want 5:\n%s", len(lines), raw)
	}
	calls := readFakeCalls(t, logPath)
	if got := countFakeCommand(calls, "go", "build"); got != 5 {
		t.Fatalf("snapshot builds = %d, want 5", got)
	}
	if !fakeCommand(calls, "go", "build", "GOOS=windows") {
		// GOOS is carried in the environment, not argv; check it on the calls.
		found := false
		for _, call := range calls {
			if call.Command == "go" && call.GOOS == "windows" && call.GOARCH == "amd64" && call.CGO == "0" {
				found = true
			}
		}
		if !found {
			t.Errorf("snapshot did not cross-build windows/amd64: %+v", calls)
		}
	}

	t.Setenv("NETWEATHER_FAKE_FAIL_AT", strconv.Itoa(len(calls)+3))
	if err := taskSnapshot(); err == nil {
		t.Fatal("taskSnapshot should surface a failing cross-build")
	}
}

func TestTaskVerifyWithFakeToolchain(t *testing.T) {
	logPath := installFakeTools(t)
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_STEP_SUMMARY", "")

	if err := taskVerify(); err != nil {
		t.Fatalf("taskVerify: %v", err)
	}
	calls := readFakeCalls(t, logPath)
	for _, want := range []string{
		"mod verify",
		"mod tidy -diff",
		"vet ./...",
		"tool golangci-lint run",
		"test -count=1 -shuffle=on ./...",
		"build",
	} {
		if !fakeCommand(calls, "go", want) {
			t.Errorf("verify did not run %q: %+v", want, calls)
		}
	}

	t.Setenv("NETWEATHER_FAKE_FAIL_MATCH", "vet ./...")
	if err := taskVerify(); err == nil || !strings.Contains(err.Error(), "vet:") {
		t.Fatalf("taskVerify with failing vet = %v", err)
	}
}

func TestTaskUIBrowsersWithFakeToolchain(t *testing.T) {
	logPath := installFakeTools(t)

	if err := taskUIBrowsers(); err != nil {
		t.Fatalf("taskUIBrowsers: %v", err)
	}
	calls := readFakeCalls(t, logPath)
	var browsers []string
	for _, call := range calls {
		if call.Command == "go" && strings.Contains(strings.Join(call.Args, " "), "-tags=uitest") {
			browsers = append(browsers, call.Browser)
		}
	}
	if got := strings.Join(browsers, ","); got != "chromium,firefox,webkit" {
		t.Fatalf("browsers = %q, want chromium,firefox,webkit", got)
	}

	t.Setenv("NETWEATHER_FAKE_FAIL_AT", strconv.Itoa(len(calls)+2))
	if err := taskUIBrowsers(); err == nil || !strings.Contains(err.Error(), "ui firefox") {
		t.Fatalf("taskUIBrowsers with failing firefox = %v", err)
	}
}
