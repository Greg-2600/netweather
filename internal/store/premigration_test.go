package store //nolint:testpackage // the pre-migration copy is deliberately unexported

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestBackupBeforeMigration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "netweather.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	current, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	// A fresh database has nothing to lose, and one already at the target has
	// nothing to do.
	if dest, err := s.backupBeforeMigration(ctx, 0, current+1, now); err != nil || dest != "" {
		t.Fatalf("fresh = (%q, %v), want no copy", dest, err)
	}
	if dest, err := s.backupBeforeMigration(ctx, current, current, now); err != nil || dest != "" {
		t.Fatalf("current = (%q, %v), want no copy", dest, err)
	}

	// A pending migration writes a sibling copy of the current schema.
	dest, err := s.backupBeforeMigration(ctx, current, current+1, now)
	if err != nil {
		t.Fatalf("backupBeforeMigration: %v", err)
	}
	if want := preMigrationBackupPath(path, current, now); dest != want {
		t.Fatalf("destination = %q, want %q", dest, want)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("the copy does not exist: %v", err)
	}

	// It is a real, openable database at the version that was copied.
	copied, err := OpenReadOnly(ctx, dest)
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	version, err := copied.SchemaVersion(ctx)
	_ = copied.Close()
	if err != nil {
		t.Fatal(err)
	}
	if version != current {
		t.Fatalf("copy schema = %d, want %d", version, current)
	}

	// The rollback point is never overwritten by a retry.
	if _, err := s.backupBeforeMigration(ctx, current, current+1, now); err == nil {
		t.Fatal("an existing rollback point was overwritten")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(dest)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("copy mode = %04o, want 0600", perm)
		}
	}
}
