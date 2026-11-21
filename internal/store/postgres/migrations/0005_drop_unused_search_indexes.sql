-- Drop the GIN indexes of migration 0001 (docs/internal/design.md §5). They serve @@
-- and pg_trgm matching, which search never used: it scores BM25F from the
-- search index of migration 0004 and expands typos with token_trigrams
-- (migration 0003). They only cost maintenance on every catalog write.
-- The pg_trgm extension stays: other objects in the database may use it.
DROP INDEX IF EXISTS tools_search_text;
DROP INDEX IF EXISTS tools_name_trgm;
