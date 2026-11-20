// Package vantagegeo labels probe origins with the continent of their egress
// country. It exists so the UI and API can say where an observation came from
// without adding geography to the vantage schema: the country is already
// verified through the proxy at startup, and the ID carries it (tor-de, ...).
//
// The mapping is deliberately small and explicit. An unknown code is reported
// as unknown, never guessed from a hostname or an operator's intention — the
// same rule the exit-country verification follows (ACCEPTABLE_USE.md §8).
package vantagegeo

import "strings"

// Continent names are the seven-continent model, matching the corridor and
// world-view language in DESIGN.md.
const (
	Africa       = "Africa"
	Asia         = "Asia"
	Europe       = "Europe"
	NorthAmerica = "North America"
	Oceania      = "Oceania"
	SouthAmerica = "South America"
	Unknown      = "Unknown"
)

var continents = map[string]string{
	// North America
	"US": NorthAmerica, "CA": NorthAmerica, "MX": NorthAmerica,
	// South America
	"AR": SouthAmerica, "BR": SouthAmerica, "CL": SouthAmerica,
	"CO": SouthAmerica, "PE": SouthAmerica, "UY": SouthAmerica,
	"VE": SouthAmerica, "EC": SouthAmerica,
	// Europe
	"AT": Europe, "BE": Europe, "BG": Europe, "CH": Europe, "CZ": Europe,
	"DE": Europe, "DK": Europe, "EE": Europe, "ES": Europe, "FI": Europe,
	"FR": Europe, "GB": Europe, "GR": Europe, "HR": Europe, "HU": Europe,
	"IE": Europe, "IS": Europe, "IT": Europe, "LT": Europe, "LU": Europe,
	"LV": Europe, "NL": Europe, "NO": Europe, "PL": Europe, "PT": Europe,
	"RO": Europe, "SE": Europe, "SI": Europe, "SK": Europe, "UA": Europe,
	// Asia
	"AE": Asia, "AM": Asia, "BD": Asia, "CN": Asia, "GE": Asia, "HK": Asia,
	"ID": Asia, "IL": Asia, "IN": Asia, "JP": Asia, "KR": Asia, "KZ": Asia,
	"LK": Asia, "MY": Asia, "PH": Asia, "PK": Asia, "SA": Asia, "SG": Asia,
	"TH": Asia, "TR": Asia, "TW": Asia, "VN": Asia,
	// Africa
	"EG": Africa, "ET": Africa, "GH": Africa, "KE": Africa, "MA": Africa,
	"NG": Africa, "SN": Africa, "TN": Africa, "ZA": Africa,
	// Oceania
	"AU": Oceania, "NZ": Oceania,
}

// FromVantage returns the continent for a vantage id such as "tor-de", or the
// country code itself for a bare "DE". Unpinned/local vantages have no country
// and report Unknown rather than assuming the operator's location.
func FromVantage(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return Unknown
	}
	cc := id
	if strings.HasPrefix(strings.ToLower(id), "tor-") {
		cc = id[len("tor-"):]
	}
	cc = strings.ToUpper(strings.TrimSpace(cc))
	if len(cc) != 2 {
		return Unknown
	}
	if name, ok := continents[cc]; ok {
		return name
	}
	return Unknown
}

// Country returns the verified two-letter egress code carried by a vantage id,
// or an empty string for an unpinned vantage.
func Country(id string) string {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(strings.ToLower(id), "tor-") {
		return ""
	}
	cc := strings.ToUpper(strings.TrimSpace(id[len("tor-"):]))
	if len(cc) != 2 {
		return ""
	}
	// Only a code we can place is a verified country label. An arbitrary
	// two-letter suffix is not evidence of geography.
	if _, ok := continents[cc]; !ok {
		return ""
	}
	return cc
}
