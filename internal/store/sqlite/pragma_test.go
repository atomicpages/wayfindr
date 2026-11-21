package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/atomicpages/wayfindr/internal/store/sqlite"
)

// TestOpenAppliesPragmasToEveryPooledConnection holds connections open at
// once so the pool must dial new ones; busy_timeout and foreign_keys are
// per-connection, so a setting applied once with Exec would show only on
// the first (design §5).
func TestOpenAppliesPragmasToEveryPooledConnection(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, filepath.Join(t.TempDir(), "wayfindr.db"))
	db := sqlite.DB(s)

	const held = 3
	conns := make([]*sql.Conn, 0, held)

	for range held {
		c, err := db.Conn(ctx)

		if err != nil {
			t.Fatalf("Conn: %v", err)
		}

		t.Cleanup(func() { _ = c.Close() })
		conns = append(conns, c)
	}

	want := map[string]string{
		"journal_mode": "wal",
		"busy_timeout": "5000",
		"foreign_keys": "1",
	}

	for i, c := range conns {
		for pragma, wantVal := range want {
			var got string

			if err := c.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
				t.Fatalf("conn %d: PRAGMA %s: %v", i, pragma, err)
			}

			if got != wantVal {
				t.Errorf("conn %d: PRAGMA %s = %q, want %q", i, pragma, got, wantVal)
			}
		}
	}
}
