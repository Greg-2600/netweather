package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/version"
)

// bundleMaxProbesPerTarget is the documented cap (LOGGING.md §10): enough to
// see the shape of a failure, not a database dump.
const bundleMaxProbesPerTarget = 20

// debugBundle writes a support bundle. It is the one command an operator can
// run on a broken instance without a shell, so it only reads the public API
// plus the local binary's identity and the config file.
func debugBundle(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("debug bundle", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	output := fs.String("o", "netweather-bundle.tar.gz", "output path")
	asJSON := fs.Bool("json", false, "print a machine-readable summary")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("debug bundle: %w", err)
	}

	client, err := newDebugClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	files, err := bundleFiles(ctx, client)
	if err != nil {
		return err
	}
	if err := writeTarGz(*output, files); err != nil {
		return err
	}

	summary := bundleSummary{Path: *output, Files: make([]string, 0, len(files))}
	for _, f := range files {
		summary.Files = append(summary.Files, f.Name)
	}
	if *asJSON {
		return writeJSON(out, summary)
	}
	_, _ = fmt.Fprintf(out, "wrote %s with %d file(s)\n", summary.Path, len(summary.Files))
	return nil
}

type bundleFile struct {
	Name string
	Body []byte
}

type bundleSummary struct {
	Path  string   `json:"path"`
	Files []string `json:"files"`
	Error string   `json:"error,omitempty"`
}

