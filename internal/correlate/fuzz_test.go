package correlate_test

import (
	"math"
	"testing"

	"github.com/Greg-2600/netweather/internal/correlate"
)

// FuzzRank checks the attribution invariants from QUALITY.md §5.2: a share is
// never above 1, a cluster never has more failures than members, scores are
// finite, the ordering is deterministic, and ranks are a contiguous run.
func FuzzRank(f *testing.F) {
	f.Add("akamai", "airline", 13335, uint8(0b1111))
	f.Add("", "", 0, uint8(0))
	f.Add("cloudflare", "banking", 15169, uint8(0xff))
	f.Add("akamai", "", 64500, uint8(0x55))

	f.Fuzz(func(t *testing.T, cdn, sector string, asn int, failingMask uint8) {
		observations := make([]correlate.Observation, 0, 8)
		for i := 0; i < 8; i++ {
			observations = append(observations, correlate.Observation{
				TargetID: "t" + string(rune('a'+i)),
				CDN:      cdn,
				ASN:      asn,
				Country:  "US",
				Sector:   sector,
				Stage:    "tls",
				Failing:  failingMask&(1<<i) != 0,
			})
		}

		first := correlate.Rank(observations)
		second := correlate.Rank(observations)

		if len(first) != len(second) {
			t.Fatalf("non-deterministic length: %d vs %d", len(first), len(second))
		}
		for i := range first {
			if first[i] != second[i] {
				t.Fatalf("non-deterministic at %d: %+v vs %+v", i, first[i], second[i])
			}
		}

		for i, candidate := range first {
			if candidate.Failing > candidate.Total {
				t.Fatalf("failing %d > total %d", candidate.Failing, candidate.Total)
			}
			if candidate.Share < 0 || candidate.Share > 1 {
				t.Fatalf("share %v outside [0,1]", candidate.Share)
			}
			if math.IsNaN(candidate.Score) || math.IsInf(candidate.Score, 0) || candidate.Score < 0 {
				t.Fatalf("score %v is not finite and non-negative", candidate.Score)
			}
			// Size orders the candidates, so a later one must not outrank an
			// earlier one.
			if i > 0 && candidate.Score > first[i-1].Score {
				t.Fatalf("order broken at %d: %v after %v", i, candidate.Score, first[i-1].Score)
			}
		}

		if winner, ok := correlate.Winner(observations); ok && !winner.Passes() {
			t.Fatalf("winner does not pass the gates: %+v", winner)
		}
	})
}
