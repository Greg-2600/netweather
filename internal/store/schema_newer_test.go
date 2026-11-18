package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/store"
)

// TestOpenRefusesANewerSchema pins THREAT_MODEL.md invariant 7's downgrade
// case: a database written by a newer binary must not be opened and silently
// used with columns this build does not know.
func TestOpenRefusesANewerSchema(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "newer.db")

	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Pretend a future release migrated the file.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE schema_meta SET value = '999' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	opened, err := store.Open(ctx, path)
	if err == nil {
		_ = opened.Close()
		t.Fatal("a database from a newer schema was opened")
	}
	if !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("error = %v, want the downgrade refusal", err)
	}
}
