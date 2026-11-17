// Package seed is the built-in starter target list. It is deliberately small:
// the project ships a working board, and `netweather seed` grows it later.
//
// Every entry is a public, unauthenticated endpoint whose availability is a
// public-interest signal. None of them requires a login or accepts terms per
// request (ACCEPTABLE_USE.md §5).
package seed

import "github.com/Greg-2600/netweather/internal/schedule"

// Entry is one starter target. Port, Probe, and Query are zero for the shipped
// HTTP list and are set by generated entries such as the ccTLD anchors, so an
// existing entry keeps its meaning without repeating defaults.
type Entry struct {
	ID     string
	Name   string
	Host   string
	URL    string
	Class  string
	Sector string
	Tier   schedule.Tier
	Port   int
	Probe  string
	Query  string
}

// Default is the shipped starter list. It covers the sectors the product is
// about: banking, airlines, cloud, social, developer, and reference. Scale is
// opt-in through a target file; this list is a working board, not a census.
func Default() []Entry {
	return []Entry{
		{ID: "svc-chase", Name: "Chase", Host: "www.chase.com", URL: "https://www.chase.com/", Class: "service", Sector: "financial/banking", Tier: schedule.TierService},
		{ID: "svc-bofa", Name: "Bank of America", Host: "www.bankofamerica.com", URL: "https://www.bankofamerica.com/", Class: "service", Sector: "financial/banking", Tier: schedule.TierService},
		{ID: "svc-united", Name: "United", Host: "www.united.com", URL: "https://www.united.com/", Class: "service", Sector: "airline", Tier: schedule.TierService},
		{ID: "svc-delta", Name: "Delta", Host: "www.delta.com", URL: "https://www.delta.com/", Class: "service", Sector: "airline", Tier: schedule.TierService},
		{ID: "svc-cloudflare", Name: "Cloudflare", Host: "www.cloudflare.com", URL: "https://www.cloudflare.com/", Class: "service", Sector: "cloud", Tier: schedule.TierCritical},
		{ID: "svc-aws", Name: "AWS", Host: "aws.amazon.com", URL: "https://aws.amazon.com/", Class: "service", Sector: "cloud", Tier: schedule.TierCritical},
		{ID: "svc-facebook", Name: "Facebook", Host: "www.facebook.com", URL: "https://www.facebook.com/", Class: "service", Sector: "social", Tier: schedule.TierService},
		{ID: "svc-x", Name: "X", Host: "x.com", URL: "https://x.com/", Class: "service", Sector: "social", Tier: schedule.TierService},
		{ID: "svc-instagram", Name: "Instagram", Host: "www.instagram.com", URL: "https://www.instagram.com/", Class: "service", Sector: "social", Tier: schedule.TierService},
		{ID: "svc-reddit", Name: "Reddit", Host: "www.reddit.com", URL: "https://www.reddit.com/", Class: "service", Sector: "social", Tier: schedule.TierService},
		{ID: "svc-github", Name: "GitHub", Host: "github.com", URL: "https://github.com/", Class: "service", Sector: "developer", Tier: schedule.TierCritical},
		{ID: "svc-wikipedia", Name: "Wikipedia", Host: "www.wikipedia.org", URL: "https://www.wikipedia.org/", Class: "service", Sector: "reference", Tier: schedule.TierService},
		{ID: "svc-hn", Name: "Hacker News", Host: "news.ycombinator.com", URL: "https://news.ycombinator.com/", Class: "service", Sector: "news", Tier: schedule.TierService},
		{ID: "ref-example", Name: "example.com", Host: "example.com", URL: "https://example.com/", Class: "reference", Sector: "reference", Tier: schedule.TierReference},
	}
}
