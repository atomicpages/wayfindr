package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/atomicpages/wayfindr/internal/store"
)

// CreateOAuthGrant implements store.Store. One statement checks the name
// against client_tokens and inserts, so the shared name space holds under
// concurrent creates: SQLite holds the write lock from the statement's start.
func (s *Store) CreateOAuthGrant(ctx context.Context, g store.OAuthGrant) error {
	if err := store.CheckOAuthGrant(g); err != nil {
		return fmt.Errorf("create oauth grant %s: %w", g.Name, err)
	}

	connectors, err := json.Marshal(g.Connectors)

	if err != nil {
		return fmt.Errorf("create oauth grant %s: %w", g.Name, err)
	}

	res, err := s.db.ExecContext(ctx, `INSERT INTO oauth_grants (user_id, name, client_id, resource, connectors, created_at)
		SELECT ?1, ?2, ?3, ?4, ?5, ?6
		WHERE NOT EXISTS (SELECT 1 FROM client_tokens WHERE user_id = ?1 AND name = ?2)
		ON CONFLICT DO NOTHING`,
		g.UserID, g.Name, g.ClientID, g.Resource, string(connectors), g.CreatedAt.UnixMilli())

	if err != nil {
		return fmt.Errorf("create oauth grant %s: %w", g.Name, err)
	}

	n, err := res.RowsAffected()

	if err != nil {
		return fmt.Errorf("create oauth grant %s: %w", g.Name, err)
	}

	if n == 0 {
		return fmt.Errorf("create oauth grant %s: %w", g.Name, store.ErrExists)
	}

	return nil
}

const oauthGrantColumns = `name, client_id, resource, connectors, created_at, last_used_at`

func scanOAuthGrant(userID string, row scanner) (store.OAuthGrant, error) {
	var (
		g          = store.OAuthGrant{UserID: userID}
		connectors string
		createdAt  int64
		lastUsedAt sql.NullInt64
	)

	if err := row.Scan(&g.Name, &g.ClientID, &g.Resource, &connectors, &createdAt, &lastUsedAt); err != nil {
		return store.OAuthGrant{}, err
	}

	if err := json.Unmarshal([]byte(connectors), &g.Connectors); err != nil {
		return store.OAuthGrant{}, fmt.Errorf("%s: connectors: %w", g.Name, err)
	}

	g.CreatedAt = time.UnixMilli(createdAt)

	if lastUsedAt.Valid {
		at := time.UnixMilli(lastUsedAt.Int64)
		g.LastUsedAt = &at
	}

	return g, nil
}

// GetOAuthGrant implements store.Store.
func (s *Store) GetOAuthGrant(ctx context.Context, userID, name string) (store.OAuthGrant, error) {
	g, err := scanOAuthGrant(userID, s.db.QueryRowContext(ctx,
		`SELECT `+oauthGrantColumns+` FROM oauth_grants WHERE user_id = ? AND name = ?`, userID, name))

	if errors.Is(err, sql.ErrNoRows) {
		return store.OAuthGrant{}, fmt.Errorf("get oauth grant %s: %w", name, store.ErrNotFound)
	}

	if err != nil {
		return store.OAuthGrant{}, fmt.Errorf("get oauth grant %s: %w", name, err)
	}

	return g, nil
}

