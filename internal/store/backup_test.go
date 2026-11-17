package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

func TestBackupWritesAConsistentCopy(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := s.Record(ctx, []store.Probe{probeAt(now, state.Up)}); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// The copy opens on its own and carries the schema and data.
	copied, err := store.OpenReadOnly(ctx, dest)
	if err != nil {
		t.Fatalf("open the backup: %v", err)
	}
	t.Cleanup(func() { _ = copied.Close() })

	version, err := copied.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != 22 {
		t.Fatalf("backup schema version = %d, want 22", version)
	}
	ids, err := copied.TargetIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		if id == "t1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("backup target ids = %v, want t1", ids)
	}

	// VACUUM INTO refuses to overwrite, and so does the wrapper, before SQLite
	// has to produce its own error.
	if err := s.Backup(ctx, dest); err == nil {
		t.Fatal("Backup overwrote an existing file")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(dest)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("backup mode = %04o, want 0600", perm)
		}
	}
}

func TestBackupRejectsAnEmptyDestination(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	if err := s.Backup(context.Background(), ""); err == nil {
		t.Fatal("an empty destination was accepted")
	}
}
