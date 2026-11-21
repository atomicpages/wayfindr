package postgres

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/golden"
	"github.com/atomicpages/wayfindr/internal/store/postgres/pgtest"
)

// TestSearchIndexMigrationIndexesExistingCatalog: migration 0004 builds the
// search index (design §7.5) for a catalog stored before it, so search on an
// upgraded database ranks exactly as on one whose catalog was written after.
// This is an internal test because only the package's embedded migrations
// can stop a database at 0003.
func TestSearchIndexMigrationIndexesExistingCatalog(t *testing.T) {
	ctx := context.Background()
	tools := golden.Tools(t)

	dsn := pgtest.NewDatabase(t)
	cfg, err := pgx.ParseConfig(dsn)

	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}

	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })

	if err := store.Migrate(ctx, db, migrationsThrough(t, 3)); err != nil {
		t.Fatalf("migrate to 0003: %v", err)
	}

	const user = "01900000-0000-7000-8000-000000000001"

	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, user); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	for connector, ts := range tools {
		for _, tl := range ts {
			insertTool(ctx, t, db, user, connector, tl)
		}
	}

	upgraded, err := Open(ctx, dsn)

	if err != nil {
		t.Fatalf("Open (migrating to the latest): %v", err)
	}

	t.Cleanup(func() { _ = upgraded.Close() })
	fresh, err := Open(ctx, pgtest.NewDatabase(t))

	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = fresh.Close() })
	freshUser, err := fresh.EnsureUser(ctx)

	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	golden.Load(t, fresh, freshUser)

	// TopFive returns the qualified names of whatever page it is asked for.
	for _, q := range golden.Queries(t) {
		want := golden.TopFive(t, fresh, store.SearchQuery{UserID: freshUser, Text: q.Query, Page: 1, Size: 50})
		got := golden.TopFive(t, upgraded, store.SearchQuery{UserID: user, Text: q.Query, Page: 1, Size: 50})

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Search(%q) after migrating (-fresh +upgraded):\n%s", q.Query, diff)
		}
	}
}

// migrationsThrough is the embedded migrations up to version n.
func migrationsThrough(t *testing.T, n int) fs.FS {
	t.Helper()
	files, err := fs.Glob(migrationFiles, "migrations/*.sql")

	if err != nil || len(files) < n {
		t.Fatalf("glob migrations: %v (%d files)", err, len(files))
	}

	out := fstest.MapFS{}

	for _, f := range files[:n] {
		body, err := migrationFiles.ReadFile(f)

		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}

		out[f[len("migrations/"):]] = &fstest.MapFile{Data: body}
	}

	return out
}

func insertTool(ctx context.Context, t *testing.T, db *sql.DB, user, connector string, tl store.Tool) {
	t.Helper()

	if _, err := db.ExecContext(ctx, `INSERT INTO tools (user_id, connector, name, definition, content_hash, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())`, user, connector, tl.Name, []byte(tl.Definition), tl.ContentHash); err != nil {
		t.Fatalf("insert %s.%s: %v", connector, tl.Name, err)
	}
}
