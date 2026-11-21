package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/atomicpages/wayfindr/internal/store"
)

// recordSync writes o as the connector's last-sync record in tx, part of
// ApplySync.
func recordSync(ctx context.Context, tx *sql.Tx, userID string, o store.SyncOutcome) error {
	var lastError, hiddenTools sql.NullString

	if o.Error != "" {
		lastError = sql.NullString{String: o.Error, Valid: true}
	} else {
		enc, err := store.EncodeHidden(o.Hidden)

		if err != nil {
			return fmt.Errorf("record sync: %w", err)
		}

		hiddenTools = sql.NullString{String: enc, Valid: true}
	}

	// A failed sync writes NULL, which keeps the hidden tools on record.
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_sync (user_id, connector, last_sync_at, last_error, hidden_tools)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id, connector) DO UPDATE SET
			last_sync_at = excluded.last_sync_at, last_error = excluded.last_error,
			hidden_tools = COALESCE(excluded.hidden_tools, connector_sync.hidden_tools)`,
		userID, o.Connector, o.At.UnixMilli(), lastError, hiddenTools); err != nil {
		return fmt.Errorf("record sync: %w", err)
	}

	return nil
}

// LastSync implements store.Store.
func (s *Store) LastSync(ctx context.Context, userID, connector string) (store.SyncRecord, error) {
	var (
		at                     sql.NullInt64
		lastError, hiddenTools sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `SELECT last_sync_at, last_error, hidden_tools FROM connector_sync
		WHERE user_id = ? AND connector = ?`, userID, connector).Scan(&at, &lastError, &hiddenTools)

	if errors.Is(err, sql.ErrNoRows) || (err == nil && !at.Valid) {
		return store.SyncRecord{}, fmt.Errorf("last sync %s: %w", connector, store.ErrNotFound)
	}

	if err != nil {
		return store.SyncRecord{}, fmt.Errorf("last sync %s: %w", connector, err)
	}

	hidden, err := store.DecodeHidden(hiddenTools.String)

	if err != nil {
		return store.SyncRecord{}, fmt.Errorf("last sync %s: %w", connector, err)
	}

	return store.SyncRecord{At: time.UnixMilli(at.Int64), Error: lastError.String, Hidden: hidden}, nil
}

// GetCredential implements store.Store.
func (s *Store) GetCredential(ctx context.Context, userID, connector string) (store.Credential, error) {
	c := store.Credential{UserID: userID, Connector: connector}
	var issuer, redirectURI, dcrClientID, clientID, granted, requested sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT resource, issuer, redirect_uri, dcr_client_id, dcr_client, token,
			granted_scopes, requested_scopes, client_id
		FROM credentials WHERE user_id = ? AND connector = ?`, userID, connector).
		Scan(&c.Resource, &issuer, &redirectURI, &dcrClientID, &c.DCRClient, &c.Token, &granted, &requested, &clientID)

	if errors.Is(err, sql.ErrNoRows) {
		return store.Credential{}, fmt.Errorf("get credential %s: %w", connector, store.ErrNotFound)
	}

	if err != nil {
		return store.Credential{}, fmt.Errorf("get credential %s: %w", connector, err)
	}

	c.Issuer = issuer.String
	c.RedirectURI = redirectURI.String
	c.DCRClientID = dcrClientID.String
	c.ClientID = clientID.String
	c.GrantedScopes = granted.String
	c.RequestedScopes = requested.String

	return c, nil
}

