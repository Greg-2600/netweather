package vantagegeo_test

import (
	"testing"

	"github.com/Greg-2600/netweather/internal/vantagegeo"
)

func TestFromVantage(t *testing.T) {
	cases := map[string]string{
		"tor-de":      vantagegeo.Europe,
		"tor-jp":      vantagegeo.Asia,
		"tor-au":      vantagegeo.Oceania,
		"tor-sg":      vantagegeo.Asia,
		"tor-cl":      vantagegeo.SouthAmerica,
		"tor-mx":      vantagegeo.NorthAmerica,
		"tor-za":      vantagegeo.Africa,
		"local":       vantagegeo.Unknown,
		"tor":         vantagegeo.Unknown,
		"tor-us-west": vantagegeo.Unknown,
		"":            vantagegeo.Unknown,
		"tor-qq":      vantagegeo.Unknown,
	}
	for id, want := range cases {
		if got := vantagegeo.FromVantage(id); got != want {
			t.Errorf("FromVantage(%q) = %q, want %q", id, got, want)
		}
	}
	if got := vantagegeo.Country("tor-de"); got != "DE" {
		t.Errorf("Country(tor-de) = %q", got)
	}
	for _, id := range []string{"local", "tor", "tor-qq"} {
		if got := vantagegeo.Country(id); got != "" {
			t.Errorf("Country(%q) = %q, want empty", id, got)
		}
	}
}
