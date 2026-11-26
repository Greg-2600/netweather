package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/logging"
)

const (
	sessionCookieName = "nw_session"
	csrfCookieName    = "nw_csrf"
	// A session expires after 12h without use and never outlives 7d from login,
	// however active it is (THREAT_MODEL.md T7). The idle window slides on use;
	// the absolute cap does not.
	sessionIdleTTL      = 12 * time.Hour
	sessionAbsoluteTTL  = 7 * 24 * time.Hour
	sessionRefreshAfter = 5 * time.Minute
	csrfTTL             = 10 * time.Minute
)

// authenticator issues stateless, HMAC-signed session cookies. The key is
// derived from the UI token, so rotating the token invalidates every session
// without a session store.
type authenticator struct {
	token string
	key   []byte
	limit *logging.Limiter
	// global caps the whole process, so a distributed attempt against many
	// source addresses cannot spend the per-client allowance indefinitely.
	global *logging.Limiter
}

func newAuthenticator(token string) *authenticator {
	sum := sha256.Sum256([]byte("netweather/session/" + token))
	return &authenticator{
		token: token,
		key:   sum[:],
		// Five login attempts per minute per client; enough for a typo, not
		// enough to be interesting.
		limit: logging.NewLimiter(time.Minute, 5),
		// Thirty per minute across the process. Six clients can each use their
		// full personal allowance before the global cap applies.
		global: logging.NewLimiter(time.Minute, 30),
	}
}

func (a *authenticator) disabled() bool { return a.token == "" }

// session is the verified payload of a session cookie: when the operator
// logged in and when the idle window closes. The issue time is what makes the
// absolute cap independent of activity.
type session struct {
	issued  time.Time
	expires time.Time
}

// issueSession signs a session that expires after the idle window. issued is the
// original login time and is preserved across refreshes, so reissuing cannot
// extend a session past sessionAbsoluteTTL.
func (a *authenticator) issueSession(now, issued time.Time) string {
	payload := strconv.FormatInt(issued.Unix(), 10) + "." +
		strconv.FormatInt(now.Add(sessionIdleTTL).Unix(), 10)
	return sign(a.key, payload)
}

// sessionClaims verifies a cookie and checks both lifetimes. The order matters:
// the signature first, then the idle deadline, then the absolute cap.
func (a *authenticator) sessionClaims(value string, now time.Time) (session, bool) {
	payload, ok := verify(a.key, value)
	if !ok {
		return session{}, false
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 2 {
		return session{}, false
	}
	issuedUnix, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return session{}, false
	}
	expUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return session{}, false
	}

	issued := time.Unix(issuedUnix, 0)
	expires := time.Unix(expUnix, 0)
	if !now.Before(expires) {
		return session{}, false
	}
	if now.Sub(issued) > sessionAbsoluteTTL {
		return session{}, false
	}
	return session{issued: issued, expires: expires}, true
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

// secureCookie decides the cookie's Secure attribute for one request. The
// documented rule (NETWORKING.md §7.1) is Secure whenever the browser's
// effective scheme is HTTPS, including a TLS-terminating proxy that sets
// X-Forwarded-Proto and is trusted. NETWEATHER_UI_SECURE_COOKIES=true forces it
// even when the request looks plaintext, which is the operator's explicit
// choice and will break login over HTTP.
func (s *server) secureCookie(r *http.Request) bool {
	return s.deps.SecureCookies || !s.insecure(r)
}

func (a *authenticator) setSessionCookie(w http.ResponseWriter, value string, now time.Time, secure bool) {
	// #nosec G124 -- Secure follows the request's TLS posture and the operator
	// override; HttpOnly and SameSite are always set, and the cookie never
	// carries the token itself.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  now.Add(sessionIdleTTL),
	})
}

func (a *authenticator) clearSessionCookie(w http.ResponseWriter, secure bool) {
	// #nosec G124 -- same rationale as setSessionCookie.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (a *authenticator) setCSRFCookie(w http.ResponseWriter, value string, now time.Time, secure bool) {
	// #nosec G124 -- CSRF token, not a credential; HttpOnly and SameSite are set.
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  now.Add(csrfTTL),
	})
}

func (a *authenticator) sessionFrom(r *http.Request) (string, session, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", session{}, false
	}
	claims, ok := a.sessionClaims(c.Value, time.Now())
	if !ok {
		return "", session{}, false
	}
	return c.Value, claims, true
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

// requireSession protects the board and the event stream. The front page draws
// the login form in place; other pages redirect to the login page rather than
// returning 401, because this is a browser surface.
// When the request is authenticated more than sessionRefreshAfter past the last
// refresh, the cookie is reissued with a fresh idle deadline and the original
// issue time, so activity extends the idle window but never the 7-day cap.
func (s *server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth.disabled() {
			next.ServeHTTP(w, r)
			return
		}
		_, claims, ok := s.auth.sessionFrom(r)
		if !ok {
			if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/" {
				s.handleLogin(w, r)
				return
			}
			dest := "/login"
			// Carry the page the browser asked for, but only a page: a
			// subresource or the stream has no useful post-login destination,
			// and a redirect to login for those is already enough.
			if r.Method == http.MethodGet && r.URL.RequestURI() != "/" && wantsPageAfterLogin(r.URL.Path) {
				dest += "?next=" + neturl.QueryEscape(r.URL.RequestURI())
			}
			http.Redirect(w, r, dest, http.StatusSeeOther)
			return
		}
		now := s.now()
		if now.Sub(claims.expires.Add(-sessionIdleTTL)) >= sessionRefreshAfter {
			s.auth.setSessionCookie(w, s.auth.issueSession(now, claims.issued), now, s.secureCookie(r))
		}
		next.ServeHTTP(w, r)
	})
}

// wantsPageAfterLogin reports whether a request is a page a browser could
// usefully return to after signing in. The event stream, icons, and static
// assets are subresources: sending their path as a post-login destination
// would land the browser on an SVG or an endless stream, so they get the bare
// login page instead.
func wantsPageAfterLogin(path string) bool {
	if path == "/events" || path == "/robots.txt" {
		return false
	}
	return !strings.HasPrefix(path, "/icon/") && !strings.HasPrefix(path, "/static/")
}
