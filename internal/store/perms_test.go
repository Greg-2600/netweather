package store_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestDatabasePermissions pins THREAT_MODEL.md T45: the database is 0600 and
// its directory is 0700. SQLite's default is the process umask, so this is a
// property the store must impose rather than inherit.
func TestDatabasePermissions(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX file modes; the release platform is Linux")
	}

	_, path := openTest(t)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("database mode = %04o, want 0600", perm)
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("data directory mode = %04o, want 0700", perm)
	}

	// The WAL and shared-memory siblings carry the same data and must not be
	// more readable than the database. They may not exist until the first
	// write, so only assert on the ones that do.
	for _, suffix := range []string{"-wal", "-shm"} {
		side, err := os.Stat(path + suffix)
		if err != nil {
			continue
		}
		if perm := side.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", suffix, perm)
		}
	}
}
