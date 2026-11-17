// Package dnsdiv compares each resolver's answer against its own history
// (CLASSIFIER.md §3.2).
//
// Resolvers are never compared to each other: GeoDNS means a CDN legitimately
// answers differently to each resolver, in each location, at each hour. What
// matters is whether a resolver still answers the way it used to. The package
// is pure so the rule table is testable without a network or a clock.
package dnsdiv

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Answer is one resolver's observation. Failure is the stable class name when
// the query did not produce addresses: "", "nxdomain", "nodata", "servfail",
// "refused", "timeout", or "error".
type Answer struct {
	Resolver  string
	Addresses []string
	// ASNs are the origin ASNs for the addresses, when the caller looked them
	// up. Empty means "not looked up", which is not the same as "no ASN".
	ASNs []int
	// CNAMESuffixes are the registrable suffixes of the answer-section CNAME
	// chain, e.g. "cloudfront.net".
	CNAMESuffixes []string
	Failure       string
	At            time.Time
}

// Baseline is what one resolver has done for one target.
type Baseline struct {
	// Addresses is the set of addresses this resolver has returned.
	Addresses map[string]struct{}
	// ASNs is the set of origin ASNs seen for those addresses.
	ASNs map[int]struct{}
	// CNAMESuffixes is the set of hosting suffixes seen in the CNAME chain.
	CNAMESuffixes map[string]struct{}
	// Samples is how many learned address rows exist.
	Samples int
	// LastSeen is the newest observation of the baseline.
	LastSeen time.Time
}

// Strength ranks a signal from "ignore" to "strong".
type Strength int

const (
	None Strength = iota
	Weak
	Medium
	Strong
)

// Signal is one resolver's deviation from its own baseline.
type Signal struct {
	Resolver string
	Strength Strength
	Reason   string
}

// Verdict is the ladder's conclusion.
type Verdict struct {
	// Kind is "", "DNS_ANOMALY", or "TAMPER".
	Kind       string
	Confidence string
	Detail     string
	Signals    []Signal
}

// Compare judges one answer against that resolver's own baseline.
func Compare(a Answer, b Baseline) Signal {
	signal := Signal{Resolver: a.Resolver}

	answeredBefore := b.Samples > 0

	switch a.Failure {
	case "":
		// A successful answer. With no baseline this is learning, not a
		// deviation.
		if !answeredBefore {
			return signal
		}
		newAddress := false
		for _, addr := range a.Addresses {
			if _, known := b.Addresses[addr]; !known {
				newAddress = true
				break
			}
		}
		if !newAddress {
			return signal
		}
		// A new address is normal when both the origin ASN and the hosting
		// suffix are already in the baseline: that is CDN edge rotation. Any
		// ASN or suffix this resolver has never shown is weak, because it is
		// either new hosting or interception and this rule cannot tell them
		// apart.
		if len(b.ASNs) > 0 && len(a.ASNs) > 0 {
			for _, asn := range a.ASNs {
				if _, known := b.ASNs[asn]; !known {
					signal.Strength = Weak
					signal.Reason = "address outside the learned ASN set"
					return signal
				}
			}
			if len(b.CNAMESuffixes) > 0 && !anySuffixKnown(a.CNAMESuffixes, b.CNAMESuffixes) {
				signal.Strength = Weak
				signal.Reason = "address behind a new hosting suffix"
				return signal
			}
			return signal
		}
		signal.Strength = Weak
		signal.Reason = "address outside the learned set"
		return signal

	case "nxdomain":
		if answeredBefore {
			signal.Strength = Strong
			signal.Reason = "NXDOMAIN where this resolver answered before"
		}
		return signal

	case "nodata":
		if answeredBefore {
			signal.Strength = Medium
			signal.Reason = "NODATA where this resolver answered before"
		}
		return signal

	case "servfail", "refused":
		if answeredBefore {
			signal.Strength = Medium
			signal.Reason = fmt.Sprintf("%s where this resolver answered before", strings.ToUpper(a.Failure))
		}
		return signal

	default:
		// A timeout or transport error is path health, which the classifier
		// already handles. It is not a divergence signal.
		return signal
	}
}

