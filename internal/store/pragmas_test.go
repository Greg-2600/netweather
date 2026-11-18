package store //nolint:testpackage // the documented pragmas are per-connection internals

import (
	"context"
	"path/filepath"
	"testing"
)

// TestDocumentedPragmas keeps SCHEMA.md §2 honest. The pragma block is policy,
// not boilerplate: WAL plus one writer is what makes SQLite predictable here,
// and a dropped line is invisible until a load test. `query_only` is checked
// where it matters, in the read path.
func TestDocumentedPragmas(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pragmas.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Read the pragmas from a writer connection: many are per-connection.
	query := func(name string) string {
		t.Helper()
		var got string
		if err := s.writer.QueryRowContext(ctx, "PRAGMA "+name).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", name, err)
		}
		return got
	}

	cases := []struct {
		name string
		want string
	}{
		{"journal_mode", "wal"},
		{"synchronous", "1"},       // NORMAL
		{"busy_timeout", "5000"},   //
		{"foreign_keys", "1"},      //
		{"temp_store", "2"},        // MEMORY
		{"cache_size", "-16000"},   //
		{"mmap_size", "268435456"}, //
		{"wal_autocheckpoint", "1000"},
		{"auto_vacuum", "2"}, // INCREMENTAL
	}
	for _, tc := range cases {
		if got := query(tc.name); got != tc.want {
			t.Errorf("PRAGMA %s = %q, want %q", tc.name, got, tc.want)
		}
	}

	// The reader pool must refuse a write. This is the pragma that turns a
	// handler-side write bug into an immediate error.
	var one int
	if err := s.reader.QueryRowContext(ctx,
		`INSERT INTO schema_meta (key, value) VALUES ('pragmas', '1')`).Scan(&one); err == nil {
		t.Fatal("the read-only pool accepted a write")
	}
}
