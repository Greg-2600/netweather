//go:build compose

// The compose network-plane suite (QUALITY.md §5.5). It runs the real image in
// real Docker networks and asserts only the things a container can break.
//
// It is build-tagged and separate from every other suite because it needs a
// Docker daemon, pulls images, and takes minutes. `go test ./...` never touches
// it; CI runs it in its own job.
package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	composeImage    = "netweather:compose-test"
	composeToken    = "compose-token-0123456789"
	composeNet      = "nw-compose-net"
	composeInternal = "nw-compose-internal"
	composeApp      = "nw-compose-app"
	composeAppDNS   = "nw-compose-app-internal"
	composeTor      = "nw-compose-tor"
	composeVolume   = "nw-compose-data"
)

func requireLinuxDocker(t *testing.T) {
	t.Helper()

	out, err := exec.Command("docker", "info", "--format", "{{.OSType}}").CombinedOutput()
	if err != nil {
		t.Skipf("docker is not available: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if got := strings.TrimSpace(string(out)); got != "linux" {
		t.Skipf("docker is running %q containers, not linux", got)
	}
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func dockerMaybe(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func composeRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// composeConfig writes one target file for a scenario and returns its path.
//
// The directory is created under the repository root, not t.TempDir(): a
// subtest name can contain a comma, t.TempDir embeds the test name in the path,
// and `docker --mount` treats a comma in the source as a field separator. The
// repo path is space-safe and comma-free, and the guard below keeps a future
// checkout with a comma in its path from producing a baffling mount error.
func composeConfig(t *testing.T, body string) string {
	t.Helper()

	dir, err := os.MkdirTemp(composeRepoRoot(t), ".compose-config-")
	if err != nil {
		t.Fatalf("temp config dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if strings.Contains(dir, ",") {
		t.Fatalf("config path %q contains a comma, which docker --mount cannot carry", dir)
	}

	path := filepath.Join(dir, "targets.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func composeCleanup(t *testing.T) {
	t.Helper()
	for _, name := range []string{composeApp, composeAppDNS, composeTor, "nw-agent-primary"} {
		_, _ = dockerMaybe(t, "rm", "-f", name)
	}
	_, _ = dockerMaybe(t, "volume", "rm", "-f", composeVolume)
	_, _ = dockerMaybe(t, "network", "rm", composeNet)
	_, _ = dockerMaybe(t, "network", "rm", composeInternal)
}

func composeUp(t *testing.T) {
	t.Helper()

	composeCleanup(t)
	docker(t, "build", "-t", composeImage, composeRepoRoot(t))
	docker(t, "network", "create",
		"--opt", "com.docker.network.driver.mtu=1280", composeNet)
	docker(t, "network", "create", "--internal", composeInternal)
}

func composeWaitHealth(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s/healthz did not answer", base)
}

func composeAPI(t *testing.T, base, path string) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+composeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %s: %s", path, resp.Status, body)
	}
	return body
}

type composeTarget struct {
	Name  string `json:"name"`
	Host  string `json:"host"`
	State string `json:"state"`
}

func composeTargets(t *testing.T, base string) map[string]composeTarget {
	t.Helper()
	var env struct {
		Data []composeTarget `json:"data"`
	}
	if err := json.Unmarshal(composeAPI(t, base, "/api/v1/targets"), &env); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]composeTarget, len(env.Data))
	for _, target := range env.Data {
		out[target.Name] = target
	}
	return out
}

type composeSelfCheck struct {
	Check  string `json:"check"`
	OK     bool   `json:"ok"`
	Value  string `json:"value"`
	Detail string `json:"detail"`
}

func composeSelfChecks(t *testing.T, base string) map[string]composeSelfCheck {
	t.Helper()
	var env struct {
		Data []composeSelfCheck `json:"data"`
	}
	if err := json.Unmarshal(composeAPI(t, base, "/api/v1/selfcheck"), &env); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]composeSelfCheck, len(env.Data))
	for _, row := range env.Data {
		out[row.Check] = row
	}
	return out
}

