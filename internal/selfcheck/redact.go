package selfcheck

import (
	"net/netip"
	"strings"
)

// RedactEgressValue hides the literal address in an egress-ip value while
// keeping the ASN and country. The stored value is the exact tuple, because
// VANTAGE_CHANGED compares it; this is a display transform for an operator who
// does not want the address on a shared screen (THREAT_MODEL.md T65).
//
// The value shape is "203.0.113.9,AS7922,US" or a bare address. Anything that
// does not begin with a parseable address is returned unchanged, so a detail
// string or an older value is never mangled.
func RedactEgressValue(value string) string {
	if value == "" {
		return value
	}
	parts := strings.Split(value, ",")
	if _, err := netip.ParseAddr(strings.TrimSpace(parts[0])); err != nil {
		return value
	}
	parts[0] = "[redacted]"
	return strings.Join(parts, ",")
}