// Evaluate applies the documented ladder:
//
//	strong from >= 2 resolvers          -> TAMPER (high)
//	strong + medium on the same target  -> TAMPER
//	exactly one strong                  -> DNS_ANOMALY
//
// A target answered by three or more resolvers where exactly one deviates and
// the rest are clean is treated as a strong signal, because "different answer
// from one resolver only" is the local-tampering case the rule exists for.
func Evaluate(answers []Answer, baselines map[string]Baseline) Verdict {
	signals := make([]Signal, 0, len(answers))
	clean := 0
	for _, answer := range answers {
		signal := Compare(answer, baselines[answer.Resolver])
		signals = append(signals, signal)
		if signal.Strength == None {
			clean++
		}
	}

	if len(answers) >= 3 {
		deviating := 0
		for i := range signals {
			if signals[i].Strength != None {
				deviating++
			}
		}
		if deviating == 1 && clean >= 2 {
			for i := range signals {
				if signals[i].Strength == Medium {
					signals[i].Strength = Strong
					signals[i].Reason += " (the only resolver that deviates)"
				}
			}
		}
	}

	strong, medium := 0, 0
	for _, signal := range signals {
		switch signal.Strength {
		case Strong:
			strong++
		case Medium:
			medium++
		case None, Weak:
			// A weak signal is recorded for the detail but cannot carry a
			// verdict on its own (CLASSIFIER.md §3.2).
		}
	}

	verdict := Verdict{}
	switch {
	case strong >= 2:
		verdict.Kind = "TAMPER"
		verdict.Confidence = "high"
		verdict.Detail = fmt.Sprintf("%d resolvers disagree with their own baselines", strong)
	case strong == 1 && medium >= 1:
		verdict.Kind = "TAMPER"
		verdict.Confidence = "medium"
		verdict.Detail = "one strong and one medium signal on the same target"
	case strong == 1:
		verdict.Kind = "DNS_ANOMALY"
		verdict.Confidence = "medium"
		verdict.Detail = "one resolver disagrees with its own baseline"
	default:
		return Verdict{}
	}

	// Name the signals in a stable order so the detail is reproducible.
	parts := make([]string, 0, len(signals))
	sorted := append([]Signal(nil), signals...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Strength != sorted[j].Strength {
			return sorted[i].Strength > sorted[j].Strength
		}
		return sorted[i].Resolver < sorted[j].Resolver
	})
	for _, signal := range sorted {
		if signal.Strength == None {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", signal.Resolver, signal.Reason))
	}
	if len(parts) > 0 {
		verdict.Detail += " (" + strings.Join(parts, "; ") + ")"
	}
	verdict.Signals = signals
	return verdict
}

// Resolvers renders the signal-bearing resolvers for a stored record.
func (v Verdict) Resolvers() string {
	names := make([]string, 0, len(v.Signals))
	for _, signal := range v.Signals {
		if signal.Strength != None {
			names = append(names, signal.Resolver)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// anySuffixKnown reports whether one of the observed suffixes is already in the
// baseline set.
func anySuffixKnown(observed []string, baseline map[string]struct{}) bool {
	for _, suffix := range observed {
		if _, ok := baseline[suffix]; ok {
			return true
		}
	}
	return false
}

// RegistrableSuffix returns the last two labels of a DNS name, lower-cased and
// without a trailing dot. It is a label, not an eTLD+1: without a public-suffix
// list "co.uk" would be wrong, but for a hosting indicator the last two labels
// are stable and good enough, and they never claim more than they are.
func RegistrableSuffix(name string) string {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	parts := strings.Split(name, ".")
	if len(parts) < 2 {
		return name
	}
	return strings.Join(parts[len(parts)-2:], ".")
}
