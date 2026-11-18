package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestDocumentedAPIPathsHaveRoutes keeps docs/API.md honest in the direction
// that matters to a client: every path the document names must exist in the
// mux. A typo in the document, or a route renamed without the docs, otherwise
// sends an integrator to a 404 that the document promised.
//
// The reverse direction is deliberately not asserted: the trampoline handlers
// under /api/v1/targets/ and /api/v1/incidents/ are real routes whose suffixes
// live inside the handler, so a route without a matching sentence is not
// evidence of an undocumented feature.
func TestDocumentedAPIPathsHaveRoutes(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	source, err := os.ReadFile(filepath.Join(root, "internal", "api", "api.go")) // #nosec G304 -- repo file.
	if err != nil {
		t.Fatal(err)
	}
	routeRE := regexp.MustCompile(`HandleFunc\("(/api/v1/[^"]*)"`)
	var routes []string
	for _, match := range routeRE.FindAllStringSubmatch(string(source), -1) {
		routes = append(routes, match[1])
	}
	if len(routes) == 0 {
		t.Fatal("the route scan found nothing; the pattern is wrong")
	}

	docs, err := os.ReadFile(filepath.Join(root, "docs", "API.md")) // #nosec G304 -- repo file.
	if err != nil {
		t.Fatal(err)
	}
	// A method followed by a path. Method combinations such as POST/GET are
	// written as separate backticked fragments in the document, so one method
	// is enough to anchor a mention.
	pathRE := regexp.MustCompile("(?:GET|POST|PUT|PATCH|DELETE) (/(?:api/v1)/[A-Za-z0-9/_{}-]+)")
	placeholders := regexp.MustCompile(`\{[A-Za-z0-9_]+\}`)

	seen := make(map[string]struct{})
	var missing []string
	for _, match := range pathRE.FindAllStringSubmatch(string(docs), -1) {
		path := placeholders.ReplaceAllString(match[1], "")
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		if !hasRoute(path, routes) {
			missing = append(missing, match[1])
		}
	}
	if len(seen) == 0 {
		t.Fatal("the docs scan found no API paths; the pattern is wrong")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("documented paths with no route: %s", strings.Join(missing, ", "))
	}
}

// hasRoute reports whether a documented path is served by one of the mux
// entries. A trailing slash is a subtree, so a documented suffix matches it.
func hasRoute(path string, routes []string) bool {
	path = strings.TrimSuffix(path, "/")
	for _, route := range routes {
		route = strings.TrimSuffix(route, "/")
		if path == route || strings.HasPrefix(path, route+"/") {
			return true
		}
	}
	return false
}
