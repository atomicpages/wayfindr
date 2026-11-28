package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/atomicpages/wayfindr/internal/store"
)

// RecordCall implements store.Store. StartedAt is truncated to the
// millisecond, as SQLite stores it.
func (s *Store) RecordCall(ctx context.Context, c store.Call) error {
	id, err := uuid.NewV7()

	if err != nil {
		return fmt.Errorf("record call: new id: %w", err)
	}

	var durationMS *int64

	if c.Duration != nil {
		ms := c.Duration.Milliseconds()
		durationMS = &ms
	}

	if _, err := s.pool.Exec(ctx, `INSERT INTO calls
		(id, user_id, client, token_name, connector, tool, started_at, duration_ms, result, error_kind, error_case, error_text, result_bytes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		id.String(), c.UserID, nullable(c.Client), nullable(c.TokenName), c.Connector, c.Tool, c.StartedAt.Truncate(time.Millisecond),
		durationMS, string(c.Result), nullable(c.ErrorKind), nullable(c.ErrorCase), nullable(c.ErrorText), c.ResultBytes); err != nil {
		return fmt.Errorf("record call: %w", err)
	}

	return nil
}

// listCallsSQL filters by user, then by the optional bounds: $2 is the
// lower bound on started_at (NULL for none), $3 drops ok calls when true,
// and $4 keeps only the listed connectors when non-empty.
const listCallsSQL = `SELECT id::text, user_id::text, client, token_name, connector, tool, started_at,
	duration_ms, result, error_kind, error_case, error_text, result_bytes
FROM calls
WHERE user_id = $1
  AND ($2::timestamptz IS NULL OR started_at >= $2)
  AND (NOT $3 OR result <> 'ok')
  AND (coalesce(cardinality($4::text[]), 0) = 0 OR connector = ANY($4::text[]))
ORDER BY started_at DESC, id DESC
LIMIT $5`

// ListCalls implements store.Store.
func (s *Store) ListCalls(ctx context.Context, f store.CallFilter) ([]store.Call, error) {
	if f.Limit <= 0 {
		return nil, errors.New("list calls: limit must be positive")
	}

	var since *time.Time

	if !f.Since.IsZero() {
		since = &f.Since
	}

	rows, err := s.pool.Query(ctx, listCallsSQL, f.UserID, since, f.ErrorsOnly, f.Connectors, f.Limit)

	if err != nil {
		return nil, fmt.Errorf("list calls: %w", err)
	}

	defer rows.Close()

	var calls []store.Call

	for rows.Next() {
		var (
			c                 store.Call
			client, tokenName *string
			errKind, errCase  *string
			errText           *string
			durationMS        *int64
			resultBytes       *int64
			result            string
		)

		if err := rows.Scan(&c.ID, &c.UserID, &client, &tokenName, &c.Connector, &c.Tool, &c.StartedAt,
			&durationMS, &result, &errKind, &errCase, &errText, &resultBytes); err != nil {
			return nil, fmt.Errorf("list calls: %w", err)
		}

		c.Client = deref(client)
		c.TokenName = deref(tokenName)
		c.Result = store.CallResult(result)
		c.ErrorKind = deref(errKind)
		c.ErrorCase = deref(errCase)
		c.ErrorText = deref(errText)
		c.ResultBytes = resultBytes

		if durationMS != nil {
			d := time.Duration(*durationMS) * time.Millisecond
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
	tag, err := s.pool.Exec(ctx, `DELETE FROM calls WHERE user_id = $1 AND started_at < $2`, userID, before)

	if err != nil {
		return 0, fmt.Errorf("prune calls: %w", err)
	}

	return tag.RowsAffected(), nil
}
