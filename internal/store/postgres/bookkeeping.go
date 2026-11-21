package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/atomicpages/wayfindr/internal/store"
)

// recordSync writes o as the connector's last-sync record in tx, part of
// ApplySync.
func recordSync(ctx context.Context, tx pgx.Tx, userID string, o store.SyncOutcome) error {
	var lastError, hiddenTools *string

	if o.Error != "" {
		lastError = &o.Error
	} else {
		enc, err := store.EncodeHidden(o.Hidden)

		if err != nil {
			return fmt.Errorf("record sync: %w", err)
		}

		hiddenTools = &enc
	}

	// A failed sync writes NULL, which keeps the hidden tools on record.
	if _, err := tx.Exec(ctx, `INSERT INTO connector_sync (user_id, connector, last_sync_at, last_error, hidden_tools)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, connector) DO UPDATE SET
			last_sync_at = excluded.last_sync_at, last_error = excluded.last_error,
			hidden_tools = COALESCE(excluded.hidden_tools, connector_sync.hidden_tools)`,
		userID, o.Connector, o.At, lastError, hiddenTools); err != nil {
		return fmt.Errorf("record sync: %w", err)
	}

	return nil
}

// LastSync implements store.Store.
func (s *Store) LastSync(ctx context.Context, userID, connector string) (store.SyncRecord, error) {
	var (
		at                     *time.Time
		lastError, hiddenTools *string
	)
	err := s.pool.QueryRow(ctx, `SELECT last_sync_at, last_error, hidden_tools FROM connector_sync
		WHERE user_id = $1 AND connector = $2`, userID, connector).Scan(&at, &lastError, &hiddenTools)

	if errors.Is(err, pgx.ErrNoRows) || (err == nil && at == nil) {
		return store.SyncRecord{}, fmt.Errorf("last sync %s: %w", connector, store.ErrNotFound)
	}

	if err != nil {
		return store.SyncRecord{}, fmt.Errorf("last sync %s: %w", connector, err)
	}

	hidden, err := store.DecodeHidden(deref(hiddenTools))

	if err != nil {
		return store.SyncRecord{}, fmt.Errorf("last sync %s: %w", connector, err)
	}

	return store.SyncRecord{At: *at, Error: deref(lastError), Hidden: hidden}, nil
}

// GetCredential implements store.Store.
func (s *Store) GetCredential(ctx context.Context, userID, connector string) (store.Credential, error) {
	c := store.Credential{UserID: userID, Connector: connector}
	var issuer, redirectURI, dcrClientID, clientID, granted, requested *string
	err := s.pool.QueryRow(ctx, `SELECT resource, issuer, redirect_uri, dcr_client_id, dcr_client, token,
			granted_scopes, requested_scopes, client_id
		FROM credentials WHERE user_id = $1 AND connector = $2`, userID, connector).
		Scan(&c.Resource, &issuer, &redirectURI, &dcrClientID, &c.DCRClient, &c.Token, &granted, &requested, &clientID)

	if errors.Is(err, pgx.ErrNoRows) {
		return store.Credential{}, fmt.Errorf("get credential %s: %w", connector, store.ErrNotFound)
	}

	if err != nil {
		return store.Credential{}, fmt.Errorf("get credential %s: %w", connector, err)
	}

	c.Issuer = deref(issuer)
	c.RedirectURI = deref(redirectURI)
	c.DCRClientID = deref(dcrClientID)
	c.ClientID = deref(clientID)
	c.GrantedScopes = deref(granted)
	c.RequestedScopes = deref(requested)

	return c, nil
}

