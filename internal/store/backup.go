package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
