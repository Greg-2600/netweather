package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

// TestOnTransitionReportsOnlyConfirmedChanges pins the contract behind the
// LOGGING.md §3 state_change line: the callback fires once per confirmed
// change, after the batch commits, and never for an observation the
// confirmation rules held (DESIGN.md §7.4).
func TestOnTransitionReportsOnlyConfirmedChanges(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	var got []store.Transition
	s.OnTransition = func(t store.Transition) { got = append(got, t) }

	t0 := time.Date(2026, 11, 17, 12, 0, 1, 0, time.UTC)

	// The first observation adopts its candidate; that is not a transition.
	if err := s.Record(ctx, []store.Probe{probeAt(t0, state.Up)}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("the first observation reported %d transitions", len(got))
	}

	// Two of the three service-class failures are held, so neither reports.
	for _, minute := range []int{1, 2} {
		if err := s.Record(ctx, []store.Probe{
			probeAt(t0.Add(time.Duration(minute)*time.Minute), state.Down),
		}); err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("held failure %d reported a transition", minute)
		}
	}

	// The third confirms DOWN and reports exactly once, with the stored reason
	// and the fields the log line carries.
	down := probeAt(t0.Add(3*time.Minute), state.Down)
	down.Reason = "certificate expired"
	down.Stage = "tls"
	down.ErrorClass = "tls_expired"
	down.HTTPStatus = 0
	down.LatencyMS = 41
	down.Confidence = "semantic"
	if err := s.Record(ctx, []store.Probe{down}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("confirmed change reported %d transitions, want 1", len(got))
	}

	tr := got[0]
	if tr.TargetID != "t1" || tr.Host != "chase.com" || tr.VantageID != "local" {
		t.Fatalf("identity = %+v", tr)
	}
	if tr.Tier != 2 {
		t.Fatalf("tier = %d, want 2", tr.Tier)
	}
	if tr.From != state.Up || tr.To != state.Down {
		t.Fatalf("direction = %s -> %s", tr.From, tr.To)
	}
	if !tr.TS.Equal(t0.Add(3 * time.Minute)) {
		t.Fatalf("ts = %v", tr.TS)
	}
	if tr.Stage != "tls" || tr.ErrorClass != "tls_expired" ||
		tr.LatencyMS != 41 || tr.Confidence != "semantic" {
		t.Fatalf("probe fields = %+v", tr)
	}
	// The reason is the same string the state_change row stores, so a log line
	// and the drawer cannot disagree about why a target changed.
	if tr.Reason != "certificate expired" {
		t.Fatalf("reason = %q", tr.Reason)
	}

	// A second failure while already DOWN is a held observation, not a change.
	if err := s.Record(ctx, []store.Probe{
		probeAt(t0.Add(4*time.Minute), state.Down),
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a repeat observation reported %d transitions", len(got))
	}

	// Recovery is a transition too: the service class needs two passes.
	for _, minute := range []int{5, 6} {
		if err := s.Record(ctx, []store.Probe{
			probeAt(t0.Add(time.Duration(minute)*time.Minute), state.Up),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 {
		t.Fatalf("recovery reported %d transitions in total, want 2", len(got))
	}
	if got[1].From != state.Down || got[1].To != state.Up {
		t.Fatalf("recovery direction = %s -> %s", got[1].From, got[1].To)
	}
}

// TestOnTransitionIsSilentForACloselyHeldBatch proves the callback runs only
// after a commit: a rejected batch leaves no transition behind.
func TestOnTransitionIsSilentForAFailedBatch(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	called := 0
	s.OnTransition = func(store.Transition) { called++ }

	// An unknown target breaks the probe insert's foreign key, so the whole
	// batch rolls back and nothing should be reported.
	bad := store.Probe{
		TargetID: "not-a-target", VantageID: "local",
		TS: time.Now().UTC(), State: state.Down, Stage: "http", Confidence: "high",
	}
	if err := s.Record(ctx, []store.Probe{bad}); err == nil {
		t.Fatal("a probe for an unknown target was accepted")
	}
	if called != 0 {
		t.Fatalf("a rolled-back batch reported %d transitions", called)
	}
}
