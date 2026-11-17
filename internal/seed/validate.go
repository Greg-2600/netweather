package seed

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/Greg-2600/netweather/internal/schedule"
)

// idRE is the shape a seeded target id may take. The id is a stable key that
// appears in URLs and history, so it is never silently rewritten.
var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// probes maps a probe name to the ports it may use. An empty probe means the
// shipped HTTP-on-443 default, which is applied by the caller.
var probes = map[string]map[int]struct{}{
	"http":    {80: {}, 443: {}},
	"tcp":     {80: {}, 443: {}},
	"tcpping": {80: {}, 443: {}},
	"tls":     {443: {}},
	"dns":     {53: {}},
	"doh":     {443: {}},
	"ntp":     {123: {}},
	"icmp":    {0: {}},
}

var classes = map[string]struct{}{
	"service": {}, "country": {}, "network": {}, "reference": {},
}

// Validate checks a seed list before any row is written. A seed is untrusted
// input (THREAT_MODEL.md T33): the built-in list is code, but the ccTLD anchors
// come from a fetched root zone, and a future entry may come from a source this
// package does not control. The rules are the ones the threat model names:
// hostname syntax, no wildcards or userinfo, no IP literal for a service
// target, an allowlisted port per probe, unique ids, and a class and tier the
// rest of the pipeline understands.
//
// The cap is checked by the caller before this runs, because an oversized list
// is refused wholesale rather than validated entry by entry.
func Validate(entries []Entry) error {
	seenIDs := make(map[string]struct{}, len(entries))
	seenTargets := make(map[string]struct{}, len(entries))

	for i, e := range entries {
		where := fmt.Sprintf("seed entry %d", i)
		if e.ID != "" {
			where = fmt.Sprintf("seed entry %q", e.ID)
		}

		if !idRE.MatchString(e.ID) {
			return fmt.Errorf("%s: id %q is not a lower-case slug", where, e.ID)
		}
		if _, ok := seenIDs[e.ID]; ok {
			return fmt.Errorf("%s: duplicate id", where)
		}
		seenIDs[e.ID] = struct{}{}

		if e.Name == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		if _, ok := classes[e.Class]; !ok {
			return fmt.Errorf("%s: class %q is not one of service, country, network, reference", where, e.Class)
		}
		if e.Tier < schedule.TierReference || e.Tier > schedule.TierAnchor {
			return fmt.Errorf("%s: tier %d is not 0-3", where, e.Tier)
		}

		probe := e.Probe
		if probe == "" {
			probe = "http"
		}
		// The apply path defaults a zero port to 443, so validation must judge
		// the port that will actually be probed, not the zero that stands for
		// "use the default". icmp keeps 0, because it has no port.
		effectivePort := e.Port
		if effectivePort == 0 && probe != "icmp" {
			effectivePort = 443
		}
		allowed, ok := probes[probe]
		if !ok {
			return fmt.Errorf("%s: probe %q is not supported", where, probe)
		}
		if _, ok := allowed[effectivePort]; !ok {
			return fmt.Errorf("%s: port %d is not valid for probe %q", where, effectivePort, probe)
		}

		if err := validateHost(where, e.Host, e.Class); err != nil {
			return err
		}

		switch probe {
		case "dns", "doh":
			if strings.TrimSpace(e.Query) == "" {
				return fmt.Errorf("%s: %s targets require a query", where, probe)
			}
			if !validQueryName(e.Query) {
				return fmt.Errorf("%s: query %q is not a DNS name", where, e.Query)
			}
		default:
			if strings.TrimSpace(e.Query) != "" {
				return fmt.Errorf("%s: query is only valid for dns and doh targets", where)
			}
		}

		if e.URL != "" && !strings.HasPrefix(strings.ToLower(e.URL), "https://") {
			return fmt.Errorf("%s: url %q is not HTTPS", where, e.URL)
		}

		key := e.Host + "|" + strconv.Itoa(e.Port) + "|" + probe
		if _, ok := seenTargets[key]; ok {
			return fmt.Errorf("%s: duplicate target %s", where, key)
		}
		seenTargets[key] = struct{}{}
	}
	return nil
}

// validQueryName accepts a DNS name a dns or doh target can ask about. Unlike a
// host, a single-label name is legal here: the ccTLD anchors ask about "ad" and
// "uk", and the root zone has no dot to require.
func validQueryName(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
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

// validateHost accepts a hostname, or a bare IP for a class other than service.
// A service target is always a name: an IP literal would pin one edge of a CDN
// and measure the wrong thing (THREAT_MODEL.md T33).
func validateHost(where, host, class string) error {
	if host == "" {
		return fmt.Errorf("%s: host is required", where)
	}
	if strings.ContainsAny(host, "*@/ ") {
		return fmt.Errorf("%s: host %q contains a wildcard, userinfo, or whitespace", where, host)
	}

	if addr, err := netip.ParseAddr(host); err == nil {
		if class == "service" {
			return fmt.Errorf("%s: a service target must use a hostname, not the IP literal %s", where, addr)
		}
		return nil
	}
	if !validHostname(host) {
		return fmt.Errorf("%s: host %q is not a hostname", where, host)
	}
	return nil
}

// validHostname is the syntax rule for a seed host. Labels are letters, digits,
// and hyphens, at most 63 bytes, with no leading or trailing hyphen; the whole
// name is at most 253 bytes and has at least one dot.
func validHostname(name string) bool {
	if len(name) == 0 || len(name) > 253 || !strings.Contains(name, ".") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
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
