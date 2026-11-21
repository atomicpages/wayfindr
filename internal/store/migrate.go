package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

// ErrSchemaTooNew is returned by Migrate when the database's schema version is
// newer than the newest migration this binary carries. Refusing to start
// protects the data from being used by an older (downgraded) binary.
var ErrSchemaTooNew = errors.New("database schema is newer than this binary")

// migrationName matches NNNN_name.sql; it is a constant-like function rather
// than a package var so there is no package-level mutable state.
func migrationName() *regexp.Regexp {
	return regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)
}

// The schema_version DDL and queries are portable between SQLite and Postgres,
// so every driver shares this runner and only supplies its own migration files.
const (
	createSchemaVersion = `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`
	selectSchemaVersion = `SELECT COALESCE(MAX(version), 0) FROM schema_version`
	clearSchemaVersion  = `DELETE FROM schema_version`
	insertSchemaVersion = `INSERT INTO schema_version (version) VALUES ($1)`
)

type migration struct {
	version int
	name    string
	sql     string
}

// SchemaVersion returns the database's schema version, or 0 when no migration
// has run yet. It creates the empty schema_version table if it is missing.
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	if _, err := db.ExecContext(ctx, createSchemaVersion); err != nil {
		return 0, fmt.Errorf("create schema_version: %w", err)
	}

	var v int

	if err := db.QueryRowContext(ctx, selectSchemaVersion).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema_version: %w", err)
	}

	return v, nil
}

// Migrate applies, in order, every migration in the root of fsys that is newer
// than the database's schema version. Files are named NNNN_name.sql and
// numbered from 0001 without gaps. Migrations are forward-only; each runs in
// its own transaction together with the schema_version update, so a failure
// leaves the database at the last migration that succeeded.
//
// Migrate returns an error wrapping ErrSchemaTooNew when the database is
// already past the newest migration in fsys.
func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	ms, err := loadMigrations(fsys)

	if err != nil {
		return err
	}

	current, err := SchemaVersion(ctx, db)

	if err != nil {
		return err
	}

	if current > len(ms) {
		return fmt.Errorf("database schema version %d, binary supports up to %d: %w", current, len(ms), ErrSchemaTooNew)
	}

	for _, m := range ms[current:] {
		if err := apply(ctx, db, m); err != nil {
			return err
		}
	}

	return nil
}

func apply(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)

	if err != nil {
		return fmt.Errorf("migration %04d_%s: begin: %w", m.version, m.name, err)
	}

	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
	}

	if _, err := tx.ExecContext(ctx, clearSchemaVersion); err != nil {
		return fmt.Errorf("migration %04d_%s: clear schema_version: %w", m.version, m.name, err)
	}

	if _, err := tx.ExecContext(ctx, insertSchemaVersion, m.version); err != nil {
		return fmt.Errorf("migration %04d_%s: record schema_version: %w", m.version, m.name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %04d_%s: commit: %w", m.version, m.name, err)
	}

	return nil
}

func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")

	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	re := migrationName()
	ms := make([]migration, 0, len(entries))

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		parts := re.FindStringSubmatch(e.Name())

		if parts == nil {
			return nil, fmt.Errorf("migration %q: name must match NNNN_name.sql", e.Name())
		}

		version, err := strconv.Atoi(parts[1])

		if err != nil {
			return nil, fmt.Errorf("migration %q: version: %w", e.Name(), err)
		}

		body, err := fs.ReadFile(fsys, e.Name())

		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}

		ms = append(ms, migration{version: version, name: parts[2], sql: string(body)})
	}

	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })

	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %04d_%s: expected version %04d (versions start at 1, without gaps or duplicates)", m.version, m.name, i+1)
		}
	}

	return ms, nil
}
