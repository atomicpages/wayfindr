-- Search index (docs/internal/design.md §7.5): the per-tool token counts, field
-- lengths, vocabulary, and vocabulary trigrams that search used to derive
-- from every tool's search_text on every query. Triggers on tools keep them
-- in step with the catalog in the writing transaction, so a search reads
-- only the rows of its query's terms. Writes to one user's catalog share
-- vocabulary rows, so postgres.Store serialises them with a per-user
-- advisory lock taken before any row lock; without it two writers could
-- update those rows in different orders and deadlock.

-- search_tokens is one tool's indexed tokens: every lexeme of search_text of
-- at least two characters, plus its folded form when folding changes it,
-- with its count in each field (A qualified name, B title, C parameter
-- names, D description and parameter descriptions). This is the token
-- stream the SQLite store's tokenizer indexes.
CREATE FUNCTION search_tokens(v tsvector)
RETURNS TABLE (token text, fa integer, fb integer, fc integer, fd integer)
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
  SELECT tok.token, sum(l.fa)::integer, sum(l.fb)::integer, sum(l.fc)::integer, sum(l.fd)::integer
  FROM (
    SELECT u.lexeme,
           coalesce(cardinality(array_positions(u.weights, 'A')), 0) AS fa,
           coalesce(cardinality(array_positions(u.weights, 'B')), 0) AS fb,
           coalesce(cardinality(array_positions(u.weights, 'C')), 0) AS fc,
           coalesce(cardinality(array_positions(u.weights, 'D')), 0) AS fd
    FROM unnest(v) AS u
    WHERE char_length(u.lexeme) >= 2
  ) AS l
  CROSS JOIN LATERAL unnest(CASE WHEN fold_plural(l.lexeme) = l.lexeme THEN ARRAY[l.lexeme]
                                 ELSE ARRAY[l.lexeme, fold_plural(l.lexeme)] END) AS tok(token)
  GROUP BY tok.token
$$;

-- One row per tool: its field lengths, in tokens. A tool with no tokens
-- still has a row, so the row count is the catalog's document count.
CREATE TABLE search_docs (
  user_id   uuid NOT NULL,
  connector text NOT NULL,
  name      text NOT NULL,
  la        integer NOT NULL,
  lb        integer NOT NULL,
  lc        integer NOT NULL,
  ld        integer NOT NULL,
  PRIMARY KEY (user_id, connector, name),
  FOREIGN KEY (user_id, connector, name) REFERENCES tools ON DELETE CASCADE
);

-- One row per tool and token: the token's count in each field.
CREATE TABLE search_postings (
  user_id   uuid NOT NULL,
  connector text NOT NULL,
  name      text NOT NULL,
  token     text NOT NULL,
  fa        integer NOT NULL,
  fb        integer NOT NULL,
  fc        integer NOT NULL,
  fd        integer NOT NULL,
  PRIMARY KEY (user_id, token, connector, name),
  FOREIGN KEY (user_id, connector, name) REFERENCES tools ON DELETE CASCADE
);
CREATE INDEX search_postings_tool ON search_postings (user_id, connector, name);

-- One row per token of a user's catalog: the number of tools that have it
-- (document frequency) and its number of distinct trigrams.
CREATE TABLE search_vocab (
  user_id uuid NOT NULL,
  token   text NOT NULL,
  df      integer NOT NULL,
  grams   integer NOT NULL,
  PRIMARY KEY (user_id, token)
);

-- The trigrams of every vocabulary token (token_trigrams), for typo
-- expansion: a misspelt word's candidates are the tokens sharing a trigram.
CREATE TABLE search_vocab_grams (
  user_id uuid NOT NULL,
  token   text NOT NULL,
  gram    text NOT NULL,
  PRIMARY KEY (user_id, token, gram)
);
CREATE INDEX search_vocab_grams_gram ON search_vocab_grams (user_id, gram, token);

-- Index the catalog already stored.
INSERT INTO search_docs (user_id, connector, name, la, lb, lc, ld)
SELECT t.user_id, t.connector, t.name,
       coalesce(sum(s.fa), 0), coalesce(sum(s.fb), 0), coalesce(sum(s.fc), 0), coalesce(sum(s.fd), 0)
FROM tools AS t LEFT JOIN LATERAL search_tokens(t.search_text) AS s ON true
GROUP BY t.user_id, t.connector, t.name;

