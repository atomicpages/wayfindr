// Package pgtest gives tests a throwaway Postgres database. The store suite
// (internal/store/storetest), the Postgres driver's own tests, the parity
// tests, and the init tests (internal/setup) share it, since none can import
// another's test files.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// DSNEnv names the variable that points the tests at a Postgres server with
// pgvector available, as a postgres:// URL whose role may create databases.
// `make test-postgres` sets it; without it the Postgres tests skip.
const DSNEnv = "WAYFINDR_TEST_PG_DSN"

// NewDatabase creates an empty database for one test on the server named by
// WAYFINDR_TEST_PG_DSN, drops it when the test ends, and returns its DSN. It
// takes the server's default encoding and locale, and skips t when the
// variable is unset.
func NewDatabase(t testing.TB) string {
	t.Helper()

	return newDatabase(t, "")
}

// Locale fixes a test database's encoding and locale instead of taking the
// server's defaults, for tests of locale-dependent behaviour.
type Locale struct {
	Encoding string // such as "UTF8" or "SQL_ASCII"
	Locale   string // LC_COLLATE and LC_CTYPE, such as "C" or "en_US.utf8"
}

// NewDatabaseWithLocale is NewDatabase for a database created from template0
// with the given encoding and locale.
func NewDatabaseWithLocale(t testing.TB, l Locale) string {
	t.Helper()

	return newDatabase(t, fmt.Sprintf(" TEMPLATE template0 ENCODING %s LOCALE %s",
		quoteLiteral(l.Encoding), quoteLiteral(l.Locale)))
}

// newDatabase creates the database with options appended to CREATE DATABASE.
func newDatabase(t testing.TB, options string) string {
	t.Helper()
	admin := os.Getenv(DSNEnv)

	if admin == "" {
		t.Skipf("%s is not set; run make test-postgres", DSNEnv)
	}

	// The per-test DSN is built by swapping the URL's path, which only a
	// URL DSN has; a key=value DSN would parse here and then point the test
	// at the server's default database.
	u, err := url.Parse(admin)

	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("%s must be a postgres:// or postgresql:// URL", DSNEnv)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)

	if err != nil {
		t.Fatalf("connect to %s: %v", DSNEnv, err)
	}

	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	name := "wayfindr_test_" + hex.EncodeToString(suffix)
	ident := pgx.Identifier{name}.Sanitize()

	if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident+options); err != nil {
		t.Fatalf("create database: %v", err)
	}

	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), "DROP DATABASE "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})
	u.Path = "/" + name

	return u.String()
}

// quoteLiteral quotes s as an SQL string literal.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
