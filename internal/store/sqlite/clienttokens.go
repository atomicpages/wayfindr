package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/atomicpages/wayfindr/internal/store"
)

// CreateClientToken implements store.Store. ON CONFLICT DO NOTHING covers
// both the name and the digest, so a conflict on either inserts no row.
func (s *Store) CreateClientToken(ctx context.Context, t store.ClientToken) error {
	if len(t.Connectors) == 0 {
		return fmt.Errorf("create client token %s: no connectors", t.Name)
	}

	if t.CreatedAt.IsZero() {
		return fmt.Errorf("create client token %s: no creation time", t.Name)
	}

	connectors, err := json.Marshal(t.Connectors)

	if err != nil {
		return fmt.Errorf("create client token %s: %w", t.Name, err)
	}

	res, err := s.db.ExecContext(ctx, `INSERT INTO client_tokens (user_id, name, digest, connectors, created_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		t.UserID, t.Name, t.Digest[:], string(connectors), t.CreatedAt.UnixMilli())

	if err != nil {
		return fmt.Errorf("create client token %s: %w", t.Name, err)
	}

	n, err := res.RowsAffected()

	if err != nil {
		return fmt.Errorf("create client token %s: %w", t.Name, err)
	}

	if n == 0 {
		return fmt.Errorf("create client token %s: %w", t.Name, store.ErrExists)
	}

	return nil
}

// ListClientTokens implements store.Store.
func (s *Store) ListClientTokens(ctx context.Context, userID string) ([]store.ClientToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, digest, connectors, created_at, last_used_at
		FROM client_tokens WHERE user_id = ? ORDER BY name`, userID)

	if err != nil {
		return nil, fmt.Errorf("list client tokens: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var tokens []store.ClientToken

	for rows.Next() {
		var (
			t          = store.ClientToken{UserID: userID}
			digest     []byte
			connectors string
			createdAt  int64
			lastUsedAt sql.NullInt64
		)

		if err := rows.Scan(&t.Name, &digest, &connectors, &createdAt, &lastUsedAt); err != nil {
			return nil, fmt.Errorf("list client tokens: %w", err)
		}

		if len(digest) != len(t.Digest) {
			return nil, fmt.Errorf("list client tokens: %s: digest is %d bytes, want %d", t.Name, len(digest), len(t.Digest))
		}

		copy(t.Digest[:], digest)

		if err := json.Unmarshal([]byte(connectors), &t.Connectors); err != nil {
			return nil, fmt.Errorf("list client tokens: %s: connectors: %w", t.Name, err)
		}

		t.CreatedAt = time.UnixMilli(createdAt)

		if lastUsedAt.Valid {
			at := time.UnixMilli(lastUsedAt.Int64)
			t.LastUsedAt = &at
		}

		tokens = append(tokens, t)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list client tokens: %w", err)
	}

	return tokens, nil
}

// DeleteClientToken implements store.Store.
func (s *Store) DeleteClientToken(ctx context.Context, userID, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM client_tokens WHERE user_id = ? AND name = ?`, userID, name)

	if err != nil {
		return fmt.Errorf("delete client token %s: %w", name, err)
	}

	n, err := res.RowsAffected()

	if err != nil {
		return fmt.Errorf("delete client token %s: %w", name, err)
	}

	if n == 0 {
		return fmt.Errorf("delete client token %s: %w", name, store.ErrNotFound)
	}

	return nil
}

// TouchClientTokens implements store.Store.
func (s *Store) TouchClientTokens(ctx context.Context, userID string, used map[[sha256.Size]byte]time.Time) error {
	if len(used) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)

	if err != nil {
		return fmt.Errorf("touch client tokens: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	for _, d := range slices.SortedFunc(maps.Keys(used), compareDigests) {
		at := used[d].UnixMilli()

		if _, err := tx.ExecContext(ctx, `UPDATE client_tokens SET last_used_at = ?
			WHERE user_id = ? AND digest = ? AND (last_used_at IS NULL OR last_used_at < ?)`,
			at, userID, d[:], at); err != nil {
			return fmt.Errorf("touch client token: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("touch client tokens: commit: %w", err)
	}

	return nil
}

func compareDigests(a, b [sha256.Size]byte) int { return bytes.Compare(a[:], b[:]) }
