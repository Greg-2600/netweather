package seed_test

import (
	"testing"

	"github.com/Greg-2600/netweather/internal/schedule"
	"github.com/Greg-2600/netweather/internal/seed"
)

func TestDefaultSeedValidates(t *testing.T) {
	t.Parallel()

	if err := seed.Validate(seed.Default()); err != nil {
		t.Fatalf("the shipped list does not validate: %v", err)
	}
}

func TestCountryAnchorsValidate(t *testing.T) {
	t.Parallel()

	// The generated anchors go through the same apply path as the shipped
	// list, so the two must validate with the same rules. Query is a bare TLD
	// here, which is legal for a DNS question even though it is not a host.
	zone := "ad.\t172800\tIN\tNS\tns1.nic.ad.\n" +
		"uk.\t172800\tIN\tNS\tns1.nic.uk.\n"
	entries := seed.ParseCountryAnchors(zone)
	if len(entries) != 2 {
		t.Fatalf("anchors = %+v", entries)
	}
	if err := seed.Validate(entries); err != nil {
		t.Fatalf("generated anchors do not validate: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	t.Parallel()

	valid := func() seed.Entry {
		return seed.Entry{
			ID: "svc-example", Name: "Example", Host: "example.com",
			Class: "service", Sector: "reference", Tier: schedule.TierService,
		}
	}

	cases := []struct {
		name   string
		mutate func(*seed.Entry)
	}{
		{"empty id", func(e *seed.Entry) { e.ID = "" }},
		{"upper-case id", func(e *seed.Entry) { e.ID = "Svc-Example" }},
		{"id with a slash", func(e *seed.Entry) { e.ID = "svc/example" }},
		{"missing name", func(e *seed.Entry) { e.Name = "" }},
		{"unknown class", func(e *seed.Entry) { e.Class = "telco" }},
		{"tier too high", func(e *seed.Entry) { e.Tier = schedule.Tier(4) }},
		{"tier negative", func(e *seed.Entry) { e.Tier = schedule.Tier(-1) }},
		{"unknown probe", func(e *seed.Entry) { e.Probe = "quic"; e.Port = 443 }},
		{"port not valid for probe", func(e *seed.Entry) { e.Probe = "tls"; e.Port = 80 }},
		{"ip literal for a service target", func(e *seed.Entry) { e.Host = "203.0.113.9" }},
		{"wildcard host", func(e *seed.Entry) { e.Host = "*.example.com" }},
		{"userinfo in a host", func(e *seed.Entry) { e.Host = "user@example.com" }},
		{"host with no dot", func(e *seed.Entry) { e.Host = "localhost" }},
		{"host with a leading hyphen", func(e *seed.Entry) { e.Host = "-example.com" }},
		{"host with an empty label", func(e *seed.Entry) { e.Host = "example..com" }},
		{"dns without a query", func(e *seed.Entry) {
			e.ID = "net-dns"
			e.Host = "198.41.0.4"
			e.Class = "network"
			e.Probe = "dns"
			e.Port = 53
		}},
		{"query on an http target", func(e *seed.Entry) { e.Query = "example.com" }},
		{"plaintext url", func(e *seed.Entry) { e.URL = "http://example.com/" }},
		{"bad query", func(e *seed.Entry) {
			e.ID = "net-dns"
			e.Host = "198.41.0.4"
			e.Class = "network"
			e.Probe = "dns"
			e.Port = 53
			e.Query = "not a name"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entry := valid()
			tc.mutate(&entry)
			if err := seed.Validate([]seed.Entry{entry}); err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
		})
	}
}

func TestValidateRejectsDuplicates(t *testing.T) {
	t.Parallel()

	entry := seed.Entry{
		ID: "svc-example", Name: "Example", Host: "example.com",
		Class: "service", Sector: "reference", Tier: schedule.TierService,
	}

	// Two rows with the same id would overwrite each other in the store.
	second := entry
	second.Host = "other.example"
	if err := seed.Validate([]seed.Entry{entry, second}); err == nil {
		t.Fatal("a duplicate id was accepted")
	}

	// Two ids for the same natural key are the same target with two names.
	second = entry
	second.ID = "svc-example-two"
	if err := seed.Validate([]seed.Entry{entry, second}); err == nil {
		t.Fatal("a duplicate target was accepted")
	}
}

func TestValidateAllowsSharedDNSWithDifferentQuestions(t *testing.T) {
	t.Parallel()

	uk := seed.Entry{
		ID: "cctld-uk", Name: "UK ccTLD", Host: "fork.sth.dnsnode.net",
		Port: 53, Probe: "dns", Query: "uk",
		Class: "country", Sector: "country", Tier: schedule.TierAnchor,
	}
	de := uk
	de.ID, de.Name, de.Query = "cctld-de", "DE ccTLD", "de"
	if err := seed.Validate([]seed.Entry{uk, de}); err != nil {
		t.Fatalf("two ccTLD anchors on one nameserver were rejected: %v", err)
	}

	// The same question twice is still a duplicate.
	dup := uk
	dup.ID = "cctld-uk-2"
	if err := seed.Validate([]seed.Entry{uk, dup}); err == nil {
		t.Fatal("a duplicate dns question was accepted")
	}
}

func TestValidateAcceptsAnAnchorIP(t *testing.T) {
	t.Parallel()

	// A non-service class may use an IP literal: the root servers are the
	// canonical example, and they are not a CDN edge.
	entry := seed.Entry{
		ID: "net-root-a", Name: "Root A", Host: "198.41.0.4", Port: 53,
		Probe: "dns", Query: "example.com", Class: "network", Tier: schedule.TierAnchor,
	}
	if err := seed.Validate([]seed.Entry{entry}); err != nil {
		t.Fatalf("a network anchor with an IP literal was rejected: %v", err)
	}
}
