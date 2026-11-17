package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Backup writes a consistent copy of the database with VACUUM INTO.
//
// It is the documented backup path (SCHEMA.md §14), and the only one available
// inside the release image: that image has no shell and no sqlite3 binary, so
// "run VACUUM INTO" has to be a flag on the binary rather than a command an
// operator types into the container. VACUUM INTO is safe while the app is
// running and produces a compact file with no WAL; copying the live database is
// the standard way to get a corrupt backup.
func (s *Store) Backup(ctx context.Context, dest string) error {
	if dest == "" {
		return fmt.Errorf("store: backup destination is empty")
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("store: backup destination %q already exists", dest)
	}
	if dir := filepath.Dir(dest); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("store: backup directory: %w", err)
		}
	}

	if _, err := s.writer.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("store: backup: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dest, 0o600); err != nil {
			return fmt.Errorf("store: backup permissions: %w", err)
		}
	}
	return nil
}

// preMigrationBackupPath names the rollback point a migration writes beside the
// database. The version and time make two releases unable to overwrite each
// other's copy, and keeping it in the same directory means a container volume
// carries it without a second mount.
func preMigrationBackupPath(dbPath string, version int, now time.Time) string {
	return fmt.Sprintf("%s.pre-migration-v%d-%s.db",
		dbPath, version, now.UTC().Format("20060102T150405Z"))
}

// backupBeforeMigration writes a consistent copy before a pending migration
// changes the schema (SCHEMA.md §15). A fresh database has nothing to lose and
// a database already at the target has nothing to do, so both are no-ops.
//
// A failure is returned rather than logged: migrating without the documented
// rollback point is the situation the backup exists to prevent, and the caller
// can free space or move the database rather than discover the problem after a
// half-applied change. The copy is kept, because it is only useful if the
// migration turns out badly.
func (s *Store) backupBeforeMigration(ctx context.Context, current, target int, now time.Time) (string, error) {
	if current <= 0 || current >= target {
		return "", nil
	}
	// A memory database is a test-only shape and has no file to copy beside.
	if s.path == "" || s.path == ":memory:" || strings.Contains(s.path, "mode=memory") {
		return "", nil
	}

	dest := preMigrationBackupPath(s.path, current, now)
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("store: pre-migration backup %q already exists", dest)
	}
	if _, err := s.writer.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return "", fmt.Errorf("store: pre-migration backup: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dest, 0o600); err != nil {
			return "", fmt.Errorf("store: pre-migration backup permissions: %w", err)
		}
	}
	return dest, nil
}
