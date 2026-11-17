package correlate_test

import (
	"math"
	"testing"

	"github.com/Greg-2600/netweather/internal/correlate"
)

func obs(cdn, sector, stage string, failing bool) correlate.Observation {
	return correlate.Observation{CDN: cdn, Sector: sector, Stage: stage, Failing: failing}
}

func TestDominantCDNWins(t *testing.T) {
	t.Parallel()

	var observations []correlate.Observation
	// 21 targets sit behind Akamai; 17 of them are failing.
	for i := 0; i < 17; i++ {
		observations = append(observations, obs("akamai", "", "", true))
	}
	for i := 0; i < 4; i++ {
		observations = append(observations, obs("akamai", "", "", false))
	}

	winner, ok := correlate.Winner(observations)
	if !ok {
		t.Fatal("expected a winner")
	}
	if winner.Dimension != correlate.CDN || winner.Value != "akamai" {
		t.Fatalf("winner = %+v", winner)
	}
	if winner.Failing != 17 || winner.Total != 21 {
		t.Fatalf("counts = %d/%d", winner.Failing, winner.Total)
	}
	if math.Abs(winner.Share-17.0/21.0) > 1e-9 {
		t.Fatalf("share = %v", winner.Share)
	}
	want := (17.0 / 21.0) * math.Log(18)
	if math.Abs(winner.Score-want) > 1e-9 {
		t.Fatalf("score = %v, want %v", winner.Score, want)
	}
}

func TestFewerThanThreeFailuresIsNotAnIncident(t *testing.T) {
	t.Parallel()

	observations := []correlate.Observation{
		obs("akamai", "", "", true),
		obs("akamai", "", "", true),
		obs("akamai", "", "", false),
	}
	if _, ok := correlate.Winner(observations); ok {
		t.Fatal("two failures must not produce an attribution")
	}
}

func TestLowShareIsNotAnIncident(t *testing.T) {
	t.Parallel()

	var observations []correlate.Observation
	for i := 0; i < 10; i++ {
		observations = append(observations, obs("akamai", "", "", true))
	}
	for i := 0; i < 90; i++ {
		observations = append(observations, obs("akamai", "", "", false))
	}

	// One CDN can legitimately carry most targets; 10 of 100 is normal.
	if _, ok := correlate.Winner(observations); ok {
		t.Fatal("a 10% failure share must not produce an attribution")
	}
}

func TestSectorCluster(t *testing.T) {
	t.Parallel()

	observations := []correlate.Observation{
		{TargetID: "a", Sector: "airline", Failing: true},
		{TargetID: "b", Sector: "airline", Failing: true},
		{TargetID: "c", Sector: "airline", Failing: true},
		{TargetID: "d", Sector: "airline", Failing: true},
		{TargetID: "e", Sector: "airline", Failing: true},
		{TargetID: "f", Sector: "airline", Failing: false},
	}

	winner, ok := correlate.Winner(observations)
	if !ok {
		t.Fatal("expected the airline sector to win")
	}
	if winner.Dimension != correlate.Sector || winner.Value != "airline" {
		t.Fatalf("winner = %+v", winner)
	}
}

func TestHighScoreLowShareIsSkippedForAValidCluster(t *testing.T) {
	t.Parallel()

	var observations []correlate.Observation
	// 20 failures out of 34 on one ASN: highest score, share 0.59, fails the gate.
	for i := 0; i < 20; i++ {
		observations = append(observations, correlate.Observation{ASN: 64500, Failing: true})
	}
	for i := 0; i < 14; i++ {
		observations = append(observations, correlate.Observation{ASN: 64500, Failing: false})
	}
	// 4 failures out of 5 on a sector: lower score, clears both gates.
	for i := 0; i < 4; i++ {
		observations = append(observations, correlate.Observation{Sector: "cloud", Failing: true})
	}
	observations = append(observations, correlate.Observation{Sector: "cloud", Failing: false})

	ranked := correlate.Rank(observations)
	if len(ranked) == 0 || ranked[0].Dimension != correlate.ASN {
		t.Fatalf("expected the ASN candidate to rank first: %+v", ranked)
	}

	winner, ok := correlate.Winner(observations)
	if !ok || winner.Dimension != correlate.Sector || winner.Value != "cloud" {
		t.Fatalf("winner = %+v, ok = %v", winner, ok)
	}
}

func TestEmptyAndSparseObservations(t *testing.T) {
	t.Parallel()

	if got := correlate.Rank(nil); len(got) != 0 {
		t.Fatalf("Rank(nil) = %v", got)
	}
	if _, ok := correlate.Winner(nil); ok {
		t.Fatal("no observations cannot produce a winner")
	}

	// A target with no enrichment contributes to no dimension.
	ranked := correlate.Rank([]correlate.Observation{{Failing: true}})
	if len(ranked) != 0 {
		t.Fatalf("empty dimensions should be skipped: %+v", ranked)
	}
}

func TestRankIsDeterministicOnTies(t *testing.T) {
	t.Parallel()

	// Identical counts on two dimensions: dimension name breaks the tie.
	observations := []correlate.Observation{
		{CDN: "x", Country: "US", Failing: true},
		{CDN: "x", Country: "US", Failing: true},
		{CDN: "x", Country: "US", Failing: true},
	}

	ranked := correlate.Rank(observations)
	if len(ranked) != 2 {
		t.Fatalf("ranked = %+v", ranked)
	}
	if ranked[0].Dimension != correlate.CDN {
		t.Fatalf("first = %+v, want CDN before Country", ranked[0])
	}
}

func TestASNIsFormattedAndZeroSkipped(t *testing.T) {
	t.Parallel()

	observations := []correlate.Observation{
		{ASN: 13335, Failing: true},
		{ASN: 13335, Failing: true},
		{ASN: 13335, Failing: true},
		{ASN: 0, Failing: true},
	}

	ranked := correlate.Rank(observations)
	if len(ranked) != 1 || ranked[0].Value != "13335" {
		t.Fatalf("ranked = %+v", ranked)
	}
}

func TestPrefix24Dimension(t *testing.T) {
	t.Parallel()

	var observations []correlate.Observation
	for i := 0; i < 6; i++ {
		observations = append(observations, correlate.Observation{
			Prefix24: "203.0.113.0/24", Failing: i < 5,
		})
	}

	winner, ok := correlate.Winner(observations)
	if !ok || winner.Dimension != correlate.Prefix24 {
		t.Fatalf("winner = %+v, ok = %v", winner, ok)
	}
	if winner.Value != "203.0.113.0/24" {
		t.Fatalf("value = %q", winner.Value)
	}
}

func TestPassesGate(t *testing.T) {
	t.Parallel()

	if !(correlate.Candidate{Failing: 3, Share: 0.60}).Passes() {
		t.Fatal("exactly at both gates should pass")
	}
	if (correlate.Candidate{Failing: 2, Share: 0.9}).Passes() {
		t.Fatal("two failures should not pass")
	}
	if (correlate.Candidate{Failing: 9, Share: 0.59}).Passes() {
		t.Fatal("a share under 0.60 should not pass")
	}
}
