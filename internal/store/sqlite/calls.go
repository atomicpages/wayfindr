package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/atomicpages/wayfindr/internal/store"
)

// RecordCall implements store.Store.
func (s *Store) RecordCall(ctx context.Context, c store.Call) error {
	id, err := uuid.NewV7()

	if err != nil {
		return fmt.Errorf("record call: new id: %w", err)
	}

	var durationMS sql.NullInt64

	if c.Duration != nil {
		durationMS = sql.NullInt64{Int64: c.Duration.Milliseconds(), Valid: true}
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO calls
		(id, user_id, client, token_name, connector, tool, started_at, duration_ms, result, error_kind, error_case, error_text, result_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.String(), c.UserID, nullString(c.Client), nullString(c.TokenName), c.Connector, c.Tool, c.StartedAt.UnixMilli(),
		durationMS, string(c.Result), nullString(c.ErrorKind), nullString(c.ErrorCase), nullString(c.ErrorText), c.ResultBytes); err != nil {
		return fmt.Errorf("record call: %w", err)
	}

	return nil
}

// listCallsSQL filters by user, then by the optional bounds: ?2 is the
// lower bound on started_at (0 for none), ?3 drops ok calls when true, and
// ?4, a JSON array of ?5 connectors, keeps only those when non-empty.
const listCallsSQL = `SELECT id, user_id, client, token_name, connector, tool, started_at,
	duration_ms, result, error_kind, error_case, error_text, result_bytes
FROM calls
WHERE user_id = ?1
  AND (?2 = 0 OR started_at >= ?2)
  AND (NOT ?3 OR result <> 'ok')
  AND (?5 = 0 OR connector IN (SELECT value FROM json_each(?4)))
ORDER BY started_at DESC, id DESC
LIMIT ?6`

// ListCalls implements store.Store.
func (s *Store) ListCalls(ctx context.Context, f store.CallFilter) ([]store.Call, error) {
	if f.Limit <= 0 {
		return nil, errors.New("list calls: limit must be positive")
	}

	var since int64 // 0: no lower bound

	if !f.Since.IsZero() {
		since = f.Since.UnixMilli()
	}

	connectors, err := json.Marshal(f.Connectors)

	if err != nil {
		return nil, fmt.Errorf("list calls: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, listCallsSQL,
		f.UserID, since, f.ErrorsOnly, string(connectors), len(f.Connectors), f.Limit)

	if err != nil {
		return nil, fmt.Errorf("list calls: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var calls []store.Call

	for rows.Next() {
		var (
			c                 store.Call
			client, tokenName sql.NullString
			errKind, errCase  sql.NullString
			errText           sql.NullString
			startedAt         int64
			durationMS        sql.NullInt64
			resultBytes       sql.NullInt64
			result            string
		)

		if err := rows.Scan(&c.ID, &c.UserID, &client, &tokenName, &c.Connector, &c.Tool, &startedAt,
			&durationMS, &result, &errKind, &errCase, &errText, &resultBytes); err != nil {
			return nil, fmt.Errorf("list calls: %w", err)
		}

		c.Client = client.String
		c.TokenName = tokenName.String
		c.StartedAt = time.UnixMilli(startedAt)
		c.Result = store.CallResult(result)
		c.ErrorKind = errKind.String
		c.ErrorCase = errCase.String
		c.ErrorText = errText.String

		if resultBytes.Valid {
			c.ResultBytes = &resultBytes.Int64
		}

		if durationMS.Valid {
			d := time.Duration(durationMS.Int64) * time.Millisecond
			c.Duration = &d
		}

		calls = append(calls, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list calls: %w", err)
	}

	return calls, nil
}

// PruneCalls implements store.Store.
func (s *Store) PruneCalls(ctx context.Context, userID string, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM calls WHERE user_id = ? AND started_at < ?`,
		userID, before.UnixMilli())

	if err != nil {
		return 0, fmt.Errorf("prune calls: %w", err)
	}

	n, err := res.RowsAffected()

	if err != nil {
		return 0, fmt.Errorf("prune calls: %w", err)
	}

	return n, nil
}
