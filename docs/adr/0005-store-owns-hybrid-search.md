# 0005 — Each store implements hybrid search; SQLite computes cosine in Go

Status: Accepted (2026-11-03). Amended:

- 2026-11-03: SQLite lexical search moved from FTS5 to an in-memory index; a
  top-K neighbor cutoff replaced the similarity floor, because a fixed floor
  depends on the embedding model.
- 2026-11-04 (wayfindr-16x): Postgres computes the same lexical score as SQLite,
  in SQL, rather than ranking with full-text search.
- 2026-11-05 (wayfindr-c3t.4, wayfindr-c3t.5, wayfindr-c3t.7): Postgres reads a
  search index that triggers keep current, takes trigrams from its own tables
  instead of `pg_trgm`, and dropped its GIN indexes.
- 2026-11-12 (wayfindr-q2rb), corrected: the lexical score is BM25F.

## Context

A local catalog holds at most a few thousand entries. sqlite-vec would need a
cgo or WASM driver ([0002](./0002-go-single-binary)). Postgres deployments have
`pgvector` and should let the database do the work.

## Decision

There is one `Store` interface with exactly two implementations. Each one
implements the whole hybrid search: lexical matches plus the top-K vector
neighbors, fused with reciprocal-rank fusion, then paginated.

- **SQLite (default)**: SQLite is persistence only. The store keeps an in-memory
  index of the catalog (loaded at open, updated on every write) and runs both
  lexical scoring (BM25F over weighted tokenized fields, corrected 2026-11-12,
  wayfindr-q2rb; plus trigram similarity for typo tolerance) and cosine
  similarity in Go. FTS5 was rejected because its trigram tokenizer matches
  substrings but does not tolerate typos (`creat_isue` must find
  `create_issue`).
- **Postgres**: `pgvector` (`<=>` cosine) plus `pg_trgm` and full-text search,
  fused in SQL. Of these, only `<=>` is used. Postgres computes the same BM25F
  and typo expansion as SQLite, in SQL, rather than `ts_rank`, which has no idf
  and no length normalization (amended 2026-11-04, wayfindr-16x). Triggers keep
  a search index current in the writing transaction: each tool's tokens and
  field lengths, and the vocabulary with its trigrams. A search reads only the
  rows of its query's terms. The trigrams come from the store's own tables
  rather than `pg_trgm`, whose word characters depend on the database's locale,
  and the unused GIN indexes were dropped (amended 2026-11-05, wayfindr-c3t.4,
  wayfindr-c3t.5, wayfindr-c3t.7).

Embeddings are optional. The provider is any OpenAI-compatible `/v1/embeddings`
endpoint (`base_url`, `model`, `dimensions`, optional `api_key`). If it is not
configured, or unreachable, search is fuzzy-only for the affected entries and
queries.

What gets embedded: the qualified name, title, description, and parameter names
and descriptions. Entries are re-embedded only when their content hash changes.
Changing the model or its dimensions re-embeds the whole catalog.

## Consequences

- First run needs no API key.
- At SQLite scale, a brute-force scan takes under a millisecond. If catalogs
  ever reach hundreds of thousands of entries, revisit this.
- Ranking must be deterministic so that pages are stable.
