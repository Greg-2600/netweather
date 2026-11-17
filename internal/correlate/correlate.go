// Package correlate turns a set of failing targets into ranked explanations.
//
// "8 sites down" is noise; "17 of 21 Akamai targets down" is an incident report.
// The score is deliberately simple and explainable:
//
//	share = failing / total
//	score = share * ln(1 + failing)
//
// with two gates: at least three failing targets, and at least 60% of the
// targets sharing that value (DESIGN.md §7.3).
package correlate

import (
	"math"
	"sort"
	"strconv"
)

// Dimension is the axis a cluster is grouped on.
type Dimension string

const (
	CDN      Dimension = "cdn"
	ASN      Dimension = "asn"
	Prefix24 Dimension = "prefix_24"
	Country  Dimension = "country"
	Sector   Dimension = "sector"
	Stage    Dimension = "stage"
)

// Gates from DESIGN.md §7.3.
const (
	MinFailing = 3
	MinShare   = 0.60
)

// Observation is one target in the current window. Non-failing targets are
// included because they are the denominator: 3 of 3 is different from 3 of 300.
type Observation struct {
	TargetID string
	CDN      string
	ASN      int
	Prefix24 string
	Country  string
	Sector   string
	Stage    string
	Failing  bool
}

// Candidate is one explanation with its evidence.
type Candidate struct {
	Dimension Dimension
	Value     string
	Failing   int
	Total     int
	Share     float64
	Score     float64
}

// Passes reports whether the candidate clears both gates.
func (c Candidate) Passes() bool {
	return c.Failing >= MinFailing && c.Share >= MinShare
}

type key struct {
	dimension Dimension
	value     string
}

// Rank returns every cluster that contains at least one failure, best first.
// Ties break on dimension then value so the output is deterministic.
func Rank(observations []Observation) []Candidate {
	totals := make(map[key]int)
	failing := make(map[key]int)

	for _, o := range observations {
		for _, kv := range []key{
			{CDN, o.CDN},
			{ASN, asnValue(o.ASN)},
			{Prefix24, o.Prefix24},
			{Country, o.Country},
			{Sector, o.Sector},
			{Stage, o.Stage},
		} {
			if kv.value == "" {
				continue
			}
			totals[kv]++
			if o.Failing {
				failing[kv]++
			}
		}
	}

	out := make([]Candidate, 0, len(totals))
	for k, total := range totals {
		f := failing[k]
		if f == 0 {
			continue
		}
		share := float64(f) / float64(total)
		out = append(out, Candidate{
			Dimension: k.dimension,
			Value:     k.value,
			Failing:   f,
			Total:     total,
			Share:     share,
			Score:     share * math.Log(1+float64(f)),
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Dimension != out[j].Dimension {
			return out[i].Dimension < out[j].Dimension
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// Winner is the highest-scoring candidate that clears both gates. A candidate
// with a higher score but too small a share is skipped rather than promoted:
// "5 of 300" is a bigger number than "4 of 5", and a worse explanation.
func Winner(observations []Observation) (Candidate, bool) {
	for _, c := range Rank(observations) {
		if c.Passes() {
			return c, true
		}
	}
	return Candidate{}, false
}

func asnValue(asn int) string {
	if asn <= 0 {
		return ""
	}
	return strconv.Itoa(asn)
}
