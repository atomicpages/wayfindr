package postgres

import (
	"context"
	"fmt"
	"math"

	"github.com/pgvector/pgvector-go"

	"github.com/atomicpages/wayfindr/internal/store"
)

// searchSQL is hybrid search (design §7.5) as one query. Its parameters are
// $1 user ID, $2 query text, $3 query vector (NULL for lexical-only), $4 the
// vector's model identity, $5 connector filter (empty or NULL for all),
// $6 offset, $7 limit. Each row is the tool's columns, then its
// store.Score: lexical rank and BM25F, vector rank and cosine (1 − the
// cosine distance), and fused score, with 0 for a ranking it is not in, and
// whether the query names the tool exactly.
//
// The exact-name boost sorts first the tools whose name or qualified name,
// split by search_words into words of 2+ characters and rejoined with
// single spaces, equals the query split the same way; SQLite's nameKey does
// the same with its tokenizer.
//
// The lexical score is the SQLite store's BM25F, computed over the weighted
// search_text lexemes rather than ts_rank, which has no idf and no length
// normalization and so ranks noisy query words (to, the, in) too high. It
// reads the search index of migration 0004, which triggers keep in step with
// tools: search_postings holds each tool's tokens (each lexeme and its folded
// form, fold_plural, counted like the surface form, which matches the token
// stream SQLite's tokenizer indexes, so a singular query matches a
// plural-only tool), search_docs each tool's field lengths, and search_vocab
// each token's document frequency. Query words get SQLite's weights: an
// exact hit, a 0.5 / 0.5 split between surface and folded forms, or, for a
// word of 4+ characters with neither, the vocabulary tokens whose trigram
// Jaccard similarity is at least 0.4, weighted by it. Trigrams come from
// token_trigrams (migration 0003), stored per vocabulary token in
// search_vocab_grams, rather than pg_trgm, whose word characters follow the
// database's ctype, and the similarity is a float8, as SQLite's is.
// The tsvector merges the description and the parameter descriptions into
// weight D, so both get the description's weight (1.0) and one length.
//
// Counts are stored as integers and cast to float8 before any arithmetic, so
// the scores equal those computed from float8 counts. Sums run in a fixed
// order (fields A to D, terms by token) so that tools with identical text
// score identically and tie on qualified name.
const searchSQL = `
WITH stats AS (
  SELECT count(*)::float8 AS n,
         sum(la)::float8 / count(*) AS avg_a, sum(lb)::float8 / count(*) AS avg_b,
         sum(lc)::float8 / count(*) AS avg_c, sum(ld)::float8 / count(*) AS avg_d
  FROM search_docs WHERE user_id = $1
  HAVING count(*) > 0
),
words AS (
  SELECT w, fold_plural(w) AS f,
         EXISTS (SELECT 1 FROM search_vocab WHERE user_id = $1 AND token = w) AS w_hit,
         EXISTS (SELECT 1 FROM search_vocab WHERE user_id = $1 AND token = fold_plural(w)) AS f_hit
  FROM (SELECT DISTINCT u.lexeme AS w FROM unnest(to_tsvector('simple', search_words($2))) AS u
        WHERE char_length(u.lexeme) >= 2) AS q
),
-- Typo expansion: the trigram Jaccard similarity of each form of a word with
-- no exact hit and every vocabulary token, counted over shared trigrams.
typo_forms AS (
  SELECT DISTINCT x.form
  FROM words CROSS JOIN LATERAL unnest(ARRAY[w, f]) AS x(form)
  WHERE NOT w_hit AND NOT f_hit AND char_length(x.form) >= 4
),
form_grams AS (
  SELECT form, g.gram, count(*) OVER (PARTITION BY form)::float8 AS n
  FROM typo_forms CROSS JOIN LATERAL token_trigrams(form) AS g(gram)
),
similar_tokens AS (
  SELECT vg.token, count(*) / (min(fg.n) + min(v.grams)::float8 - count(*)) AS similarity
  FROM form_grams AS fg
  JOIN search_vocab_grams AS vg ON vg.user_id = $1 AND vg.gram = fg.gram
  JOIN search_vocab AS v ON v.user_id = $1 AND v.token = vg.token
  GROUP BY fg.form, vg.token
),
term_candidates AS (
  SELECT w AS token, CASE WHEN f_hit AND f <> w THEN 0.5 ELSE 1 END::float8 AS weight
  FROM words WHERE w_hit
  UNION ALL
  SELECT f, CASE WHEN w_hit THEN 0.5 ELSE 1 END::float8
  FROM words WHERE f_hit AND f <> w
  UNION ALL
  SELECT fold_plural(token), similarity FROM similar_tokens WHERE similarity >= 0.4
),
terms AS (
  SELECT token, max(weight) AS weight FROM term_candidates GROUP BY token
),
lexical AS (
  SELECT p.connector, p.name,
         sum(tm.weight * ln(1 + (st.n - v.df::float8 + 0.5) / (v.df::float8 + 0.5)) * tf.tf / (1.2 + tf.tf)
             ORDER BY tm.token COLLATE "C") AS score
  FROM terms AS tm
  JOIN search_vocab AS v ON v.user_id = $1 AND v.token = tm.token
  JOIN search_postings AS p ON p.user_id = $1 AND p.token = tm.token
  JOIN search_docs AS dl ON dl.user_id = $1 AND dl.connector = p.connector AND dl.name = p.name
  CROSS JOIN stats AS st
  CROSS JOIN LATERAL (SELECT
      CASE WHEN p.fa > 0 THEN 3.0::float8 * p.fa::float8 / (1 - 0.75::float8 + 0.75::float8 * dl.la::float8 / st.avg_a) ELSE 0 END
    + CASE WHEN p.fb > 0 THEN 2.0::float8 * p.fb::float8 / (1 - 0.75::float8 + 0.75::float8 * dl.lb::float8 / st.avg_b) ELSE 0 END
    + CASE WHEN p.fc > 0 THEN 1.5::float8 * p.fc::float8 / (1 - 0.75::float8 + 0.75::float8 * dl.lc::float8 / st.avg_c) ELSE 0 END
    + CASE WHEN p.fd > 0 THEN 1.0::float8 * p.fd::float8 / (1 - 0.75::float8 + 0.75::float8 * dl.ld::float8 / st.avg_d) ELSE 0 END
    AS tf) AS tf
  GROUP BY p.connector, p.name
),
lexical_ranked AS (
  SELECT l.connector, l.name, l.score,
         row_number() OVER (ORDER BY l.score DESC, d.qualified_name COLLATE "C", d.connector COLLATE "C") AS rank
  FROM lexical AS l JOIN tools AS d ON d.user_id = $1 AND d.connector = l.connector AND d.name = l.name
  WHERE l.score > 0
),
-- The CASE keeps <=> away from vectors of another length, which it rejects.
-- A cosine above 0 is a distance below 1; a zero vector's NaN is neither.
vector_ranked AS (
  SELECT connector, name, dist,
         row_number() OVER (ORDER BY dist, qualified_name COLLATE "C", connector COLLATE "C") AS rank
  FROM (SELECT connector, name, qualified_name,
               CASE WHEN vector_dims(embedding) = vector_dims($3::vector) THEN embedding <=> $3::vector END AS dist
        FROM tools WHERE user_id = $1 AND embedding_model = $4) AS c
  WHERE dist < 1
  ORDER BY rank
  LIMIT 50
),
-- Each ranking's rank and score ride along in its own columns, so the final
-- select needs no join back to lexical_ranked or vector_ranked: the planner
-- can't estimate fused's size, and a nested loop over a CTE scan there is
-- quadratic in the number of matching tools (wayfindr-xcq2).
fused AS (
  SELECT connector, name, sum(1::float8 / (5 + rank)) AS score,
         max(lex_rank) AS lex_rank, max(lex_score) AS lex_score,
         max(vec_rank) AS vec_rank, max(dist) AS dist
  FROM (SELECT connector, name, rank, rank AS lex_rank, score AS lex_score,
               NULL::bigint AS vec_rank, NULL::float8 AS dist
        FROM lexical_ranked
        UNION ALL
        SELECT connector, name, rank, NULL, NULL, rank, dist FROM vector_ranked) AS r
  GROUP BY connector, name
),
-- The exact-name boost: the query's words, unfolded, joined by one space.
-- Each word of a name is a substring of the name lowercased the same way,
-- so a tool whose qualified name lacks the last word is skipped before the
-- costlier split.
query_name AS (
  SELECT array_to_string(kw, ' ') AS k, coalesce(kw[cardinality(kw)], '') AS last
  FROM (SELECT ARRAY(SELECT w FROM regexp_split_to_table(search_words($2), ' ') AS w
                     WHERE char_length(w) >= 2) AS kw) AS q
)
SELECT t.connector, t.definition, t.content_hash, t.embedding, t.embedding_model,
       coalesce(f.lex_rank, 0), coalesce(f.lex_score, 0), coalesce(f.vec_rank, 0), coalesce(1 - f.dist, 0), f.score, x.exact
FROM fused AS f
JOIN tools AS t ON t.user_id = $1 AND t.connector = f.connector AND t.name = f.name
CROSS JOIN query_name AS qn
CROSS JOIN LATERAL (SELECT CASE
    WHEN qn.k = '' OR strpos(lower(t.qualified_name COLLATE "und-x-icu"), qn.last) = 0 THEN false
    ELSE EXISTS (
    SELECT 1 FROM unnest(ARRAY[t.name, t.qualified_name]) AS n(s)
    WHERE array_to_string(ARRAY(SELECT w FROM regexp_split_to_table(search_words(n.s), ' ') AS w
                                WHERE char_length(w) >= 2), ' ') = qn.k) END AS exact) AS x
WHERE coalesce(cardinality($5::text[]), 0) = 0 OR t.connector = ANY($5::text[])
ORDER BY x.exact DESC, f.score DESC, t.qualified_name COLLATE "C", t.connector COLLATE "C"
OFFSET $6 LIMIT $7`