// PutCredential implements store.Store. Empty optional fields are stored as NULL.
func (s *Store) PutCredential(ctx context.Context, c store.Credential) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO credentials (user_id, connector, resource, issuer,
			redirect_uri, dcr_client_id, dcr_client, token, granted_scopes, requested_scopes, client_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, connector) DO UPDATE SET
			resource = excluded.resource, issuer = excluded.issuer, redirect_uri = excluded.redirect_uri,
			dcr_client_id = excluded.dcr_client_id, dcr_client = excluded.dcr_client, token = excluded.token,
			granted_scopes = excluded.granted_scopes, requested_scopes = excluded.requested_scopes,
			client_id = excluded.client_id, updated_at = excluded.updated_at`,
		c.UserID, c.Connector, c.Resource, nullString(c.Issuer), nullString(c.RedirectURI),
		nullString(c.DCRClientID), c.DCRClient, c.Token, nullString(c.GrantedScopes),
		nullString(c.RequestedScopes), nullString(c.ClientID), s.now().UnixMilli()); err != nil {
		return fmt.Errorf("put credential %s: %w", c.Connector, err)
	}

	return nil
}

// DeleteCredential implements store.Store.
func (s *Store) DeleteCredential(ctx context.Context, userID, connector string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM credentials WHERE user_id = ? AND connector = ?`,
		userID, connector); err != nil {
		return fmt.Errorf("delete credential %s: %w", connector, err)
	}

	return nil
}

// ListCredentialConnectors implements store.Store.
func (s *Store) ListCredentialConnectors(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT connector FROM credentials WHERE user_id = ? ORDER BY connector`, userID)

	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}

	defer func() { _ = rows.Close() }()
	var connectors []string

	for rows.Next() {
		var c string

		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("list credentials: %w", err)
		}

		connectors = append(connectors, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}

	return connectors, nil
}

// GetSecret implements store.Store.
func (s *Store) GetSecret(ctx context.Context, userID, name string) ([]byte, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM secrets WHERE user_id = ? AND name = ?`,
		userID, name).Scan(&sealed)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get secret %s: %w", name, store.ErrNotFound)
	}

	if err != nil {
		return nil, fmt.Errorf("get secret %s: %w", name, err)
	}

	return sealed, nil
}

// PutSecret implements store.Store.
func (s *Store) PutSecret(ctx context.Context, userID, name string, sealed []byte) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO secrets (user_id, name, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		userID, name, sealed, s.now().UnixMilli()); err != nil {
		return fmt.Errorf("put secret %s: %w", name, err)
	}

	return nil
}

// DeleteSecret implements store.Store.
func (s *Store) DeleteSecret(ctx context.Context, userID, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE user_id = ? AND name = ?`, userID, name)

	if err != nil {
		return fmt.Errorf("delete secret %s: %w", name, err)
	}

	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("delete secret %s: %w", name, err)
	} else if n == 0 {
		return fmt.Errorf("delete secret %s: %w", name, store.ErrNotFound)
	}

	return nil
}

// ListSecrets implements store.Store.
func (s *Store) ListSecrets(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM secrets WHERE user_id = ? ORDER BY name`, userID)

	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}

	defer func() { _ = rows.Close() }()
	var names []string

	for rows.Next() {
		var n string

		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("list secrets: %w", err)
		}

		names = append(names, n)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}

	return names, nil
}

// HasSealedData implements store.Store.
func (s *Store) HasSealedData(ctx context.Context) (bool, error) {
	var exists bool

	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM credentials) OR EXISTS (SELECT 1 FROM secrets)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("check sealed data: %w", err)
	}

	return exists, nil
}

// GetMetadata implements store.Store.
func (s *Store) GetMetadata(ctx context.Context, url string) (store.Metadata, error) {
	m := store.Metadata{URL: url}
	var fetchedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT kind, document, fetched_at FROM oauth_metadata WHERE url = ?`, url).
		Scan(&m.Kind, &m.Document, &fetchedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return store.Metadata{}, fmt.Errorf("get oauth metadata: %w", store.ErrNotFound)
	}

	if err != nil {
		return store.Metadata{}, fmt.Errorf("get oauth metadata: %w", err)
	}

	m.FetchedAt = time.UnixMilli(fetchedAt)

	return m, nil
}

// PutMetadata implements store.Store.
func (s *Store) PutMetadata(ctx context.Context, m store.Metadata) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO oauth_metadata (url, kind, document, fetched_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (url) DO UPDATE SET kind = excluded.kind, document = excluded.document,
			fetched_at = excluded.fetched_at`,
		m.URL, string(m.Kind), m.Document, m.FetchedAt.UnixMilli()); err != nil {
		return fmt.Errorf("put oauth metadata: %w", err)
	}

	return nil
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
