-- Search tokenises by Unicode rules whatever the database's locale
-- (docs/internal/design.md §7.5), as the SQLite store's Go tokenizer does. Regex
-- character classes, lower(), and pg_trgm otherwise follow the database's
-- LC_CTYPE, and a C ctype treats every non-ASCII letter as punctuation and
-- never lowercases it. The ICU root collation "und-x-icu" pins Unicode
-- classes and case mapping; postgres.Open checks that it exists.

-- search_words splits identifiers the way the tokenizer does: camelCase
-- boundaries and every run of non-alphanumerics become spaces, and the result
-- is lowercased here so the tsvector's own (locale-dependent) lowercasing has
-- nothing left to do.
CREATE OR REPLACE FUNCTION search_words(t text) RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
  SELECT lower(regexp_replace(regexp_replace(t COLLATE "und-x-icu",
                                             '([[:lower:][:digit:]])([[:upper:]])', '\1 \2', 'g'),
                              '[^[:alnum:]]+', ' ', 'g'))
$$;

-- token_trigrams returns the distinct trigrams of a lowercase token, padded
-- the way pg_trgm pads a word (two spaces before, one after). Search uses it
-- in place of pg_trgm's similarity(), which drops non-ASCII letters on a C
-- ctype database.
CREATE FUNCTION token_trigrams(tok text) RETURNS SETOF text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
  SELECT DISTINCT substr('  ' || tok || ' ', i, 3)
  FROM generate_series(1, char_length(tok) + 1) AS i
$$;

-- Recompute the stored search_text with the new search_words.
UPDATE tools SET definition = definition;
