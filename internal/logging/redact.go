package logging

import (
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

// Redaction is centralised here and applied by every sink, so a caller cannot
// forget it. The rule is: a log line must be safe to paste into a public issue.

const redacted = "[REDACTED]"

// secretKeys are matched exactly (case-insensitive).
var secretKeys = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
	"x-api-key":           {},
	"x-auth-token":        {},
	"api-key":             {},
	"apikey":              {},
	"token":               {},
	"password":            {},
	"passwd":              {},
	"secret":              {},
	"client-secret":       {},
	"client_secret":       {},
	"access-token":        {},
	"access_token":        {},
	"refresh-token":       {},
	"refresh_token":       {},
	"authkey":             {},
	"ts_authkey":          {},
	"private-key":         {},
	"private_key":         {},
}

// secretFragments catch keys that are obviously credentials without matching
// innocuous words: "monkey" must not be redacted by a "key" rule.
var secretFragments = []string{"token", "secret", "password", "passwd", "apikey", "api_key", "authkey", "credential"}

// urlKeys carry a URL whose query string may include a credential.
var urlKeys = map[string]struct{}{
	"url":         {},
	"uri":         {},
	"endpoint":    {},
	"request_url": {},
	"feed_url":    {},
	"redirect":    {},
}

// textKeys may contain a URL embedded in prose (typically an error string).
var textKeys = map[string]struct{}{
	"error":  {},
	"err":    {},
	"detail": {},
	"reason": {},
}

var (
	reUserinfo    = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s]+@`)
	reQuerySecret = regexp.MustCompile(`(?i)([?&](?:token|key|api[_-]?key|apikey|sig|signature|password|passwd|secret|access_token|refresh_token)=)[^&\s"']+`)
)

// isSecretKey reports whether the attribute key names a credential.
func isSecretKey(key string) bool {
	k := strings.ToLower(key)
	if _, ok := secretKeys[k]; ok {
		return true
	}
	for _, frag := range secretFragments {
		if strings.Contains(k, frag) {
			return true
		}
	}
	return false
}

// ScrubString removes userinfo and credential query parameters from any URL
// appearing in free text.
func ScrubString(s string) string {
	s = reUserinfo.ReplaceAllString(s, "$1"+redacted+"@")
	s = reQuerySecret.ReplaceAllString(s, "$1"+redacted)
	return s
}

// SafeURL renders a URL with credentials and sensitive query values removed. All
// call sites that log a URL must use this instead of u.String().
func SafeURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	if c.User != nil {
		c.User = url.User(redacted)
	}
	if c.RawQuery != "" {
		q := c.Query()
		for k := range q {
			if isSecretKey(k) {
				q.Set(k, redacted)
			}
		}
		c.RawQuery = q.Encode()
	}
	return c.String()
}

// RedactAttr is the ReplaceAttr function shared by every sink.
func RedactAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey || a.Key == slog.LevelKey || a.Key == slog.SourceKey || a.Key == slog.MessageKey {
		return a
	}

	if isSecretKey(a.Key) {
		return slog.String(a.Key, redacted)
	}

	v := a.Value.Resolve()
	if v.Kind() != slog.KindString {
		return a
	}
	s := v.String()

	if _, ok := urlKeys[strings.ToLower(a.Key)]; ok {
		// Parse when possible so query ordering and escaping are normalised;
		// fall back to text scrubbing for a malformed URL.
		if u, err := url.Parse(s); err == nil {
			return slog.String(a.Key, SafeURL(u))
		}
		return slog.String(a.Key, ScrubString(s))
	}
	if _, ok := textKeys[strings.ToLower(a.Key)]; ok {
		return slog.String(a.Key, ScrubString(s))
	}
	return a
}
