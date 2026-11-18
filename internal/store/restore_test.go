package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Greg-2600/netweather/internal/store"
)

// TestRestoreFromBackupDrillsTheDocumentedProcedure pins SCHEMA.md §14's
// restore path: a backup taken with VACUUM INTO is a self-contained database
// that opens after the live file is replaced, and the -wal/-shm siblings that
// belonged to the replaced file are not needed.
func TestRestoreFromBackupDrillsTheDocumentedProcedure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "netweather.db")

	// Seed a live database.
	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTarget(ctx, store.Target{
		ID: "t1", Host: "chase.com", Port: 443, Probe: "http",
		Name: "Chase", Class: "service", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Take the documented backup while the service is stopped for the drill.
	backup := filepath.Join(dir, "backup.db")
	reopened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	_ = reopened.Close()

	// Restore procedure: stop the service (done), replace the database file, and
	// delete any -wal/-shm siblings that belonged to the replaced file. The
	// backup itself carries no WAL, so none are recreated.
	for _, sibling := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(sibling); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", sibling, err)
		}
	}
	if err := copyFile(backup, path); err != nil {
		t.Fatalf("replace the database: %v", err)
	}

	restored, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open the restored database: %v", err)
	}
	defer func() { _ = restored.Close() }()

	gotVersion, err := restored.SchemaVersion(ctx)
	if err != nil || gotVersion != version {
		t.Fatalf("restored schema = %d, want %d (err=%v)", gotVersion, version, err)
	}
	ids, err := restored.TargetIDs(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("restored targets = %v, err=%v", ids, err)
	}

	// The restored file is self-contained: it opens and reads back the row we
	// seeded, which is what the documented restore path exists to guarantee.
	// (A -wal/-shm may be recreated while the database is open; the hazard the
	// procedure warns about is a stale sibling from the replaced file, which
	// the remove step above already cleared.)
	_ = restored
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, in, 0o600); err != nil {
		return err
	}
	return nil
}
