package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/atomicpages/wayfindr/internal/store"
)

// CreateClientToken implements store.Store. ON CONFLICT DO NOTHING covers
// both the name and the digest, so a conflict on either inserts no row, and
// so does an OAuth grant of the same name. CreatedAt is truncated to the
// millisecond, as SQLite stores it.
func (s *Store) CreateClientToken(ctx context.Context, t store.ClientToken) error {
	if len(t.Connectors) == 0 {
		return fmt.Errorf("create client token %s: no connectors", t.Name)
	}

	if t.CreatedAt.IsZero() {
		return fmt.Errorf("create client token %s: no creation time", t.Name)
	}

	// The name lock makes the check for a grant of that name and the insert
	// atomic against CreateOAuthGrant, which takes the same lock.
	tx, err := s.pool.Begin(ctx)

	if err != nil {
		return fmt.Errorf("create client token %s: %w", t.Name, err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockName(ctx, tx, t.UserID, t.Name); err != nil {
		return fmt.Errorf("create client token %s: %w", t.Name, err)
	}

	tag, err := tx.Exec(ctx, `INSERT INTO client_tokens (user_id, name, digest, connectors, created_at, remote, approval)
		SELECT $1::uuid, $2::text, $3::bytea, $4::text[], $5::timestamptz, $6::boolean, $7::text[]
		WHERE NOT EXISTS (SELECT 1 FROM oauth_grants WHERE user_id = $1 AND name = $2)
		ON CONFLICT DO NOTHING`,
		t.UserID, t.Name, t.Digest[:], t.Connectors, t.CreatedAt.Truncate(time.Millisecond), t.Remote, approvalArg(t.Approval))

	if err != nil {
		return fmt.Errorf("create client token %s: %w", t.Name, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("create client token %s: %w", t.Name, store.ErrExists)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("create client token %s: commit: %w", t.Name, err)
	}

	return nil
}

// lockName takes a transaction-scoped lock on the user's name in the space
// client tokens and OAuth grants share, so two creates of one name run one
// after the other and the second sees the first.
func lockName(ctx context.Context, tx pgx.Tx, userID, name string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || '/' || $2::text, 0))`, userID, name)

	return err
}

// ListClientTokens implements store.Store. The "C" collation sorts
// bytewise, as SQLite does.
func (s *Store) ListClientTokens(ctx context.Context, userID string) ([]store.ClientToken, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, digest, connectors, created_at, last_used_at, remote, approval
		FROM client_tokens WHERE user_id = $1 ORDER BY name COLLATE "C"`, userID)

	if err != nil {
		return nil, fmt.Errorf("list client tokens: %w", err)
	}

	defer rows.Close()

	var tokens []store.ClientToken

	for rows.Next() {
		var (
			t      = store.ClientToken{UserID: userID}
			digest []byte
		)

		if err := rows.Scan(&t.Name, &digest, &t.Connectors, &t.CreatedAt, &t.LastUsedAt, &t.Remote, &t.Approval); err != nil {
			return nil, fmt.Errorf("list client tokens: %w", err)
		}

		if len(digest) != len(t.Digest) {
			return nil, fmt.Errorf("list client tokens: %s: digest is %d bytes, want %d", t.Name, len(digest), len(t.Digest))
		}

		copy(t.Digest[:], digest)
		tokens = append(tokens, t)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list client tokens: %w", err)
	}

	return tokens, nil
}

// DeleteClientToken implements store.Store.
func (s *Store) DeleteClientToken(ctx context.Context, userID, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM client_tokens WHERE user_id = $1 AND name = $2`, userID, name)

	if err != nil {
		return fmt.Errorf("delete client token %s: %w", name, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete client token %s: %w", name, store.ErrNotFound)
	}

	return nil
}

// TouchClientTokens implements store.Store. It updates the tokens in digest
// order, so concurrent touches lock rows in the same order.
func (s *Store) TouchClientTokens(ctx context.Context, userID string, used map[[sha256.Size]byte]time.Time) error {
	if len(used) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)

	if err != nil {
		return fmt.Errorf("touch client tokens: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	for _, d := range slices.SortedFunc(maps.Keys(used), compareDigests) {
		at := used[d].Truncate(time.Millisecond)

		if _, err := tx.Exec(ctx, `UPDATE client_tokens SET last_used_at = $3
			WHERE user_id = $1 AND digest = $2 AND (last_used_at IS NULL OR last_used_at < $3)`,
			userID, d[:], at); err != nil {
			return fmt.Errorf("touch client token: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("touch client tokens: commit: %w", err)
	}

	return nil
}

func compareDigests(a, b [sha256.Size]byte) int { return bytes.Compare(a[:], b[:]) }

// approvalArg is the approval column for entries: nil (NULL) for none, so
// an empty list and an absent one read back the same.
func approvalArg(entries []string) []string {
	if len(entries) == 0 {
		return nil
	}

	return entries
}
