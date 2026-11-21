package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"

	"github.com/atomicpages/wayfindr/internal/store"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migrate.db"))

	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

func migrations(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}

	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}

	return fsys
}

func schemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	v, err := store.SchemaVersion(context.Background(), db)

	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}

	return v
}

func TestMigrateAppliesAllMigrationsInOrder(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	fsys := migrations(map[string]string{
		"0001_create.sql": "CREATE TABLE a (x INTEGER);",
		"0002_fill.sql":   "INSERT INTO a (x) VALUES (1); INSERT INTO a (x) VALUES (2);",
	})

	if err := store.Migrate(ctx, db, fsys); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if got := schemaVersion(t, db); got != 2 {
		t.Errorf("schema version = %d, want 2", got)
	}
}

func TestSchemaVersionOfEmptyDatabaseIsZero(t *testing.T) {
	if got := schemaVersion(t, openDB(t)); got != 0 {
		t.Errorf("schema version = %d, want 0", got)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	// Neither statement is safe to run twice, so a rerun would fail.
	fsys := migrations(map[string]string{
		"0001_create.sql": "CREATE TABLE a (x INTEGER PRIMARY KEY);",
		"0002_fill.sql":   "INSERT INTO a (x) VALUES (1);",
	})

	for i := range 3 {
		if err := store.Migrate(ctx, db, fsys); err != nil {
			t.Fatalf("Migrate run %d: %v", i+1, err)
		}
	}

	if got := schemaVersion(t, db); got != 2 {
		t.Errorf("schema version = %d, want 2", got)
	}
}

func TestMigrateAppliesOnlyNewMigrations(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	first := migrations(map[string]string{
		"0001_create.sql": "CREATE TABLE a (x INTEGER PRIMARY KEY);",
	})
	second := migrations(map[string]string{
		"0001_create.sql": "CREATE TABLE a (x INTEGER PRIMARY KEY);",
		"0002_more.sql":   "CREATE TABLE b (y INTEGER);",
	})

	if err := store.Migrate(ctx, db, first); err != nil {
		t.Fatalf("Migrate first: %v", err)
	}

	if err := store.Migrate(ctx, db, second); err != nil {
		t.Fatalf("Migrate second: %v", err)
	}

	if got := schemaVersion(t, db); got != 2 {
		t.Errorf("schema version = %d, want 2", got)
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	newer := migrations(map[string]string{
		"0001_create.sql": "CREATE TABLE a (x INTEGER);",
		"0002_more.sql":   "CREATE TABLE b (y INTEGER);",
	})
	older := migrations(map[string]string{
		"0001_create.sql": "CREATE TABLE a (x INTEGER);",
	})

	if err := store.Migrate(ctx, db, newer); err != nil {
		t.Fatalf("Migrate newer: %v", err)
	}

	err := store.Migrate(ctx, db, older)

	if !errors.Is(err, store.ErrSchemaTooNew) {
		t.Fatalf("Migrate older binary: err = %v, want ErrSchemaTooNew", err)
	}

	if got := schemaVersion(t, db); got != 2 {
		t.Errorf("schema version = %d, want 2 (unchanged)", got)
	}
}

func TestFailedMigrationRollsBackOnlyItself(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	broken := migrations(map[string]string{
		"0001_create.sql": "CREATE TABLE a (x INTEGER);",
		"0002_broken.sql": "CREATE TABLE b (y INTEGER); SELECT * FROM missing;",
	})
	fixed := migrations(map[string]string{
		"0001_create.sql": "CREATE TABLE a (x INTEGER);",
		"0002_broken.sql": "CREATE TABLE b (y INTEGER);",
	})

	if err := store.Migrate(ctx, db, broken); err == nil {
		t.Fatal("Migrate broken: err = nil, want error")
	}

	if got := schemaVersion(t, db); got != 1 {
		t.Fatalf("schema version after failure = %d, want 1", got)
	}

	// If the failed migration had leaked table b, this rerun would fail.
	if err := store.Migrate(ctx, db, fixed); err != nil {
		t.Fatalf("Migrate fixed: %v", err)
	}

	if got := schemaVersion(t, db); got != 2 {
		t.Errorf("schema version = %d, want 2", got)
	}
}

func TestMigrateRejectsMalformedMigrationSets(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
	}{
		{"gap in versions", map[string]string{"0001_a.sql": "SELECT 1;", "0003_c.sql": "SELECT 1;"}},
		{"does not start at 1", map[string]string{"0002_b.sql": "SELECT 1;"}},
		{"duplicate version", map[string]string{"0001_a.sql": "SELECT 1;", "0001_b.sql": "SELECT 1;"}},
		{"no name", map[string]string{"0001.sql": "SELECT 1;"}},
		{"not four digits", map[string]string{"1_a.sql": "SELECT 1;"}},
		{"stray file", map[string]string{"0001_a.sql": "SELECT 1;", "README": "x"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openDB(t)

			if err := store.Migrate(context.Background(), db, migrations(tt.files)); err == nil {
				t.Fatal("Migrate: err = nil, want error")
			}

			if got := schemaVersion(t, db); got != 0 {
				t.Errorf("schema version = %d, want 0 (nothing applied)", got)
			}
		})
	}
}
