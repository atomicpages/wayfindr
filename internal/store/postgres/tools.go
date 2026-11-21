package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/atomicpages/wayfindr/internal/store"
)

const toolColumns = `connector, definition, content_hash, embedding, embedding_model`

// ApplySync implements store.Store. Only a successful sync writes the
// catalog, and so takes lockCatalog; a failed one writes only its record,
// which the search tables do not derive from.
func (s *Store) ApplySync(ctx context.Context, userID string, o store.SyncOutcome) ([]string, error) {
	tx, err := s.pool.Begin(ctx)

	if err != nil {
		return nil, fmt.Errorf("apply sync %s: %w", o.Connector, err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	var plan store.CatalogPlan

	if o.Error == "" {
		if plan, err = writeCatalog(ctx, tx, userID, o.Connector, o.Tools); err != nil {
			return nil, fmt.Errorf("apply sync %s: %w", o.Connector, err)
		}
	}

	if err := recordSync(ctx, tx, userID, o); err != nil {
		return nil, fmt.Errorf("apply sync %s: %w", o.Connector, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("apply sync %s: commit: %w", o.Connector, err)
	}

	return plan.Changed, nil
}

// writeCatalog makes tools connector's complete catalog in tx, and returns
// the plan it executed. The triggers on tools keep the search tables in
// step.
func writeCatalog(ctx context.Context, tx pgx.Tx, userID, connector string, tools []store.Tool) (store.CatalogPlan, error) {
	if err := lockCatalog(ctx, tx, userID); err != nil {
		return store.CatalogPlan{}, err
	}

	existing, err := contentHashes(ctx, tx, userID, connector)

	if err != nil {
		return store.CatalogPlan{}, err
	}

	plan, err := store.PlanCatalog(connector, existing, tools)

	if err != nil {
		return store.CatalogPlan{}, err
	}

	for _, t := range plan.Insert {
		if _, err := tx.Exec(ctx, `INSERT INTO tools (user_id, connector, name, definition, content_hash, updated_at)
			VALUES ($1, $2, $3, $4, $5, now())`, userID, connector, t.Name, []byte(t.Definition), t.ContentHash); err != nil {
			return store.CatalogPlan{}, fmt.Errorf("write %q: %w", t.Name, err)
		}
	}

	for _, t := range plan.Update {
		if _, err := tx.Exec(ctx, `UPDATE tools SET definition = $1, content_hash = $2, embedding = NULL,
			embedding_model = NULL, updated_at = now() WHERE user_id = $3 AND connector = $4 AND name = $5`,
			[]byte(t.Definition), t.ContentHash, userID, connector, t.Name); err != nil {
			return store.CatalogPlan{}, fmt.Errorf("write %q: %w", t.Name, err)
		}
	}

	for _, t := range plan.Touch {
		if _, err := tx.Exec(ctx, `UPDATE tools SET definition = $1, updated_at = now()
			WHERE user_id = $2 AND connector = $3 AND name = $4`,
			[]byte(t.Definition), userID, connector, t.Name); err != nil {
			return store.CatalogPlan{}, fmt.Errorf("write %q: %w", t.Name, err)
		}
	}

	for _, name := range plan.Delete {
		if _, err := tx.Exec(ctx, `DELETE FROM tools WHERE user_id = $1 AND connector = $2 AND name = $3`,
			userID, connector, name); err != nil {
			return store.CatalogPlan{}, fmt.Errorf("delete %q: %w", name, err)
		}
	}

	return plan, nil
}

func contentHashes(ctx context.Context, tx pgx.Tx, userID, connector string) (map[string][]byte, error) {
	rows, err := tx.Query(ctx, `SELECT name, content_hash FROM tools WHERE user_id = $1 AND connector = $2`,
		userID, connector)

	if err != nil {
		return nil, fmt.Errorf("read content hashes: %w", err)
	}

	defer rows.Close()
	hashes := map[string][]byte{}

	for rows.Next() {
		var name string
		var hash []byte

		if err := rows.Scan(&name, &hash); err != nil {
			return nil, fmt.Errorf("read content hashes: %w", err)
		}

		hashes[name] = hash
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read content hashes: %w", err)
	}

	return hashes, nil
}

// lockCatalog serialises the transactions that write userID's catalog, and
// is taken before any row lock. Two concurrent replacements of one connector
// would otherwise both see a tool as new and collide on insert. And every
// catalog write updates the search index's shared vocabulary rows (migration
// 0004), which two writers could lock in different orders and deadlock.
// Catalog writes are rare (a sync per connector), so serialising one user's
// costs little; other users' writes do not wait.
func lockCatalog(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('wayfindr catalog ' || $1, 0))`,
		userID); err != nil {
		return fmt.Errorf("lock catalog: %w", err)
	}

	return nil
}

// DeleteCatalog implements store.Store.
func (s *Store) DeleteCatalog(ctx context.Context, userID, connector string) error {
	tx, err := s.pool.Begin(ctx)

	if err != nil {
		return fmt.Errorf("delete catalog %s: %w", connector, err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockCatalog(ctx, tx, userID); err != nil {
		return fmt.Errorf("delete catalog %s: %w", connector, err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM tools WHERE user_id = $1 AND connector = $2`, userID, connector); err != nil {
		return fmt.Errorf("delete catalog %s: tools: %w", connector, err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM connector_sync WHERE user_id = $1 AND connector = $2`, userID, connector); err != nil {
		return fmt.Errorf("delete catalog %s: sync record: %w", connector, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("delete catalog %s: commit: %w", connector, err)
	}

	return nil
}

// ToolCounts implements store.Store.
func (s *Store) ToolCounts(ctx context.Context, userID string) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT connector, COUNT(*) FROM tools
		WHERE user_id = $1 GROUP BY connector`, userID)

	if err != nil {
		return nil, fmt.Errorf("count tools: %w", err)
	}

	defer rows.Close()
	counts := map[string]int{}

	for rows.Next() {
		var (
			connector string
			n         int64
		)

		if err := rows.Scan(&connector, &n); err != nil {
			return nil, fmt.Errorf("count tools: %w", err)
		}

		counts[connector] = int(n)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count tools: %w", err)
	}

	return counts, nil
}

// GetTool implements store.Store.
func (s *Store) GetTool(ctx context.Context, userID, connector, name string) (store.Tool, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+toolColumns+` FROM tools
		WHERE user_id = $1 AND connector = $2 AND name = $3`, userID, connector, name)
	t, err := scanTool(row)

	if errors.Is(err, pgx.ErrNoRows) {
		return store.Tool{}, fmt.Errorf("get tool %s.%s: %w", connector, name, store.ErrNotFound)
	}

	if err != nil {
		return store.Tool{}, fmt.Errorf("get tool %s.%s: %w", connector, name, err)
	}

	return t, nil
}

// PendingEmbeddings implements store.Store. The "C" collation orders names
// bytewise, as SQLite does, whatever the database's default collation is.
func (s *Store) PendingEmbeddings(ctx context.Context, userID, modelID string, limit int) ([]store.Tool, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("pending embeddings: limit %d must be positive", limit)
	}

	rows, err := s.pool.Query(ctx, `SELECT `+toolColumns+` FROM tools
		WHERE user_id = $1 AND embedding_model IS DISTINCT FROM $2
		ORDER BY connector COLLATE "C", name COLLATE "C" LIMIT $3`, userID, modelID, limit)

	if err != nil {
		return nil, fmt.Errorf("pending embeddings: %w", err)
	}

	defer rows.Close()
	var tools []store.Tool

	for rows.Next() {
		t, err := scanTool(rows)

		if err != nil {
			return nil, fmt.Errorf("pending embeddings: %w", err)
		}

		tools = append(tools, t)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pending embeddings: %w", err)
	}

	return tools, nil
}

