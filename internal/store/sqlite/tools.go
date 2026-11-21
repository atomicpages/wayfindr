package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/atomicpages/wayfindr/internal/store"
)

const toolColumns = `connector, definition, content_hash, embedding, embedding_model`

// ApplySync implements store.Store.
func (s *Store) ApplySync(ctx context.Context, userID string, o store.SyncOutcome) ([]string, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)

	if err != nil {
		return nil, fmt.Errorf("apply sync %s: %w", o.Connector, err)
	}

	defer func() { _ = tx.Rollback() }()

	var (
		plan   store.CatalogPlan
		stored []store.Tool
	)

	if o.Error == "" {
		if plan, stored, err = s.writeCatalog(ctx, tx, userID, o.Connector, o.Tools); err != nil {
			return nil, fmt.Errorf("apply sync %s: %w", o.Connector, err)
		}
	}

	if err := recordSync(ctx, tx, userID, o); err != nil {
		return nil, fmt.Errorf("apply sync %s: %w", o.Connector, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("apply sync %s: commit: %w", o.Connector, err)
	}

	// Only a committed catalog reaches the index.
	if o.Error == "" {
		s.index.replaceConnector(userID, o.Connector, stored)
	}

	return plan.Changed, nil
}

// writeCatalog makes tools connector's complete catalog in tx, and returns
// the plan it executed and the rows it leaves, which the index mirrors
// once tx commits (kept embeddings included).
func (s *Store) writeCatalog(ctx context.Context, tx *sql.Tx, userID, connector string, tools []store.Tool) (store.CatalogPlan, []store.Tool, error) {
	existing, err := contentHashes(ctx, tx, userID, connector)

	if err != nil {
		return store.CatalogPlan{}, nil, err
	}

	plan, err := store.PlanCatalog(connector, existing, tools)

	if err != nil {
		return store.CatalogPlan{}, nil, err
	}

	now := s.now().UnixMilli()

	for _, t := range plan.Insert {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tools (user_id, connector, name, definition, content_hash, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, userID, connector, t.Name, []byte(t.Definition), t.ContentHash, now); err != nil {
			return store.CatalogPlan{}, nil, fmt.Errorf("write %q: %w", t.Name, err)
		}
	}

	for _, t := range plan.Update {
		if _, err := tx.ExecContext(ctx, `UPDATE tools SET definition = ?, content_hash = ?, embedding = NULL,
			embedding_model = NULL, updated_at = ? WHERE user_id = ? AND connector = ? AND name = ?`,
			[]byte(t.Definition), t.ContentHash, now, userID, connector, t.Name); err != nil {
			return store.CatalogPlan{}, nil, fmt.Errorf("write %q: %w", t.Name, err)
		}
	}

	for _, t := range plan.Touch {
		if _, err := tx.ExecContext(ctx, `UPDATE tools SET definition = ?, updated_at = ?
			WHERE user_id = ? AND connector = ? AND name = ?`,
			[]byte(t.Definition), now, userID, connector, t.Name); err != nil {
			return store.CatalogPlan{}, nil, fmt.Errorf("write %q: %w", t.Name, err)
		}
	}

	for _, name := range plan.Delete {
		if _, err := tx.ExecContext(ctx, `DELETE FROM tools WHERE user_id = ? AND connector = ? AND name = ?`,
			userID, connector, name); err != nil {
			return store.CatalogPlan{}, nil, fmt.Errorf("delete %q: %w", name, err)
		}
	}

	stored, err := queryTools(ctx, tx, `WHERE user_id = ? AND connector = ?`, userID, connector)

	if err != nil {
		return store.CatalogPlan{}, nil, fmt.Errorf("read back: %w", err)
	}

	return plan, stored, nil
}

func contentHashes(ctx context.Context, tx *sql.Tx, userID, connector string) (map[string][]byte, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, content_hash FROM tools WHERE user_id = ? AND connector = ?`,
		userID, connector)

	if err != nil {
		return nil, fmt.Errorf("read content hashes: %w", err)
	}

	defer func() { _ = rows.Close() }()
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

// DeleteCatalog implements store.Store.
func (s *Store) DeleteCatalog(ctx context.Context, userID, connector string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)

	if err != nil {
		return fmt.Errorf("delete catalog %s: %w", connector, err)
	}

	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM tools WHERE user_id = ? AND connector = ?`, userID, connector); err != nil {
		return fmt.Errorf("delete catalog %s: tools: %w", connector, err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_sync WHERE user_id = ? AND connector = ?`, userID, connector); err != nil {
		return fmt.Errorf("delete catalog %s: sync record: %w", connector, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete catalog %s: commit: %w", connector, err)
	}

	s.index.deleteConnector(userID, connector)

	return nil
}

// ToolCounts implements store.Store.
func (s *Store) ToolCounts(ctx context.Context, userID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT connector, COUNT(*) FROM tools
		WHERE user_id = ? GROUP BY connector`, userID)

	if err != nil {
		return nil, fmt.Errorf("count tools: %w", err)
	}

	defer func() { _ = rows.Close() }()
	counts := map[string]int{}

	for rows.Next() {
		var (
			connector string
			n         int
		)

		if err := rows.Scan(&connector, &n); err != nil {
			return nil, fmt.Errorf("count tools: %w", err)
		}

		counts[connector] = n
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count tools: %w", err)
	}

	return counts, nil
}