// PutCredential implements store.Store. Empty optional fields are stored as NULL.
func (s *Store) PutCredential(ctx context.Context, c store.Credential) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO credentials (user_id, connector, resource, issuer,
			redirect_uri, dcr_client_id, dcr_client, token, granted_scopes, requested_scopes, client_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
		ON CONFLICT (user_id, connector) DO UPDATE SET
			resource = excluded.resource, issuer = excluded.issuer, redirect_uri = excluded.redirect_uri,
			dcr_client_id = excluded.dcr_client_id, dcr_client = excluded.dcr_client, token = excluded.token,
			granted_scopes = excluded.granted_scopes, requested_scopes = excluded.requested_scopes,
			client_id = excluded.client_id, updated_at = excluded.updated_at`,
		c.UserID, c.Connector, c.Resource, nullable(c.Issuer), nullable(c.RedirectURI),
		nullable(c.DCRClientID), c.DCRClient, c.Token, nullable(c.GrantedScopes),
		nullable(c.RequestedScopes), nullable(c.ClientID)); err != nil {
		return fmt.Errorf("put credential %s: %w", c.Connector, err)
	}

	return nil
}

// DeleteCredential implements store.Store.
func (s *Store) DeleteCredential(ctx context.Context, userID, connector string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM credentials WHERE user_id = $1 AND connector = $2`,
		userID, connector); err != nil {
		return fmt.Errorf("delete credential %s: %w", connector, err)
	}

	return nil
}

// ListCredentialConnectors implements store.Store. The "C" collation sorts
// bytewise, as SQLite does.
func (s *Store) ListCredentialConnectors(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT connector FROM credentials WHERE user_id = $1
		ORDER BY connector COLLATE "C"`, userID)

	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}

	connectors, err := pgx.CollectRows(rows, pgx.RowTo[string])

	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}

	return connectors, nil
}

// GetSecret implements store.Store.
func (s *Store) GetSecret(ctx context.Context, userID, name string) ([]byte, error) {
	var sealed []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM secrets WHERE user_id = $1 AND name = $2`,
		userID, name).Scan(&sealed)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("get secret %s: %w", name, store.ErrNotFound)
	}

	if err != nil {
		return nil, fmt.Errorf("get secret %s: %w", name, err)
	}

	return sealed, nil
}

// PutSecret implements store.Store.
func (s *Store) PutSecret(ctx context.Context, userID, name string, sealed []byte) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO secrets (user_id, name, value, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (user_id, name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		userID, name, sealed); err != nil {
		return fmt.Errorf("put secret %s: %w", name, err)
	}

	return nil
}

// DeleteSecret implements store.Store.
func (s *Store) DeleteSecret(ctx context.Context, userID, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM secrets WHERE user_id = $1 AND name = $2`, userID, name)

	if err != nil {
		return fmt.Errorf("delete secret %s: %w", name, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete secret %s: %w", name, store.ErrNotFound)
	}

	return nil
}

// ListSecrets implements store.Store. The "C" collation sorts bytewise, as
// SQLite does.
func (s *Store) ListSecrets(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM secrets WHERE user_id = $1
		ORDER BY name COLLATE "C"`, userID)

	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}

	names, err := pgx.CollectRows(rows, pgx.RowTo[string])

	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}

	return names, nil
}

// HasSealedData implements store.Store.
func (s *Store) HasSealedData(ctx context.Context) (bool, error) {
	var exists bool

	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM credentials) OR EXISTS (SELECT 1 FROM secrets)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("check sealed data: %w", err)
	}

	return exists, nil
}

// GetMetadata implements store.Store.
func (s *Store) GetMetadata(ctx context.Context, url string) (store.Metadata, error) {
	m := store.Metadata{URL: url}
	var kind string
	err := s.pool.QueryRow(ctx, `SELECT kind, document, fetched_at FROM oauth_metadata WHERE url = $1`, url).
		Scan(&kind, &m.Document, &m.FetchedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return store.Metadata{}, fmt.Errorf("get oauth metadata: %w", store.ErrNotFound)
	}

	if err != nil {
		return store.Metadata{}, fmt.Errorf("get oauth metadata: %w", err)
	}

	m.Kind = store.MetadataKind(kind)

	return m, nil
}

// PutMetadata implements store.Store.
func (s *Store) PutMetadata(ctx context.Context, m store.Metadata) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO oauth_metadata (url, kind, document, fetched_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (url) DO UPDATE SET kind = excluded.kind, document = excluded.document,
			fetched_at = excluded.fetched_at`,
		m.URL, string(m.Kind), m.Document, m.FetchedAt); err != nil {
		return fmt.Errorf("put oauth metadata: %w", err)
	}

	return nil
}

// nullable stores an empty optional field as NULL.
func nullable(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
