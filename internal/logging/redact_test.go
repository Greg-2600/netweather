package logging_test

import (
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/logging"
)

func TestRedactAttrSecretKeys(t *testing.T) {
	t.Parallel()

	secret := []string{"token", "Authorization", "set-cookie", "api_key", "TS_AUTHKEY", "client_secret", "password"}
	for _, key := range secret {
		got := logging.RedactAttr(nil, slog.String(key, "s3cr3t"))
		if got.Value.String() != "[REDACTED]" {
			t.Errorf("key %q: got %q, want [REDACTED]", key, got.Value.String())
		}
	}
}

func TestRedactAttrLeavesInnocuousKeys(t *testing.T) {
	t.Parallel()

	// "monkey" contains "key" but is not a credential. A naive substring rule
	// on "key" would redact it and hide real debugging data.
	for _, key := range []string{"monkey", "keyboard", "target", "host", "latency_ms"} {
		got := logging.RedactAttr(nil, slog.String(key, "value"))
		if got.Value.String() != "value" {
			t.Errorf("key %q was redacted unexpectedly", key)
		}
	}
}

func TestRedactAttrURLKey(t *testing.T) {
	t.Parallel()

	got := logging.RedactAttr(nil, slog.String("url", "https://user:pw@example.com/path?token=abc&page=2"))
	s := got.Value.String()

	if strings.Contains(s, "pw") || strings.Contains(s, "abc") {
		t.Fatalf("URL not redacted: %q", s)
	}
	if !strings.Contains(s, "page=2") {
		t.Fatalf("non-secret query parameter lost: %q", s)
	}
}

func TestScrubString(t *testing.T) {
	t.Parallel()

	in := "dial failed: https://user:pw@example.com/x?access_token=abc123&limit=5: timeout"
	out := logging.ScrubString(in)

	if strings.Contains(out, "pw") || strings.Contains(out, "abc123") {
		t.Fatalf("credentials survived: %q", out)
	}
	if !strings.Contains(out, "limit=5") {
		t.Fatalf("benign parameter lost: %q", out)
	}
}

func TestSafeURL(t *testing.T) {
	t.Parallel()

	u, err := url.Parse("https://alice:hunter2@api.example.com/v1?api_key=deadbeef&since=0")
	if err != nil {
		t.Fatal(err)
	}
	got := logging.SafeURL(u)

	if strings.Contains(got, "hunter2") || strings.Contains(got, "deadbeef") {
		t.Fatalf("SafeURL leaked a secret: %q", got)
	}
	if !strings.Contains(got, "since=0") {
		t.Fatalf("SafeURL dropped a benign parameter: %q", got)
	}
}

func TestSafeURLNil(t *testing.T) {
	t.Parallel()

	if got := logging.SafeURL(nil); got != "" {
		t.Fatalf("SafeURL(nil) = %q, want empty", got)
	}
}
