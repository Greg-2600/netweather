// Package seed is the built-in starter target list. It is deliberately small:
// the project ships a working board, and `netweather seed` grows it later.
//
// Every entry is a public, unauthenticated endpoint whose availability is a
// public-interest signal. None of them requires a login or accepts terms per
// request (ACCEPTABLE_USE.md §5).
package seed

import "github.com/Greg-2600/netweather/internal/schedule"

// Entry is one starter target.
type Entry struct {
	ID     string
	Name   string
	Host   string
	URL    string
	Class  string
	Sector string
	Tier   schedule.Tier
}

// Default is the shipped starter list. It covers the sectors the product is
// about: banking, airlines, cloud, social, developer, and reference. Scale is
// opt-in through a target file; this list is a working board, not a census.
func Default() []Entry {
	return []Entry{
		{"svc-chase", "Chase", "www.chase.com", "https://www.chase.com/", "service", "financial/banking", schedule.TierService},
		{"svc-bofa", "Bank of America", "www.bankofamerica.com", "https://www.bankofamerica.com/", "service", "financial/banking", schedule.TierService},
		{"svc-united", "United", "www.united.com", "https://www.united.com/", "service", "airline", schedule.TierService},
		{"svc-delta", "Delta", "www.delta.com", "https://www.delta.com/", "service", "airline", schedule.TierService},
		{"svc-cloudflare", "Cloudflare", "www.cloudflare.com", "https://www.cloudflare.com/", "service", "cloud", schedule.TierCritical},
		{"svc-aws", "AWS", "aws.amazon.com", "https://aws.amazon.com/", "service", "cloud", schedule.TierCritical},
		{"svc-facebook", "Facebook", "www.facebook.com", "https://www.facebook.com/", "service", "social", schedule.TierService},
		{"svc-x", "X", "x.com", "https://x.com/", "service", "social", schedule.TierService},
		{"svc-instagram", "Instagram", "www.instagram.com", "https://www.instagram.com/", "service", "social", schedule.TierService},
		{"svc-reddit", "Reddit", "www.reddit.com", "https://www.reddit.com/", "service", "social", schedule.TierService},
		{"svc-github", "GitHub", "github.com", "https://github.com/", "service", "developer", schedule.TierCritical},
		{"svc-wikipedia", "Wikipedia", "www.wikipedia.org", "https://www.wikipedia.org/", "service", "reference", schedule.TierService},
		{"svc-hn", "Hacker News", "news.ycombinator.com", "https://news.ycombinator.com/", "service", "news", schedule.TierService},
		{"ref-example", "example.com", "example.com", "https://example.com/", "reference", "reference", schedule.TierReference},
	}
}
