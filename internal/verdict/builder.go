package verdict

import (
	"context"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

// Store is the narrow read surface the builder needs. It is an interface so the
// composition is testable without a database, and so the store package does not
// have to know the verdict exists.
type Store interface {
	LastProbeAt(ctx context.Context, vantageID string) (time.Time, error)
	StateCounts(ctx context.Context, vantageID string) (map[state.State]int, error)
	ReferenceStates(ctx context.Context, vantageID string) (map[state.State]int, error)
	FailureSpread(ctx context.Context, vantageID string) (sectors, asns int, err error)
	OpenIncidentCount(ctx context.Context) (int, error)
	RecentDNSDivergences(ctx context.Context, limit int) ([]store.DNSDivergenceRecord, error)
	CrossVantageDisagreement(ctx context.Context) (bool, error)
	ScopeHistory(ctx context.Context, scope, value string, at time.Time) ([]float64, error)
}

// Builder reads the evidence and returns one verdict. Every read failure is
// treated as an absent signal rather than as evidence: a broken query must not
// turn into an outage, a tamper, or a local-path claim.
type Builder struct {
	Store     Store
	Vantage   string
	StartedAt time.Time
	Now       func() time.Time

	ReferenceInterval time.Duration
	TamperWindow      time.Duration
	BaselineWindow    int
}

// Build gathers the inputs and evaluates the rule table.
func (b *Builder) Build(ctx context.Context) Result {
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	at := now()

	in := Inputs{
		Now:               at,
		StartedAt:         b.StartedAt,
		ReferenceInterval: b.ReferenceInterval,
		TamperWindow:      b.TamperWindow,
		BaselineWindow:    b.BaselineWindow,
	}

	if last, err := b.Store.LastProbeAt(ctx, b.Vantage); err == nil {
		in.LastProbe = last
	}
	if counts, err := b.Store.StateCounts(ctx, b.Vantage); err == nil {
		for _, n := range counts {
			in.Probed += n
		}
	}
	if refs, err := b.Store.ReferenceStates(ctx, b.Vantage); err == nil {
		for st, n := range refs {
			in.ReferenceTotal += n
			if st == state.Down || st == state.Unknown {
				in.ReferenceFailing += n
			}
		}
	}
	if open, err := b.Store.OpenIncidentCount(ctx); err == nil {
		in.OpenIncidents = open
	}
	if sectors, asns, err := b.Store.FailureSpread(ctx, b.Vantage); err == nil {
		in.FailureSectors = sectors
		in.FailureASNs = asns
	}
	if records, err := b.Store.RecentDNSDivergences(ctx, 200); err == nil {
		for _, rec := range records {
			if !strings.EqualFold(rec.Verdict, "TAMPER") {
				continue
			}
			if in.TamperAt.IsZero() || rec.TS.After(in.TamperAt) {
				in.TamperAt = rec.TS
			}
		}
	}
	if cross, err := b.Store.CrossVantageDisagreement(ctx); err == nil {
		in.CrossVantage = cross
	}
	if history, err := b.Store.ScopeHistory(ctx, "global", "", at); err == nil {
		in.BaselineSamples = len(history)
	}

	return Evaluate(in)
}
