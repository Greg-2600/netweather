package seed_test

import (
	"testing"

	"github.com/Greg-2600/netweather/internal/schedule"
	"github.com/Greg-2600/netweather/internal/seed"
)

func TestParseCountryAnchors(t *testing.T) {
	t.Parallel()

	// A root zone is a master file: comments, directives, tabs, and optional
	// TTL/class fields. Only ccTLD NS delegations are countries.
	zone := `; IANA root zone fixture
$TTL 172800
$ORIGIN .
.			172800	IN	NS	a.root-servers.net.
ad.			172800	IN	NS	ns2.nic.ad.
ad.			172800	IN	NS	ns1.nic.ad.
ae.			172800	IN	NS	ns1.aedns.ae.
com.			172800	IN	NS	a.gtld-servers.net.
uk.			172800	IN	NS	nsb.nic.uk.
uk.			172800	IN	NS	nsa.nic.uk.
example.com.		172800	IN	NS	ns1.example.com.
xn--p1ai.		172800	IN	NS	ns1.nic.xn--p1ai.
`

	got := seed.ParseCountryAnchors(zone)
	want := []struct {
		tld  string
		host string
	}{
		{"ad", "ns1.nic.ad"},
		{"ae", "ns1.aedns.ae"},
		{"uk", "nsa.nic.uk"},
		{"xn--p1ai", "ns1.nic.xn--p1ai"},
	}
	if len(got) != len(want) {
		t.Fatalf("anchors = %+v, want %d", got, len(want))
	}
	for i, w := range want {
		e := got[i]
		if e.ID != "cctld-"+w.tld || e.Host != w.host || e.Query != w.tld {
			t.Errorf("anchor %d = %+v, want tld %q host %q", i, e, w.tld, w.host)
		}
		if e.Port != 53 || e.Probe != "dns" || e.Class != "country" || e.Tier != schedule.TierAnchor {
			t.Errorf("anchor %d = %+v, want a DNS country anchor", i, e)
		}
	}

	// The nameserver choice is deterministic, not file order: ns1 before ns2
	// and nsa before nsb.
	if got[0].Host != "ns1.nic.ad" || got[2].Host != "nsa.nic.uk" {
		t.Fatalf("nameserver choice is not deterministic: %+v", got)
	}
}

func TestParseCountryAnchorsSkipsNonCountries(t *testing.T) {
	t.Parallel()

	zone := `; only generic and infrastructure delegations
.			172800	IN	NS	a.root-servers.net.
com.			172800	IN	NS	a.gtld-servers.net.
arpa.			172800	IN	NS	a.root-servers.net.
a.root-servers.net.	172800	IN	A	198.41.0.4
`
	if got := seed.ParseCountryAnchors(zone); len(got) != 0 {
		t.Fatalf("anchors = %+v, want none", got)
	}

	// A malformed NS line without a dotted nameserver is not a target.
	bad := "ad.\t172800\tIN\tNS\tbroken\n"
	if got := seed.ParseCountryAnchors(bad); len(got) != 0 {
		t.Fatalf("anchors = %+v, want none for a bad NS", got)
	}
}
