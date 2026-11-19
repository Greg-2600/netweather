package seed

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
)

// cctldNames maps a root-zone TLD label to its English display name. The
// two-letter names come from CLDR (via golang.org/x/text/language/display) and
// the internationalised names from IANA's root zone database (the "Translation"
// and "Country Code" fields, plus the Unicode form of the A-label), captured
// 2026-11-19. It is embedded so a seed run is deterministic and offline.
//
// Regenerate with scripts/gen_cctld_names.py (which calls the small Go
// generator that has the CLDR dependency) when the IANA delegation set changes.
//
//go:embed cctld_names.json
var cctldNamesFS embed.FS

var cctldNames = func() map[string]string {
	b, err := cctldNamesFS.ReadFile("cctld_names.json")
	if err != nil {
		panic(fmt.Sprintf("seed: embedded ccTLD names: %v", err))
	}
	var names map[string]string
	if err := json.Unmarshal(b, &names); err != nil {
		panic(fmt.Sprintf("seed: decode ccTLD names: %v", err))
	}
	return names
}()

// CountryName returns a human-readable name for a ccTLD label: "np" becomes
// "Nepal", "xn--wgbh1c" becomes "Egypt (مصر)". Unknown labels fall back to the
// upper-case label so a newly delegated TLD still produces a usable target.
func CountryName(tld string) string {
	if name, ok := cctldNames[strings.ToLower(strings.TrimSuffix(tld, "."))]; ok && name != "" {
		return name
	}
	return strings.ToUpper(tld)
}

// CountryAnchorName is the full target name used by the country-anchor seed:
// "Nepal ccTLD" rather than the opaque "NP ccTLD".
func CountryAnchorName(tld string) string {
	return CountryName(tld) + " ccTLD"
}
