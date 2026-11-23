# Harness compatibility

This page checks that real MCP clients ("harnesses") work with Wayfindr. It has
two parts:

- **Automated:** `make conformance` runs the MCP conformance suite against the
  daemon's `/mcp` at each client protocol version (the server leg).
  `make conformance-client` runs the suite's client scenarios against Wayfindr's
  upstream side, its MCP client and its OAuth client (the client leg). Both
  results are at the end of this page.
- **Manual:** a checklist for each harness `wayfindr init` prints a quick
  connect for, over the stdio shim and, where the clients guide has an HTTP
  snippet for the harness, over HTTP. A person runs it and fills in the results
  table.

## Automated: `make conformance`

```bash
make conformance   # needs Go and Node.js (npx); not part of `make check`
```

The target builds `bin/wayfindr` and runs `scripts/conformance.sh`, which:

1. Builds `conformancekit` (`internal/testutil/cmd/conformancekit`), a test-only
   helper that never ships in the `wayfindr` binary.
2. Creates a temp dir and points `WAYFINDR_CONFIG`, `WAYFINDR_DATA_DIR`, and the
   `XDG_*` bases into it. It sets `WAYFINDR_SECRET_KEY` to a random key and
   `WAYFINDR_CLIENT_TOKEN` to a random token, so the real home, config, data,
   and OS keychain are never touched.
3. Writes a config with a free loopback port and one stdio connector, `fake`,
   which runs `conformancekit upstream` (a `fakeupstream` server with one `echo`
   tool). Then it runs `wayfindr init` and starts `wayfindr start --foreground`.
4. Starts `conformancekit proxy` on a second port. The suite cannot send an
   `Authorization` header, so the proxy adds `Bearer $WAYFINDR_CLIENT_TOKEN`. It
   keeps the `Host` name and `Origin` the suite sent, so the daemon's
   DNS-rebinding guards are still tested.
5. Waits for `/healthz`, then runs
   `npx @modelcontextprotocol/conformance@0.2.0-alpha.12 server --suite all`
   once for each version: 2025-06-18, 2025-11-25, and 2026-07-28.
6. Stops the daemon and the proxy and removes the temp dir, however the run
   ends.

Each version has an expected-failures file,
`scripts/conformance-baseline/<version>.yml`. Every entry names one check,
`<scenario>:<check-id>`, never a whole scenario, so a scenario that fails by
design for one check still fails the run if another of its checks regresses (for
example the `tools/list` caching-hints check in `caching`, or
`wire-schema-valid` in any scenario). The run fails on any failing or warning
check that is not listed, and on a listed check that now passes. Results (one
`checks.json` per scenario; check IDs are its `id` fields) are written to
`bin/conformance/<version>/`.

Knobs: `CONFORMANCE_VERSIONS="2026-07-28"` runs one version, and
`CONFORMANCE_ARGS="--scenario tools-list"` runs one scenario.

To test a daemon that is already running (for example one reached through a
tunnel), set `CONFORMANCE_URL` to its `/mcp` URL (`http://` or `https://`) and
`CONFORMANCE_TOKEN` to its bearer token. Then the script starts no daemon: it
runs the same suite against that URL, with the same proxy adding the token, and
compares to the same baselines. Set both variables or neither; setting one alone
fails. The token goes only to the proxy's environment, and the script never
prints it. The daemon sees the URL's host name in `Host`, so its DNS-rebinding
guard judges that name.

```bash
CONFORMANCE_URL=http://127.0.0.1:7777/mcp CONFORMANCE_TOKEN="$(cat <data dir>/client-token)" make conformance
```

## Automated: `make conformance-client`

```bash
make conformance-client   # needs Go and Node.js (npx); not part of `make check`
```

Here Wayfindr is the client under test. The suite starts a test MCP server (and,
for `auth/*` scenarios, a test authorization server that approves every
authorization request at once), then runs a command with the server's URL. The
target builds `bin/wayfindr` and runs `scripts/conformance-client.sh`, which
builds that command from `scripts/conformance-client/` and, for each of
2025-11-25 and 2026-07-28:

1. Runs
   `npx @modelcontextprotocol/conformance@0.2.0-alpha.12 client --requirements <version>`:
   every client scenario the version requires, plus the ones its requirement set
   lists as not scored, in parallel.
2. Runs each not-scored auth extension (`client-credentials-basic`,
   `client-credentials-jwt`, `enterprise-managed-authorization`, `dpop`,
   `dpop-nonce`, `wif-jwt-bearer`) again on its own with `--scenario`, because
   the suite checks only scored scenarios against the baseline in a
   `--requirements` run.

For one scenario, the command (`scripts/conformance-client/main.go`):