// SetEmbeddings implements store.Store.
func (s *Store) SetEmbeddings(ctx context.Context, userID, modelID string, vecs []store.ToolVector) error {
	tx, err := s.pool.Begin(ctx)

	if err != nil {
		return fmt.Errorf("set embeddings: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	for _, v := range vecs {
		// The content_hash condition skips rows that changed after the
		// caller read them; they stay pending for the next pass.
		if _, err := tx.Exec(ctx, `UPDATE tools SET embedding = $1, embedding_model = $2
			WHERE user_id = $3 AND connector = $4 AND name = $5 AND content_hash = $6`,
			pgvector.NewVector(v.Vector), modelID, userID, v.Connector, v.Name, v.ContentHash); err != nil {
			return fmt.Errorf("set embeddings: %s.%s: %w", v.Connector, v.Name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("set embeddings: commit: %w", err)
	}

	return nil
}

// scanTool reads one row of toolColumns, then any columns after them into
// extra.
func scanTool(row pgx.Row, extra ...any) (store.Tool, error) {
	var (
		connector  string
		definition []byte
		hash       []byte
		embedding  *pgvector.Vector
		model      *string
	)

	if err := row.Scan(append([]any{&connector, &definition, &hash, &embedding, &model}, extra...)...); err != nil {
		return store.Tool{}, err
	}

	t, err := store.NewTool(connector, definition, hash)

	if err != nil {
		return store.Tool{}, err
	}

	if embedding != nil {
		t.Embedding = embedding.Slice()
	}

	if model != nil {
		t.EmbeddingModel = *model
	}

	return t, nil
}
