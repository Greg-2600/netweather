package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDeployDryRunPrintsThePipeline pins the contract a scheduled run depends on:
// the script must print what it would do and touch nothing, so a cron job can
// be exercised without Docker and without risking the running container.
func TestDeployDryRunPrintsThePipeline(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "scripts", "deploy.ps1")

	cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass",
		"-File", script, "-DryRun")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out.String())
	}

	got := out.String()
	for _, want := range []string{"DRY: docker build", "DRY: docker run", "DRY: docker volume create"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry run is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "deployed: http://") {
		// A dry run must never claim success; it only prints intent.
		t.Errorf("dry run claimed success:\n%s", got)
	}
}

// TestDeployScriptIsPresentAndExecutable covers the POSIX entry point on hosts
// that have a shell. It is skipped where bash is unavailable.
func TestDeployScriptIsPresentAndExecutable(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "scripts", "deploy.sh")

	// Git Bash cannot resolve a Windows drive path; the script is exercised on
	// Linux in CI, so skip the POSIX entry point on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("deploy.sh is exercised on Linux in CI")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available; deploy.sh is exercised in CI")
	}
	cmd := exec.Command(bash, script, "--dry-run")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "DRY: docker build") {
		t.Errorf("dry run did not print the build step:\n%s", out.String())
	}
}
