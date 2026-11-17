package correlate_test

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"testing"

	"github.com/Greg-2600/netweather/internal/correlate"
)

// TestRankRecountsEveryCluster is the property generator QUALITY.md §5.2
// described as the remaining step. It builds random failure distributions and
// checks the ranked candidates against an independent recount: every Failing,
// Total, and Share value is recomputed from the observations rather than
// trusted, the winner is the highest-scoring candidate that clears the gates,
// and a winner exists exactly when some candidate passes.
//
// It deliberately does not assert "the winner has the most failures". That is
// false: a 5-of-5 cluster scores above a 6-of-10 cluster while containing fewer
// failures, and the score is the documented ranking, not the count.
func TestRankRecountsEveryCluster(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(1))

	cdn := []string{"", "akamai", "cloudflare"}
	asn := []int{0, 13335, 64500, 64501}
	prefixes := []string{"", "203.0.113.0/24", "198.51.100.0/24"}
	countries := []string{"", "US", "DE"}
	sectors := []string{"", "airline", "banking"}
	stages := []string{"", "tls", "http"}

	pick := func(n int) int { return rng.Intn(n) }

	for iter := 0; iter < 500; iter++ {
		n := 1 + rng.Intn(30)
		observations := make([]correlate.Observation, 0, n)
		for i := 0; i < n; i++ {
			observations = append(observations, correlate.Observation{
				TargetID: fmt.Sprintf("t%d", i),
				CDN:      cdn[pick(len(cdn))],
				ASN:      asn[pick(len(asn))],
				Prefix24: prefixes[pick(len(prefixes))],
				Country:  countries[pick(len(countries))],
				Sector:   sectors[pick(len(sectors))],
				Stage:    stages[pick(len(stages))],
				Failing:  rng.Intn(2) == 0,
			})
		}

		ranked := correlate.Rank(observations)
		for _, candidate := range ranked {
			wantFailing, wantTotal := 0, 0
			for _, o := range observations {
				if valueFor(o, candidate.Dimension) != candidate.Value {
					continue
				}
				wantTotal++
				if o.Failing {
					wantFailing++
				}
			}
			if candidate.Failing != wantFailing || candidate.Total != wantTotal {
				t.Fatalf("iteration %d: %s=%q reported %d/%d, recount is %d/%d",
					iter, candidate.Dimension, candidate.Value,
					candidate.Failing, candidate.Total, wantFailing, wantTotal)
			}
			wantShare := float64(wantFailing) / float64(wantTotal)
			if math.Abs(candidate.Share-wantShare) > 1e-12 {
				t.Fatalf("iteration %d: %s=%q share %.15f, want %.15f",
					iter, candidate.Dimension, candidate.Value, candidate.Share, wantShare)
			}
		}

		winner, ok := correlate.Winner(observations)
		var (
			anyPassing bool
			bestScore  float64
		)
		for _, candidate := range ranked {
			if !candidate.Passes() {
				continue
			}
			anyPassing = true
			if candidate.Score > bestScore {
				bestScore = candidate.Score
			}
		}
		if ok != anyPassing {
			t.Fatalf("iteration %d: winner present=%v, a passing candidate exists=%v", iter, ok, anyPassing)
		}
		if !ok {
			continue
		}
		if !winner.Passes() {
			t.Fatalf("iteration %d: winner does not clear the gates: %+v", iter, winner)
		}
		if math.Abs(winner.Score-bestScore) > 1e-12 {
			t.Fatalf("iteration %d: winner score %v, best passing score %v", iter, winner.Score, bestScore)
		}
	}
}

// valueFor is the independent mapping from an observation to a dimension value,
// written separately from the implementation so the test is not a copy of it.
func valueFor(o correlate.Observation, dimension correlate.Dimension) string {
	switch dimension {
	case correlate.CDN:
		return o.CDN
	case correlate.ASN:
		if o.ASN <= 0 {
			return ""
		}
		return strconv.Itoa(o.ASN)
	case correlate.Prefix24:
		return o.Prefix24
	case correlate.Country:
		return o.Country
	case correlate.Sector:
		return o.Sector
	case correlate.Stage:
		return o.Stage
	}
	return ""
}