// ListOAuthGrants implements store.Store.
func (s *Store) ListOAuthGrants(ctx context.Context, userID string) ([]store.OAuthGrant, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+oauthGrantColumns+` FROM oauth_grants WHERE user_id = ? ORDER BY name`, userID)

	if err != nil {
		return nil, fmt.Errorf("list oauth grants: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var grants []store.OAuthGrant

	for rows.Next() {
		g, err := scanOAuthGrant(userID, rows)

		if err != nil {
			return nil, fmt.Errorf("list oauth grants: %w", err)
		}

		grants = append(grants, g)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list oauth grants: %w", err)
	}

	return grants, nil
}

// DeleteOAuthGrant implements store.Store. The tokens go by the foreign
// key's cascade (the foreign_keys pragma is on).
func (s *Store) DeleteOAuthGrant(ctx context.Context, userID, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oauth_grants WHERE user_id = ? AND name = ?`, userID, name)

	if err != nil {
		return fmt.Errorf("delete oauth grant %s: %w", name, err)
	}

	n, err := res.RowsAffected()

	if err != nil {
		return fmt.Errorf("delete oauth grant %s: %w", name, err)
	}

	if n == 0 {
		return fmt.Errorf("delete oauth grant %s: %w", name, store.ErrNotFound)
	}

	return nil
}

func scanOAuthToken(userID string, row scanner) (store.OAuthToken, error) {
	var (
		t         = store.OAuthToken{UserID: userID}
		digest    []byte
		kind      string
		createdAt int64
		expiresAt sql.NullInt64
		usedAt    sql.NullInt64
	)

	if err := row.Scan(&digest, &t.Grant, &kind, &createdAt, &expiresAt, &usedAt); err != nil {
		return store.OAuthToken{}, err
	}

	if len(digest) != len(t.Digest) {
		return store.OAuthToken{}, fmt.Errorf("%s: digest is %d bytes, want %d", t.Grant, len(digest), len(t.Digest))
	}

	copy(t.Digest[:], digest)
	t.Kind = store.OAuthTokenKind(kind)
	t.CreatedAt = time.UnixMilli(createdAt)

	if expiresAt.Valid {
		at := time.UnixMilli(expiresAt.Int64)
		t.ExpiresAt = &at
	}

	if usedAt.Valid {
		at := time.UnixMilli(usedAt.Int64)
		t.UsedAt = &at
	}

	return t, nil
}

const oauthTokenColumns = `digest, grant_name, kind, created_at, expires_at, used_at`

// GetOAuthToken implements store.Store.
func (s *Store) GetOAuthToken(ctx context.Context, userID string, d [sha256.Size]byte) (store.OAuthToken, error) {
	t, err := scanOAuthToken(userID, s.db.QueryRowContext(ctx,
		`SELECT `+oauthTokenColumns+` FROM oauth_tokens WHERE user_id = ? AND digest = ?`, userID, d[:]))

	if errors.Is(err, sql.ErrNoRows) {
		return store.OAuthToken{}, fmt.Errorf("get oauth token: %w", store.ErrNotFound)
	}

	if err != nil {
		return store.OAuthToken{}, fmt.Errorf("get oauth token: %w", err)
	}

	return t, nil
}

// ListOAuthAccessTokens implements store.Store.
func (s *Store) ListOAuthAccessTokens(ctx context.Context, userID string, now time.Time) ([]store.OAuthToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+oauthTokenColumns+` FROM oauth_tokens
		WHERE user_id = ? AND kind = 'access' AND expires_at > ? ORDER BY grant_name`, userID, now.UnixMilli())

	if err != nil {
		return nil, fmt.Errorf("list oauth access tokens: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var tokens []store.OAuthToken

	for rows.Next() {
		t, err := scanOAuthToken(userID, rows)

		if err != nil {
			return nil, fmt.Errorf("list oauth access tokens: %w", err)
		}

		tokens = append(tokens, t)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list oauth access tokens: %w", err)
	}

	return tokens, nil
}

// insertOAuthToken inserts t, whose Grant, Kind, and times were checked.
func insertOAuthToken(ctx context.Context, tx *sql.Tx, userID string, t store.OAuthToken) error {
	var expiresAt any

	if t.ExpiresAt != nil {
		expiresAt = t.ExpiresAt.UnixMilli()
	}

	_, err := tx.ExecContext(ctx, `INSERT INTO oauth_tokens (digest, user_id, grant_name, kind, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		t.Digest[:], userID, t.Grant, string(t.Kind), t.CreatedAt.UnixMilli(), expiresAt)

	return err
}

// IssueOAuthTokens implements store.Store. The transaction takes the write
// lock first (_txlock=immediate), so two code exchanges for one grant run one
// after the other, and the second finds the first's tokens.
func (s *Store) IssueOAuthTokens(ctx context.Context, userID string, access, refresh store.OAuthToken) error {
	if err := store.CheckOAuthTokenPair(userID, access, refresh); err != nil {
		return fmt.Errorf("issue oauth tokens for %s: %w", access.Grant, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)

	if err != nil {
		return fmt.Errorf("issue oauth tokens for %s: %w", access.Grant, err)
	}

	defer func() { _ = tx.Rollback() }()

	var has bool

	err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM oauth_tokens WHERE user_id = ?1 AND grant_name = ?2)
		FROM oauth_grants WHERE user_id = ?1 AND name = ?2`, userID, access.Grant).Scan(&has)

	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("issue oauth tokens for %s: %w", access.Grant, store.ErrNotFound)
	}

	if err != nil {
		return fmt.Errorf("issue oauth tokens for %s: %w", access.Grant, err)
	}

	if has {
		return fmt.Errorf("issue oauth tokens for %s: %w", access.Grant, store.ErrExists)
	}

	for _, t := range []store.OAuthToken{access, refresh} {
		if err := insertOAuthToken(ctx, tx, userID, t); err != nil {
			return fmt.Errorf("issue oauth tokens for %s: %w", access.Grant, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("issue oauth tokens for %s: commit: %w", access.Grant, err)
	}

	return nil
}

// RotateOAuthTokens implements store.Store. The conditional UPDATE is the
// only thing that decides a race: of two rotations of one refresh token, one
// updates the row and the other updates none and reports ErrSpent.
func (s *Store) RotateOAuthTokens(ctx context.Context, userID string, spent [sha256.Size]byte, at time.Time, access, refresh store.OAuthToken) error {
	if err := store.CheckOAuthRotation(userID, at, access, refresh); err != nil {
		return fmt.Errorf("rotate oauth tokens: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)

	if err != nil {
		return fmt.Errorf("rotate oauth tokens: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	var grant string

	err = tx.QueryRowContext(ctx, `UPDATE oauth_tokens SET used_at = ?
		WHERE user_id = ? AND digest = ? AND kind = 'refresh' AND used_at IS NULL
		RETURNING grant_name`, at.UnixMilli(), userID, spent[:]).Scan(&grant)

	if errors.Is(err, sql.ErrNoRows) {
		var one int

		err = tx.QueryRowContext(ctx, `SELECT 1 FROM oauth_tokens WHERE user_id = ? AND digest = ? AND kind = 'refresh'`,
			userID, spent[:]).Scan(&one)

		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("rotate oauth tokens: %w", store.ErrNotFound)
		}

		if err != nil {
			return fmt.Errorf("rotate oauth tokens: %w", err)
		}

		return fmt.Errorf("rotate oauth tokens: %w", store.ErrSpent)
	}

	if err != nil {
		return fmt.Errorf("rotate oauth tokens: %w", err)
	}

	if access.Grant != grant {
		return fmt.Errorf("rotate oauth tokens: the new tokens name grant %s, not %s", access.Grant, grant)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE user_id = ? AND grant_name = ? AND kind = 'access'`,
		userID, grant); err != nil {
		return fmt.Errorf("rotate oauth tokens: %w", err)
	}

	for _, t := range []store.OAuthToken{access, refresh} {
		if err := insertOAuthToken(ctx, tx, userID, t); err != nil {
			return fmt.Errorf("rotate oauth tokens: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rotate oauth tokens: commit: %w", err)
	}

	return nil
}

// TouchOAuthGrants implements store.Store.
func (s *Store) TouchOAuthGrants(ctx context.Context, userID string, used map[[sha256.Size]byte]time.Time) error {
	if len(used) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)

	if err != nil {
		return fmt.Errorf("touch oauth grants: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	for _, d := range slices.SortedFunc(maps.Keys(used), compareDigests) {
		at := used[d].UnixMilli()

		if _, err := tx.ExecContext(ctx, `UPDATE oauth_grants SET last_used_at = ?1
			WHERE user_id = ?2 AND (last_used_at IS NULL OR last_used_at < ?1)
			AND name IN (SELECT grant_name FROM oauth_tokens WHERE user_id = ?2 AND digest = ?3 AND kind = 'access')`,
			at, userID, d[:]); err != nil {
			return fmt.Errorf("touch oauth grant: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("touch oauth grants: commit: %w", err)
	}

	return nil
}

// PruneOAuth implements store.Store. It drops the never-exchanged grants
// first, so a grant that loses its last token to this prune is not also taken
// as never exchanged.
func (s *Store) PruneOAuth(ctx context.Context, userID string, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)

	if err != nil {
		return 0, fmt.Errorf("prune oauth: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	var total int64

	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM oauth_grants WHERE user_id = ? AND created_at <= ?
			AND NOT EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.user_id = oauth_grants.user_id AND t.grant_name = oauth_grants.name)`,
			[]any{userID, now.Add(-store.OAuthUnexchangedTTL).UnixMilli()}},
		{`DELETE FROM oauth_tokens WHERE user_id = ? AND kind = 'access' AND expires_at <= ?`,
			[]any{userID, now.UnixMilli()}},
		{`DELETE FROM oauth_tokens WHERE user_id = ? AND kind = 'refresh' AND used_at <= ?`,
			[]any{userID, now.Add(-store.OAuthSpentRetention).UnixMilli()}},
	} {
		res, err := tx.ExecContext(ctx, q.sql, q.args...)

		if err != nil {
			return 0, fmt.Errorf("prune oauth: %w", err)
		}

		n, err := res.RowsAffected()

		if err != nil {
			return 0, fmt.Errorf("prune oauth: %w", err)
		}

		total += n
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("prune oauth: commit: %w", err)
	}

	return total, nil
}
