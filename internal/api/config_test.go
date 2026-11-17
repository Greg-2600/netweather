package api_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Greg-2600/netweather/internal/api"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestConfigDefaultsToLoopback(t *testing.T) {
	t.Parallel()

	cfg, err := api.FromEnv(env(nil))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Listen != "127.0.0.1:2600" {
		t.Fatalf("Listen = %q", cfg.Listen)
	}
	if cfg.AuthMode() != "none" {
		t.Fatalf("AuthMode = %q", cfg.AuthMode())
	}
}

func TestConfigRefusesPublicBindWithoutToken(t *testing.T) {
	t.Parallel()

	for _, listen := range []string{"0.0.0.0:2600", ":2600", "[::]:2600"} {
		_, err := api.FromEnv(env(map[string]string{"NETWEATHER_UI_LISTEN": listen}))
		if err == nil {
			t.Errorf("%s without a token should refuse to start", listen)
		}
	}
}

func TestConfigPublicBindNeedsExplicitOptIn(t *testing.T) {
	t.Parallel()

	cfg, err := api.FromEnv(env(map[string]string{
		"NETWEATHER_UI_LISTEN":       "0.0.0.0:2600",
		"NETWEATHER_UI_ALLOW_PUBLIC": "true",
	}))
	if err != nil {
		t.Fatalf("explicit opt-in should be accepted: %v", err)
	}
	if cfg.AllowPublic != true {
		t.Fatal("AllowPublic not set")
	}
}

func TestConfigRejectsShortToken(t *testing.T) {
	t.Parallel()

	_, err := api.FromEnv(env(map[string]string{
		"NETWEATHER_UI_TOKEN": "short",
	}))
	if err == nil {
		t.Fatal("a 5-character token should be rejected")
	}
}

func TestConfigReadsTokenFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ui_token")
	if err := os.WriteFile(path, []byte("  a-long-enough-token-value  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := api.FromEnv(env(map[string]string{
		"NETWEATHER_UI_TOKEN_FILE": path,
		"NETWEATHER_UI_LISTEN":     "0.0.0.0:2600",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Token != "a-long-enough-token-value" {
		t.Fatalf("token = %q (whitespace should be trimmed)", cfg.Token)
	}
	if cfg.AuthMode() != "bearer" {
		t.Fatalf("AuthMode = %q", cfg.AuthMode())
	}
}

func TestConfigRejectsEmptyTokenFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "empty")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := api.FromEnv(env(map[string]string{"NETWEATHER_UI_TOKEN_FILE": path})); err == nil {
		t.Fatal("an empty token file should be rejected, not treated as auth-none")
	}
}

func TestConfigRejectsMissingTokenFile(t *testing.T) {
	t.Parallel()

	if _, err := api.FromEnv(env(map[string]string{
		"NETWEATHER_UI_TOKEN_FILE": filepath.Join(t.TempDir(), "absent"),
	})); err == nil {
		t.Fatal("a missing token file should be an error")
	}
}

func TestConfigRejectsWorldReadableTokenFile(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX modes")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "ui_token")
	if err := os.WriteFile(path, []byte("a-long-enough-token-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := api.FromEnv(env(map[string]string{"NETWEATHER_UI_TOKEN_FILE": path})); err == nil {
		t.Fatal("a group/world-readable token file should be rejected")
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := api.FromEnv(env(map[string]string{"NETWEATHER_UI_TOKEN_FILE": path})); err != nil {
		t.Fatalf("0600 token file should be accepted: %v", err)
	}
}

func TestConfigRejectsBadListen(t *testing.T) {
	t.Parallel()

	if _, err := api.FromEnv(env(map[string]string{"NETWEATHER_UI_LISTEN": "not-an-address"})); err == nil {
		t.Fatal("an unparseable listen address should be rejected")
	}
}

func TestIsLoopbackThroughValidation(t *testing.T) {
	t.Parallel()

	for _, listen := range []string{"127.0.0.1:2600", "localhost:2600", "[::1]:2600"} {
		if _, err := api.FromEnv(env(map[string]string{"NETWEATHER_UI_LISTEN": listen})); err != nil {
			t.Errorf("%s should be accepted without a token: %v", listen, err)
		}
	}
}