INSERT INTO search_postings (user_id, connector, name, token, fa, fb, fc, fd)
SELECT t.user_id, t.connector, t.name, s.token, s.fa, s.fb, s.fc, s.fd
FROM tools AS t CROSS JOIN LATERAL search_tokens(t.search_text) AS s;

INSERT INTO search_vocab (user_id, token, df, grams)
SELECT user_id, token, count(*), (SELECT count(*) FROM token_trigrams(token))
FROM search_postings GROUP BY user_id, token;

INSERT INTO search_vocab_grams (user_id, token, gram)
SELECT v.user_id, v.token, g.gram FROM search_vocab AS v CROSS JOIN LATERAL token_trigrams(v.token) AS g(gram);

-- search_index_tool rewrites one tool's postings and field lengths from its
-- search_text. Deleting a tool deletes them by cascade.
CREATE FUNCTION search_index_tool() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    DELETE FROM search_postings
    WHERE user_id = OLD.user_id AND connector = OLD.connector AND name = OLD.name;
  END IF;
  WITH s AS (
    SELECT * FROM search_tokens(NEW.search_text)
  ), postings AS (
    INSERT INTO search_postings (user_id, connector, name, token, fa, fb, fc, fd)
    SELECT NEW.user_id, NEW.connector, NEW.name, s.token, s.fa, s.fb, s.fc, s.fd FROM s
  )
  INSERT INTO search_docs (user_id, connector, name, la, lb, lc, ld)
  SELECT NEW.user_id, NEW.connector, NEW.name,
         coalesce(sum(s.fa), 0), coalesce(sum(s.fb), 0), coalesce(sum(s.fc), 0), coalesce(sum(s.fd), 0)
  FROM s
  ON CONFLICT (user_id, connector, name) DO UPDATE
    SET la = EXCLUDED.la, lb = EXCLUDED.lb, lc = EXCLUDED.lc, ld = EXCLUDED.ld;
  RETURN NULL;
END
$$;

CREATE TRIGGER tools_search_index_insert AFTER INSERT ON tools
  FOR EACH ROW EXECUTE FUNCTION search_index_tool();
CREATE TRIGGER tools_search_index_update AFTER UPDATE ON tools
  FOR EACH ROW WHEN (OLD.search_text IS DISTINCT FROM NEW.search_text) EXECUTE FUNCTION search_index_tool();

-- search_vocab_add counts added postings into the vocabulary, adding the
-- trigrams of tokens new to it.
CREATE FUNCTION search_vocab_add() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO search_vocab_grams (user_id, token, gram)
  SELECT n.user_id, n.token, g.gram
  FROM (SELECT DISTINCT user_id, token FROM added) AS n
  CROSS JOIN LATERAL token_trigrams(n.token) AS g(gram)
  WHERE NOT EXISTS (SELECT 1 FROM search_vocab AS v WHERE v.user_id = n.user_id AND v.token = n.token);

  INSERT INTO search_vocab (user_id, token, df, grams)
  SELECT user_id, token, count(*), (SELECT count(*) FROM token_trigrams(token))
  FROM added GROUP BY user_id, token
  ON CONFLICT (user_id, token) DO UPDATE SET df = search_vocab.df + EXCLUDED.df;
  RETURN NULL;
END
$$;

-- search_vocab_remove uncounts removed postings, dropping the tokens no tool
-- has any more with their trigrams.
CREATE FUNCTION search_vocab_remove() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  UPDATE search_vocab AS v SET df = v.df - r.n
  FROM (SELECT user_id, token, count(*) AS n FROM removed GROUP BY user_id, token) AS r
  WHERE v.user_id = r.user_id AND v.token = r.token;

  WITH gone AS (
    DELETE FROM search_vocab AS v
    USING (SELECT DISTINCT user_id, token FROM removed) AS r
    WHERE v.user_id = r.user_id AND v.token = r.token AND v.df <= 0
    RETURNING v.user_id, v.token
  )
  DELETE FROM search_vocab_grams AS g
  USING gone WHERE g.user_id = gone.user_id AND g.token = gone.token;
  RETURN NULL;
END
$$;

CREATE TRIGGER search_postings_vocab_add AFTER INSERT ON search_postings
  REFERENCING NEW TABLE AS added FOR EACH STATEMENT EXECUTE FUNCTION search_vocab_add();
CREATE TRIGGER search_postings_vocab_remove AFTER DELETE ON search_postings
  REFERENCING OLD TABLE AS removed FOR EACH STATEMENT EXECUTE FUNCTION search_vocab_remove();
