package main

import (
	"context"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fileURL turns an absolute local path into a file:// URL that fetchRootZone
// accepts on both Windows and POSIX.
func fileURL(t *testing.T, path string) string {
	t.Helper()

	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	u := neturl.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	return u.String()
}

func TestFetchRootZoneReadsALocalMirror(t *testing.T) {
	t.Parallel()

	body := "ad.\t172800\tIN\tNS\tns1.nic.ad.\n"
	path := filepath.Join(t.TempDir(), "root.zone")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := fetchRootZone(context.Background(), fileURL(t, path))
	if err != nil {
		t.Fatalf("fetchRootZone: %v", err)
	}
	if got != body {
		t.Fatalf("body = %q", got)
	}
}

func TestFetchRootZoneRefusesOtherSchemes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
		want string
	}{
		{"plaintext HTTP", "http://www.internic.net/domain/root.zone", "must be https"},
		{"another scheme", "ftp://example.com/root.zone", "must be https"},
		{"file on another host", "file://example.com/root.zone", "local file"},
		{"https userinfo", "https://user:pass@example.com/root.zone", "userinfo"},
		{"https without a host", "https:///root.zone", "no host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := fetchRootZone(context.Background(), tc.url)
			if err == nil {
				t.Fatalf("%s was accepted", tc.url)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFetchRootZoneRefusesAMissingFile(t *testing.T) {
	t.Parallel()

	_, err := fetchRootZone(context.Background(),
		fileURL(t, filepath.Join(t.TempDir(), "missing.zone")))
	if err == nil {
		t.Fatal("a missing local mirror was accepted")
	}
}

func TestReadRootZoneRefusesOversize(t *testing.T) {
	t.Parallel()

	// Exactly at the cap is still a payload; one byte more is a refusal, not a
	// silently truncated zone.
	atCap := strings.NewReader(strings.Repeat("x", int(maxRootZoneBytes)))
	if got, err := readRootZone(atCap); err != nil || int64(len(got)) != maxRootZoneBytes {
		t.Fatalf("at-cap read = (%d, %v)", len(got), err)
	}

	overCap := strings.NewReader(strings.Repeat("x", int(maxRootZoneBytes)+1))
	if _, err := readRootZone(overCap); err == nil {
		t.Fatal("an oversized payload was truncated instead of refused")
	}
}

func TestRootZoneRedirectPolicy(t *testing.T) {
	t.Parallel()

	https := func() *http.Request {
		req, err := http.NewRequest(http.MethodGet, "https://www.internic.net/domain/root.zone", nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}

	if err := rootZoneRedirectPolicy(https(), nil); err != nil {
		t.Fatalf("an HTTPS hop was refused: %v", err)
	}

	plain, err := http.NewRequest(http.MethodGet, "http://www.internic.net/domain/root.zone", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rootZoneRedirectPolicy(plain, nil); err == nil {
		t.Fatal("a redirect to plaintext HTTP was followed")
	}

	if err := rootZoneRedirectPolicy(https(), make([]*http.Request, 5)); err == nil {
		t.Fatal("a sixth redirect hop was accepted")
	}
}
