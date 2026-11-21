package storetest

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/postgres"
	"github.com/atomicpages/wayfindr/internal/store/postgres/pgtest"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
)

// Driver is one store driver, in one configuration, that the suite runs on.
type Driver struct {
	Name        string
	NewDatabase NewDatabase
}

// Driver names. Postgres runs twice: once on the server's default locale
// and once on a C ctype, since search must not depend on the database's
// locale (design §7.5).
const (
	SQLite         = "sqlite"
	Postgres       = "postgres"
	PostgresCCtype = "postgres-c-ctype"
)

// Drivers lists every driver configuration. The Postgres ones skip their
// test unless WAYFINDR_TEST_PG_DSN names a server (`make test-postgres`).
func Drivers() []Driver {
	return []Driver{
		{SQLite, newSQLiteDatabase},
		{Postgres, func(t *testing.T) Database {
			t.Helper()

			return postgresDatabase(pgtest.NewDatabase(t))
		}},
		// A C ctype classifies and lowercases only ASCII letters itself.
		{PostgresCCtype, func(t *testing.T) Database {
			t.Helper()

			return postgresDatabase(pgtest.NewDatabaseWithLocale(t, pgtest.Locale{Encoding: "UTF8", Locale: "C"}))
		}},
	}
}

// RunDriver runs the suite on the named driver configuration.
func RunDriver(t *testing.T, name string) {
	t.Helper()

	for _, d := range Drivers() {
		if d.Name == name {
			Run(t, d.NewDatabase)

			return
		}
	}

	t.Fatalf("no store driver %q", name)
}

// OpenStore opens a store on a fresh database of driver d, closes it when
// the test ends, and returns it with the local user.
func OpenStore(t *testing.T, d Driver) (store.Store, string) {
	t.Helper()

	return newStore(t, d.NewDatabase)
}

func newSQLiteDatabase(t *testing.T) Database {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wayfindr.db")

	return Database{
		Open: func(ctx context.Context) (store.Store, error) {
			s, err := sqlite.Open(ctx, path)

			if err != nil {
				return nil, err
			}

			return s, nil
		},
		SQL: func(t *testing.T) *sql.DB { return openSQL(t, "sqlite", path) },
	}
}

func postgresDatabase(dsn string) Database {
	return Database{
		Open: func(ctx context.Context) (store.Store, error) {
			s, err := postgres.Open(ctx, dsn)

			if err != nil {
				return nil, err
			}

			return s, nil
		},
		SQL: func(t *testing.T) *sql.DB { return openSQL(t, "pgx", dsn) },
	}
}

// openSQL opens a raw handle and closes it when the test ends.
func openSQL(t *testing.T, driverName, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, dsn)

	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}
