package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestMaintenanceWindowLifecycle(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()
	if windows, err := s.MaintenanceWindows(ctx, 10); err != nil || len(windows) != 0 {
		t.Fatalf("empty windows = (%d, %v)", len(windows), err)
	}

	starts := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	ends := starts.Add(2 * time.Hour)
	id, err := s.CreateMaintenanceWindow(ctx, "dns, cdn=akamai", "planned core switch upgrade", starts, ends)
	if err != nil {
		t.Fatal(err)
	}
	if id <= 0 {
		t.Fatalf("id = %d, want a stored row id", id)
	}

	windows, err := s.MaintenanceWindows(ctx, 10)
	if err != nil || len(windows) != 1 {
		t.Fatalf("windows = (%d, %v)", len(windows), err)
	}
	w := windows[0]
	if w.ID != id || w.Components != "dns, cdn=akamai" || w.Reason != "planned core switch upgrade" ||
		!w.StartsAt.Equal(starts) || !w.EndsAt.Equal(ends) || !w.CompletedAt.IsZero() {
		t.Fatalf("window = %+v", w)
	}

	// Completing is idempotent and does not rewrite the completion time.
	done := ends.Add(time.Minute)
	if err := s.CompleteMaintenanceWindow(ctx, id, done); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteMaintenanceWindow(ctx, id, done.Add(time.Hour)); err != nil {
		t.Fatalf("repeat completion: %v", err)
	}
	windows, err = s.MaintenanceWindows(ctx, 10)
	if err != nil || len(windows) != 1 {
		t.Fatalf("windows = (%d, %v)", len(windows), err)
	}
	if got := windows[0].CompletedAt; !got.Equal(done) {
		t.Fatalf("completed_at = %v, want %v", got, done)
	}

	if err := s.CompleteMaintenanceWindow(ctx, id+99, done); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown id error = %v, want sql.ErrNoRows", err)
	}
}

func TestMaintenanceWindowsAreBoundedAndNewestFirst(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if _, err := s.CreateMaintenanceWindow(ctx, "dns", "work", start.Add(time.Duration(i)*time.Hour),
			start.Add(time.Duration(i)*time.Hour+30*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	windows, err := s.MaintenanceWindows(ctx, 2)
	if err != nil || len(windows) != 2 {
		t.Fatalf("limited windows = (%d, %v)", len(windows), err)
	}
	if !windows[0].StartsAt.After(windows[1].StartsAt) {
		t.Fatalf("not newest first: %v then %v", windows[0].StartsAt, windows[1].StartsAt)
	}

	// A caller asking for more than the hard cap still gets a bounded page.
	windows, err = s.MaintenanceWindows(ctx, 1000)
	if err != nil || len(windows) != 5 {
		t.Fatalf("all windows = (%d, %v)", len(windows), err)
	}
}
