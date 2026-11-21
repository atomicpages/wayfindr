package setup_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/setup"
	"github.com/atomicpages/wayfindr/internal/store/postgres/pgtest"
)

// pgSnapshot records the database's schema version and the row count of
// every table in the public schema, as dbSnapshot does for SQLite.
func pgSnapshot(t *testing.T, dsn string) map[string]int {
	t.Helper()
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, dsn)

	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	defer func() { _ = conn.Close(context.Background()) }()

	got := map[string]int{}
	var version int

	if err := conn.QueryRow(ctx, "SELECT MAX(version) FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}

	got["schema version"] = version

	rows, err := conn.Query(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'")

	if err != nil {
		t.Fatalf("list tables: %v", err)
	}

	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])

	if err != nil {
		t.Fatalf("list tables: %v", err)
	}

	for _, table := range tables {
		var n int

		if err := conn.QueryRow(ctx, "SELECT COUNT(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}

		got["rows in "+table] = n
	}

	return got
}

func TestInitPreparesAPostgresStoreAndASecondRunChangesNothing(t *testing.T) {
	dsn := pgtest.NewDatabase(t)
	f := newFixture(t)
	f.writeConfig(t, fmt.Sprintf("[storage]\ndriver = \"postgres\"\ndsn = %q\n", dsn))
	ctx := context.Background()

	first, err := setup.Init(ctx, f.options())

	if err != nil {
		t.Fatalf("first Init: %v", err)
	}

	if id, err := uuid.Parse(first.UserID); err != nil || id.Version() != 7 {
		t.Errorf("UserID = %q, want a UUIDv7", first.UserID)
	}

	if first.KeySource != secrets.SourceKeychain || !first.TokenCreated {
		t.Errorf("KeySource, TokenCreated = %q, %v, want keychain, true", first.KeySource, first.TokenCreated)
	}

	if m := mode(t, f.paths.ClientTokenFile()); m != 0o600 {
		t.Errorf("client token mode = %v, want 0600", m)
	}

	if _, err := os.Stat(f.paths.DBFile()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("sqlite database stat err = %v, want none created for postgres", err)
	}

	dbBefore := pgSnapshot(t, dsn)

	if dbBefore["rows in users"] != 1 || dbBefore["schema version"] < 1 {
		t.Fatalf("first run left %v, want schema version >= 1 and 1 user", dbBefore)
	}

	root := filepath.Dir(f.paths.DataDir)
	before := snapshot(t, root)
	keychainBefore := maps.Clone(f.keychain.Items)

	second, err := setup.Init(ctx, f.options())

	if err != nil {
		t.Fatalf("second Init: %v", err)
	}

	if diff := cmp.Diff(before, snapshot(t, root)); diff != "" {
		t.Errorf("files changed on the second run (-before +after):\n%s", diff)
	}

	if diff := cmp.Diff(dbBefore, pgSnapshot(t, dsn)); diff != "" {
		t.Errorf("database changed on the second run (-before +after):\n%s", diff)
	}

	if diff := cmp.Diff(keychainBefore, f.keychain.Items); diff != "" {
		t.Errorf("keychain changed on the second run (-before +after):\n%s", diff)
	}

	if second.TokenCreated || second.UserID != first.UserID || second.KeySource != first.KeySource {
		t.Errorf("second run TokenCreated, user, key source = %v, %q, %q, want false, %q, %q",
			second.TokenCreated, second.UserID, second.KeySource, first.UserID, first.KeySource)
	}
}
