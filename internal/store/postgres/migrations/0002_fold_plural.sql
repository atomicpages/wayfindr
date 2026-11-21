-- fold_plural reduces an English plural to its singular by the suffix rules of
-- docs/internal/design.md §7.5, the same rules as the SQLite store's foldPlural. Search
-- folds both the query words and the indexed lexemes with it at query time, so
-- the stored search_text keeps only the surface words.
CREATE FUNCTION fold_plural(tok text) RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
  SELECT CASE
    WHEN char_length(tok) < 4 THEN tok
    WHEN tok LIKE '%ies' THEN
      CASE WHEN char_length(tok) < 5 THEN tok ELSE left(tok, -3) || 'y' END
    WHEN tok LIKE '%sses' OR tok LIKE '%xes' OR tok LIKE '%ches' OR tok LIKE '%shes' THEN left(tok, -2)
    WHEN tok LIKE '%ss' OR tok LIKE '%us' OR tok LIKE '%is' THEN tok
    WHEN tok LIKE '%s' THEN left(tok, -1)
    ELSE tok
  END
$$;