// Search implements store.Store.
func (s *Store) Search(ctx context.Context, q store.SearchQuery) (store.SearchPage, error) {
	if q.Page < 1 || q.Size < 1 {
		return store.SearchPage{}, fmt.Errorf("search: page %d and size %d must be positive", q.Page, q.Size)
	}

	// One row past the page says whether there is another. A page so far out
	// that its offset would overflow is past any catalog.
	if q.Page-1 > (math.MaxInt64-q.Size-1)/q.Size {
		return store.SearchPage{}, nil
	}

	offset, limit := (q.Page-1)*q.Size, q.Size+1

	var vec *pgvector.Vector

	if q.Vector != nil {
		v := pgvector.NewVector(q.Vector)
		vec = &v
	}

	rows, err := s.pool.Query(ctx, searchSQL, q.UserID, q.Text, vec, q.ModelID, q.Connectors, offset, limit)

	if err != nil {
		return store.SearchPage{}, fmt.Errorf("search: %w", err)
	}

	defer rows.Close()
	var page store.SearchPage

	for rows.Next() {
		var sc store.Score
		t, err := scanTool(rows, &sc.LexicalRank, &sc.Lexical, &sc.VectorRank, &sc.Cosine, &sc.Fused, &sc.ExactName)

		if err != nil {
			return store.SearchPage{}, fmt.Errorf("search: %w", err)
		}

		page.Tools = append(page.Tools, t)

		if q.Explain {
			page.Scores = append(page.Scores, sc)
		}
	}

	if err := rows.Err(); err != nil {
		return store.SearchPage{}, fmt.Errorf("search: %w", err)
	}

	if len(page.Tools) > q.Size {
		page.Tools, page.HasMore = page.Tools[:q.Size], true

		if q.Explain {
			page.Scores = page.Scores[:q.Size]
		}
	}

	return page, nil
}
