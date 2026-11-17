// Package enrich learns the facts the config cannot know: the origin ASN and
// prefix behind a hostname, and whether its CNAME chain ends at a known CDN.
//
// It is deliberately conservative: an unknown answer is better than a guessed
// one, because attribution and coverage both key off these values.
package enrich

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
)

// Resolver is the DNS surface enrichment needs. *net.Resolver satisfies it.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
}

// Team Cymru's origin lookup. The TXT answer looks like:
//
//	"13335 | 1.1.1.0/24 | US | arin | 2010-07-14"
//
// The first two fields are the ASN and the covering prefix.
const (
	originV4Zone = "origin.asn.cymru.com"
	originV6Zone = "origin6.asn.cymru.com"
)

// ASN looks up the origin ASN and prefix for one address.
func ASN(ctx context.Context, r Resolver, addr netip.Addr) (int, string, error) {
	if r == nil {
		return 0, "", fmt.Errorf("enrich: resolver is not configured")
	}
	if !addr.IsValid() {
		return 0, "", fmt.Errorf("enrich: invalid address")
	}

	a := addr.Unmap()
	zone := originV4Zone
	if a.Is6() {
		zone = originV6Zone
	}
	query := reversed(a.String()) + "." + zone

	answers, err := r.LookupTXT(ctx, query)
	if err != nil {
		return 0, "", fmt.Errorf("enrich: asn lookup %s: %w", a, err)
	}
	for _, answer := range answers {
		if asn, prefix, ok := parseOrigin(answer); ok {
			return asn, prefix, nil
		}
	}
	return 0, "", fmt.Errorf("enrich: no origin answer for %s", a)
}

// parseOrigin extracts the ASN and prefix from one Cymru TXT answer.
func parseOrigin(answer string) (int, string, bool) {
	fields := strings.Split(answer, "|")
	if len(fields) < 2 {
		return 0, "", false
	}

	asnText := strings.TrimSpace(fields[0])
	prefix := strings.TrimSpace(fields[1])
	if asnText == "" || prefix == "" || asnText == "NA" {
		return 0, "", false
	}

	asn := 0
	for _, ch := range asnText {
		if ch < '0' || ch > '9' {
			return 0, "", false
		}
		asn = asn*10 + int(ch-'0')
	}
	if asn == 0 {
		return 0, "", false
	}
	return asn, prefix, true
}

// reversed turns an address into the zone-label order Cymru expects: IPv4 in
// place, IPv6 as the 32 nibbles of the expanded address, reversed.
func reversed(addr string) string {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return addr
	}
	if a.Is4() {
		return a.String()
	}

	bytes := a.As16()
	nibbles := make([]string, 0, 32)
	for i := len(bytes) - 1; i >= 0; i-- {
		nibbles = append(nibbles, fmt.Sprintf("%x", bytes[i]&0x0f))
		nibbles = append(nibbles, fmt.Sprintf("%x", bytes[i]>>4))
	}
	return strings.Join(nibbles, ".")
}

// CDN maps a CNAME chain to a known CDN. An empty result means "not a CDN we
// recognise", never "no CDN": direct A records and private CDNs are invisible
// to this method.
func CDN(cname string) string {
	c := strings.ToLower(strings.TrimSuffix(cname, "."))
	for suffix, name := range cdnSuffixes {
		if strings.HasSuffix(c, suffix) {
			return name
		}
	}
	return ""
}

var cdnSuffixes = map[string]string{
	"cloudflare.net":         "cloudflare",
	"cloudflare.com":         "cloudflare",
	"akamai.net":             "akamai",
	"akamaiedge.net":         "akamai",
	"edgekey.net":            "akamai",
	"edgesuite.net":          "akamai",
	"akamaitechnologies.com": "akamai",
	"cloudfront.net":         "cloudfront",
	"fastly.net":             "fastly",
	"fastlylb.net":           "fastly",
	"azureedge.net":          "azure",
	"trafficmanager.net":     "azure",
	"googleusercontent.com":  "google",
	"googlehosted.com":       "google",
	"cdn77.org":              "cdn77",
}

// Result is what enrichment learned about one target.
type Result struct {
	ASN       int
	Prefix    string
	CDN       string
	ASCountry string
	ASName    string
}

// Org looks up the organisation name and country registered for an ASN.
//
// The TXT answer looks like:
//
//	"13335 | US | arin | 2010-07-14 | CLOUDFLARENET, US"
//
// Field order matters: the name is last and may itself contain commas.
func Org(ctx context.Context, r Resolver, asn int) (name, country string, err error) {
	if r == nil {
		return "", "", fmt.Errorf("enrich: resolver is not configured")
	}
	if asn <= 0 {
		return "", "", fmt.Errorf("enrich: ASN %d is not positive", asn)
	}

	answers, err := r.LookupTXT(ctx, fmt.Sprintf("AS%d.asn.cymru.com", asn))
	if err != nil {
		return "", "", fmt.Errorf("enrich: org lookup AS%d: %w", asn, err)
	}
	for _, answer := range answers {
		if n, c, ok := parseOrg(answer); ok {
			return n, c, nil
		}
	}
	return "", "", fmt.Errorf("enrich: no organisation answer for AS%d", asn)
}

func parseOrg(answer string) (name, country string, ok bool) {
	fields := strings.Split(answer, "|")
	if len(fields) < 2 {
		return "", "", false
	}
	country = strings.ToUpper(strings.TrimSpace(fields[1]))
	if country == "" || country == "NA" {
		country = ""
	}
	if len(fields) >= 5 {
		name = strings.TrimSpace(strings.Join(fields[4:], "|"))
	}
	return name, country, true
}

// Host resolves the CNAME and the first address's origin. A failure to learn
// the CNAME is not fatal: direct A records simply leave CDN empty. The same
// applies to the organisation lookup: the ASN is the fact that matters.
func Host(ctx context.Context, r Resolver, host string, addrs []netip.Addr) (Result, error) {
	if len(addrs) == 0 {
		return Result{}, fmt.Errorf("enrich: no addresses for %s", host)
	}

	asn, prefix, err := ASN(ctx, r, addrs[0])
	if err != nil {
		return Result{}, err
	}

	out := Result{ASN: asn, Prefix: prefix}
	if name, country, err := Org(ctx, r, asn); err == nil {
		out.ASName = name
		out.ASCountry = country
	}
	if cname, err := r.LookupCNAME(ctx, host); err == nil {
		out.CDN = CDN(cname)
	}
	return out, nil
}