// GetTool implements store.Store.
func (s *Store) GetTool(ctx context.Context, userID, connector, name string) (store.Tool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+toolColumns+` FROM tools
		WHERE user_id = ? AND connector = ? AND name = ?`, userID, connector, name)
	t, err := scanTool(row)

	if errors.Is(err, sql.ErrNoRows) {
		return store.Tool{}, fmt.Errorf("get tool %s.%s: %w", connector, name, store.ErrNotFound)
	}

	if err != nil {
		return store.Tool{}, fmt.Errorf("get tool %s.%s: %w", connector, name, err)
	}

	return t, nil
}

// PendingEmbeddings implements store.Store.
func (s *Store) PendingEmbeddings(ctx context.Context, userID, modelID string, limit int) ([]store.Tool, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("pending embeddings: limit %d must be positive", limit)
	}

	tools, err := queryTools(ctx, s.db, `WHERE user_id = ? AND (embedding_model IS NULL OR embedding_model <> ?)
		ORDER BY connector, name LIMIT ?`, userID, modelID, limit)

	if err != nil {
		return nil, fmt.Errorf("pending embeddings: %w", err)
	}

	return tools, nil
}

// SetEmbeddings implements store.Store.
func (s *Store) SetEmbeddings(ctx context.Context, userID, modelID string, vecs []store.ToolVector) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)

	if err != nil {
		return fmt.Errorf("set embeddings: %w", err)
	}

	defer func() { _ = tx.Rollback() }()
	var stored []store.ToolVector

	for _, v := range vecs {
		// The content_hash condition skips rows that changed after the
		// caller read them; they stay pending for the next pass.
		res, err := tx.ExecContext(ctx, `UPDATE tools SET embedding = ?, embedding_model = ?
			WHERE user_id = ? AND connector = ? AND name = ? AND content_hash = ?`,
			encodeVector(v.Vector), modelID, userID, v.Connector, v.Name, v.ContentHash)

		if err != nil {
			return fmt.Errorf("set embeddings: %s.%s: %w", v.Connector, v.Name, err)
		}

		n, err := res.RowsAffected()

		if err != nil {
			return fmt.Errorf("set embeddings: %s.%s: %w", v.Connector, v.Name, err)
		}

		if n > 0 {
			stored = append(stored, v)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set embeddings: commit: %w", err)
	}

	for _, v := range stored {
		s.index.setEmbedding(userID, v.Connector, v.Name, modelID, v.Vector)
	}

	return nil
}

// Search implements store.Store over the in-memory index: BM25F lexical
// candidates fused with the cosine top 50 when q carries a vector (design
// §7.5).
func (s *Store) Search(_ context.Context, q store.SearchQuery) (store.SearchPage, error) {
	page, err := s.index.search(q)

	if err != nil {
		return store.SearchPage{}, fmt.Errorf("search: %w", err)
	}

	return page, nil
}

// queryTools reads the tools rows matching where, in the shape scanTool reads.
func queryTools(ctx context.Context, q querier, where string, args ...any) ([]store.Tool, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+toolColumns+` FROM tools `+where, args...)

	if err != nil {
		return nil, err
	}

	defer func() { _ = rows.Close() }()
	var tools []store.Tool

	for rows.Next() {
		t, err := scanTool(rows)

		if err != nil {
			return nil, err
		}

		tools = append(tools, t)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return tools, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// loadIndex builds the search index from every stored catalog entry.
func loadIndex(ctx context.Context, db *sql.DB) (*index, error) {
	rows, err := db.QueryContext(ctx, `SELECT user_id, `+toolColumns+` FROM tools`)

	if err != nil {
		return nil, fmt.Errorf("load search index: %w", err)
	}

	defer func() { _ = rows.Close() }()
	idx := newIndex()

	for rows.Next() {
		var userID string
		t, err := scanTool(rows, &userID)

		if err != nil {
			return nil, fmt.Errorf("load search index: %w", err)
		}

		idx.put(userID, t)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load search index: %w", err)
	}

	return idx, nil
}

type scanner interface {
	Scan(dest ...any) error
}

// scanTool reads one row of toolColumns, after any leading columns that
// lead says where to put.
func scanTool(row scanner, lead ...any) (store.Tool, error) {
	var (
		connector  string
		definition []byte
		hash       []byte
		embedding  []byte
		model      sql.NullString
	)

	if err := row.Scan(append(lead, &connector, &definition, &hash, &embedding, &model)...); err != nil {
		return store.Tool{}, err
	}

	t, err := store.NewTool(connector, definition, hash)

	if err != nil {
		return store.Tool{}, err
	}

	t.Embedding, err = decodeVector(embedding)

	if err != nil {
		return store.Tool{}, err
	}

	t.EmbeddingModel = model.String

	return t, nil
}

// encodeVector stores a vector as little-endian float32s (design §5).
func encodeVector(v []float32) []byte {
	if v == nil {
		return nil
	}

	b := make([]byte, 4*len(v))

	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}

	return b
}

func decodeVector(b []byte) ([]float32, error) {
	if b == nil {
		return nil, nil
	}

	if len(b)%4 != 0 {
		return nil, fmt.Errorf("embedding is %d bytes, not a multiple of 4", len(b))
	}

	v := make([]float32, len(b)/4)

	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}

	return v, nil
}
