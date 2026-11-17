package dnsdiv_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/dnsdiv"
)

func baseline(addrs ...string) dnsdiv.Baseline {
	set := make(map[string]struct{}, len(addrs))
	for _, addr := range addrs {
		set[addr] = struct{}{}
	}
	return dnsdiv.Baseline{Addresses: set, Samples: len(addrs), LastSeen: time.Now()}
}

func answer(resolver string, failure string, addrs ...string) dnsdiv.Answer {
	return dnsdiv.Answer{
		Resolver: resolver, Failure: failure, Addresses: addrs,
		At: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC),
	}
}

func TestCompareUsesTheResolversOwnHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		answer dnsdiv.Answer
		base   dnsdiv.Baseline
		want   dnsdiv.Strength
	}{
		{"learning is not a signal", answer("a", "", "192.0.2.1"), dnsdiv.Baseline{}, dnsdiv.None},
		{"known address is normal", answer("a", "", "192.0.2.1"), baseline("192.0.2.1"), dnsdiv.None},
		{"new address is weak", answer("a", "", "192.0.2.9"), baseline("192.0.2.1"), dnsdiv.Weak},
		{"nxdomain after answering is strong", answer("a", "nxdomain"), baseline("192.0.2.1"), dnsdiv.Strong},
		{"nxdomain with no history is quiet", answer("a", "nxdomain"), dnsdiv.Baseline{}, dnsdiv.None},
		{"nodata after answering is medium", answer("a", "nodata"), baseline("192.0.2.1"), dnsdiv.Medium},
		{"servfail after answering is medium", answer("a", "servfail"), baseline("192.0.2.1"), dnsdiv.Medium},
		{"refused after answering is medium", answer("a", "refused"), baseline("192.0.2.1"), dnsdiv.Medium},
		{"timeout is path health, not divergence", answer("a", "timeout"), baseline("192.0.2.1"), dnsdiv.None},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := dnsdiv.Compare(tc.answer, tc.base)
			if got.Strength != tc.want {
				t.Fatalf("strength = %v, want %v (%s)", got.Strength, tc.want, got.Reason)
			}
			if got.Resolver != tc.answer.Resolver {
				t.Fatalf("resolver = %q", got.Resolver)
			}
			if tc.want != dnsdiv.None && got.Reason == "" {
				t.Fatal("a signal had no reason")
			}
		})
	}
}

func TestEvaluateLadder(t *testing.T) {
	t.Parallel()

	good := baseline("192.0.2.1")

	tests := []struct {
		name         string
		answers      []dnsdiv.Answer
		baselines    map[string]dnsdiv.Baseline
		wantKind     string
		wantConf     string
		wantContains string
	}{
		{
			name:    "no divergence",
			answers: []dnsdiv.Answer{answer("a", "", "192.0.2.1"), answer("b", "", "192.0.2.1")},
			baselines: map[string]dnsdiv.Baseline{
				"a": good, "b": good,
			},
		},
		{
			name:    "one nxdomain is an advisory",
			answers: []dnsdiv.Answer{answer("a", "nxdomain"), answer("b", "", "192.0.2.1")},
			baselines: map[string]dnsdiv.Baseline{
				"a": good, "b": good,
			},
			wantKind: "DNS_ANOMALY", wantConf: "medium", wantContains: "a:",
		},
		{
			name:    "two nxdomains are tampering",
			answers: []dnsdiv.Answer{answer("a", "nxdomain"), answer("b", "nxdomain")},
			baselines: map[string]dnsdiv.Baseline{
				"a": good, "b": good,
			},
			wantKind: "TAMPER", wantConf: "high",
		},
		{
			name:    "strong plus medium on one target is tampering",
			answers: []dnsdiv.Answer{answer("a", "nxdomain"), answer("b", "servfail")},
			baselines: map[string]dnsdiv.Baseline{
				"a": good, "b": good,
			},
			wantKind: "TAMPER", wantConf: "medium",
		},
		{
			name:    "medium alone is not a verdict",
			answers: []dnsdiv.Answer{answer("a", "servfail"), answer("b", "", "192.0.2.1")},
			baselines: map[string]dnsdiv.Baseline{
				"a": good, "b": good,
			},
		},
		{
			name: "a lone deviation among three resolvers is strong",
			answers: []dnsdiv.Answer{
				answer("a", "servfail"),
				answer("b", "", "192.0.2.1"),
				answer("c", "", "192.0.2.1"),
			},
			baselines: map[string]dnsdiv.Baseline{
				"a": good, "b": good, "c": good,
			},
			wantKind: "DNS_ANOMALY", wantConf: "medium", wantContains: "only resolver that deviates",
		},
		{
			name: "no baseline means no verdict",
			answers: []dnsdiv.Answer{
				answer("a", "nxdomain"), answer("b", "nxdomain"),
			},
			baselines: map[string]dnsdiv.Baseline{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := dnsdiv.Evaluate(tc.answers, tc.baselines)
			if got.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q (%s)", got.Kind, tc.wantKind, got.Detail)
			}
			if tc.wantKind != "" && got.Confidence != tc.wantConf {
				t.Fatalf("confidence = %q, want %q", got.Confidence, tc.wantConf)
			}
			if tc.wantContains != "" && !strings.Contains(got.Detail, tc.wantContains) {
				t.Fatalf("detail = %q, want it to mention %q", got.Detail, tc.wantContains)
			}
			if tc.wantKind == "" && got.Detail != "" {
				t.Fatalf("an empty verdict carried detail %q", got.Detail)
			}
		})
	}
}

func TestEvaluateIsDeterministicAndNamesResolvers(t *testing.T) {
	t.Parallel()

	good := baseline("192.0.2.1")
	answers := []dnsdiv.Answer{answer("z", "nxdomain"), answer("a", "nxdomain")}
	baselines := map[string]dnsdiv.Baseline{"z": good, "a": good}

	first := dnsdiv.Evaluate(answers, baselines)
	second := dnsdiv.Evaluate(answers, baselines)
	if first.Detail != second.Detail {
		t.Fatalf("detail is not deterministic: %q vs %q", first.Detail, second.Detail)
	}
	if !strings.Contains(first.Detail, "a:") || !strings.Contains(first.Detail, "z:") {
		t.Fatalf("detail = %q, want every deviating resolver named", first.Detail)
	}
	if got := first.Resolvers(); got != "a,z" {
		t.Fatalf("resolvers = %q, want a stable sorted list", got)
	}
}
