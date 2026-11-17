package web

import (
	"fmt"
	"hash/fnv"
	"html"
	"net/http"
	"strings"
)

// handleIcon serves a generated square icon for one target.
//
// It is deliberately generated rather than fetched: pulling /favicon.ico from
// every target would add a request per site that has no measurement
// justification, which ACCEPTABLE_USE.md forbids. The icon carries the site's
// initial and a stable colour derived from its host, so the board reads as a
// grid of distinct marks without touching the targets.
func (s *server) handleIcon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.Source == nil {
		http.Error(w, "board unavailable", http.StatusServiceUnavailable)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/icon/")
	id := strings.TrimSuffix(rest, ".svg")
	if id == "" || strings.Contains(id, "/") || !strings.HasSuffix(rest, ".svg") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	targets, err := s.deps.Source.Snapshot(r.Context())
	if err != nil {
		s.deps.Logger.Warn("icon snapshot failed", "event", "icon_failed", "component", "http", "error", err)
		http.Error(w, "board unavailable", http.StatusServiceUnavailable)
		return
	}

	for _, t := range targets {
		if t.ID != id {
			continue
		}

		icon := renderIcon(t.Name, t.Host)
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		// The icon is stable for a target's lifetime; caching keeps the board
		// from re-fetching a screen of them on every navigation.
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write([]byte(icon))
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

// renderIcon builds the SVG. The hue is stable per host and the initial comes
// from the display name, so two targets behind the same CDN still differ.
func renderIcon(name, host string) string {
	initial := "?"
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		runes := []rune(trimmed)
		initial = strings.ToUpper(string(runes[0]))
	} else if host != "" {
		initial = strings.ToUpper(string([]rune(host)[0]))
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(host)))
	hue := int(h.Sum32() % 360)

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" role="img" aria-label="%s">
  <rect width="64" height="64" rx="14" fill="hsl(%d 55%% 42%%)"/>
  <text x="32" y="43" text-anchor="middle" font-family="system-ui, -apple-system, Segoe UI, sans-serif" font-size="32" font-weight="600" fill="#ffffff">%s</text>
</svg>
`, html.EscapeString(name), hue, html.EscapeString(initial))
}

// iconURL is the template-facing path. Keeping it in one place means the tile
// and the route cannot disagree about the suffix.
func iconURL(id string) string {
	return "/icon/" + id + ".svg"
}
