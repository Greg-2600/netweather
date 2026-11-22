// Package region classifies a target's existing country code into a continent
// for the board's Region grouping lens. It adds no schema field: the country is
// already learned from the target, and the mapping is a presentation fact.
//
// The label answers "which part of the world is this label in?" It is not a
// measured hosting location, not a vantage location, and not a legal or
// political statement. An unmapped code is Unknown rather than a guess.
package region

import "strings"

// Names is the stable order used by validation and tests. Unknown is included
// so a filter can deliberately look at the targets we cannot place.
var Names = []string{
	Africa, Antarctica, Asia, Europe, NorthAmerica, Oceania, SouthAmerica, Unknown,
}

// ForCountry maps a two-letter country/ccTLD code to a continent. Lowercase and
// surrounding whitespace are normalised because config and query strings do
// not always agree on case. Empty and unmapped codes are Unknown.
func ForCountry(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if name, ok := byCountry[code]; ok {
		return name
	}
	return Unknown
}

// Normalise validates a region filter value. It returns the canonical name or
// the empty string for a value that is not a known region, so an invalid query
// is ignored rather than silently becoming a different filter.
func Normalise(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, name := range Names {
		if strings.EqualFold(raw, name) {
			return name
		}
	}
	// Accept the compact form used in URLs ("North America" -> north-america)
	// as well as the plain name, without accepting arbitrary text.
	for _, name := range Names {
		if strings.EqualFold(strings.ReplaceAll(raw, "-", " "), name) {
			return name
		}
	}
	return ""
}