// composeWaitSelfChecks waits for the named checks to appear. The maintenance
// loop runs the self-checks asynchronously after the listener is up, and one of
// them makes an external request, so /healthz answering does not mean the
// self-check table is populated yet.
func composeWaitSelfChecks(t *testing.T, base string, want ...string) map[string]composeSelfCheck {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last map[string]composeSelfCheck
	for time.Now().Before(deadline) {
		last = composeSelfChecks(t, base)
		matched := true
		for _, name := range want {
			if _, ok := last[name]; !ok {
				matched = false
				break
			}
		}
		if matched {
			return last
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("self-checks %v did not appear: %+v", want, last)
	return last
}

// composeWaitTargets waits for every named target to reach its wanted state.
func composeWaitTargets(t *testing.T, base string, want map[string]string) {
	t.Helper()
	deadline := time.Now().Add(100 * time.Second)
	var last map[string]composeTarget
	for time.Now().Before(deadline) {
		last = composeTargets(t, base)
		matched := true
		for name, state := range want {
			if last[name].State != state {
				matched = false
				break
			}
		}
		if matched {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("targets never reached %v; last = %+v", want, last)
}

// composeRunApp starts the release image against the shared data volume. An
// empty hostPort means no published port: Docker Desktop does not publish ports
// from an `--internal` network, so those scenarios are queried from a container
// on the same network instead (composeInNetwork).
func composeRunApp(t *testing.T, name, network, hostPort, config string) string {
	t.Helper()
	args := []string{"run", "-d", "--name", name, "--network", network}
	if hostPort != "" {
		args = append(args, "-p", "127.0.0.1:"+hostPort+":2600")
	}
	args = append(args,
		"--read-only", "--tmpfs", "/tmp",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"-e", "NETWEATHER_UI_TOKEN="+composeToken,
		"-e", "NETWEATHER_SEED=none",
		"-e", "NETWEATHER_CONFIG=/config/targets.yaml",
		"--mount", "type=bind,source="+config+",target=/config/targets.yaml,readonly",
		"--mount", "type=volume,source="+composeVolume+",target=/data",
		composeImage)
	docker(t, args...)
	if hostPort == "" {
		return ""
	}
	return "http://127.0.0.1:" + hostPort
}

// composeInNetwork runs one alpine helper on network and returns its output.
// It is how the internal-network scenarios are observed: the image has no
// shell, and Docker Desktop does not publish ports from an internal network.
func composeInNetwork(t *testing.T, network string, argv ...string) (string, error) {
	t.Helper()
	args := append([]string{"run", "--rm", "--network", network, "alpine:3.20"}, argv...)
	return dockerMaybe(t, args...)
}

// composeGetInNetwork fetches an app path from a container on the same network.
func composeGetInNetwork(t *testing.T, network, host, path string) []byte {
	t.Helper()
	out, err := composeInNetwork(t, network,
		"wget", "-qO-", "--timeout=10",
		"--header=Authorization: Bearer "+composeToken,
		"http://"+host+":2600"+path)
	if err != nil {
		t.Fatalf("GET %s from %s: %v\n%s", path, host, err, out)
	}
	return []byte(out)
}

// composeWaitHealthInNetwork waits for the named container to answer /healthz
// from inside its own network.
func composeWaitHealthInNetwork(t *testing.T, network, host string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if out, err := composeInNetwork(t, network,
			"wget", "-qO-", "--timeout=3", "http://"+host+":2600/healthz"); err == nil && out != "" {
			return
		}
		time.Sleep(time.Second)
	}
	logs, _ := dockerMaybe(t, "logs", "--tail", "30", host)
	t.Fatalf("%s/healthz did not answer from inside %s\n%s", host, network, logs)
}

// composeTargetsInNetwork is composeTargets over the in-network helper.
func composeTargetsInNetwork(t *testing.T, network, host string) map[string]composeTarget {
	t.Helper()
	var env struct {
		Data []composeTarget `json:"data"`
	}
	if err := json.Unmarshal(composeGetInNetwork(t, network, host, "/api/v1/targets"), &env); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]composeTarget, len(env.Data))
	for _, target := range env.Data {
		out[target.Name] = target
	}
	return out
}

// composeSelfChecksInNetwork is composeSelfChecks over the in-network helper.
func composeSelfChecksInNetwork(t *testing.T, network, host string) map[string]composeSelfCheck {
	t.Helper()
	var env struct {
		Data []composeSelfCheck `json:"data"`
	}
	if err := json.Unmarshal(composeGetInNetwork(t, network, host, "/api/v1/selfcheck"), &env); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]composeSelfCheck, len(env.Data))
	for _, row := range env.Data {
		out[row.Check] = row
	}
	return out
}

