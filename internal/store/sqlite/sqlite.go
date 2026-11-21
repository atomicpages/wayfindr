// Package sqlite is the default store: SQLite persistence through the pure-Go
// modernc.org/sqlite driver. Search runs in Go over an in-memory index of the
// catalog (ADR 0005), loaded at Open and updated after every catalog write.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/atomicpages/wayfindr/internal/newfile"
	"github.com/atomicpages/wayfindr/internal/store"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store is the SQLite implementation of store.Store. It is safe for
// concurrent use. The daemon is the only writer (design §9).
type Store struct {
	db    *sql.DB
	now   func() time.Time
	index *index

	// writeMu serializes catalog writes with their index updates, so the
	// index applies commits in the order the database did.
	writeMu sync.Mutex
}

var _ store.Store = (*Store)(nil)

// Open opens (creating if needed) the database at path, applies pending
// migrations, and returns the store. A new database file is created 0600,
// like every other file in the data dir; SQLite gives its -wal and -shm
// files the database's mode. It fails with an error wrapping
// store.ErrSchemaTooNew when a newer binary has migrated the database.
func Open(ctx context.Context, path string) (*Store, error) {
	// SQLite treats the empty file as a new database; left to the driver,
	// the file would get the umask default (usually 0644).
	if err := newfile.Create(path, nil, 0o600, path); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("open store: %w", err)
	}

	db, err := sql.Open("sqlite", dsn(path))

	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}

	migrations, err := fs.Sub(migrationFiles, "migrations")

	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("open store %s: %w", path, err)
	}

	if err := store.Migrate(ctx, db, migrations); err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("open store %s: %w", path, err)
	}

	idx, err := loadIndex(ctx, db)

	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("open store %s: %w", path, err)
	}

	return &Store{db: db, now: time.Now, index: idx}, nil
}

// dsn applies the connection settings from design §5 to every pooled
// connection. _txlock=immediate takes the write lock at BEGIN, so a write
// transaction waits on busy_timeout instead of failing to upgrade its lock.
//
// A SQLite URI path uses forward slashes, and a Windows drive letter follows
// a leading slash: file:///C:/Users/…/wayfindr.db. Without that slash the
// drive would be read as the URI's authority, which SQLite rejects.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Set("_txlock", "immediate")

	p := filepath.ToSlash(path)

	if filepath.VolumeName(path) != "" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}

	return (&url.URL{Scheme: "file", Path: p, RawQuery: q.Encode()}).String()
}

// Close closes the database.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close store: %w", err)
	}

	return nil
}

// EnsureUser implements store.Store.
func (s *Store) EnsureUser(ctx context.Context) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)

	if err != nil {
		return "", fmt.Errorf("ensure user: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM users ORDER BY created_at, id LIMIT 1`).Scan(&id)

	switch {
	case err == nil:
		return id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("ensure user: %w", err)
	}

	u, err := uuid.NewV7()

	if err != nil {
		return "", fmt.Errorf("ensure user: new id: %w", err)
	}

	id = u.String()

	if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, created_at) VALUES (?, ?)`, id, s.now().UnixMilli()); err != nil {
		return "", fmt.Errorf("ensure user: insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("ensure user: commit: %w", err)
	}

	return id, nil
}
