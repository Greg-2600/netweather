package logging

import (
	"fmt"
	"log/slog"
	"strconv"
)

// Config is the logging configuration. Every field has a safe default; an empty
// Config is a valid, quiet, human-readable logger.
type Config struct {
	Level  slog.Level
	Format string // "auto", "text", "json"
	File   string // empty disables the file sink

	MaxMB int // per log file, file sink only
	Keep  int // rotated files to retain, file sink only

	RingRecords int // in-memory ring capacity, 0 disables
	RingMaxKB   int // byte cap for the ring

	AddSource bool
}

// DefaultConfig returns the defaults described in docs/LOGGING.md.
func DefaultConfig() Config {
	return Config{
		Level:       slog.LevelInfo,
		Format:      "auto",
		MaxMB:       50,
		Keep:        3,
		RingRecords: 2000,
		RingMaxKB:   4096,
	}
}

// Validate rejects configurations that would silently do nothing useful.
func (c Config) Validate() error {
	switch c.Format {
	case "auto", "text", "json":
	default:
		return fmt.Errorf("unknown log format %q (want auto, text, json)", c.Format)
	}
	if c.MaxMB <= 0 {
		return fmt.Errorf("log.max_mb must be > 0")
	}
	if c.Keep < 1 {
		return fmt.Errorf("log.keep must be >= 1")
	}
	if c.RingRecords < 0 || c.RingMaxKB < 0 {
		return fmt.Errorf("ring sizes cannot be negative")
	}
	return nil
}

// FromEnv builds a Config from NETWEATHER_LOG* variables. The getenv function is
// injected so tests do not mutate the process environment.
func FromEnv(getenv func(string) string) (Config, error) {
	c := DefaultConfig()

	level, err := ParseLevel(getenv("NETWEATHER_LOG_LEVEL"))
	if err != nil {
		return c, err
	}
	c.Level = level

	if v := getenv("NETWEATHER_LOG_FORMAT"); v != "" {
		c.Format = v
	}
	c.File = getenv("NETWEATHER_LOG_FILE")
	c.AddSource = getenv("NETWEATHER_LOG_SOURCE") == "1"

	if v := getenv("NETWEATHER_LOG_MAX_MB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, fmt.Errorf("NETWEATHER_LOG_MAX_MB: %w", err)
		}
		c.MaxMB = n
	}
	if v := getenv("NETWEATHER_LOG_KEEP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, fmt.Errorf("NETWEATHER_LOG_KEEP: %w", err)
		}
		c.Keep = n
	}
	if v := getenv("NETWEATHER_LOG_RING"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, fmt.Errorf("NETWEATHER_LOG_RING: %w", err)
		}
		c.RingRecords = n
	}

	return c, c.Validate()
}
