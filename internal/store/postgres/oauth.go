package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/atomicpages/wayfindr/internal/store"
)

// CreateOAuthGrant implements store.Store. It takes the name lock that
// CreateClientToken takes, so the shared name space holds under concurrent
// creates. Times are truncated to the millisecond, as SQLite stores them.
func (s *Store) CreateOAuthGrant(ctx context.Context, g store.OAuthGrant) error {
	if err := store.CheckOAuthGrant(g); err != nil {
		return fmt.Errorf("create oauth grant %s: %w", g.Name, err)
	}

	tx, err := s.pool.Begin(ctx)

	if err != nil {
		return fmt.Errorf("create oauth grant %s: %w", g.Name, err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockName(ctx, tx, g.UserID, g.Name); err != nil {
		return fmt.Errorf("create oauth grant %s: %w", g.Name, err)
	}

	tag, err := tx.Exec(ctx, `INSERT INTO oauth_grants (user_id, name, client_id, resource, connectors, created_at)
		SELECT $1::uuid, $2::text, $3::text, $4::text, $5::text[], $6::timestamptz
		WHERE NOT EXISTS (SELECT 1 FROM client_tokens WHERE user_id = $1 AND name = $2)
		ON CONFLICT DO NOTHING`,
		g.UserID, g.Name, g.ClientID, g.Resource, g.Connectors, g.CreatedAt.Truncate(time.Millisecond))

	if err != nil {
		return fmt.Errorf("create oauth grant %s: %w", g.Name, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("create oauth grant %s: %w", g.Name, store.ErrExists)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("create oauth grant %s: commit: %w", g.Name, err)
	}

	return nil
}

const oauthGrantColumns = `name, client_id, resource, connectors, created_at, last_used_at`

func scanOAuthGrant(userID string, row pgx.Row) (store.OAuthGrant, error) {
	g := store.OAuthGrant{UserID: userID}
	err := row.Scan(&g.Name, &g.ClientID, &g.Resource, &g.Connectors, &g.CreatedAt, &g.LastUsedAt)

	return g, err
}

// GetOAuthGrant implements store.Store.
func (s *Store) GetOAuthGrant(ctx context.Context, userID, name string) (store.OAuthGrant, error) {
	g, err := scanOAuthGrant(userID, s.pool.QueryRow(ctx,
		`SELECT `+oauthGrantColumns+` FROM oauth_grants WHERE user_id = $1 AND name = $2`, userID, name))

	if errors.Is(err, pgx.ErrNoRows) {
		return store.OAuthGrant{}, fmt.Errorf("get oauth grant %s: %w", name, store.ErrNotFound)
	}

	if err != nil {
		return store.OAuthGrant{}, fmt.Errorf("get oauth grant %s: %w", name, err)
	}

	return g, nil
}

// ListOAuthGrants implements store.Store. The "C" collation sorts bytewise,
// as SQLite does.
func (s *Store) ListOAuthGrants(ctx context.Context, userID string) ([]store.OAuthGrant, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+oauthGrantColumns+` FROM oauth_grants WHERE user_id = $1 ORDER BY name COLLATE "C"`, userID)

	if err != nil {
		return nil, fmt.Errorf("list oauth grants: %w", err)
	}

	defer rows.Close()

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
// key's cascade.
func (s *Store) DeleteOAuthGrant(ctx context.Context, userID, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM oauth_grants WHERE user_id = $1 AND name = $2`, userID, name)

	if err != nil {
		return fmt.Errorf("delete oauth grant %s: %w", name, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete oauth grant %s: %w", name, store.ErrNotFound)
	}

	return nil
}

const oauthTokenColumns = `digest, grant_name, kind, created_at, expires_at, used_at`

func scanOAuthToken(userID string, row pgx.Row) (store.OAuthToken, error) {
	var (
		t      = store.OAuthToken{UserID: userID}
		digest []byte
		kind   string
	)

	if err := row.Scan(&digest, &t.Grant, &kind, &t.CreatedAt, &t.ExpiresAt, &t.UsedAt); err != nil {
		return store.OAuthToken{}, err
	}

	if len(digest) != len(t.Digest) {
		return store.OAuthToken{}, fmt.Errorf("%s: digest is %d bytes, want %d", t.Grant, len(digest), len(t.Digest))
	}

	copy(t.Digest[:], digest)
	t.Kind = store.OAuthTokenKind(kind)

	return t, nil
}

// GetOAuthToken implements store.Store.
func (s *Store) GetOAuthToken(ctx context.Context, userID string, d [sha256.Size]byte) (store.OAuthToken, error) {
	t, err := scanOAuthToken(userID, s.pool.QueryRow(ctx,
		`SELECT `+oauthTokenColumns+` FROM oauth_tokens WHERE user_id = $1 AND digest = $2`, userID, d[:]))

	if errors.Is(err, pgx.ErrNoRows) {
		return store.OAuthToken{}, fmt.Errorf("get oauth token: %w", store.ErrNotFound)
	}

	if err != nil {
		return store.OAuthToken{}, fmt.Errorf("get oauth token: %w", err)
	}

	return t, nil
}

// ListOAuthAccessTokens implements store.Store.
func (s *Store) ListOAuthAccessTokens(ctx context.Context, userID string, now time.Time) ([]store.OAuthToken, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+oauthTokenColumns+` FROM oauth_tokens
		WHERE user_id = $1 AND kind = 'access' AND expires_at > $2 ORDER BY grant_name COLLATE "C"`,
		userID, now.Truncate(time.Millisecond))

	if err != nil {
		return nil, fmt.Errorf("list oauth access tokens: %w", err)
	}

	defer rows.Close()

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
func insertOAuthToken(ctx context.Context, tx pgx.Tx, userID string, t store.OAuthToken) error {
	var expiresAt *time.Time

	if t.ExpiresAt != nil {
		at := t.ExpiresAt.Truncate(time.Millisecond)
		expiresAt = &at
	}

	_, err := tx.Exec(ctx, `INSERT INTO oauth_tokens (digest, user_id, grant_name, kind, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		t.Digest[:], userID, t.Grant, string(t.Kind), t.CreatedAt.Truncate(time.Millisecond), expiresAt)

	return err
}

// IssueOAuthTokens implements store.Store. It locks the grant's row, so two
// code exchanges for one grant run one after the other, and the second finds
// the first's tokens.
func (s *Store) IssueOAuthTokens(ctx context.Context, userID string, access, refresh store.OAuthToken) error {
	if err := store.CheckOAuthTokenPair(userID, access, refresh); err != nil {
		return fmt.Errorf("issue oauth tokens for %s: %w", access.Grant, err)
	}

	tx, err := s.pool.Begin(ctx)

	if err != nil {
		return fmt.Errorf("issue oauth tokens for %s: %w", access.Grant, err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	var name string

	err = tx.QueryRow(ctx, `SELECT name FROM oauth_grants WHERE user_id = $1 AND name = $2 FOR UPDATE`,
		userID, access.Grant).Scan(&name)

	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("issue oauth tokens for %s: %w", access.Grant, store.ErrNotFound)
	}

	if err != nil {
		return fmt.Errorf("issue oauth tokens for %s: %w", access.Grant, err)
	}

	var has bool

	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM oauth_tokens WHERE user_id = $1 AND grant_name = $2)`,
		userID, access.Grant).Scan(&has); err != nil {
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

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("issue oauth tokens for %s: commit: %w", access.Grant, err)
	}

	return nil
}

// RotateOAuthTokens implements store.Store. It first takes a key-share lock
// on the grant's row, the order DeleteOAuthGrant works in (grant, then the
// tokens its cascade removes), so a rotation and a delete cannot deadlock: the
// loser waits, then finds the grant gone and reports ErrNotFound. Then the
// conditional UPDATE decides a race between rotations: it locks the refresh
// token's row, so of two rotations of one token the second waits, then finds
// the row spent, updates none, and reports ErrSpent.
func (s *Store) RotateOAuthTokens(ctx context.Context, userID string, spent [sha256.Size]byte, at time.Time, access, refresh store.OAuthToken) error {
	if err := store.CheckOAuthRotation(userID, at, access, refresh); err != nil {
		return fmt.Errorf("rotate oauth tokens: %w", err)
	}

	tx, err := s.pool.Begin(ctx)

	if err != nil {
		return fmt.Errorf("rotate oauth tokens: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	var one int

	err = tx.QueryRow(ctx, `SELECT 1 FROM oauth_grants WHERE user_id = $1 AND name = $2 FOR KEY SHARE`,
		userID, access.Grant).Scan(&one)

	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("rotate oauth tokens: %w", store.ErrNotFound)
	}

	if err != nil {
		return fmt.Errorf("rotate oauth tokens: %w", err)
	}

	var grant string

	err = tx.QueryRow(ctx, `UPDATE oauth_tokens SET used_at = $3
		WHERE user_id = $1 AND digest = $2 AND kind = 'refresh' AND used_at IS NULL
		RETURNING grant_name`, userID, spent[:], at.Truncate(time.Millisecond)).Scan(&grant)

	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT 1 FROM oauth_tokens WHERE user_id = $1 AND digest = $2 AND kind = 'refresh'`,
			userID, spent[:]).Scan(&one)

		if errors.Is(err, pgx.ErrNoRows) {
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

	if _, err := tx.Exec(ctx, `DELETE FROM oauth_tokens WHERE user_id = $1 AND grant_name = $2 AND kind = 'access'`,
		userID, grant); err != nil {
		return fmt.Errorf("rotate oauth tokens: %w", err)
	}

	for _, t := range []store.OAuthToken{access, refresh} {
		if err := insertOAuthToken(ctx, tx, userID, t); err != nil {
			return fmt.Errorf("rotate oauth tokens: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rotate oauth tokens: commit: %w", err)
	}

	return nil
}

// TouchOAuthGrants implements store.Store. It updates in digest order, so
// concurrent touches lock rows in the same order.
func (s *Store) TouchOAuthGrants(ctx context.Context, userID string, used map[[sha256.Size]byte]time.Time) error {
	if len(used) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)

	if err != nil {
		return fmt.Errorf("touch oauth grants: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	for _, d := range slices.SortedFunc(maps.Keys(used), compareDigests) {
		at := used[d].Truncate(time.Millisecond)

		if _, err := tx.Exec(ctx, `UPDATE oauth_grants SET last_used_at = $3
			WHERE user_id = $1 AND (last_used_at IS NULL OR last_used_at < $3)
			AND name IN (SELECT grant_name FROM oauth_tokens WHERE user_id = $1 AND digest = $2 AND kind = 'access')`,
			userID, d[:], at); err != nil {
			return fmt.Errorf("touch oauth grant: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("touch oauth grants: commit: %w", err)
	}

	return nil
}

// PruneOAuth implements store.Store. It drops the never-exchanged grants
// first, so a grant that loses its last token to this prune is not also taken
// as never exchanged. It locks the candidate grants before it deletes them,
// so a code exchange that commits in between keeps its grant: the delete
// re-checks for tokens after the lock.
func (s *Store) PruneOAuth(ctx context.Context, userID string, now time.Time) (int64, error) {
	tx, err := s.pool.Begin(ctx)

	if err != nil {
		return 0, fmt.Errorf("prune oauth: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `SELECT name FROM oauth_grants WHERE user_id = $1 AND created_at <= $2
		AND NOT EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.user_id = oauth_grants.user_id AND t.grant_name = oauth_grants.name)
		ORDER BY name FOR UPDATE`, userID, now.Add(-store.OAuthUnexchangedTTL).Truncate(time.Millisecond))

	if err != nil {
		return 0, fmt.Errorf("prune oauth: %w", err)
	}

	candidates, err := pgx.CollectRows(rows, pgx.RowTo[string])

	if err != nil {
		return 0, fmt.Errorf("prune oauth: %w", err)
	}

	var total int64

	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM oauth_grants WHERE user_id = $1 AND name = ANY($2)
			AND NOT EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.user_id = oauth_grants.user_id AND t.grant_name = oauth_grants.name)`,
			[]any{userID, candidates}},
		{`DELETE FROM oauth_tokens WHERE user_id = $1 AND kind = 'access' AND expires_at <= $2`,
			[]any{userID, now.Truncate(time.Millisecond)}},
		{`DELETE FROM oauth_tokens WHERE user_id = $1 AND kind = 'refresh' AND used_at <= $2`,
			[]any{userID, now.Add(-store.OAuthSpentRetention).Truncate(time.Millisecond)}},
	} {
		tag, err := tx.Exec(ctx, q.sql, q.args...)

		if err != nil {
			return 0, fmt.Errorf("prune oauth: %w", err)
		}

		total += tag.RowsAffected()
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("prune oauth: commit: %w", err)
	}

	return total, nil
}