1. Writes a temp config with one url connector, `conf`, at the test server. An
   `auth/*` scenario gets `[connectors.conf.auth] type = "oauth"`, plus the
   `client_id` and `client_secret` of the suite's `MCP_CONFORMANCE_CONTEXT` when
   it has them. An `auth/client-credentials-*` scenario gets
   `grant = "client_credentials"`, and its context's `private_key_pem` becomes
   `[auth.signing_key] key = "${env:…}"`; a context with `valid_jwt` gets
   `grant = "jwt_bearer"` with the JWT as `[auth.assertion] value = "${env:…}"`.
   The key and the JWT reach the daemon through its environment, never the
   config file. A context this version can't express (an IdP token) ends the run
   with exit status 3 before any daemon starts, so that scenario fails as
   expected.
2. Starts a throwaway daemon as `make conformance` does: temp config, data dir,
   and `XDG_*` bases, a random `WAYFINDR_SECRET_KEY` and
   `WAYFINDR_CLIENT_TOKEN`, so the real home and keychain are never touched.
3. While the connector is `needs_auth` (an unattended grant never is, so those
   scenarios skip this step), plays the user through the admin API, the way
   `wayfindr auth login` does: `StartAuth`, then a GET of the authorize URL that
   follows the test authorization server's redirect to the daemon's
   `/oauth/callback`, then `WaitAuth`. A call that leaves the connector in
   `needs_auth` (a step-up) gets the same, then the call again. At most three
   authorizations per run.
4. Plays the agent over `/mcp`: lists the connector's tools, reads their
   definitions with `search_tools`, and calls each with `call_tool`, so the
   upstream sees a real `tools/list` and `tools/call`. Arguments come from the
   tool's input schema, or from the context's `toolCalls` when the scenario
   names its calls (`http-custom-headers`). `json-schema-2020-12-preservation`
   gets the schema tool's `inputSchema` as `search_tools` returned it, so the
   suite sees what the catalog kept. The `/mcp` session accepts each elicitation
   with the form's defaults.
5. Stops the daemon and removes the temp dir. Its progress, and the end of the
   daemon log when something failed, go to the scenario's `stderr.txt`.

The suite scores what its servers saw, never the command's exit status; a
non-zero status only prints the command's output in the run (expected for the
scenarios that refuse a flow, such as `auth/iss-*` and
`auth/scope-retry-limit`).

Each version has an expected-failures file,
`scripts/conformance-baseline/client-<version>.yml`, with the same per-check
format as the server leg's. It covers both runs: an entry for a scenario a run
did not include is ignored. Results are in `bin/conformance-client/<version>/`,
the extensions in `bin/conformance-client/<version>/extensions/`.

Knobs: `CONFORMANCE_VERSIONS="2026-07-28"` runs one version,
`CONFORMANCE_EXTENSIONS=""` skips the extension runs, and `CONFORMANCE_TIMEOUT`
sets the per-scenario timeout in ms (default 150000; the command gives up at 100
seconds and stops its daemon). One scenario by hand, after `make build` and
`go build -o bin/cc-harness ./scripts/conformance-client`:

```bash
WAYFINDR_BIN=$PWD/bin/wayfindr npx -y @modelcontextprotocol/conformance@0.2.0-alpha.12 \
  client --command "$PWD/bin/cc-harness" --scenario auth/metadata-default \
  --spec-version 2026-07-28 -o bin/cc-try
```

## Manual checklist

### Setup (once)

1. Install `wayfindr` on `PATH` (`make build`, then copy `bin/wayfindr`).
2. Run `wayfindr init`. It prints a quick-connect command (or the entry to add
   to a config file) for each harness, all over the stdio shim. Keep that output
   open for the stdio rows; the HTTP setup is in the clients guide (below).
3. Add two connectors and a profile to the config that `init` printed:

   ```toml
   # A working connector. `go build -o ~/bin/conformancekit ./internal/testutil/cmd/conformancekit`
   # gives a local fake with one tool, `echo`; any real stdio server works too.
   [connectors.fake]
   command = "~/bin/conformancekit"
   args    = ["upstream"]

   # An OAuth connector you have NOT authorized, so it stays in needs_auth.
   # Without the auth table it is not an OAuth connector: it never reaches
   # needs_auth, and its /auth page answers that it does not use OAuth.
   [connectors.linear]
   url  = "https://mcp.linear.app/mcp"
   auth = { type = "oauth" }

   # For C10: a profile that leaves linear out.
   [profiles.compat]
   connectors = ["fake"]
   ```

   C8 also needs a connector whose tool asks the user for input (elicitation).
   `conformancekit`'s fake has none; use a real server that elicits, and skip C8
   (write "n/a") if you have none.

4. For the HTTP rows, export the token in the shell that starts the harness, as
   the clients guide shows:
   `export WAYFINDR_CLIENT_TOKEN="$(cat <data dir>/client-token)"`.
