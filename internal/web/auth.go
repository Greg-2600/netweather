package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/logging"
)

const (
	sessionCookieName = "nw_session"
	csrfCookieName    = "nw_csrf"
	sessionTTL        = 12 * time.Hour
	csrfTTL           = 10 * time.Minute
)

// authenticator issues stateless, HMAC-signed session cookies. The key is
// derived from the UI token, so rotating the token invalidates every session
// without a session store.
type authenticator struct {
	token  string
	key    []byte
	secure bool
	limit  *logging.Limiter
	// global caps the whole process, so a distributed attempt against many
	// source addresses cannot spend the per-client allowance indefinitely.
	global *logging.Limiter
}

func newAuthenticator(token string, secure bool) *authenticator {
	sum := sha256.Sum256([]byte("netweather/session/" + token))
	return &authenticator{
		token:  token,
		key:    sum[:],
		secure: secure,
		// Five login attempts per minute per client; enough for a typo, not
		// enough to be interesting.
		limit: logging.NewLimiter(time.Minute, 5),
		// Thirty per minute across the process. Six clients can each use their
		// full personal allowance before the global cap applies.
		global: logging.NewLimiter(time.Minute, 30),
	}
}

func (a *authenticator) disabled() bool { return a.token == "" }

func (a *authenticator) issueSession(now time.Time) string {
	exp := strconv.FormatInt(now.Add(sessionTTL).Unix(), 10)
	return sign(a.key, exp)
}

func (a *authenticator) validSession(v string, now time.Time) bool {
	payload, ok := verify(a.key, v)
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return false
	}
	return now.Unix() < exp
}

func (a *authenticator) checkToken(candidate string) bool {
	// Hash both sides so the comparison is constant-time over a fixed length.
	want := sha256.Sum256([]byte(a.token))
	got := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func sign(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verify(key []byte, value string) (string, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	return string(payload), true
}

func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

func (a *authenticator) setSessionCookie(w http.ResponseWriter, value string, now time.Time) {
	// #nosec G124 -- Secure follows the deployment's TLS posture; HttpOnly and
	// SameSite are always set, and the cookie never carries the token itself.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  now.Add(sessionTTL),
	})
}

func (a *authenticator) clearSessionCookie(w http.ResponseWriter) {
	// #nosec G124 -- same rationale as setSessionCookie.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (a *authenticator) setCSRFCookie(w http.ResponseWriter, value string, now time.Time) {
	// #nosec G124 -- CSRF token, not a credential; HttpOnly and SameSite are set.
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  now.Add(csrfTTL),
	})
}

func (a *authenticator) sessionFrom(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", false
	}
	if !a.validSession(c.Value, time.Now()) {
		return "", false
	}
	return c.Value, true
}

func (a *authenticator) csrfFrom(r *http.Request) (string, bool) {
	c, err := r.Cookie(csrfCookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}

func (a *authenticator) csrfMatches(r *http.Request, formValue string) bool {
	cookie, ok := a.csrfFrom(r)
	if !ok || formValue == "" || len(cookie) != len(formValue) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie), []byte(formValue)) == 1
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requireSession protects the board and the event stream. It redirects to the
// login page rather than returning 401, because this is a browser surface.
func (s *server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth.disabled() {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := s.auth.sessionFrom(r); !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
