// Package store is the SQLite persistence layer. One writer connection, N
// read-only readers, forward-only migrations, and a write path that updates the
// probe row, the latest state, and the 5-minute rollup in one transaction
// (SCHEMA.md §2, §12).
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store owns the two connection pools.
type Store struct {
	path   string
	writer *sql.DB
	reader *sql.DB
}

// Open creates or opens the database at path, applies pragmas, and migrates.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: create directory: %w", err)
		}
	}

	writer, err := openPool(ctx, path, true)
	if err != nil {
		return nil, err
	}
	reader, err := openPool(ctx, path, false)
	if err != nil {
		_ = writer.Close()
		return nil, err
	}

	s := &Store{path: path, writer: writer, reader: reader}
	if err := s.migrate(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}

	// SQLite creates the database with the process umask, which is usually
	// 0644. The documented posture is 0600 with a 0700 directory
	// (THREAT_MODEL.md T45). WAL and shared-memory siblings inherit the
	// database's mode, so tightening the database is what keeps later ones
	// private; the loop also fixes siblings that already exist. Windows has no
	// POSIX modes, and the release platform is Linux.
	if runtime.GOOS != "windows" {
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			if _, err := os.Stat(p); err != nil {
				continue
			}
			if err := os.Chmod(p, 0o600); err != nil {
				_ = s.Close()
				return nil, fmt.Errorf("store: database permissions: %w", err)
			}
		}
	}
	return s, nil
}

func openPool(ctx context.Context, path string, writer bool) (*sql.DB, error) {
	pragmas := []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(ON)",
		"_pragma=temp_store(MEMORY)",
		"_pragma=cache_size(-16000)",
	}
	if writer {
		pragmas = append(pragmas, "_pragma=auto_vacuum(INCREMENTAL)")
	} else {
		pragmas = append(pragmas, "_pragma=query_only(1)")
	}

	dsn := "file:" + filepath.ToSlash(path) + "?" + strings.Join(pragmas, "&")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if writer {
		// Exactly one writer. This is what keeps SQLite predictable.
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(4)
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	return db, nil
}

// Close shuts both pools down.
func (s *Store) Close() error {
	var errs []error
	if s.reader != nil {
		errs = append(errs, s.reader.Close())
	}
	if s.writer != nil {
		errs = append(errs, s.writer.Close())
	}
	return errors.Join(errs...)
}

// Path reports the database file, for diagnostics.
func (s *Store) Path() string { return s.path }

// SchemaVersion reports the applied migration number.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var raw string
	err := s.reader.QueryRowContext(ctx,
		`SELECT value FROM schema_meta WHERE key = 'schema_version'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(raw)
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}

	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("store: migration %q has no numeric prefix", name)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("store: migration %q: %w", name, err)
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: version, name: name, sql: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: migration versions must be contiguous from 1, got %d at position %d", m.version, i+1)
		}
	}
	return out, nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.writer.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("store: schema_meta: %w", err)
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	current, err := s.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("store: database schema v%d is newer than this binary (v%d); refusing to start", current, len(migrations))
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, m migration) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("store: apply migration %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO schema_meta (key, value) VALUES ('schema_version', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(m.version)); err != nil {
		return fmt.Errorf("store: record migration %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", m.name, err)
	}
	return nil
}