// bundleFiles collects every entry. A failed optional read is recorded in the
// bundle as a stated gap instead of aborting the whole collection: a bundle
// that explains what it could not read is still useful.
func bundleFiles(ctx context.Context, client *debugClient) ([]bundleFile, error) {
	files := []bundleFile{}

	files = append(files, bundleFile{Name: "version.txt", Body: []byte(versionText())})
	files = append(files, bundleFile{Name: "config.redacted.yaml", Body: redactedConfig()})

	for _, item := range []struct {
		name string
		path string
	}{
		{"meta.json", "/api/v1/meta"},
		{"selfcheck.json", "/api/v1/selfcheck"},
	} {
		body, err := client.get(ctx, item.path)
		if err != nil {
			files = append(files, bundleFile{
				Name: item.name,
				Body: []byte(fmt.Sprintf(`{"error":%q}`+"\n", err.Error())),
			})
			continue
		}
		files = append(files, bundleFile{Name: item.name, Body: body})
	}

	ring, err := client.get(ctx, "/api/v1/logs?limit=2000")
	switch {
	case err != nil:
		files = append(files, bundleFile{
			Name: "ring.jsonl",
			Body: []byte(fmt.Sprintf(`{"error":%q}`+"\n", err.Error())),
		})
	default:
		files = append(files, bundleFile{Name: "ring.jsonl", Body: jsonLines(ring, "data")})
	}

	probes, err := collectProblemProbes(ctx, client)
	if err != nil {
		files = append(files, bundleFile{
			Name: "probes.jsonl",
			Body: []byte(fmt.Sprintf(`{"error":%q}`+"\n", err.Error())),
		})
	} else {
		files = append(files, bundleFile{Name: "probes.jsonl", Body: probes})
	}

	sort.SliceStable(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	files = append(files, bundleFile{Name: "MANIFEST.txt", Body: manifest(files)})
	return files, nil
}

// versionText is the identity block a maintainer reads first.
func versionText() string {
	return fmt.Sprintf("version: %s\ncommit: %s\ndate: %s\ngo: %s\nos/arch: %s/%s\n",
		version.Version, version.Commit, version.Date,
		runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// configRedaction hides anything that looks like a credential. The target file
// is not supposed to carry secrets (the token is an environment variable), but
// a bundle leaves the operator's machine and defensive redaction costs nothing.
var configRedaction = regexp.MustCompile(`(?mi)^(\s*(?:token|ui_token|password|secret|api_key|key)\s*:).*$`)

func redactedConfig() []byte {
	path := strings.TrimSpace(os.Getenv("NETWEATHER_CONFIG"))
	if path == "" {
		return []byte("# NETWEATHER_CONFIG is not set; the built in full fleet was used.\n")
	}
	raw, err := os.ReadFile(path) // #nosec G304,G703 -- operator-supplied config path.
	if err != nil {
		return []byte(fmt.Sprintf("# config unavailable: %s\n", err))
	}
	return []byte(configRedaction.ReplaceAllString(string(raw), "$1 \"[redacted]\""))
}

// get fetches one API path and returns the raw envelope body.
func (c *debugClient) get(ctx context.Context, path string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return body, nil
}

// jsonLines turns the data array of a list envelope into JSONL. A body that is
// not the expected envelope is written as one error line rather than dropped.
func jsonLines(envelope []byte, key string) []byte {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(envelope, &env); err != nil {
		return []byte(fmt.Sprintf(`{"error":%q}`+"\n", "unreadable envelope: "+err.Error()))
	}
	raw, ok := env[key]
	if !ok {
		return []byte(fmt.Sprintf(`{"error":%q}`+"\n", "envelope has no "+key))
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return []byte(fmt.Sprintf(`{"error":%q}`+"\n", "data is not a list: "+err.Error()))
	}
	var out strings.Builder
	for _, item := range items {
		out.Write(item)
		out.WriteByte('\n')
	}
	return []byte(out.String())
}

// collectProblemProbes records the last probes for every DOWN or UNKNOWN
// target: the ones a bug report is about.
func collectProblemProbes(ctx context.Context, client *debugClient) ([]byte, error) {
	var out strings.Builder
	for _, state := range []string{"down", "unknown", "degraded", "blocked"} {
		body, err := client.get(ctx, "/api/v1/targets?state="+state)
		if err != nil {
			return nil, err
		}
		var env struct {
			Data []struct {
				ID   string `json:"id"`
				Host string `json:"host"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, fmt.Errorf("targets %s: %w", state, err)
		}
		for _, target := range env.Data {
			probes, err := client.get(ctx, fmt.Sprintf(
				"/api/v1/targets/%s/probes?limit=%d", target.ID, bundleMaxProbesPerTarget))
			if err != nil {
				fmt.Fprintf(&out, `{"target":%q,"error":%q}`+"\n", target.Host, err.Error())
				continue
			}
			for _, line := range splitLines(jsonLines(probes, "data")) {
				if line == "" {
					continue
				}
				// Prefix each row with the target so one stream stays readable.
				var row map[string]any
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					fmt.Fprintf(&out, `{"target":%q,"error":%q}`+"\n", target.Host, err.Error())
					continue
				}
				row["target"] = target.Host
				encoded, err := json.Marshal(row)
				if err != nil {
					continue
				}
				out.Write(encoded)
				out.WriteByte('\n')
			}
		}
	}
	return []byte(out.String()), nil
}

func splitLines(raw []byte) []string {
	text := strings.TrimSuffix(string(raw), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// manifest lists every entry with its SHA-256. A maintainer can check that the
// bundle they received is the one that was written.
func manifest(files []bundleFile) []byte {
	var out strings.Builder
	for _, f := range files {
		sum := sha256.Sum256(f.Body)
		fmt.Fprintf(&out, "%s  %s  %d bytes\n", hex.EncodeToString(sum[:]), f.Name, len(f.Body))
	}
	return []byte(out.String())
}

// writeTarGz writes the bundle atomically enough for a support tool: a partial
// file is removed rather than left looking complete.
func writeTarGz(path string, files []bundleFile) error {
	if path == "" {
		return errors.New("debug bundle: output path is required")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304,G703 -- operator-supplied output path.
	if err != nil {
		return fmt.Errorf("debug bundle: %w", err)
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, file := range files {
		header := &tar.Header{
			Name: file.Name, Mode: 0o600, Size: int64(len(file.Body)),
			ModTime: time.Now().UTC(),
		}
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("debug bundle: %w", err)
		}
		if _, err := tw.Write(file.Body); err != nil {
			return fmt.Errorf("debug bundle: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("debug bundle: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("debug bundle: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("debug bundle: %w", err)
	}
	ok = true
	return nil
}
