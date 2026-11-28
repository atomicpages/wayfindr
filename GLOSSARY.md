# Wayfindr — Domain Glossary

Use these terms consistently in code, docs, issues, and logs. If a term here is
wrong, fix it here first.

## Actors and processes

- **Wayfindr** — the product: an MCP gateway that holds credentials for many MCP
  servers and exposes them to agents through two meta-tools.
- **Daemon** — the single long-running Wayfindr process (`wayfindr start`). It
  owns all upstream connections, credentials, the catalog, and token refresh.
  There is exactly one per machine (local mode), and at most one per data
  directory, which the daemon lock enforces.
- **Daemon lock** — `<data>/daemon.lock`: an OS file lock the running daemon
  holds, with its pid and `server.listen` written in the file. A second
  daemon on the data directory refuses to start, and the shim reports the
  running daemon instead of starting one. The OS drops it when the daemon
  exits, so it is never stale.
- **Client** — anything that connects _to_ Wayfindr over MCP: Claude Code,
  Codex, Cursor, and other harnesses. Never call an upstream server a client.
  In the call log, the client is the `clientInfo` name the harness sends.
  Not to be confused with what a client token resolves to on a request
  (`clienttoken.Client`: the token's name and the connectors it allows),
  which the call log records as the token name.
- **Cloud agent** — an always-on agent that runs in a vendor's cloud, not on the
  user's machine: Grok Bot, Muse (Meta), and ChatGPT. It is a client of
  Wayfindr, over HTTPS through a tunnel the user runs: Grok Bot and Muse with a
  remote client token (ADR 0021), ChatGPT with an OAuth grant from Wayfindr's
  authorization server (ADR 0022). Wayfindr has no adapter for it and
  never calls it. Say _cloud agent_, not _bot_ (Grok's word for one product) or
  _remote client_.
- **Public URL** — `server.public_url`: the `https` address the user's tunnel
  serves the daemon at. The console uses it for a cloud agent's setup text and
  to test the connection (ADR 0021). The host guard never checks a request
  against it. While `[server.oauth]` is on, it is the issuer of Wayfindr's
  authorization server (without a trailing slash), and `<public_url>/mcp` is the
  resource every OAuth grant is bound to, so changing it suspends OAuth grants
  until it is set back (ADR 0022). Not the `redirect_uri` of a
  connector, and not a listen address. Don't call it the _tunnel URL_, _external
  URL_, or _base URL_. The tunnel itself is the user's, never Wayfindr's.
- **Shim** — `wayfindr stdio`: a thin stdio↔HTTP forwarder for clients that only
  speak stdio. It holds no state and starts the daemon if it is not running.
  On every request it sends the project scope it finds (`Wayfindr-Scope`)
  and its `--profile` (`Wayfindr-Profile`). It relays elicitation to a
  client older than 2026-07-28 as `elicitation/create`, and passes the
  answers back to the daemon.
- **Daemon log** — `<data>/wayfindr.log`, where a daemon running in the
  background writes its JSON log: one that `wayfindr start` or the shim
  started, and the service units (`--log-file`). `wayfindr logs` prints
  it; `wayfindr start --foreground` logs to stderr instead.
- **Quick connect** — the per-harness setup `wayfindr init` prints: an
  `mcp add` command for harnesses that have one (`claude mcp add … --
wayfindr stdio`), else the entry to add to the harness's config file. All
  of them go through the shim and never hold the client token.

## Connectors and tools

- **Connector** — one configured upstream MCP server: its name (the TOML table
  key), transport, and auth settings. The connector name is the stable
  identifier; renaming it is a remove plus an add.
- **Imported connector** — a connector `wayfindr connector import <client>`
  added from another client's MCP config (Claude Code, Cursor, and so on).
  The client's files are only read; literal `env` and `headers` values
  become secrets. Once added it is an ordinary connector.
- **Upstream** — the daemon's live connection to a connector. A connector exists
  in config whether or not an upstream is currently open.
- **Transport** — how an upstream is reached: `stdio` (connector has `command`),
  Streamable HTTP (connector has `url`), or legacy HTTP+SSE (fallback, or
  `transport = "sse"`).
- **Tool** — an upstream tool, always addressed by its **qualified name**
  `<connector>.<tool>` (e.g. `linear.create_issue`).
- **Tools list** — a connector's `tools = [...]` in the config: which of its
  tools agents can see and call. Entries are classes (`@read`, `@write`),
  globs on the upstream tool name, and `!glob` exclusions. A project scope
  or profile can carry its own tools lists, which only narrow. The code
  and ADR 0009 call it the _tool policy_ (`config.ToolPolicy`).
- **Tool class** — what a tool's annotations say it does: `read`
  (`readOnlyHint`), `write` (`destructiveHint` explicitly false), or
  `destructive` (everything else, including no annotations).
- **Hidden tool** — an upstream tool that something narrowing a request
  hides. The connector's tools list hides it from everyone: syncs keep it
  out of the catalog (the sync record keeps its name and class), and
  `call_tool` refuses it as `hidden_by_policy`. A profile or project scope
  hides it only from the requests that carry them, as `hidden_by_profile`
  or `hidden_by_scope`. `search_tools` returns none of them, and a lookup
  by `names` lists them under `missing`.
- **Project scope** — a `.wayfindr.toml` in a project directory that narrows
  what the config exposes to agents working there: an optional `connectors`
  allowlist and per-connector tools lists, ANDed with the tools list. It can
  never add a connector, widen a list, or reach past the client token. The
  shim finds the nearest one below the home directory and sends it on every
  request as the `Wayfindr-Scope` header; `call_tool` refuses what it hides
  as `hidden_by_scope` (ADR 0010).
- **Profile** — a named project scope in the config: a `[profiles.<name>]`
  table with the same keys as `.wayfindr.toml`. A client picks one by name,
  with `wayfindr stdio --profile <name>` (the `Wayfindr-Profile` header) or
  `/mcp?profile=<name>`, so it also works for clients that skip the shim.
  The daemon resolves the name on every request against the running config,
  and ANDs it with the tools list and any project scope file, within what
  the client token allows. An unknown name is refused as `unknown_profile`;
  `call_tool` refuses what it hides as `hidden_by_profile` (ADR 0011).
- **Narrowing** — everything that limits one request, checked in this
  order: the client token's connectors, then the profile, then the project
  scope file, each ANDed, and then, for tools, the connector's tools list
  (`config.Narrowing`, built per request by the gateway). Each only
  narrows. A connector the token leaves out is refused as not configured
  (`no_connector`), so the client can't tell it exists; a tool the others
  hide is refused as `hidden_by_policy`, `hidden_by_profile`, or
  `hidden_by_scope`.
- **Meta-tools** — the only two tools Wayfindr exposes to clients:
  `search_tools` and `call_tool`.
- **Elicitation** — an upstream asking the user something during a tool
  call: a form to fill in, or a URL to open. Wayfindr relays it to the
  client (ADR 0008): as an `input_required` result of `call_tool`, which
  the client answers by calling again, or, through the stdio shim, as an
  `elicitation/create` to an older client. Sampling, an upstream asking the
  client's model to write something, is declined.
- **Interaction** — a question a 2025-x upstream sent as `elicitation/create`
  during a call, parked by `upstream` until the user's answer arrives, for at
  most 10 minutes. The upstream call waits meanwhile, with the connector's
  timeout paused. It is accepted only while its connector has a single call
  in flight, since nothing in the request names the call.
- **Request state** — the opaque `requestState` of a `call_tool`
  `input_required` result. The gateway seals it with the master key and binds
  it to the user, the client token the call came with (that instance of it: a
  token created again under a revoked one's name is another), the tool, its
  arguments, and a 10-minute expiry, so an answer is applied only to the call
  that asked.

## Catalog and search

- **Catalog** — the daemon's cached, persisted copy of every connector's tool
  definitions. Search runs against the catalog, never against live upstreams.
- **Catalog entry** — one tool's full definition (`name`, `title`,
  `description`, `inputSchema`, `outputSchema`, `annotations`) plus its content
  hash and, optionally, its embedding.
- **Content hash** — hash of the text that gets embedded. An entry is
  re-embedded only when its hash changes or the embedding model changes.
- **Sync** — fetching a connector's tool list and reconciling it into the
  catalog, leaving out the tools its tools list hides. Triggered by every new
  upstream session (startup, a config change, recovery after auth, a stdio
  spawn, a reconnect), by an upstream `list_changed`, and by the poll of
  HTTP connectors. The poll runs every `refresh_interval`, unless the
  upstream's `tools/list` carried a `ttlMs` cache hint, which replaces it
  for that connector (bounded to 1 minute–24 hours).
- **Hybrid search** — one ranked result list fused (reciprocal-rank fusion) from
  fuzzy/full-text matches and embedding-similarity neighbors. With no embedding
  provider configured, hybrid search degrades to fuzzy-only.
- **Brief entry** — a compact search result (`detail: "brief"`): qualified
  name, title, one-sentence summary, required argument names, and annotations,
  without schemas. The agent fetches full definitions with `names`.
- **`unavailable`** — the list in every `search_tools` result of the
  connectors in reach that are not `ready`, each with its state and advice
  (summary, action, link), so the agent learns why a connector's tools are
  missing and how to fix it.
- **`missing`** — the list in a `search_tools` lookup by `names` of the
  names it could not return: not qualified, on a connector not configured
  for this client, not in the catalog, or hidden. Finding none is an
  error instead.
- **Score** — why a search result ranked where it did: its lexical rank and
  BM25F score, its vector rank and cosine, and the fused RRF score
  (`store.Score`). Only `wayfindr search --explain` asks for it (the admin
  API's `explain`); agents never see scores.
- **Embedding provider** — any OpenAI-compatible `/v1/embeddings` endpoint.
- **Store** — the persistence backend. Exactly two: SQLite (default) and
  Postgres. Each store implements hybrid search itself.
- **Call log** — the daemon's record of every `call_tool` call, kept in the
  store for `[calls] retention`: when it arrived, the client, the name of
  the client token it authenticated with, the qualified name as the agent
  wrote it, how long the upstream took, and its result (`ok`,
  `tool_error`, `input_required`, `rejected`, or `failed`) with a Kind and
  Case, and the error text the agent got back. Never arguments or results.
  `wayfindr calls` shows it.
- **Usage report** — the opt-in daily report the daemon sends the Wayfindr
  maintainers when `[telemetry] enabled` (ADR 0020): a random install ID,
  build and platform, connector counts by transport and auth kind, and
  `search_tools` and `call_tool` counts, with client and embedding names
  only from an allowlist. Never connector or tool names, URLs, arguments,
  or results. Also called telemetry.

## Auth and secrets

- **Credential** — what the daemon presents to an upstream: OAuth tokens, static
  headers (API keys), or nothing. Clients never see credentials. A stored
  OAuth credential is one per connector: its token, and the client Wayfindr
  registered for it through DCR, if any. `wayfindr auth logout <connector>`
  deletes both and sends nothing to the authorization server. A connector
  with an unattended grant has no stored credential: revoke forgets its
  in-memory token instead, and deletes any stored credential left from an
  earlier auth flow.
- **Client binding** — a stored OAuth credential belongs to the OAuth
  client that got it: the row records that `client_id`. When a connector's
  configured `client_id` changes, its old token is never sent; the
  connector needs auth with Case `client_changed`. Only a configured
  `client_id` is checked: a DCR client is Wayfindr's own and may change on
  re-registration, and a row from before the binding has no client
  recorded and is accepted.
- **Token endpoint binding** — a stored OAuth token records the token
  endpoint it was obtained at, and a refresh spends it only there. When
  discovery (which follows what the MCP server names) or the config now
  gives another token endpoint, the refresh token is never sent; the
  connector needs auth with Case `authorization_server_changed`. A token
  from before the binding records none and needs auth too. Unattended
  grants store no token; their config must name the authorization server
  instead (`authorization_server` or `token_endpoint`).
- **Orphaned credential** — a stored OAuth credential whose connector the
  running config no longer names. Removing a connector keeps its credential
  (ADR 0006), so re-adding it needs no new auth flow. `wayfindr status`
  lists orphans until `wayfindr auth logout <connector>` deletes them.
- **Secret** — a sensitive value stored encrypted in the store, referenced from
  config as `${secret:<name>}`. Environment values are referenced as
  `${env:<NAME>}`.
- **Client token** — a bearer token clients present to the daemon: the default
  client token or a scoped client token. The daemon resolves it to a client on
  every request, and it is the outer limit of what that client can use: a
  profile or project scope only narrows within it (ADR 0012). An OAuth grant's
  access token is presented the same way and resolves as a remote client token
  does (ADR 0022). Not to be confused with upstream OAuth tokens.
- **Default client token** — the client token `wayfindr init` generates
  (`wf_…`, in `<data>/client-token` or `WAYFINDR_CLIENT_TOKEN`). It can use
  every connector and the admin API. Its name, in the call log, is
  `default`.
- **Scoped client token** — a named client token limited to a list of
  connectors, made with `wayfindr token create` and printed once
  (`wf_s_…`). Only its SHA-256 is stored. A connector it doesn't allow is as
  if not configured, checked before the profile, project scope, and tools
  list, and the admin API refuses it with 403. Its connectors can't be
  edited: revoke it and create another (ADR 0012).
- **Remote client token** — a scoped client token created with `--remote` (or in
  the console's Cloud agents page), for a cloud agent. Only it, or an OAuth
  grant's access token (ADR 0022), which resolves to the same kind of
  client, lets a remote request reach `/mcp`. In every other way it is a scoped
  token: it reaches only its connectors, the admin API refuses it, and it works
  from the local machine too. `remote` is stored with the token's digest, is
  fixed at creation, and can't be set on the default client token; to change it,
  revoke the token and create another (ADR 0021). Shown as `REMOTE` by
  `wayfindr token list`. Don't call it a _tunnel token_ or _public token_.
- **Remote request** — a request that carries a forwarding header (`Forwarded`,
  `Via`, `X-Forwarded-*`, `X-Real-IP`, `CF-Connecting-IP`), or whose `Host`
  isn't a loopback name with the daemon's port. The host guard decides alone
  (`httpd`). On `/mcp` it proceeds only with a remote client token or an OAuth
  grant's access token; any other token, or none, gets `404 page not found`, or,
  while `[server.oauth]` is on, `401` with
  `WWW-Authenticate: Bearer resource_metadata="…"` (ADR 0022). Every
  other route but the callback path, and the authorization server's routes while
  it is on, refuses it with 404, whatever token it carries. A tunnel that adds
  no forwarding header and keeps a loopback `Host` isn't seen as remote
  (wayfindr-gz1y). The opposite of a _local request_ (ADR 0021).
- **Connection test** — what the console's Cloud agents page runs after it
  creates a remote token: `POST /admin/remote/test` sends MCP requests to the
  public URL with the token, and again with none, the way a cloud agent would.
  For ChatGPT, which has no token, it instead checks the 401 challenge and both
  metadata documents through the public URL (ADR 0022). Its outcome
  (`ok`, `not_found`, `proxy_treats_remote_as_local`, `metadata_missing`, …) is
  a closed set in `admin`; match on it, never on the message.
- **Discovery chain** — the ordered metadata lookups (protected-resource
  metadata, then authorization-server metadata, then legacy fallback) used to
  find an upstream's OAuth endpoints. See ADR 0007.
- **Lazy refresh** — access tokens are refreshed only when a call needs them and
  they are expired or within 60s of expiry, or after an upstream 401. Never on a
  background timer. An unattended grant is never refreshed: it runs again at
  the same moments.
- **Step-up** — re-authorizing with a larger scope set after an upstream returns
  403 `insufficient_scope`. For an unattended grant, the grant runs once more
  with the larger set instead.
- **OAuth client** — the identity Wayfindr has at a connector's
  authorization server: a configured `client_id`, a CIMD URL, or a DCR
  registration. Not an MCP client, not a client token, and not an allowed
  client of Wayfindr's own authorization server.
- **Client authentication** — how Wayfindr proves it is that OAuth client
  at the authorization server's token endpoint: none (the client only
  names itself), a client secret (basic, post, or JWT), a private key JWT,
  or a client certificate. Each connector uses one: the method its config
  names, else the one its DCR client was registered with, else the first
  the server supports that fits its credential. A client certificate is
  never chosen unless named.
- **Signing key** — a connector's private key, which Wayfindr signs JWTs
  with: for private key JWT client authentication, and for self-signed
  assertions. With a JWT bearer grant it signs the assertion, and
  authenticates the client only when private key JWT is named. Stored as a
  secret; the provider holds only its public key.
- **Assertion** — the JWT a JWT bearer grant presents. Either a **workload
  token** read from a file or variable (issued by Kubernetes, a CI system,
  or a cloud), or one Wayfindr signs with the signing key.
- **Sender constraint** — what binds a token to Wayfindr so a stolen copy
  is useless: none (bearer), a DPoP key, or a client certificate.
- **DPoP** — a sender constraint with a key per grant. Every token request
  and every upstream call carries a fresh **DPoP proof** signed with it.
  Servers may demand a **DPoP nonce** in the proof. An auth flow's key is
  kept with its stored token; an unattended grant's lives in memory with
  its token. A token the authorization server didn't bind is sent as a
  bearer token.
- **Client certificate** — a connector's TLS identity (`[tls]`), presented
  on both the authorization server and the MCP server connections. It
  serves as client authentication, as a sender constraint, or as the only
  credential of a server that takes mutual TLS. A connector's `[tls]` table
  may also name a private CA to trust, with or without a client
  certificate.

## Auth flow

- **Grant** — how a connector gets a token: an auth flow or an unattended grant.
  A refresh renews a grant's token and is not a grant. Not an OAuth grant, which
  Wayfindr's authorization server gives a cloud agent (ADR 0022).
- **Auth flow** — a grant the user takes part in. _Authorization code_: the
  user opens the authorize URL and consents, and the browser comes back to
  the callback path, where the code is exchanged. _Device code_: the user
  enters a **user code** at the provider's **verification URI** on any
  device, while the daemon polls. Either way the token is stored as the
  connector's credential. It has two doors, `wayfindr auth login <connector>`
  (the admin API) and the auth link, and lasts at most 10 minutes. It reads
  `pending`, `done` (token stored, and the connector's recovery settled),
  `failed`, or `expired`. Never call a token refresh an auth flow.
- **Unattended grant** — a grant with no user. _Client credentials_: the
  OAuth client's own access. _JWT bearer_: an assertion stands in for the
  user or workload. Wayfindr gets the token itself whenever a call needs
  one and holds it in memory only: it is never stored, and such a connector
  has no stored credential. The connector is never `needs_auth`: a refused
  grant is `auth_error`, fixed in config or on the provider's side. A
  step-up re-runs the grant once with the larger scope set.
- **Flow entry** — `auth.Flows`, the one way into an auth flow for both
  doors. It looks the connector up in the running config, checks that it
  uses OAuth, starts the flow, and decides the HTTP status and advice of a
  flow that cannot start, so both doors answer alike.
- **Auth link** — `http://<listen>/auth/<connector>` (`Broker.Link`), the
  page that starts a connector's auth flow. Every `needs_auth` error and
  status carries it. Opening it shows a "Continue" page; a plain GET never
  starts a flow.
- **Flow nonce** — the single-use value in the auth link's "Continue" form.
  Starting the flow spends it; it is bound to the connector and expires
  with the flow. Not the OAuth `state`.
- **Callback path** — `/oauth/callback` (`auth.CallbackPath`), where the
  authorization server sends the browser back. The only path served on a
  non-loopback `Host` with no token, for a `redirect_uri` through a tunnel (ADR
  0007), besides the authorization server's routes while `[server.oauth]` is on
  (ADR 0022); `/mcp` is served there only for a remote client token or
  an OAuth grant's access token (ADR 0021). Not `/oauth/authorize` or
  `/oauth/token`, which are Wayfindr's own authorization server's.
- **Flow phases** — how far a pending flow has got. _Starting_: in the flow
  table from the first moment, so a revoke or a reload can end it, but no
  callback can find it yet. _Armed_: it has its authorize URL and `state`,
  and its callback can find it; a device code flow has its user code, and
  its polling has begun. _Granted_: its callback's token passed every
  check and is being stored; from then on only a revoke ends it.
  _Outdated_: its connector was removed, or its `url`, `[auth]` table, or
  `[tls]` table changed since it started, so its token would be bound or
  granted under the old settings (the old url, client, or client
  certificate); it ends as `flow_cancelled`.
- **Recovery** — what follows "the user fixed it" (a token stored, a
  credential revoked, a secret set): the daemon reconnects the affected
  connectors and, for each that comes back `ready`, waits for the catalog
  sync its new session started. A connector has _recovered_ once it has
  reconnected and, if ready, synced once, so it is reported with its tools.

## Wayfindr's authorization server

The terms of ADR 0022. Its limits are in design §7.11.

- **Wayfindr's authorization server** — the OAuth authorization server in
  the daemon (`oauthserver`, ADR 0022), for cloud agents that sign in with
  OAuth instead of sending a token, such as ChatGPT. Off unless
  `[server.oauth] enabled`; it then serves its metadata documents,
  `/oauth/authorize`, and `/oauth/token` through the tunnel, with the public
  URL as its issuer. Everywhere else in this glossary, an _authorization
  server_ is a connector's; say _Wayfindr's authorization server_ for this
  one.
- **Allowed client** — a client ID that may start an authorization at
  Wayfindr's authorization server: a Client ID Metadata Document URL listed
  in `[server.oauth] clients`, matched exactly (by default ChatGPT's,
  `https://chatgpt.com/oauth/client.json`). Any other `client_id` gets an
  error page before the daemon fetches anything. Not an _OAuth client_, which
  is Wayfindr's own identity at a connector's authorization server.
- **Arm** — what the console's Connect button does to Wayfindr's
  authorization server: for a few minutes it keeps the authorization requests
  that pass its checks pending, until the user's signed-in approve page
  _claims_ one, which uses the arm up and drops the others. Unarmed,
  `/oauth/authorize` answers an error page and fetches nothing. Only one
  request is expected per arm: when more arrived, the claimed one can't be
  approved, only denied. Turning OAuth off or on, or changing `public_url` or
  the allowed clients, _disarms_ it, dropping what it kept. Say _arm_ or
  _Connect_, not _pairing mode_.
- **Pending authorization** — an authorization request the public
  `/oauth/authorize` checked, while armed, and keeps in memory, for a few
  minutes and at most 8 at a time, until the console's approve page claims
  one (the others are dropped) and the user approves or denies it there. The
  page shows when it started and the IP the tunnel reported. The browser reaches that page only by the redirect from
  `/oauth/authorize`, on the daemon's loopback address; the console lists no
  pending authorizations. The Go type is `oauthserver.Request`. Don't call
  it a _pending request_ or a _consent request_.
- **OAuth grant** — a cloud agent's access to Wayfindr, which the user
  approved on the console's approve page, limited to the connectors picked
  there and bound to the resource `<public_url>/mcp`. It is a client token to
  the user: its name is in the name space scoped client tokens use, its
  access token resolves to a remote scoped client, and `wayfindr token list`
  shows it as kind `oauth` with the client's host. `wayfindr token revoke`
  deletes it with its tokens. Its access tokens expire within the hour; its
  refresh token rotates on every use and never expires; a spent code or
  refresh token presented again revokes the grant. It is _suspended_, its
  tokens refused until the change is undone, while the authorization server
  is off, `public_url` differs from the one it was approved under, or its
  client is not in `clients`. Not a _grant_, which is how a connector gets
  its token. Don't call it an _OAuth token_ or _OAuth connection_.

## Connector states

Exactly one at a time; shown by `wayfindr status` and in `search_tools`'
`unavailable` list.

| State         | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                          | Fixed by                                                                                                                                                                                               |
| ------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `starting`    | Configured, first connect/sync not finished yet (startup, add, or reconfigure).                                                                                                                                                                                                                                                                                                                                                  | Resolves on its own.                                                                                                                                                                                   |
| `ready`       | Synced and callable.                                                                                                                                                                                                                                                                                                                                                                                                             | —                                                                                                                                                                                                      |
| `needs_auth`  | An auth flow's connector with no valid credential: never authorized, refresh failed, `url` or `client_id` changed, step-up required, revoked, or a freshly refreshed token refused. Never an unattended grant's.                                                                                                                                                                                                                 | User runs the auth flow; the daemon then reconnects the connector at once (recovery).                                                                                                                  |
| `auth_error`  | Auth cannot proceed as configured (no PKCE, no `client_id` and no DCR, issuer mismatch, no device code support), a `${secret:…}`/`${env:…}` value is missing, an unattended grant was refused, the client authentication method isn't supported, the signing key or client certificate can't be used, the authorization server's certificate isn't trusted, or the sender constraint can't be met (DPoP refused or unsupported). | User changes config, or sets the missing value (`wayfindr secret set`); the daemon then reconnects the connector at once. A grant fixed on the provider's side is retried with `wayfindr auth logout`. |
| `unreachable` | Transport-level failure (spawn failed, connection refused, timeout).                                                                                                                                                                                                                                                                                                                                                             | Retries automatically on next sync or call.                                                                                                                                                            |

## Errors

- **Kind** — the coarse class of a failure at a package seam (`needs_auth`,
  `missing_value`, `unreachable`, …). Each package owns its own closed set;
  callers match on it, never on error text.
- **Case** — the finer reason within a kind (`token_revoked`, `step_up`,
  `provider_error`, …). Also a closed set per package.
- **Kind `request`** — `auth`'s kind for an auth flow asked for a connector
  it cannot authorize. Says nothing about the connector's state; both
  doors answer 404. Cases:
  - `unknown_connector` — the running config has no connector of that
    name. (In `upstream`, `unknown_connector` is a Kind of its own.)
  - `not_oauth` — the connector has no `[auth]` table of type `oauth`.
  - `not_interactive` — the connector uses an unattended grant, which has
    no auth flow.
- **Kinds `conflict`, `unsafe`, and `invalid`** — `config`'s kinds for a
  refused edit of the config file by `wayfindr connector add`, `edit`, or
  `remove`; the file is left as it was. `conflict` Cases:
  `connector_exists`, `connector_not_found`, `edit_not_found`,
  `key_not_set`. `unsafe` Cases: `unparsable`, `inline_connectors`,
  `not_tables` (a quoted or dotted key), `defined_elsewhere`,
  `reads_differently`, `dotted_keys`, `multiline`, `changes_others`.
  `invalid` Cases: `nothing_to_change`, `key_twice`.
- **Auth-flow Cases** — the reasons an auth flow fails or cannot start:
  - `pkce_unsupported` (`auth_error`) — the connector has no `client_id`
    and its authorization server does not advertise PKCE S256 (ADR 0007).
    Fixed by registering an app and setting `client_id`.
  - `authorization_denied` (`needs_auth`) — the authorization server
    answered with an error, such as `access_denied` when the user declined
    (in the browser, or for a device code); its provider code is kept.
  - `device_unsupported` (`auth_error`) — a device code flow, and the
    authorization server names no device authorization endpoint and config
    sets none.
  - `authorization_pending`, `slow_down` — a device code flow's user hasn't
    entered the code yet, or the server asks to be polled less often. The
    flow keeps polling; neither ends it.
  - `flow_expired` (`needs_auth`) — the browser did not come back within
    the flow's 10 minutes, or the device code expired first.
  - `flow_cancelled` (`needs_auth`) — a revoke, or a config change that
    outdated the flow, ended it before it stored a token.
  - `callback_issuer_mismatch` (`needs_auth`) — the callback's `iss` failed
    the RFC 9207 check, a possible mix-up attack, so the code is not
    exchanged. Unlike the discovery-time `issuer_mismatch`, no config change
    fixes it.
- **Client authentication, TLS, and DPoP Cases** — all `auth_error`, found
  for any grant, most before anything is sent:
  - `auth_method_unsupported` — the server lists no client authentication
    method the client's credential can do, or the named method needs a
    credential the client doesn't have.
  - `signing_key_invalid` — the signing key isn't a private key Wayfindr
    can sign with, or can't sign the named algorithm.
  - `tls_untrusted` — the authorization server's certificate isn't trusted,
    or the connector's CA file can't be used.
  - `tls_handshake` — the TLS handshake with the authorization server
    failed, such as over a refused client certificate, or the client
    certificate can't be loaded.
  - `dpop_alg_unsupported` — the connector always uses DPoP, and the
    server accepts no proof Wayfindr can sign.
  - `dpop_rejected` — the authorization server rejected a DPoP proof,
    usually because the host's clock is off. One the MCP server rejects
    reads as `token_rejected` or `grant_rejected`.
- **Unattended-grant Cases** — the reasons an unattended grant fails, all
  `auth_error`, since no auth flow can fix them:
  - `grant_rejected` — the authorization server refused the grant
    (`invalid_grant`, `invalid_scope`, `unauthorized_client`, or a code
    Wayfindr does not know; its provider code is kept), or the upstream
    rejected a token fresh from it. A refusal is final: no retry with the
    same assertion, and no other grant to fall back to.
  - `scope_denied` — the upstream answered `insufficient_scope` to a token
    from a grant that already asked for every scope it names.
- **Verdict** — `upstream`'s single decision about what a use's outcome
  means: the connector state event, whether to drop the session, and the
  typed error the caller gets.
- **Advice** — a domain's user-facing reading of one of its errors: a summary,
  the action that fixes it, and an optional link. Produced next to the error
  by the domain that owns it; the gateway only renders it.
- **Warning** — advice about a failure that hasn't happened yet: a connector
  that will break unless the user acts (`no_refresh`, `refresh_expiring`,
  `registration_expiring` from `auth`; `certificate_expiring`, a client
  certificate that ends within 7 days, from the daemon; `stuck` from
  `upstream`). Shown in
  `wayfindr status`, logged once per transition, and given to the agent in
  `search_tools` when due within a day. A **note** is a `no_refresh` more than
  a day out. Warnings only read; they never refresh or re-authorize.
- **Provider code** — the raw error code an authorization server returned
  (e.g. Slack's `invalid_refresh_token`), kept sanitised alongside the case so
  errors stay useful for providers Wayfindr hasn't catalogued.
- **Inspector** — a per-provider rule that reads a token-endpoint response
  before the standard parser, for servers that bend RFC 6749 (e.g. HTTP 200
  with `"ok": false`).