5. Configure only one Wayfindr entry per harness at a time (HTTP or stdio), so
   you know which transport you are testing.

### Install snippets

Use the stdio command or entry that `wayfindr init` prints, and the HTTP
snippets in [docs/guide/clients.md](../guide/clients.md). That page is the one
source for every harness's setup; don't copy snippets here. If what you ran
differs from it, note the difference in the results table and fix the page.

Zed is tested over stdio only: it sends remote server header values as written,
without expanding environment variables (zed-industries/zed#60459), so there is
no HTTP snippet that keeps the token out of its settings.

### Checks

Run every check for each harness × transport pair. For HTTP, start the daemon
first with `wayfindr start`. For stdio, make sure no daemon is running
(`wayfindr status` reports it is not reachable) before C1, so C7 is tested too.

| ID  | Check                      | Steps                                                                                                                                                                                                                                                         | Pass when                                                                                                                                                                                                                                                                                                                                                                      |
| --- | -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| C1  | Install                    | Add the snippet, then start or reload the harness.                                                                                                                                                                                                            | The harness shows `wayfindr` as connected, with no auth or protocol error. Write down the protocol version it negotiated, from the harness's own log (Claude Code `--debug-file`, Codex `RUST_LOG=rmcp=info`); the daemon doesn't log it.                                                                                                                                      |
| C2  | Exactly two tools          | Open the harness's tool list for `wayfindr`.                                                                                                                                                                                                                  | It lists exactly `search_tools` and `call_tool`, with their descriptions.                                                                                                                                                                                                                                                                                                      |
| C3  | `search_tools` results     | Ask the agent: "Use wayfindr's search_tools to find a tool that echoes text."                                                                                                                                                                                 | The result includes `fake.echo` with its schema, and an `unavailable` entry for `linear` with state `needs_auth`.                                                                                                                                                                                                                                                              |
| C4  | `call_tool` calls a tool   | Ask: "Call fake.echo with text hello through call_tool."                                                                                                                                                                                                      | The result is `{"text":"hello"}`. If the harness asks for permission, note that it sees only `call_tool`, a known risk: it can't ask per upstream tool.                                                                                                                                                                                                                        |
| C5  | `needs_auth` link          | Ask: "Call any linear tool through call_tool", or look at C3's `unavailable` entry.                                                                                                                                                                           | The agent shows a readable message (summary and action) and the link `http://127.0.0.1:7777/auth/linear`, and the link opens the "Continue" page in a browser. Note whether the harness renders it as a clickable link.                                                                                                                                                        |
| C6  | Daemon restart mid-session | With the harness session open, stop the daemon (Ctrl-C on `wayfindr start --foreground`, or `wayfindr stop` for stdio) and start it again (`wayfindr start`; for stdio, let the shim restart it or start it by hand). Repeat C4 in the same session.          | C4 succeeds without restarting the harness or re-adding the server. Note any error the harness shows during the gap and whether it reconnects by itself.                                                                                                                                                                                                                       |
| C7  | Shim starts the daemon     | stdio rows only: with no daemon running, start the harness (C1).                                                                                                                                                                                              | The harness connects; `wayfindr status` now reports a running daemon; `<data dir>/wayfindr.log` holds the daemon's log. HTTP rows: write "n/a".                                                                                                                                                                                                                                |
| C8  | Elicitation                | Ask the agent to call the eliciting connector's tool through `call_tool`, and answer the question.                                                                                                                                                            | The harness shows the question and the call finishes with your answer. Over HTTP at 2026-07-28 it arrives as an `input_required` result the harness answers by calling again; over stdio, a client older than 2026-07-28 gets it from the shim as `elicitation/create`. A client that can't be asked gets a readable `cannot_ask` error, never a hang. Note which way it went. |
| C9  | Scoped client token        | HTTP rows only: `wayfindr token create compat --connector fake`, put the printed `wf_s_…` value in the harness's `WAYFINDR_CLIENT_TOKEN`, restart the harness, and repeat C3, then ask it to call any linear tool. Afterwards `wayfindr token revoke compat`. | C3 finds `fake.echo` and lists nothing about `linear`, not even under `unavailable`; the linear call is refused as a connector that is not configured, with no auth link. stdio rows: write "n/a".                                                                                                                                                                             |
| C10 | Profile                    | Point the entry at the profile: HTTP `http://127.0.0.1:7777/mcp?profile=compat`, stdio `wayfindr stdio --profile compat`. Repeat C3. Then try `?profile=nope` (or `--profile nope`).                                                                          | With `compat`, C3 finds `fake.echo` and `linear` is gone from `unavailable`. With `nope`, every `search_tools` and `call_tool` answers an `unknown_profile` error naming the profile.                                                                                                                                                                                          |

Also record, for each pair: the harness version, the OS, and anything the
harness logged about `Mcp-Session-Id` (see R1 below).

### Things to watch

- **`prompts/*`, `resources/*`, `completion/*` answer -32601.** The gateway
  declares only `tools`, and since wayfindr-so9.13 it answers every request of
  the prompts, resources, and completions capabilities with -32601 "method not
  found" (HTTP 404 at 2026-07-28) instead of the SDK's empty lists. Since
  wayfindr-so9.18 the stdio shim answers them the same way. A harness that calls
  `prompts/list` or `resources/list` without checking the server's capabilities
  now gets an error. During C1 and C2, note whether the harness shows or logs
  such an error, or marks `wayfindr` as failed or degraded; if it does, record
  the harness, version, and message in Notes.
- **`logging/setLevel` is accepted but `logging` is not declared.** Before
  2026-07-28 the SDK answers it with `{}` and the gateway sends no log messages
  (a stateless handler cannot). From 2026-07-28 it is a removed method and
  answers -32601.

### Results

Fill in `pass`, `fail` (with a note), or `n/a`. Add the harness version in the
first column.

| Harness (version)           | Transport | C1 install | C2 two tools | C3 search | C4 call | C5 needs_auth link | C6 restart | C7 shim auto-start | C8 elicitation | C9 scoped token | C10 profile | Notes                    |
| --------------------------- | --------- | ---------- | ------------ | --------- | ------- | ------------------ | ---------- | ------------------ | -------------- | --------------- | ----------- | ------------------------ |
| Claude Code (2.1.326)       | HTTP      | pass       | pass         | pass      | pass    |                    |            | n/a                |                | pass            | pass        | 2026-07-28; see (a), (b) |
| Claude Code (2.1.326)       | stdio     | pass       | pass         | pass      | pass    |                    |            | pass               |                | n/a             | pass        | 2026-07-28; see (a), (b) |
| Codex (0.169.0)             | HTTP      | pass       | pass         | pass      | pass    |                    |            | n/a                |                | pass            | pass        | 2025-06-18; see (a), (c) |
| Codex (0.169.0)             | stdio     | pass       | pass         | pass      | pass    |                    |            | pass               |                | n/a             | pass        | 2025-06-18; see (a), (c) |
| OpenCode ()                 | HTTP      |            |              |           |         |                    |            | n/a                |                |                 |             |                          |
| OpenCode ()                 | stdio     |            |              |           |         |                    |            |                    |                | n/a             |             |                          |
| Hermes Agent ()             | HTTP      |            |              |           |         |                    |            | n/a                |                |                 |             |                          |
| Hermes Agent ()             | stdio     |            |              |           |         |                    |            |                    |                | n/a             |             |                          |
| Gemini CLI ()               | HTTP      |            |              |           |         |                    |            | n/a                |                |                 |             |                          |
| Gemini CLI ()               | stdio     |            |              |           |         |                    |            |                    |                | n/a             |             |                          |
| GitHub Copilot CLI ()       | HTTP      |            |              |           |         |                    |            | n/a                |                |                 |             |                          |
| GitHub Copilot CLI ()       | stdio     |            |              |           |         |                    |            |                    |                | n/a             |             |                          |
| VS Code ()                  | HTTP      |            |              |           |         |                    |            | n/a                |                |                 |             |                          |
| VS Code ()                  | stdio     |            |              |           |         |                    |            |                    |                | n/a             |             |                          |
| Cursor ()                   | HTTP      |            |              |           |         |                    |            | n/a                |                |                 |             |                          |
| Cursor ()                   | stdio     |            |              |           |         |                    |            |                    |                | n/a             |             |                          |
| Zed ()                      | stdio     |            |              |           |         |                    |            |                    |                | n/a             |             |                          |
| Windsurf (Devin Desktop) () | HTTP      |            |              |           |         |                    |            | n/a                |                |                 |             |                          |
| Windsurf (Devin Desktop) () | stdio     |            |              |           |         |                    |            |                    |                | n/a             |             |                          |
| goose ()                    | HTTP      |            |              |           |         |                    |            | n/a                |                |                 |             |                          |
| goose ()                    | stdio     |            |              |           |         |                    |            |                    |                | n/a             |             |                          |

Notes on the Claude Code and Codex rows (2026-11-16, commit `ecc20fb`, macOS
27.2.1 arm64). C5, C6, and C8 need a person and are still open.

- (a) Run headless against an isolated daemon (temp config, data dir, and XDG
  bases; master key from `WAYFINDR_SECRET_KEY`; a free loopback port), set up as
  in Setup above. Each entry was passed on the command line, not written to the
  harness's user config, but has the shape the clients guide shows: the
  `.mcp.json` HTTP entry, or a stdio entry running the built binary's `stdio`,
  with the isolation variables in its `env`. C9 and C10 were run as extra
  entries in one session (a scoped-token entry, and entries with
  `?profile=compat`, `?profile=nope`, or `--profile compat`/`nope`). Neither
  harness logged anything about `Mcp-Session-Id`, and neither called `prompts/*`
  or `resources/*`, so no -32601 showed up.
- (b)
  `claude -p … --mcp-config <file> --strict-mcp-config --tools "" --allowedTools mcp__wayfindr__search_tools,mcp__wayfindr__call_tool`.
  Claude Code negotiated 2026-07-28 over both transports; its `--debug-file` log
  has `"negotiatedProtocolVersion":"2026-07-28"`. It logs the shim's stderr
  lines at `[ERROR]` ("Server stderr: … profile applies"), whatever their level;
  cosmetic.
- (c) `codex exec --ignore-user-config --ephemeral -c mcp_servers.wayfindr.…`.
  Codex negotiated 2025-06-18 over both transports, so over HTTP it gets no tool
  questions (C8 must use stdio). `RUST_LOG=rmcp=info` shows it as
  `protocol_version: ProtocolVersion("2025-06-18")`. In `codex exec`,
  `search_tools` ran without approval (it is marked read-only), but the first
  `call_tool` failed with
  `MCP tool call requires approval, but approval policy is never`. With
  `-c mcp_servers.wayfindr.tools.call_tool.approval_mode="approve"` it passed.
  The clients guide now says so.
- The daemon does not log the negotiated protocol version at any level; read it
  from the harness's own log, as above.

## R1: stateful sessions

The gateway serves `/mcp` with one **stateless** Streamable HTTP handler (design
§7.6, ADR 0003): it never sends `Mcp-Session-Id`, and it accepts every request
without one. That one handler serves 2025-06-18, 2025-11-25, and 2026-07-28
clients. The conformance suite agrees for the protocol checks; at 2025-11-25 it
warns only that it gets no session ID (see below).

Risk R1 is that a harness needs a session anyway: for example, it refuses a
server that sends no `Mcp-Session-Id`, it requires the GET stream to belong to a
session, or it re-initializes after every request. **If any row above shows
this, record it here**: harness, version, transport, the exact error, and the
negotiated protocol version.

| Harness (version) | Transport | Protocol version | Symptom |
| ----------------- | --------- | ---------------- | ------- |
|                   |           |                  |         |

None so far. Claude Code 2.1.326 (2026-07-28) and Codex 0.169.0 (2025-06-18, a
pre-2026-07-28 client over the stateless handler) worked over HTTP with no
session (2026-11-16). Codex's `rmcp` logs, at debug only, "sse response ended
before an event ID was received; cannot resume" after each POST; it does not
retry or fail.

Proposed fallback if R1 occurs: a **dual handler**. The gateway keeps one
`mcp.Server` and builds two handlers on it, a stateless one and a stateful one
(`StreamableHTTPOptions{Stateless: false}`), and `/mcp` dispatches between them
per request:

- A request that carries `Mcp-Session-Id` goes to the stateful handler.
- An `initialize` POST from a pre-2026-07-28 client goes to the stateful
  handler, which assigns a session. The dispatch can be limited to the harnesses
  that need it, matched on `clientInfo.name`, or turned on by a config switch.
- Everything else, including all 2026-07-28 traffic, stays stateless.

Sessions live only in the daemon's memory. After a daemon restart the stateful
handler answers an unknown session ID with 404, and the spec requires the client
to initialize again, so C6 must be checked again for that harness. The gateway
would still declare `tools` without `listChanged` and no `logging`, because only
some sessions could deliver them.

## Conformance results

`make conformance` with `@modelcontextprotocol/conformance@0.2.0-alpha.12`,
`--suite all`, run on 2026-11-12 against commit `2497e7b`. Every version matches
its baseline, every count below is unchanged since the first run (2026-11-04,
`3d14693`), and the target exits 0.

Cells are checks passed / failed / warned. **fixture** means the scenario calls
something that only the suite's reference server defines: a tool named `test_*`,
a prompt, a resource, `completion/complete`, or `resources/subscribe`. Wayfindr
exposes only `search_tools` and `call_tool` and no prompts or resources (ADR
0001), so these fail by design ("unknown tool", or -32601 for prompts,
resources, and completions) and say nothing about protocol conformance. In each
of them the one passing check is `wire-schema-valid`: every message Wayfindr
sent was valid for the negotiated version. A dash means the suite does not run
the scenario at that version.

| Scenario                                                                                                                                                                                                 | 2025-06-18          | 2025-11-25          | 2026-07-28           |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------- | ------------------- | -------------------- |
| server-initialize                                                                                                                                                                                        | 2/0/0 pass          | 2/0/0 pass          | -                    |
| server-session-lifecycle                                                                                                                                                                                 | 0/0/0 pass (1)      | 0/0/0 pass (1)      | -                    |
| logging-set-level                                                                                                                                                                                        | 2/0/0 pass          | 2/0/0 pass          | -                    |
| ping                                                                                                                                                                                                     | 2/0/0 pass          | 2/0/0 pass          | -                    |
| tools-list                                                                                                                                                                                               | 2/0/0 pass          | 3/0/0 pass          | 4/0/0 pass           |
| resources-list                                                                                                                                                                                           | 1/1/0 baselined (5) | 1/1/0 baselined (5) | 1/1/0 baselined (5)  |
| prompts-list                                                                                                                                                                                             | 1/1/0 baselined (5) | 1/1/0 baselined (5) | 1/1/0 baselined (5)  |
| dns-rebinding-protection                                                                                                                                                                                 | -                   | 2/0/0 pass          | 2/0/0 pass           |
| server-sse-multiple-streams                                                                                                                                                                              | -                   | 0/0/1 baselined (2) | 2/0/0 pass           |
| server-sse-polling                                                                                                                                                                                       | -                   | 0/0/3 baselined (2) | -                    |
| server-stateless                                                                                                                                                                                         | -                   | -                   | 24/4/0 baselined (3) |
| sep-2164-resource-not-found                                                                                                                                                                              | -                   | -                   | 2/0/2 baselined (5)  |
| caching                                                                                                                                                                                                  | -                   | -                   | 4/3/0 baselined (5)  |
| http-header-validation                                                                                                                                                                                   | -                   | -                   | 14/0/0 pass          |
| http-custom-header-server-validation                                                                                                                                                                     | -                   | -                   | 1/5/0 fixture (4)    |
| input-required-result-unsupported-methods                                                                                                                                                                | -                   | -                   | 2/0/0 pass           |
| input-required-result-validate-input                                                                                                                                                                     | -                   | -                   | 3/0/0 pass           |
| input-required-result-missing-input-response                                                                                                                                                             | -                   | -                   | 1/0/1 fixture        |
| input-required-result-ignore-extra-params                                                                                                                                                                | -                   | -                   | 1/0/1 fixture        |
| completion-complete                                                                                                                                                                                      | 1/1/0 fixture       | 1/1/0 fixture       | 1/1/0 fixture        |
| tools-call-simple-text, -image, -audio, -embedded-resource, -mixed-content, -error, -with-progress                                                                                                       | 1/1/0 fixture each  | 1/1/0 fixture each  | 1/1/0 fixture each   |
| tools-call-with-logging, -sampling, -elicitation                                                                                                                                                         | 1/1/0 fixture each  | 1/1/0 fixture each  | -                    |
| json-schema-2020-12                                                                                                                                                                                      | -                   | 1/1/0 fixture       | 1/1/0 fixture        |
| elicitation-sep1034-defaults, elicitation-sep1330-enums                                                                                                                                                  | -                   | 1/1/0 fixture each  | -                    |
| resources-read-text, -read-binary, -templates-read                                                                                                                                                       | 1/1/0 fixture each  | 1/1/0 fixture each  | 1/1/0 fixture each   |
| resources-subscribe, resources-unsubscribe                                                                                                                                                               | 1/1/0 fixture each  | 1/1/0 fixture each  | -                    |
| prompts-get-simple, -with-args, -embedded-resource, -with-image                                                                                                                                          | 1/1/0 fixture each  | 1/1/0 fixture each  | 1/1/0 fixture each   |
| input-required-result-basic-elicitation, -basic-sampling, -basic-list-roots, -request-state, -multiple-input-requests, -multi-round, -non-tool-request, -result-type, -tampered-state, -capability-check | -                   | -                   | 1/1/0 fixture each   |
| **Total**                                                                                                                                                                                                | 30/22/0             | 36/25/4             | 88/40/4              |

Notes:

1. The suite reports "Server is stateless (no MCP-Session-Id): lifecycle checks
   not applicable", as an INFO. This is the design (R1).
2. Warnings only, all from the stateless design: no `Mcp-Session-Id`, and on the
   POST SSE stream no priming event and no `retry` field (resumability is a
   SHOULD). `server-sse-polling` also calls the `test_reconnection` fixture.
   Both pass at 2026-07-28, which has no sessions.
3. The four failures are fixtures (`test_missing_capability`,
   `test_streaming_elicitation`, `test_logging_tool`), reported as "not
   testable". `sep-2575-server-declares-prompts-in-discover` ("Servers that
   support prompts MUST declare the prompts capability in their DiscoverResult")
   passes since wayfindr-so9.13: the gateway declares only `tools` and answers
   `prompts/list` with -32601. Before, the Go SDK answered it with an empty
   list, so the suite concluded that Wayfindr handles prompts.
4. The suite marks all five custom-header checks "not testable": no tool carries
   `x-mcp-header` annotations.
5. The gateway answers every `prompts/*`, `resources/*`, and `completion/*`
   request with -32601 "method not found" (HTTP 404 at 2026-07-28), matching the
   capabilities it declares (design §7.6). These scenarios assume a server with
   prompts and resources, so they now fail by design. `prompts-list` and
   `resources-list` want a list (they passed with the SDK's empty lists before
   wayfindr-so9.13). `caching` wants caching hints on `prompts/list`,
   `resources/list`, and `resources/templates/list`; its `tools/list` check
   passes and, like every check not listed in the baseline, must keep passing.
   `sep-2164-resource-not-found` wants -32602 with the URI in `data` from
   `resources/read` and warns on -32601. `logging-set-level` still passes:
   before 2026-07-28 the SDK accepts `logging/setLevel` as a no-op although
   `logging` is not declared (see the checklist's "Things to watch").

## Client leg (upstream)

`make conformance-client` with
`@modelcontextprotocol/conformance@0.2.0-alpha.12`, first run on 2026-11-17
against `next` at `1dfd3ed` (Phase 0 of the upstream OAuth work), and again the
same day for Phase 1b (wayfindr-9fep, client authentication), whose DCR request
names `application_type`: that fixed the SEP-837 check at 2026-07-28 in every
scenario that registers a client, with no other change at either version. Phases
2 and 2b (wayfindr-sf2z, wayfindr-6xdm, unattended grants) made the three
extensions `client-credentials-basic`, `client-credentials-jwt`, and
`wif-jwt-bearer` pass at both versions; the harness maps their contexts to
`grant = "client_credentials"` (with `client_secret`, or the private key as
`signing_key`) and `grant = "jwt_bearer"` (the workload JWT as
`assertion.value`), and no other scenario changed. Re-run on 2026-11-22 for
wayfindr-n6nc: an auth decision on any request of a connect now ends it, so the
403 `insufficient_scope` on `server/discover` reaches `needs_auth` (step_up) and
`auth/scope-step-up:scope-step-up-escalation` passes at 2026-07-28; the step-up
and retry-limit scenarios run more rounds, and the blocked `initialize` no
longer runs a second discovery, one `prm-pathbased-requested` fewer in
`metadata-var2` and `resource-mismatch`. Both versions match their baselines
and the target exits 0. Cells are checks passed / failed / warned as
the suite counts them (a check repeated in one scenario counts each time). A
dash means the requirement set does not list the scenario at that version.

### Scored

| Scenario                            | 2025-11-25  | 2026-07-28      |
| ----------------------------------- | ----------- | --------------- |
| initialize                          | 1/0/0 pass  | -               |
| tools_call                          | 2/0/0 pass  | 2/0/0 pass      |
| elicitation-sep1034-client-defaults | 5/0/0 pass  | -               |
| sse-retry                           | 3/0/0 pass  | -               |
| request-metadata                    | -           | 6/0/0 pass      |
| auth/metadata-default               | 22/0/0 pass | 21/0/0 pass     |
| auth/metadata-var1                  | 22/0/0 pass | 21/0/0 pass     |
| auth/metadata-var2                  | 2/6/0 (2)   | 2/6/0 (2)       |
| auth/metadata-var3                  | 22/0/0 pass | 21/0/0 pass     |
| auth/basic-cimd                     | 17/0/1 (3)  | 16/0/1 (3)      |
| auth/scope-from-www-authenticate    | 18/0/0 pass | 17/0/0 pass     |
| auth/scope-from-scopes-supported    | 18/0/0 pass | 17/0/0 pass     |
| auth/scope-omitted-when-undefined   | 18/0/0 pass | 17/0/0 pass     |
| auth/scope-step-up                  | 31/0/1 (4)  | 32/0/2 (4), (5) |
| auth/scope-retry-limit              | 28/0/0 pass | 29/0/0 pass     |
| auth/token-endpoint-auth-basic      | 23/0/0 pass | 22/0/0 pass     |
| auth/token-endpoint-auth-post       | 23/0/0 pass | 22/0/0 pass     |
| auth/token-endpoint-auth-none       | 23/0/0 pass | 22/0/0 pass     |
| auth/pre-registration               | 15/0/0 pass | 13/0/0 pass     |
| auth/resource-mismatch              | -           | 3/0/0 pass      |
| auth/offline-access-scope           | -           | 17/0/0 pass     |
| auth/offline-access-not-supported   | -           | 17/0/0 pass     |
| auth/authorization-server-migration | -           | 31/0/0 pass     |
| auth/iss-supported                  | -           | 17/0/0 pass     |
| auth/iss-not-advertised             | -           | 17/0/0 pass     |
| auth/iss-supported-missing          | -           | 10/0/0 pass     |
| auth/iss-wrong-issuer               | -           | 10/0/0 pass     |
| auth/iss-unexpected                 | -           | 10/0/0 pass     |
| auth/iss-normalized                 | -           | 16/1/0 (6)      |
| auth/metadata-issuer-mismatch       | -           | 5/0/0 pass      |
| sep-2322-client-request-state       | -           | 5/0/0 pass      |
| http-standard-headers               | -           | 3/0/0 pass      |
| http-custom-headers                 | -           | 17/1/0 (7)      |
| http-invalid-tool-headers           | -           | 12/0/0 pass     |
| json-schema-ref-no-deref            | -           | 1/0/0 pass      |
| **Total**                           | 293/6/2     | 471/8/3         |

At 2025-11-25, 15 of 18 scenarios pass. At 2026-07-28, 27 of 32 pass; before
Phase 1b it was 9, with (1) alone accounting for 18 of the 23 others.

### Not scored

The requirement sets run these and report them, but never score them. The
extension runs (`extensions/`) give the same counts as the `--requirements`
runs.

| Scenario                              | 2025-11-25  | 2026-07-28  | Why it fails                                                                                                                                                                                    |
| ------------------------------------- | ----------- | ----------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| auth/client-credentials-basic         | 10/0/0 pass | 8/0/0 pass  | Passes since Phase 2: the `client_credentials` grant with `client_secret_basic`.                                                                                                                |
| auth/client-credentials-jwt           | 10/0/0 pass | 8/0/0 pass  | Passes since Phase 2: the `client_credentials` grant with `private_key_jwt`, the context's key in `signing_key`.                                                                                |
| auth/enterprise-managed-authorization | 0/2/0       | 0/2/0       | No EMA (token exchange, jwt-bearer): the IdP token has no config.                                                                                                                               |
| auth/dpop                             | 14/0/0 pass | 15/0/0 pass | Passes since Phase 4: with `dpop = "auto"`, the resource's `DPoP` challenge and the server's ES256 choose DPoP; proofs on the token request and every MCP request, sent with the `DPoP` scheme. |
| auth/dpop-nonce                       | 14/0/0 pass | 15/0/0 pass | Passes since Phase 4: as `auth/dpop`, with one retry on each `use_dpop_nonce`, at the token endpoint and at the MCP server.                                                                     |
| auth/wif-jwt-bearer                   | 10/0/0 pass | 8/0/0 pass  | Passes since Phase 2b: the `jwt_bearer` grant, the workload JWT in `assertion.value`.                                                                                                           |
| json-schema-2020-12-preservation      | 9/0/0 pass  | 9/0/0 pass  | Passes: the catalog keeps the 2020-12 keywords `search_tools` returns.                                                                                                                          |

Notes:

1. Fixed in Phase 1b. SEP-837: the DCR request carried no `application_type` (it
   should be `native` for the loopback redirect URI). The check runs only at
   2026-07-28, once per registration. It now passes everywhere, so no row cites
   this note.
2. The PRM at the root (`/.well-known/oauth-protected-resource`) names the
   origin as its `resource`. Wayfindr rejects it because it is not the
   connector's URL, falls back to the MCP server's origin as the authorization
   server, finds no metadata or registration endpoint there, and refuses to
   start the flow ("There is no OAuth client to authorize with").
3. Warning: CIMD needs `client_metadata_url` in the config, since Wayfindr hosts
   no metadata document. The harness sets none, so Wayfindr registers with DCR.
4. Warning: an OAuth connector with no credential goes to `needs_auth` without
   sending a request, so Wayfindr never sees the 401 challenge's scope
   (`mcp:basic`) and asks for the PRM's `scopes_supported` instead
   (wayfindr-311c). The step-up itself passes: the 403 puts the connector in
   `needs_auth`, and the second authorization asks for the union.
5. Warning: at 2026-07-28 the 403 `insufficient_scope` comes on
   `server/discover`. Until wayfindr-n6nc the transport then fell back to
   `initialize` (400 at this version) and to SSE, so the connector ended
   `unreachable` and there was no second authorization. Now the step-up ends the
   connect and the escalation check passes. SEP-2350's union check warns: the
   second authorization asks for `mcp:profile mcp:write`, the union of what was
   requested and challenged, but the check wants the 401 challenge's `mcp:basic`
   in it, which Wayfindr never asked for (note 4).
6. SEP-2468: the RFC 9207 check accepts `iss` = `<issuer>/` for the issuer
   `<issuer>` and redeems the code; a trailing-slash variant must be refused.
7. `call_tool` checks arguments against the tool's `inputSchema` and refuses the
   scenario's `verbose: null` (the schema says boolean), so that call never
   reaches the server to show the null header is left out. The other 17 checks,
   the encodings included, pass.
