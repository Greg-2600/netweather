package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/store"
)

// TestLatestProbeJoinUsesTargetIndex pins the plan behind the 1000-target fleet's
// board and API reads. SQLite's vantage-first probe index is correct but scales
// with every probe ever recorded at that vantage, so the shared join must stay
// pinned to the target-first index.
func TestLatestProbeJoinUsesTargetIndex(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "plan.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	q := `EXPLAIN QUERY PLAN SELECT t.id, p.id FROM target t` + store.LatestProbeJoin + ` WHERE t.enabled = 1`
	rows, err := db.QueryContext(context.Background(), q, "local")
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if !strings.Contains(plan.String(), "probe_target_ts") {
		t.Fatalf("latest-probe join is not using probe_target_ts:\n%s", plan.String())
	}
	if strings.Contains(plan.String(), "probe_vantage_ts") {
		t.Fatalf("latest-probe join regressed to probe_vantage_ts:\n%s", plan.String())
	}
}
