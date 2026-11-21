# Search golden fixture

The search quality fixture (design §10). It belongs to no driver: this
package embeds it and is its one loader. `internal/store/sqlite` holds SQLite
to every expectation (`TestSearchGolden`, lexical, and
`TestSearchGoldenHybrid`), and `internal/store/parity` holds Postgres to a
top-5 overlap with SQLite on it (`TestLexicalParityOnGolden`,
`TestHybridParityOnGolden`).

- `tools/<connector>.json` holds an array of MCP tool definitions (`mcp.Tool`
  JSON); the file name is the connector name. There are 206 tools across 14
  connectors.
- `queries.json` lists queries by kind (`exact`, `partial`, `natural`, `typo`,
  `synonym`, `filtered`). `first`, when present, must be the top result;
  every name in `top5` must appear in the first page of five.
- `hybrid_queries.json` lists natural-language and synonym queries that lexical
  search cannot meet, in the same shape. Only the hybrid tests run them.
- `vectors-<model id>.json` holds real embeddings, made by one model, of every
  tool's embed text (`catalog.EmbedText`, with its sha256) and every query in
  both files. See "Hybrid golden" below.

## Provenance

The definitions are hand-written to resemble the tool names, titles,
descriptions, and input schemas of well-known public MCP servers: GitHub
(github-mcp-server), Linear, Slack and filesystem/git/memory/time/fetch (the
modelcontextprotocol reference servers), Brave Search, Notion (hosted MCP),
Sentry, Postgres (reference server and Postgres MCP Pro), Playwright, and
Stripe. They are trimmed and are not verbatim copies of any server's current
tool list.

Expected results were written by hand from what a user would want for each
query, not by running the scorer. Expectations that the lexical algorithm
of design §7.5 cannot meet are left out rather than encoded as wrong answers.
Plural folding lets a singular query (`invoice`, `subscription`) match a tool
that only uses the plural (`list_invoices`), so those expectations are in.
Synonym queries (design §10) use a different word than the tool's name:
`fetch customers` for `stripe.list_customers`, `remove a file …` for
`github.delete_file`, `switch branches` for `git.git_checkout`. Lexical search
has no synonym table, so each case is grounded in the fixture: the synonym
appears in the tool's description (`fetch`, `retrieve`, `switches`,
`unstages`, `download`), or the rest of the query identifies the tool on its
own (`file`, `github`, `repository` for `delete_file`). In `stop a
subscription` and `save my changes`, the synonym (`stop`, `save`) appears in
no definition, so those cases pass on the other words alone: they guard the
ranking, not synonym matching. A query made only of a synonym the definitions
never use has no lexical path; that is hybrid search's job (`hybrid_queries.json`).

Some natural-language queries still expect less than a user would want:

- `show the commit history` expects only `git.git_log`, not also
  `github.list_commits`. Folding makes `commit` match `list_commits`, but it
  also folds `Shows` to `show`, and `git_diff` and `git_diff_staged` have short
  descriptions ("Shows differences between branches or commits") that contain
  both words. Nothing lexical ties `history` to `log` or `list`, so
  `list_commits` ranks just outside the top five. Counting a plural query
  word once (design §7.5) does not change this: the query has no plurals, so
  the extra weight comes from the indexed side.
- `send a message to a slack channel` expects `slack.slack_post_message` in the
  top five, not first. No tool says `send`, so typo expansion maps it to
  `sent`, which only `slack_schedule_message` uses. Folding does not change
  that.
- `who am i` expects `sentry.whoami` first, but not `github.get_me` in the top
  five, though a user asking that wants both. `get_me` says "authenticated
  GitHub user" and "the user's own profile"; it never says `who`, `whoami`,
  or `am` (`i` is below the two-character minimum), so no query word reaches
  it. `hybrid_queries.json` expects it.

## Hybrid golden

`TestSearchGoldenHybrid` and `TestHybridParityOnGolden` run once per
`vectors-*.json` file and skip when there is none. Each loads every tool's
vector under the file's model identity and runs every query in
`queries.json` and `hybrid_queries.json` with its query vector. The SQLite
test requires all expectations of both files to hold. It also runs
`hybrid_queries.json` lexical-only and fails if that meets every
expectation, since then the file would show no improvement. The parity test
holds Postgres to the lexical parity floors. Both fail, asking for a
regeneration, if a tool's embed text or a query has no vector.

`hybrid_queries.json` restores what the lexical gaps above leave out
(`github.list_commits` for `show the commit history`,
`slack.slack_post_message` first for `send a message to a slack channel`,
`github.get_me` for `who am i`) and adds queries whose key words appear in
no definition: `folder`, `website`, `recurring`, `copy`, `people`, `money`,
`tickets`, `erase`, `terminate`. Like the lexical expectations, they were
written by hand from what a user would want, not by running a scorer.

To generate vectors (wayfindr-mvg.4), point the same variables as
`internal/embed`'s live test at a real provider and run the generator, which
writes `vectors-<model id>.json` into this directory:

```sh
WAYFINDR_EMBED_LIVE_URL=http://localhost:11434/v1 \
WAYFINDR_EMBED_LIVE_MODEL=qwen3-embedding:0.6b \
WAYFINDR_EMBED_LIVE_DIMENSIONS=1024 \
go test ./internal/store/golden -run TestGenerateHybridGolden -count=1 -args -update
```

Add `WAYFINDR_EMBED_LIVE_KEY` for a provider that needs one. Rerun it after
changing any tool definition or query.
