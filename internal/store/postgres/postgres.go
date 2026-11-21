// Package postgres is the Postgres store: persistence through a pgx/v5 pool,
// with embeddings in pgvector columns. Hybrid search runs in SQL (ADR 0005).
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	pgxvec "github.com/pgvector/pgvector-go/pgx"

	"github.com/atomicpages/wayfindr/internal/store"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store is the Postgres implementation of store.Store. It is safe for
// concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

var _ store.Store = (*Store)(nil)

// Open connects to the database at dsn (a postgres:// URL or a keyword/value
// string), applies pending migrations, and returns the store. The role needs
// to be able to create the vector and pg_trgm extensions on first run. Open
// fails with an error wrapping store.ErrSchemaTooNew when a newer binary has
// migrated the database, and with one wrapping ErrUnsupportedDatabase when the
// database is not UTF8 or the server lacks ICU. Any LC_CTYPE, C included, is
// fine.
//
// The DSN may hold a password. A DSN that does not parse gets a fixed
// message, because pgx masks the password in its parse errors only best
// effort. Later errors are pgx's connect and query errors, which name the
// user and database but not the password.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)

	if err != nil {
		return nil, errors.New("open postgres store: cannot parse the DSN (want a postgres:// URL or a keyword/value string)")
	}

	// Migrate before the pool exists: the pool registers the vector type on
	// every new connection, and the first migration is what creates it.
	if err := migrate(ctx, cfg.ConnConfig.Copy()); err != nil {
		return nil, fmt.Errorf("open postgres store: %w", err)
	}

	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if err := pgxvec.RegisterTypes(ctx, conn); err != nil {
			return fmt.Errorf("register pgvector types: %w", err)
		}

		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)

	if err != nil {
		return nil, fmt.Errorf("open postgres store: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()

		return nil, fmt.Errorf("open postgres store: %w", err)
	}

	return &Store{pool: pool}, nil
}

// ErrUnsupportedDatabase is returned by Open when the database cannot hold or
// search the catalog: its encoding is not UTF8, or the server has no ICU
// root collation "und-x-icu" (design §7.5).
var ErrUnsupportedDatabase = errors.New("unsupported postgres database")

// checkDatabase refuses a database that search cannot tokenise by Unicode
// rules. Tool definitions are UTF-8 JSON, and search_words pins the ICU root
// collation so that it does not depend on the database's LC_CTYPE.
func checkDatabase(ctx context.Context, db *sql.DB) error {
	var encoding string
	var icu bool

	if err := db.QueryRowContext(ctx, `SELECT current_setting('server_encoding'),
		EXISTS (SELECT 1 FROM pg_collation WHERE collname = 'und-x-icu')`).Scan(&encoding, &icu); err != nil {
		return fmt.Errorf("check database: %w", err)
	}

	if encoding != "UTF8" {
		return fmt.Errorf("%w: encoding is %s, want UTF8; create the database with ENCODING 'UTF8' TEMPLATE template0",
			ErrUnsupportedDatabase, encoding)
	}

	if !icu {
		return fmt.Errorf(`%w: collation "und-x-icu" does not exist; use a Postgres server built with ICU support`,
			ErrUnsupportedDatabase)
	}

	return nil
}

// migrate checks the database and runs the shared migration runner over
// database/sql, through pgx's stdlib adapter on a short-lived connection pool
// of its own.
func migrate(ctx context.Context, cfg *pgx.ConnConfig) error {
	db := stdlib.OpenDB(*cfg)
	defer func() { _ = db.Close() }()

	if err := checkDatabase(ctx, db); err != nil {
		return err
	}

	migrations, err := fs.Sub(migrationFiles, "migrations")

	if err != nil {
		return err
	}

	return store.Migrate(ctx, db, migrations)
}

// Close closes the connection pool.
func (s *Store) Close() error {
	s.pool.Close()

	return nil
}

// EnsureUser implements store.Store. The table lock makes two concurrent
// first calls agree on one user instead of creating two.
func (s *Store) EnsureUser(ctx context.Context) (string, error) {
	tx, err := s.pool.Begin(ctx)

	if err != nil {
		return "", fmt.Errorf("ensure user: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return "", fmt.Errorf("ensure user: lock: %w", err)
	}

	var id string
	err = tx.QueryRow(ctx, `SELECT id::text FROM users ORDER BY created_at, id LIMIT 1`).Scan(&id)

	switch {
	case err == nil:
		return id, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("ensure user: %w", err)
	}

	u, err := uuid.NewV7()

	if err != nil {
		return "", fmt.Errorf("ensure user: new id: %w", err)
	}

	id = u.String()

	if _, err := tx.Exec(ctx, `INSERT INTO users (id) VALUES ($1)`, id); err != nil {
		return "", fmt.Errorf("ensure user: insert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("ensure user: commit: %w", err)
	}

	return id, nil
}