// composeWaitTargetsInNetwork is composeWaitTargets over the in-network helper.
func composeWaitTargetsInNetwork(t *testing.T, network, host string, want map[string]string) {
	t.Helper()
	deadline := time.Now().Add(100 * time.Second)
	var last map[string]composeTarget
	for time.Now().Before(deadline) {
		last = composeTargetsInNetwork(t, network, host)
		matched := true
		for name, state := range want {
			if last[name].State != state {
				matched = false
				break
			}
		}
		if matched {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("targets never reached %v; last = %+v", want, last)
}

func TestComposeNetworkPlane(t *testing.T) {
	requireLinuxDocker(t)
	composeUp(t)
	t.Cleanup(func() { composeCleanup(t) })

	// The Tor sidecar is a TCP listener with a dotted alias. It is a stand-in
	// for the SOCKS proxy: the point is the network topology, not Tor itself.
	docker(t, "run", "-d", "--name", composeTor,
		"--network", composeNet, "--network-alias", "tor.nw",
		"alpine:3.20", "sh", "-c",
		"while true; do nc -l -p 9050 -e /bin/true; done")

	t.Run("tor is reachable by service name, not on loopback", func(t *testing.T) {
		// The sidecar has no published port, which is the rule: 9050 must not
		// exist on the host or the app's loopback.
		if out, err := dockerMaybe(t, "port", composeTor, "9050"); err == nil && out != "" {
			t.Fatalf("9050 is published: %q", out)
		}

		probe := func(host, port string) error {
			// Host and port are separate argv entries: busybox nc reads them that
			// way, and passing "host port" as one string makes it look for a
			// file with a space in its name.
			_, err := dockerMaybe(t, "run", "--rm", "--network", composeNet,
				"alpine:3.20", "nc", "-z", "-w", "3", host, port)
			return err
		}

		// The sidecar's shell loop needs a moment to bind. Waiting is the
		// readiness check; retrying a connect is what an operator would do.
		deadline := time.Now().Add(20 * time.Second)
		var lastErr error
		for time.Now().Before(deadline) {
			if lastErr = probe("tor.nw", "9050"); lastErr == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if lastErr != nil {
			logs, _ := dockerMaybe(t, "logs", composeTor)
			t.Fatalf("tor.nw:9050 is not reachable: %v\n%s", lastErr, logs)
		}
		if err := probe("127.0.0.1", "9050"); err == nil {
			t.Fatal("127.0.0.1:9050 answered; the sidecar leaked onto loopback")
		}
	})

	appConfig := composeConfig(t, `version: 1
targets:
  - name: Direct resolver
    host: 1.1.1.1
    port: 53
    probe: dns
    qname: example.com
    class: network
    tier: 3
    interval_s: 30
  - name: Embedded DNS
    host: 127.0.0.11
    port: 53
    probe: dns
    qname: example.com
    class: network
    tier: 3
    interval_s: 30
  - name: Metadata
    host: 169.254.169.254
    port: 80
    probe: http
    class: network
    tier: 3
    interval_s: 30
  - name: Host gateway
    host: 172.17.0.1
    port: 80
    probe: http
    class: network
    tier: 3
    interval_s: 30
`)
	base := composeRunApp(t, composeApp, composeNet, "2610", appConfig)
	composeWaitHealth(t, base)

	t.Run("the image is the hardened release image", func(t *testing.T) {
		if got := docker(t, "inspect", "--format", "{{.Config.User}}", composeApp); got != "65532:65532" {
			t.Fatalf("user = %q", got)
		}
		if got := docker(t, "inspect", "--format", "{{.HostConfig.ReadonlyRootfs}}", composeApp); got != "true" {
			t.Fatalf("read-only rootfs = %q", got)
		}
		if _, err := dockerMaybe(t, "exec", composeApp, "sh", "-c", "true"); err == nil {
			t.Fatal("the release image has a shell")
		}
	})

	t.Run("a low MTU is reported, not hidden", func(t *testing.T) {
		checks := composeWaitSelfChecks(t, base, "mtu")
		mtu, ok := checks["mtu"]
		if !ok {
			t.Fatalf("no mtu self-check: %+v", checks)
		}
		if mtu.OK {
			t.Fatalf("a 1280-byte network passed the MTU check: %+v", mtu)
		}
		if !strings.Contains(mtu.Detail, "1280") {
			t.Fatalf("mtu detail = %q, want the measured MTU", mtu.Detail)
		}
		if port53, ok := checks["port53"]; ok && !port53.OK {
			t.Fatalf("port53 failed with working egress: %+v", port53)
		}
	})

	t.Run("dns probes the configured resolver and refuses private ones", func(t *testing.T) {
		// The container's own resolver points at Docker's embedded DNS, but the
		// DNS instrument queries the configured server directly. The direct
		// target succeeding while the embedded one is refused proves there is
		// no silent fallback to the host/Docker resolver.
		composeWaitTargets(t, base, map[string]string{
			"Direct resolver": "up_reachability",
			"Embedded DNS":    "unmeasurable",
			"Metadata":        "unmeasurable",
			"Host gateway":    "unmeasurable",
		})
	})

	t.Run("port 53 blocked is unmeasurable, not down", func(t *testing.T) {
		internalConfig := composeConfig(t, `version: 1
targets:
  - name: Blackholed DNS
    host: 1.1.1.1
    port: 53
    probe: dns
    qname: example.com
    class: network
    tier: 3
    interval_s: 30
`)
		// No published port: Docker Desktop does not forward host traffic into
		// an `--internal` network, and that network is exactly what makes :53
		// unreachable. The app is observed from a container beside it instead.
		composeRunApp(t, composeAppDNS, composeInternal, "", internalConfig)
		composeWaitHealthInNetwork(t, composeInternal, composeAppDNS)

		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			checks := composeSelfChecksInNetwork(t, composeInternal, composeAppDNS)
			if port53, ok := checks["port53"]; ok && !port53.OK {
				// The self-check has fired; the gate needs one probe pass with
				// it in place.
				composeWaitTargetsInNetwork(t, composeInternal, composeAppDNS,
					map[string]string{"Blackholed DNS": "unmeasurable"})
				return
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("the port53 self-check never failed on an internal network: %+v",
			composeSelfChecksInNetwork(t, composeInternal, composeAppDNS))
	})

	t.Run("backup produces a usable copy", func(t *testing.T) {
		// The documented restore path starts with a VACUUM INTO copy taken
		// while the service is live. This scenario proves that step works in
		// the real image, which has no shell and therefore no sqlite3 binary
		// to fall back on: the binary is the only tool available.
		dest := "/data/compose-backup.db"
		// Clear any stale copy from a previous run; the backup refuses to
		// overwrite, which is the documented contract.
		_, _ = dockerMaybe(t, "exec", composeApp, "rm", "-f", dest)
		out, err := dockerMaybe(t, "exec", composeApp, "/netweather", "backup", "-o", dest)
		if err != nil {
			t.Fatalf("backup: %v\n%s", err, out)
		}
		if !strings.Contains(out, "backup written to") {
			t.Fatalf("backup output = %q", out)
		}

		// The copy is a real SQLite database: open it from a helper container
		// that has the sqlite3 client, since the release image does not.
		helper := "nw-compose-backup-check"
		docker(t, "rm", "-f", helper)
		defer func() { _, _ = dockerMaybe(t, "rm", "-f", helper) }()
		args := []string{
			"run", "--rm", "--name", helper,
			"--network", composeNet,
			"--mount", "type=volume,source=" + composeVolume + ",target=/data",
			"alpine:3.20",
			"sh", "-c",
			"apk add --no-cache sqlite >/dev/null 2>&1; " +
				"test -s /data/compose-backup.db && " +
				"sqlite3 /data/compose-backup.db 'PRAGMA integrity_check;' | grep -qx ok",
		}
		if out, err := dockerMaybe(t, args...); err != nil {
			t.Fatalf("backup is not a usable database: %v\n%s", err, out)
		}

		// Clean up so the next run starts fresh.
		_, _ = dockerMaybe(t, "exec", composeApp, "rm", "-f", dest)
	})
	t.Run("remote agent reports a vantage", func(t *testing.T) {
		// The agent runs the same instruments on a second container and
		// reports each batch to the primary under a corroborating vantage.
		// This is the end-to-end proof that federation-lite works across
		// the container boundary, not just in a unit test.
		agentConfig := `
version: 1
targets:
  - id: example
    name: Example
    host: example.com
    port: 443
    probe: http
    class: service
    tier: 2
    interval_s: 300
`
		agentConfigFile := filepath.Join(t.TempDir(), "agent.yaml")
		if err := os.WriteFile(agentConfigFile, []byte(agentConfig), 0o600); err != nil {
			t.Fatal(err)
		}

		// Start the primary with the remote vantage enabled. composeRunApp
		// does not set NETWEATHER_REMOTE_VANTAGES, so run the container
		// directly with that env added.
		args := []string{
			"run", "-d", "--name", "nw-agent-primary", "--network", composeNet,
			"--read-only", "--tmpfs", "/tmp",
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
			"-e", "NETWEATHER_UI_TOKEN=" + composeToken,
			"-e", "NETWEATHER_SEED=none",
			"-e", "NETWEATHER_CONFIG=/config/targets.yaml",
			"-e", "NETWEATHER_REMOTE_VANTAGES=remote",
			"--mount", "type=bind,source=" + agentConfigFile + ",target=/config/targets.yaml,readonly",
			"--mount", "type=volume,source=" + composeVolume + ",target=/data",
			composeImage,
		}
		docker(t, args...)
		composeWaitHealthInNetwork(t, composeNet, "nw-agent-primary")

		// Run the agent once against the primary.
		docker(t, "run", "--rm", "--network", composeNet,
			"--mount", "type=bind,source="+agentConfigFile+",target=/config/targets.yaml,readonly",
			composeImage,
			"agent", "--server", "http://nw-agent-primary:2600",
			"--vantage", "remote", "--token", composeToken,
			"--config", "/config/targets.yaml", "--once")

		// Give the primary a moment to record the batch.
		time.Sleep(2 * time.Second)

		// The remote vantage should now appear for the example target.
		body := composeGetInNetwork(t, composeNet, "nw-agent-primary",
			"/api/v1/targets/example/vantages")
		if !strings.Contains(string(body), `"remote"`) {
			t.Fatalf("remote vantage did not report: %s", string(body))
		}
	})
}
