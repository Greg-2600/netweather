package board_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
)

func fixture() *board.Memory {
	return board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", State: board.StateUp},
		board.Target{ID: "2", Name: "United", Host: "united.com", State: board.StateDown},
		board.Target{ID: "3", Name: "Cloudflare", Host: "cloudflare.com", State: board.StateBlocked},
	)
}

func TestSnapshotPreservesOrder(t *testing.T) {
	t.Parallel()

	got, err := fixture().Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Host != "chase.com" || got[2].Host != "cloudflare.com" {
		t.Fatalf("order changed: %v", got)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	t.Parallel()

	m := fixture()
	got, _ := m.Snapshot(context.Background())
	got[0].State = board.StateDown

	again, _ := m.Snapshot(context.Background())
	if again[0].State != board.StateUp {
		t.Fatal("Snapshot exposed internal state")
	}
}

func TestSetReportsChangeAndNotifies(t *testing.T) {
	t.Parallel()

	m := fixture()
	ch, cancel := m.Subscribe()
	defer cancel()

	found, changed := m.Set("2", board.StateUp)
	if !found || !changed {
		t.Fatalf("Set = (%v, %v), want (true, true)", found, changed)
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no notification after a state change")
	}

	found, changed = m.Set("2", board.StateUp)
	if !found || changed {
		t.Fatalf("no-op Set = (%v, %v), want (true, false)", found, changed)
	}
}

func TestSetUnknownTarget(t *testing.T) {
	t.Parallel()

	found, changed := fixture().Set("nope", board.StateDown)
	if found || changed {
		t.Fatalf("Set unknown = (%v, %v)", found, changed)
	}
}

func TestUnsubscribeStopsNotifications(t *testing.T) {
	t.Parallel()

	m := fixture()
	ch, cancel := m.Subscribe()
	cancel()

	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}

	// A second cancel must not panic on a closed channel.
	cancel()
	m.Set("1", board.StateDown)
}

func TestStateLabels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state board.State
		text  string
		class string
	}{
		{board.StateUp, "Up", "state-up"},
		{board.StateUpReachability, "Up (reachability only)", "state-up_reachability"},
		{board.StateDegraded, "Degraded", "state-degraded"},
		{board.StateDown, "Down", "state-down"},
		{board.StateBlocked, "Blocked", "state-blocked"},
		{board.StateConfigError, "Config error", "state-config_error"},
		{board.StateUnmeasurable, "Unmeasurable", "state-unmeasurable"},
		{board.State("weird"), "Unknown", "state-weird"},
	}
	for _, c := range cases {
		if got := c.state.Text(); got != c.text {
			t.Errorf("%s.Text() = %q, want %q", c.state, got, c.text)
		}
		if got := c.state.CSSClass(); got != c.class {
			t.Errorf("%s.CSSClass() = %q, want %q", c.state, got, c.class)
		}
		if c.state.Glyph() == "" {
			t.Errorf("%s has no glyph; colour alone is not enough", c.state)
		}
	}
}

func TestConcurrentSetAndSnapshot(t *testing.T) {
	t.Parallel()

	m := fixture()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.Set("1", board.StateDown)
				m.Set("1", board.StateUp)
				_, _ = m.Snapshot(context.Background())
			}
		}(i)
	}
	wg.Wait()
}
