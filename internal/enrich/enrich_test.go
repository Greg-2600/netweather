package enrich_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/enrich"
)

type fakeResolver struct {
	txt     map[string][]string
	cnames  map[string]string
	err     error
	queries []string
}

func (f *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	f.queries = append(f.queries, name)
	if f.err != nil {
		return nil, f.err
	}
	return f.txt[name], nil
}

func (f *fakeResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.cnames[host], nil
}

func TestASNParsesCymruAnswer(t *testing.T) {
	t.Parallel()

	r := &fakeResolver{txt: map[string][]string{
		"1.1.1.1.origin.asn.cymru.com": {
			"13335 | 1.1.1.0/24 | US | arin | 2010-07-14",
		},
	}}

	asn, prefix, err := enrich.ASN(context.Background(), r, netip.MustParseAddr("1.1.1.1"))
	if err != nil {
		t.Fatalf("ASN: %v", err)
	}
	if asn != 13335 || prefix != "1.1.1.0/24" {
		t.Fatalf("got AS%d %q", asn, prefix)
	}
}

func TestASNIPv6ReversesNibbles(t *testing.T) {
	t.Parallel()

	addr := netip.MustParseAddr("2606:4700::1111")
	bytes := addr.As16()

	var nibbles []string
	for _, b := range bytes {
		nibbles = append(nibbles, fmt.Sprintf("%x", b>>4), fmt.Sprintf("%x", b&0x0f))
	}
	for i, j := 0, len(nibbles)-1; i < j; i, j = i+1, j-1 {
		nibbles[i], nibbles[j] = nibbles[j], nibbles[i]
	}
	query := strings.Join(nibbles, ".") + ".origin6.asn.cymru.com"

	r := &fakeResolver{txt: map[string][]string{
		query: {"13335 | 2606:4700::/32 | US | arin | 2014-03-28"},
	}}

	asn, prefix, err := enrich.ASN(context.Background(), r, addr)
	if err != nil {
		t.Fatalf("ASN: %v", err)
	}
	if asn != 13335 || !strings.HasPrefix(prefix, "2606:4700") {
		t.Fatalf("got AS%d %q", asn, prefix)
	}
	if len(r.queries) != 1 || r.queries[0] != query {
		t.Fatalf("queries = %v, want %q", r.queries, query)
	}
}

func TestASNRejectsBadAnswers(t *testing.T) {
	t.Parallel()

	r := &fakeResolver{txt: map[string][]string{
		"1.1.1.1.origin.asn.cymru.com": {"NA | NA | NA", "garbage"},
	}}
	if _, _, err := enrich.ASN(context.Background(), r, netip.MustParseAddr("1.1.1.1")); err == nil {
		t.Fatal("an unparseable answer should be an error")
	}

	empty := &fakeResolver{txt: map[string][]string{}}
	if _, _, err := enrich.ASN(context.Background(), empty, netip.MustParseAddr("1.1.1.1")); err == nil {
		t.Fatal("an empty answer should be an error")
	}

	if _, _, err := enrich.ASN(context.Background(), nil, netip.MustParseAddr("1.1.1.1")); err == nil {
		t.Fatal("a nil resolver should be an error")
	}
	if _, _, err := enrich.ASN(context.Background(), r, netip.Addr{}); err == nil {
		t.Fatal("an invalid address should be an error")
	}

	failing := &fakeResolver{err: errors.New("servfail")}
	if _, _, err := enrich.ASN(context.Background(), failing, netip.MustParseAddr("1.1.1.1")); err == nil {
		t.Fatal("a resolver failure should surface")
	}
}

func TestCDNMapping(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"d123.cloudfront.net.":    "cloudfront",
		"e1.akamaiedge.net":       "akamai",
		"site.fastly.net":         "fastly",
		"app.cloudflare.net":      "cloudflare",
		"x.azureedge.net":         "azure",
		"y.googleusercontent.com": "google",
		"cdn.cdn77.org":           "cdn77",
		"www.example.com":         "",
		"not-a-cdn.example":       "",
		"":                        "",
	}
	for cname, want := range tests {
		if got := enrich.CDN(cname); got != want {
			t.Errorf("CDN(%q) = %q, want %q", cname, got, want)
		}
	}
}

func TestHostCombinesASNAndCDN(t *testing.T) {
	t.Parallel()

	r := &fakeResolver{
		txt: map[string][]string{
			"93.184.216.34.origin.asn.cymru.com": {"15133 | 93.184.216.0/24 | US | arin | 2008-01-01"},
		},
		cnames: map[string]string{"www.example.com": "example.com.edgekey.net"},
	}

	got, err := enrich.Host(context.Background(), r, "www.example.com",
		[]netip.Addr{netip.MustParseAddr("93.184.216.34")})
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if got.ASN != 15133 || got.Prefix != "93.184.216.0/24" || got.CDN != "akamai" {
		t.Fatalf("result = %+v", got)
	}
}

func TestHostWithDirectRecordsLeavesCDNEmpty(t *testing.T) {
	t.Parallel()

	r := &fakeResolver{
		txt: map[string][]string{
			"93.184.216.34.origin.asn.cymru.com": {"15133 | 93.184.216.0/24 | US | arin | 2008-01-01"},
		},
		cnames: map[string]string{"example.com": "example.com."},
	}

	got, err := enrich.Host(context.Background(), r, "example.com",
		[]netip.Addr{netip.MustParseAddr("93.184.216.34")})
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if got.CDN != "" {
		t.Fatalf("direct record produced CDN %q", got.CDN)
	}
}

func TestHostRequiresAddresses(t *testing.T) {
	t.Parallel()

	if _, err := enrich.Host(context.Background(), &fakeResolver{}, "example.com", nil); err == nil {
		t.Fatal("no addresses should be an error")
	}
}
