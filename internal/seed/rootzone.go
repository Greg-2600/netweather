package seed

import (
	"sort"
	"strings"

	"github.com/Greg-2600/netweather/internal/schedule"
)

// RootZoneURL is the IANA root zone file. The design's country-anchor seed
// (DESIGN.md §4.2) reads the ccTLD delegations from it: every country and
// territory has a stable, published NS set, and a DNS probe costs one UDP round
// trip.
const RootZoneURL = "https://www.internic.net/domain/root.zone"

// ParseCountryAnchors extracts one DNS target per ccTLD from a root zone file.
//
// A root zone is a master file, so the parser accepts the shapes that matter
// for NS records: tabs or spaces, an optional TTL and class, `;` comments, and
// `$TTL`/`$ORIGIN` directives (which are not NS records and are skipped).
// Multi-line parenthesised records are not NS delegations and are skipped line
// by line; an NS record is always one line.
//
// Only two-letter TLDs and IDN ccTLDs (`xn--…`) are countries. `com`, `arpa`,
// the root, and every multi-label name are ignored. The result is deterministic:
// sorted by TLD, one entry per territory, with the lexicographically first
// nameserver so two runs against the same zone produce the same list.
func ParseCountryAnchors(body string) []Entry {
	// tld -> chosen nameserver
	chosen := make(map[string]string)

	for _, raw := range strings.Split(body, "\n") {
		line := raw
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if strings.EqualFold(fields[0], "$TTL") || strings.EqualFold(fields[0], "$ORIGIN") {
			continue
		}

		name := strings.TrimSuffix(strings.ToLower(fields[0]), ".")
		// The record type is the last token before the RDATA, but a TTL and
		// class are optional, so look for "NS" and take the next field as the
		// nameserver.
		ns := ""
		for i := 1; i < len(fields)-1; i++ {
			if strings.EqualFold(fields[i], "NS") {
				ns = strings.TrimSuffix(strings.ToLower(fields[i+1]), ".")
				break
			}
		}
		if ns == "" || !strings.Contains(ns, ".") {
			continue
		}
		if !validNSName(ns) {
			continue
		}
		if !isCountryTLD(name) {
			continue
		}
		if current, ok := chosen[name]; !ok || ns < current {
			chosen[name] = ns
		}
	}

	tlds := make([]string, 0, len(chosen))
	for tld := range chosen {
		tlds = append(tlds, tld)
	}
	sort.Strings(tlds)

	out := make([]Entry, 0, len(tlds))
	for _, tld := range tlds {
		out = append(out, Entry{
			ID:     "cctld-" + tld,
			Name:   CountryAnchorName(tld),
			Host:   chosen[tld],
			Port:   53,
			Probe:  "dns",
			Query:  tld,
			Class:  "country",
			Sector: "country",
			Tier:   schedule.TierAnchor,
		})
	}
	return out
}

// isCountryTLD reports whether a root-zone owner name is a country-code TLD.
// `ad` and `uk` are; `com` and `arpa` are longer, so the two-letter rule
// excludes generic and infrastructure TLDs without a hand-maintained list.
// IDN ccTLDs are punycode labels, which are two characters plus an `xn--`
// prefix and are equally a territory.
func isCountryTLD(name string) bool {
	if strings.Contains(name, ".") || name == "" {
		return false
	}
	if strings.HasPrefix(name, "xn--") {
		return true
	}
	if len(name) != 2 {
		return false
	}
	for _, r := range name {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// validNSName accepts the hostname shape a root-zone NS RDATA can take: one or
// more dot-separated labels of letters, digits, and hyphens, with no leading or
// trailing hyphen, at most 253 characters. The root zone is external input, so
// a malformed nameserver is skipped rather than handed to the scheduler
// (THREAT_MODEL.md T33).
func validNSName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}
