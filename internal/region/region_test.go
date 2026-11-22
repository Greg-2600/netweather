package region_test

import (
	"testing"

	"github.com/Greg-2600/netweather/internal/region"
	"github.com/Greg-2600/netweather/internal/vantagegeo"
)

func TestForCountry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code string
		want string
	}{
		{"US", region.NorthAmerica},
		{"de", region.Europe}, // lowercase config/query values are normalised
		{" JP ", region.Asia},
		{"BR", region.SouthAmerica},
		{"ZA", region.Africa},
		{"AU", region.Oceania},
		{"AQ", region.Antarctica},
		// ccTLD aliases that Natural Earth does not carry as features.
		{"UK", region.Europe},
		{"AC", region.Africa},
		{"GF", region.SouthAmerica},
		// Not a place: reported, never folded into a real continent.
		{"XN", region.Unknown},
		{"SU", region.Unknown},
		{"", region.Unknown},
		{"ZZ", region.Unknown},
		{"USA", region.Unknown}, // not a two-letter code
	}
	for _, tc := range cases {
		if got := region.ForCountry(tc.code); got != tc.want {
			t.Errorf("ForCountry(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

func TestNormalise(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"europe":         region.Europe,
		"EUROPE":         region.Europe,
		" north america": region.NorthAmerica,
		"north-america":  region.NorthAmerica,
		"unknown":        region.Unknown,
		"":               "",
		"Europe, Asia":   "",
		"mars":           "",
	}
	for raw, want := range cases {
		if got := region.Normalise(raw); got != want {
			t.Errorf("Normalise(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestNamesStayInStepWithVantageGeography(t *testing.T) {
	t.Parallel()

	// The region lens and the vantage labels must use the same words; a
	// divergent continent name would make the two views disagree silently.
	for _, name := range region.Names {
		switch name {
		case region.Africa:
			if name != vantagegeo.Africa {
				t.Error("Africa vocabulary diverged")
			}
		case region.Asia:
			if name != vantagegeo.Asia {
				t.Error("Asia vocabulary diverged")
			}
		case region.Europe:
			if name != vantagegeo.Europe {
				t.Error("Europe vocabulary diverged")
			}
		case region.NorthAmerica:
			if name != vantagegeo.NorthAmerica {
				t.Error("North America vocabulary diverged")
			}
		case region.Oceania:
			if name != vantagegeo.Oceania {
				t.Error("Oceania vocabulary diverged")
			}
		case region.SouthAmerica:
			if name != vantagegeo.SouthAmerica {
				t.Error("South America vocabulary diverged")
			}
		case region.Unknown:
			if name != vantagegeo.Unknown {
				t.Error("Unknown vocabulary diverged")
			}
		case region.Antarctica:
			// vantagegeo has no Antarctic exits, so this name is local.
		default:
			t.Errorf("unexpected region name %q", name)
		}
	}
}
