# §10 Testing

Part of the [Wayfindr technical design](../design.md). Go code cites this file
as "design §10".

- **Unit:** config (every documented example, every validation rule), secrets
  (tamper, AAD swap, wrong version byte), tokenizer and scoring, `config.Diff`,
  the state machine transitions.
- **Fakes** (`internal/testutil`):
  - `fakeupstream`: an SDK server that can speak 2025-06-18, 2025-11-25, or
    2026-07-28, over a stdio helper binary (`go test` re-execs itself), an
    `httptest` Streamable server, and an `httptest` legacy SSE server. The
    transport × version matrix is stdio and Streamable at all three versions,
    and SSE at 2025-06-18 and 2025-11-25 only: 2026-07-28 defines no HTTP+SSE
    transport. Switches: send `list_changed`, request elicitation, return
    `input_required`, slow responses, 4xx on Streamable.
  - `fakeas`: PRM and AS metadata at configurable well-known paths, DCR,
    authorize and token endpoints. Quirk switches: no metadata, no PKCE
    advertised, issuer mismatch, rotating refresh tokens, early revocation
    (`invalid_grant`), `insufficient_scope`.
- **Integration:** a full daemon on a random port per test, using SQLite in a
  temp dir.
- **Race:** `go test -race ./...`. A dedicated test sends 50 concurrent calls
  against a token 30s from expiry, with rotating refresh tokens. It asserts
  exactly one refresh and zero failed calls.
- **Search quality:** a golden fixture of about 200 real tool definitions plus
  queries with expected top-5 results, including typo, synonym, and
  natural-language cases. The fixture, its loader, and the generator of its
  real-embedding vectors files live in `internal/store/golden`, which belongs to
  no driver. SQLite must meet every expectation.
- **Store suite** (`internal/store/storetest`): every driver runs one behaviour
  suite at the `Store` seam, search included wherever it has one worked answer
  (pagination, `has_more`, fusion, ties, the connector filter after fusion,
  model identity, vector length, plural folding, typos, non-ASCII letters).
  Postgres runs it on a default-locale and on a `C`-ctype database.
  `storetest.Drivers` lists the driver configurations. A driver's own tests keep
  only what is internal to it, such as SQLite's index consistency, tokenizer
  table, and pragmas.
- **Search parity** (`internal/store/parity`): only cross-driver comparison. On
  the golden fixture, Postgres's lexical top five must overlap SQLite's by at
  least 0.8 on average and 0.6 on every query, and must contain every name a
  query expects. With a golden vectors file, the same floors apply to hybrid
  search.
- **Conformance:** `npx @modelcontextprotocol/conformance@0.2.0-alpha.12` in
  server mode against `/mcp`, for each supported version (`make conformance`).
  Client-mode conformance is deferred.
- **Harness compatibility:** a manual checklist for each harness `wayfindr init`
  prints (see `docs/guide/clients.md`), over the shim and, where the harness can
  read the client token from its environment, over HTTP. It runs before each
  release.
- **Make targets:** `make build test race lint conformance check`, where `check`
  runs everything except `conformance`. `make test-postgres` runs the store
  suite (`internal/store/storetest`), the search parity suite, and the `init`
  tests against a throwaway `pgvector/pgvector` container; `make bench-postgres`
  runs the search latency benchmark ([§7.5](7.5-search.md)) on one. Without
  `WAYFINDR_TEST_PG_DSN` the Postgres tests skip, so `check` needs no Docker.
- **CI** (`.github/workflows/ci.yml`, on every push to `main` or `next` and
  every pull request) calls `checks.yml`: golangci-lint (pinned v2.15.0),
  `go test ./...` on Linux, macOS, and Windows, and `make race` on Linux; each
  of them gates. CI also runs shellcheck on `install.sh` and actionlint, a
  GoReleaser snapshot of every release target, and the installers against it
  (`install-test.yml`); `release.yml` repeats all of it for a `v*` tag
  (`docs/internal/releasing.md`). `docs.yml` builds the site. Neither
  `make conformance` nor `make test-postgres` runs in CI: run them before a
  release, and conformance after any change to the gateway or the SDK.
