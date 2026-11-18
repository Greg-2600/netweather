package web

import "net/http"

// contentSecurityPolicy is the browser surface's one policy: own assets only, no
// inline script or style, no framing, and forms post back to this origin. The
// templates carry no inline handler or third-party asset, so nothing weaker is
// needed (THREAT_MODEL.md T4).
const contentSecurityPolicy = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// withSecurityHeaders applies the browser surface's response headers. They are
// the same posture the API takes (THREAT_MODEL.md T12): no MIME sniffing, no
// framing, no referrer leakage, and no caching of a page that names the
// operator's vantage.
//
// A handler may replace Cache-Control when it has a better answer: the icon
// endpoint serves an immutable generated image, and the event stream must not
// be cached at all. Replacements are deliberate and local, which is why the
// default is set here rather than in each handler.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		// Set here, not only in the page renderers: a static asset, a method
		// refusal, or a generated icon is still a response this browser loads,
		// and the policy must not depend on which handler wrote it.
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}
