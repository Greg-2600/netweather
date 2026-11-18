package classify_test

import (
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/classify"
	"github.com/Greg-2600/netweather/internal/state"
)

// fixture is one recorded response plus the outcome the documented table
// requires. CLASSIFIER.md §13 asks for files rather than only struct literals:
// the point is that the status line and headers are parsed from the wire shape
// an operator can paste into a bug report, not constructed by the test.
type fixture struct {
	name       string
	expect     state.State
	confidence classify.Confidence
	reason     string
	bodyMarker string
	errorClass string
	response   classify.Response
}

func TestClassifierFixtures(t *testing.T) {
	t.Parallel()

	dir := filepath.Join("testdata", "fixtures")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("fixtures: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no fixtures; the directory is the suite")
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		t.Run(entry.Name(), func(t *testing.T) {
			t.Parallel()

			f := loadFixture(t, path)
			if f.expect == "" {
				t.Fatalf("%s has no # expect:", path)
			}

			expected := classify.Expected{BodyContains: f.bodyMarker}
			got := classify.Decide(expected, f.response)

			if got.State != f.expect {
				t.Fatalf("state = %s, want %s (reason %q)", got.State, f.expect, got.Reason)
			}
			if f.confidence != "" && got.Confidence != f.confidence {
				t.Fatalf("confidence = %s, want %s (reason %q)", got.Confidence, f.confidence, got.Reason)
			}
			if f.reason != "" && !strings.Contains(got.Reason, f.reason) {
				t.Fatalf("reason = %q, want it to contain %q", got.Reason, f.reason)
			}
		})
	}
}

// loadFixture parses the "# key: value" preamble and the optional request and
// response sections. The request section is read so a fixture stays a complete
// transcript, but only the response reaches the classifier.
func loadFixture(t *testing.T, path string) fixture {
	t.Helper()

	raw, err := os.ReadFile(path) // #nosec G304 -- repo fixture chosen by the test.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	f := fixture{name: filepath.Base(path)}
	var (
		lines   = strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
		section string
		block   []string
	)
	for _, line := range lines {
		switch strings.TrimSpace(line) {
		case "--- request":
			section = "request"
			continue
		case "--- response":
			section = "response"
			continue
		}

		if section == "" {
			key, value, ok := parseMeta(line)
			if !ok {
				continue
			}
			switch key {
			case "expect":
				f.expect = state.State(strings.ToLower(value))
			case "confidence":
				f.confidence = classify.Confidence(strings.ToLower(value))
			case "reason-contains":
				f.reason = value
			case "body-contains":
				f.bodyMarker = value
			case "error-class":
				f.errorClass = value
			}
			continue
		}
		if section == "response" {
			block = append(block, line)
		}
	}

	f.response = parseResponse(t, path, block)
	if f.errorClass != "" {
		f.response = classify.Response{ErrorClass: f.errorClass}
	}
	return f
}

func parseMeta(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "#") {
		return "", "", false
	}
	body := strings.TrimSpace(strings.TrimPrefix(line, "#"))
	key, value, ok := strings.Cut(body, ":")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(key), strings.TrimSpace(value), true
}

// parseResponse reads a raw HTTP response block: the status line, headers
// until a blank line, then the body verbatim.
func parseResponse(t *testing.T, path string, lines []string) classify.Response {
	t.Helper()

	// Find the status line; a fixture may start the block with a blank line.
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) {
		t.Fatalf("%s: response section is empty", path)
	}

	fields := strings.Fields(lines[i])
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		t.Fatalf("%s: status line %q is not HTTP", path, lines[i])
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("%s: status %q is not a number", path, fields[1])
	}
	i++

	header := make(http.Header)
	for ; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			i++
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("%s: header line %q has no colon", path, line)
		}
		header.Add(textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(key)), strings.TrimSpace(value))
	}

	body := strings.Join(lines[i:], "\n")
	body = strings.TrimSuffix(body, "\n")

	return classify.Response{
		Status: code,
		Header: header,
		Body:   []byte(body),
	}
}
