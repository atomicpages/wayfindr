package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5"

	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/postgres"
	"github.com/atomicpages/wayfindr/internal/store/postgres/pgtest"
	"github.com/atomicpages/wayfindr/internal/store/storetest"
)

func openStore(t testing.TB, dsn string) *postgres.Store {
	t.Helper()
	s, err := postgres.Open(context.Background(), dsn)

	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

// TestStore runs the shared Store suite, search included, on a database with
// the server's default locale and on one with a C ctype: search must not
// depend on the locale (design §7.5).
func TestStore(t *testing.T) {
	for _, driver := range []string{storetest.Postgres, storetest.PostgresCCtype} {
		t.Run(driver, func(t *testing.T) { storetest.RunDriver(t, driver) })
	}
}

// TestOpenDoesNotEchoAnUnparseableDSN keeps a password out of the error for
// a DSN pgx cannot parse. pgx masks passwords in such errors only best
// effort; this one, with spaces around "=", it echoes.
func TestOpenDoesNotEchoAnUnparseableDSN(t *testing.T) {
	const dsn = "host=db.example port=notaport password = hunter2" //nolint:gosec // a fake password, asserted absent from the error

	_, err := postgres.Open(t.Context(), dsn)

	if err == nil {
		t.Fatal("Open: err = nil, want a parse error")
	}

	if got := err.Error(); strings.Contains(got, "hunter2") {
		t.Errorf("error = %q, want it without the password", got)
	}
}

// TestMigrationGeneratesSearchColumns checks the schema that Postgres search
// (design §7.5) reads: the weighted search_text vector its index is built
// from, and the qualified name and title, all generated from the stored row.
func TestMigrationGeneratesSearchColumns(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.NewDatabase(t)
	s := openStore(t, dsn)
	user, err := s.EnsureUser(ctx)

	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	def := `{"name":"createIssue","annotations":{"title":"New ticket"},"description":"Opens one.",` +
		`"inputSchema":{"type":"object","properties":{"teamId":{"description":"Owning squad"}}}}`
	tl, err := store.NewTool("linear", []byte(def), []byte{1})

	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}

	if _, err := s.ApplySync(ctx, user, store.SyncOutcome{Connector: "linear", Tools: []store.Tool{tl}}); err != nil {
		t.Fatalf("ApplySync: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)

	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	var qualified, title, searchText string

	// Each lexeme with its weights; positions are left out.
	if err := conn.QueryRow(ctx, `SELECT qualified_name, title,
			(SELECT string_agg(lexeme || ':' || array_to_string(weights, ''), ' ' ORDER BY lexeme)
			 FROM unnest(search_text))
		FROM tools`).Scan(&qualified, &title, &searchText); err != nil {
		t.Fatalf("read generated columns: %v", err)
	}

	want := []string{"linear.createIssue", "New ticket",
		"create:A id:C issue:A linear:A new:B one:D opens:D owning:D squad:D team:C ticket:B"}
	got := []string{qualified, title, searchText}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("generated columns (-want +got):\n%s", diff)
	}
}

// TestToolsHasOnlyItsPrimaryKeyIndex: search reads its own index tables
// (design §7.5), so tools needs no index beyond its primary key, and every
// other one is maintenance on each catalog write for nothing. Migration
// 0005 dropped the GIN indexes of 0001 for that reason.
func TestToolsHasOnlyItsPrimaryKeyIndex(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.NewDatabase(t)
	_ = openStore(t, dsn)
	conn, err := pgx.Connect(ctx, dsn)

	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	rows, err := conn.Query(ctx, `SELECT indexname FROM pg_indexes WHERE tablename = 'tools' ORDER BY indexname`)

	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}

	got, err := pgx.CollectRows(rows, pgx.RowTo[string])

	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}

	if diff := cmp.Diff([]string{"tools_pkey"}, got); diff != "" {
		t.Errorf("indexes on tools (-want +got):\n%s", diff)
	}
}

// TestOpenRefusesNonUTF8Database: tool definitions are UTF-8 JSON and search
// tokenises them with the ICU root collation (design §7.5), so Open refuses a
// database in another encoding instead of failing inside a migration.
func TestOpenRefusesNonUTF8Database(t *testing.T) {
	dsn := pgtest.NewDatabaseWithLocale(t, pgtest.Locale{Encoding: "SQL_ASCII", Locale: "C"})

	s, err := postgres.Open(context.Background(), dsn)

	if err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded, want ErrUnsupportedDatabase")
	}

	if !errors.Is(err, postgres.ErrUnsupportedDatabase) {
		t.Errorf("Open error = %v, want ErrUnsupportedDatabase", err)
	}
}
