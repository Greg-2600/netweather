package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readBundle unpacks a bundle into memory: the test asserts what an operator
// would actually receive, not what the writer meant to send.
func readBundle(t *testing.T, path string) map[string]string {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()

	out := make(map[string]string)
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[header.Name] = string(body)
	}
	return out
}

func bundleServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bundle-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/v1/meta":
			_, _ = io.WriteString(w, `{"data":{"version":"test","schema_version":19},"meta":{}}`)
		case r.URL.Path == "/api/v1/selfcheck":
			_, _ = io.WriteString(w, `{"data":[{"check":"clock","ok":true}],"meta":{}}`)
		case r.URL.Path == "/api/v1/logs":
			_, _ = io.WriteString(w, `{"data":[`+
				`{"ts":"2026-11-17T12:00:00Z","level":"WARN","message":"first line"},`+
				`{"ts":"2026-11-17T12:00:01Z","level":"INFO","message":"second line"}],"meta":{}}`)
		case r.URL.Path == "/api/v1/targets":
			if r.URL.Query().Get("state") == "down" {
				_, _ = io.WriteString(w, `{"data":[{"id":"1","host":"chase.com"}],"meta":{}}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":[],"meta":{}}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/targets/1/probes"):
			_, _ = io.WriteString(w, `{"data":[`+
				`{"ts":"2026-11-17T12:00:00Z","state":"down","error_class":"tcp_refused"}],"meta":{}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDebugBundleCollectsAndRedacts(t *testing.T) {
	srv := bundleServer(t)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "targets.yaml")
	config := "targets:\n  - name: Chase\n    token: supersecret\n    host: chase.com\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("NETWEATHER_API_URL", srv.URL)
	t.Setenv("NETWEATHER_UI_TOKEN", "bundle-token")
	t.Setenv("NETWEATHER_CONFIG", configPath)

	output := filepath.Join(dir, "bundle.tar.gz")
	var stdout bytes.Buffer
	if err := debugBundle([]string{"-o", output}, &stdout); err != nil {
		t.Fatalf("debugBundle: %v", err)
	}

	files := readBundle(t, output)
	for _, name := range []string{
		"version.txt", "config.redacted.yaml", "meta.json", "selfcheck.json",
		"ring.jsonl", "probes.jsonl", "MANIFEST.txt",
	} {
		if _, ok := files[name]; !ok {
			t.Errorf("bundle is missing %s", name)
		}
	}

	// The credential never leaves the operator's machine, even if a config
	// grows one by mistake.
	if strings.Contains(files["config.redacted.yaml"], "supersecret") {
		t.Fatal("the bundle contains the config token")
	}
	if !strings.Contains(files["config.redacted.yaml"], "[redacted]") {
		t.Fatalf("config was not redacted:\n%s", files["config.redacted.yaml"])
	}
	if !strings.Contains(files["config.redacted.yaml"], "host: chase.com") {
		t.Error("redaction removed the useful config")
	}

	// Every ring record is kept, not only WARN: the bundle is for a human,
	// unlike the SSE stream.
	if !strings.Contains(files["ring.jsonl"], "first line") ||
		!strings.Contains(files["ring.jsonl"], "second line") {
		t.Fatalf("ring.jsonl = %q", files["ring.jsonl"])
	}

	// Problem probes carry their target, or one JSONL stream is unreadable.
	if !strings.Contains(files["probes.jsonl"], `"target":"chase.com"`) {
		t.Fatalf("probes.jsonl = %q", files["probes.jsonl"])
	}
	if !strings.Contains(files["probes.jsonl"], "tcp_refused") {
		t.Fatalf("probes.jsonl lost the class: %q", files["probes.jsonl"])
	}

	if !strings.Contains(files["version.txt"], "go:") || !strings.Contains(files["version.txt"], "os/arch:") {
		t.Fatalf("version.txt = %q", files["version.txt"])
	}

	// The manifest names every file with a real SHA-256.
	manifestPattern := regexp.MustCompile(`(?m)^[0-9a-f]{64}  ring\.jsonl  \d+ bytes$`)
	if !manifestPattern.MatchString(files["MANIFEST.txt"]) {
		t.Fatalf("MANIFEST.txt = %q", files["MANIFEST.txt"])
	}
	for name := range files {
		if name == "MANIFEST.txt" {
			continue
		}
		if !strings.Contains(files["MANIFEST.txt"], "  "+name+"  ") {
			t.Errorf("manifest does not list %s", name)
		}
	}

	if !strings.Contains(stdout.String(), "wrote") {
		t.Fatalf("summary = %q", stdout.String())
	}
}

func TestDebugBundleJSONSummaryAndUnsetConfig(t *testing.T) {
	srv := bundleServer(t)
	t.Setenv("NETWEATHER_API_URL", srv.URL)
	t.Setenv("NETWEATHER_UI_TOKEN", "bundle-token")
	t.Setenv("NETWEATHER_CONFIG", "")

	output := filepath.Join(t.TempDir(), "bundle.tar.gz")
	var stdout bytes.Buffer
	if err := debugBundle([]string{"-o", output, "-json"}, &stdout); err != nil {
		t.Fatalf("debugBundle: %v", err)
	}

	var env struct {
		Data struct {
			Path  string   `json:"path"`
			Files []string `json:"files"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("summary is not an envelope: %v (%s)", err, stdout.String())
	}
	if env.Data.Path != output || len(env.Data.Files) < 7 {
		t.Fatalf("summary = %+v", env.Data)
	}

	files := readBundle(t, output)
	if !strings.Contains(files["config.redacted.yaml"], "NETWEATHER_CONFIG is not set") {
		t.Fatalf("unset config = %q", files["config.redacted.yaml"])
	}
}

func TestDebugBundleRequiresATokenAndAPath(t *testing.T) {
	t.Setenv("NETWEATHER_UI_TOKEN", "")
	t.Setenv("NETWEATHER_UI_TOKEN_FILE", "")
	if err := debugBundle(nil, io.Discard); err == nil {
		t.Fatal("a bundle without a token was accepted")
	}

	if err := writeTarGz("", nil); err == nil {
		t.Fatal("an empty output path was accepted")
	}
}
