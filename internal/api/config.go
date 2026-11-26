package api

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
)

// MinTokenLen is the shortest accepted UI/API token. Short tokens are rejected
// at startup rather than accepted and quietly brute-forced.
const MinTokenLen = 16

// Config is the HTTP listener configuration.
type Config struct {
	Listen string
	Token  string
	// TokenFromEnv records that the token came from NETWEATHER_UI_TOKEN rather
	// than a file. It is not a secret; serve logs one warning because an inline
	// environment value is visible to anything that can read the process
	// environment, while a mounted file need not be (THREAT_MODEL.md T14).
	TokenFromEnv bool
	AllowPublic  bool
}

// FromEnv reads NETWEATHER_UI_* variables. The token file wins over the inline
// variable, because a file can be a secret mount and an env var cannot.
func FromEnv(getenv func(string) string) (Config, error) {
	c := Config{Listen: "0.0.0.0:2600"}

	if v := strings.TrimSpace(getenv("NETWEATHER_UI_LISTEN")); v != "" {
		c.Listen = v
	}
	c.AllowPublic = strings.EqualFold(getenv("NETWEATHER_UI_ALLOW_PUBLIC"), "true")

	if path := strings.TrimSpace(getenv("NETWEATHER_UI_TOKEN_FILE")); path != "" {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return c, fmt.Errorf("token file: %w", statErr)
		}
		// A token the whole host can read is not a secret. Windows has no POSIX
		// modes, so the check applies where it means something.
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return c, fmt.Errorf("token file %q is group/world readable (%04o); chmod 600 it", path, info.Mode().Perm())
		}
		// #nosec G304 -- operator-supplied config path, by design.
		b, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("read token file: %w", err)
		}
		c.Token = strings.TrimSpace(string(b))
		if c.Token == "" {
			return c, fmt.Errorf("token file %q is empty", path)
		}
	} else {
		c.Token = strings.TrimSpace(getenv("NETWEATHER_UI_TOKEN"))
		c.TokenFromEnv = c.Token != ""
	}

	return c, c.Validate()
}

// Validate enforces the exposure guardrail: a reachable listener without a token
// is a configuration error, not a warning.
func (c Config) Validate() error {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("ui.listen %q: %w", c.Listen, err)
	}

	if c.Token != "" && len(c.Token) < MinTokenLen {
		return fmt.Errorf("ui token is %d characters; minimum is %d", len(c.Token), MinTokenLen)
	}

	if c.Token == "" && !c.AllowPublic && !isLoopbackHost(host) {
		return fmt.Errorf(
			"refusing to listen on %q without auth: set NETWEATHER_UI_TOKEN_FILE, or bind loopback, or set NETWEATHER_UI_ALLOW_PUBLIC=true to accept the risk",
			c.Listen,
		)
	}
	return nil
}

// AuthMode is "bearer" when a token is configured, otherwise "none".
func (c Config) AuthMode() string {
	if c.Token == "" {
		return "none"
	}
	return "bearer"
}

func isLoopbackHost(host string) bool {
	if host == "" {
		return false // ":2600" means all interfaces
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
