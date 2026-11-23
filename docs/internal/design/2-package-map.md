# §2 Package map

Part of the [Wayfindr technical design](../design.md). Go code cites this file
as "design §2".

```
cmd/wayfindr/                   main(): calls cli.Execute()
internal/cli/                   cobra commands: parse flags, then call admin.Client, setup.Init, daemonctl, selfupdate, the shim, or config's file editor; no domain logic
internal/setup/                 wayfindr init: default config, master key, store + local user, client token; the report with quick-connect commands and importable clients
internal/clientconfig/          connector import: read other clients' MCP configs (JSONC, TOML), plan their entries as connectors and secrets
internal/clienttoken/           client tokens: Read (WAYFINDR_CLIENT_TOKEN over <data>/client-token), Generate, Digest; Client (what a token resolves to) and its TokenInfo round trip; Registry of scoped tokens and OAuth grants' access tokens (AddUntil: with an expiry)
internal/config/                types, strict decode, defaults, validation, ${…} templates, Diff; tools lists (ToolPolicy); project scopes, profiles, Narrowing; the file editor (AppendConnector, EditConnector, RemoveConnector, SetPublicURL, SetOAuthEnabled); ListenAddress; Server.Issuer
internal/secrets/               master-key sourcing, Sealer (AES-256-GCM)
internal/resolve/               config.Resolver: ${env:…} from the environment, ${secret:…} from sealed store rows; PutSecret seals one
internal/store/                 Store interface, row types, PlanCatalog, migration runner
internal/store/sqlite/          persistence + in-memory search index
internal/store/postgres/        persistence + SQL hybrid search
internal/store/postgres/pgtest/ tests only: a throwaway database per test on WAYFINDR_TEST_PG_DSN
internal/store/storetest/       shared Store behavior suite that every driver's tests run
internal/store/golden/          search golden fixture: tool definitions, queries, expectations, vectors (§10)
internal/store/parity/          cross-driver search parity tests on the golden fixture (tests only, §10)
internal/store/drivers/         Open(ctx, driver, dsn): the one storage.driver switch, shared by setup and daemon
internal/httpclient/            a url connector's HTTP client: For builds one from its [tls] table (private CA, client certificate re-read on change); Clients caches one per connector; PublicOnly, a client whose dialer refuses non-public addresses
internal/embed/                 OpenAI-compatible /v1/embeddings client, model identity
internal/catalog/               Syncer: tool list → entries (the tools list applied), embed text, content hash, embedding pass
internal/auth/                  Broker: discovery, registration, token source, RoundTripper, credential health; Flows: the one auth-flow entry; Pages; Probe
internal/oauthserver/           Wayfindr's own authorization server (ADR 0022): metadata documents, authorize, pending authorizations, codes, the token endpoint, CIMD fetch; Approve/Deny for the console
internal/upstream/              Manager: connector lifecycle, state machine, transports, Call/Continue/ListTools/PeekTools, elicitation
internal/gateway/               client-facing MCP server: search_tools, call_tool, request narrowing, schema validation, input_required round trip, call recording
internal/httpd/                 listener, routes, Host/Origin/bearer guards (handlers are injected)
internal/admin/                 the /admin API: routes, handlers, request/response types, and the Client the CLI calls (NewLocalClient: in process)
internal/webui/                 the /ui web console: pages over admin.Client, login codes and sessions, embedded templates and assets
internal/daemon/                wiring, config watcher and reload, poll, recovery, call-log writer, health check, client-token registry, usage-report schedule, shutdown
internal/shim/                  wayfindr stdio: stdio MCP server forwarding to /mcp with scope and profile headers; relays elicitation; starts the daemon on demand
internal/daemonctl/             the daemon from outside: Ensure (start detached), Stop (SIGTERM the lock holder), Tail (the daemon log)
internal/daemonlock/            the daemon lock: OS file lock in the data dir naming its holder (daemon takes, daemonctl probes)
internal/service/               wayfindr service install/uninstall: launchd agent or systemd user unit; Restart (reload an installed unit)
internal/selfupdate/            wayfindr update (ADR 0023): latest release tag, version comparison, download + checksums.txt check, extract, in-place binary swap
internal/telemetry/             the opt-in usage report (ADR 0020): Report schema, allowlists, proof-of-work, Reporter (counts, install ID, POST)
internal/paths/                 XDG-style paths for config, data, token, key file, lock, log, telemetry ID
internal/redact/                URL query/fragment redaction for errors, logs, and stored text (§9); Printable for upstream text shown in a terminal
internal/toolsonly/             MCP receiving middleware: -32601 for prompts/resources/completion (gateway and shim)
internal/newfile/               file writes: create-only (O_EXCL, pinned mode, removed on failure) and atomic replace (same-dir temp, fsync, rename, follows symlinks)
internal/testutil/              fakeupstream (stdio helper, Streamable, SSE; per protocol version), fakeas, fakekeychain,
                                cmd/conformancekit (`make conformance` helper; never in the binary)
```

The dependencies point one way, checked against
`go list -f '{{.ImportPath}} {{.Imports}}' ./internal/...`. The trunk is
`cli → daemon → {httpd, admin, gateway, upstream, catalog, auth, oauthserver} → {store, secrets, embed, config}`.
In the middle row only two imports go sideways, both into `upstream`:
`gateway → upstream` (it calls the manager and renders its errors) and
`admin → upstream` (its rows carry `upstream.State` and the manager's statuses).
`auth`, `upstream`, `catalog`, `oauthserver`, and `httpd` never import one
another, and none imports `admin` or `gateway`: `upstream` matches auth's errors
by behaviour, and `catalog` matches upstream's the same way
([§7.3](7.3-auth.md), [§7.4](7.4-catalog.md)). `daemon` wires them. The rest of
the middle row's imports go down:
`gateway → {store, secrets, config, clienttoken, toolsonly, redact}` (`secrets`
seals request state, [§7.6](7.6-gateway.md)),
`auth → {store, secrets, config, redact, httpclient}`,
`oauthserver → {store, config, clienttoken, redact, httpclient}` (it writes
grants and tokens, and the registry, only through its `Grants` interface, which
`daemon`'s token service implements; [§7.11](7.11-oauthserver.md)),
`catalog → {store, embed, config, redact}`,
`upstream → {config, redact, httpclient}`,
`admin → {store, config, clienttoken}`, and `httpd → clienttoken` alone.
`webui → {admin, store, upstream}` sits beside them: it reads only through
`admin.Client` (the daemon passes `admin.NewLocalClient`) and imports `store`
and `upstream` for the state and result types the admin bodies carry. Nothing
but `daemon` imports `webui`; `admin` gets its login codes through
`Options.UILogin`, a function `daemon` wires to `webui.Sessions`. Nothing but
`daemon` imports `oauthserver` either: `admin` reaches its pending
authorizations through `Options.OAuth` (`admin.OAuthRequests`), which `daemon`
adapts, and `httpd` gets its four handlers in `Handlers` and its on/off state as
`Config.ResourceMetadataURL`, a function.
`daemon → resolve → {config, store, secrets}` keeps those three free of each
other. `daemon → telemetry → {config, store, paths, newfile}` (and
`github.com/google/uuid` for the install ID): only `daemon` and `setup` import
`telemetry`; `setup` takes `DoNotTrack` from it, so `init` does not ask about
telemetry when `DO_NOT_TRACK` already keeps it off (a source build still asks:
the choice is saved, though that build never sends). The reporter reads the
running config through a function `daemon` passes in, and learns of calls and
searches through the callbacks `daemon` wires: a `gateway.Calls` that counts
each call before the call log records it, and `gateway.Options.OnSearch`
([§7.8](7.8-daemon.md)).

The other paths from `cli`:

- `init`:
  `cli → setup → {config, paths, secrets, newfile, store/drivers, clienttoken, clientconfig, telemetry}`
  (`clientconfig` to name the clients the report says can be imported).
- `connector import`: `cli → clientconfig → config`.
- `wayfindr stdio`:
  `cli → shim → {config, daemonctl, paths, clienttoken, toolsonly}`. `config`
  gives it `LoadScope`, the scope and profile headers, and `ListenAddress`. It
  reaches the daemon only over HTTP, never through an import.
- `start`, `stop`, and `logs`:
  `cli → daemonctl → {config, paths, clienttoken, daemonlock}`
  (`clienttoken.WithoutScoped`, [§3](3-paths.md)). `daemonctl` starts the daemon
  as a detached process and stops it by the pid in the lock, so it never imports
  `daemon` either. `start --foreground` also imports `daemonlock`, to match its
  `HeldError` (another daemon runs, which is not a failure; wayfindr-p9vn).
- `wayfindr service`: `cli → service → paths`.
- `wayfindr update`: `cli → {selfupdate, service, daemonctl}`. `selfupdate`
  fetches, checks, and swaps the binary; `cli` then reloads an installed unit
  with `service.Restart` or stops a running daemon with `daemonctl.Stop`, so the
  new binary runs without `cli` importing `daemon` ([§7.9](7.9-cli.md)).
- `connector add`, `edit`, and `remove` edit the file with
  `config.AppendConnector`/`EditConnector`/`RemoveConnector` and write it with
  `newfile.Replace`. `cli` imports `auth` only for `auth.Probe`, add's check of
  whether a server uses OAuth, the same probe discovery uses, and `httpclient`
  for the client that probe goes through (the new connector's `[tls]` table);
  `upstream` and `store` only for their state, Kind, and row types, which the
  admin API's bodies carry.

`store/drivers → {store/sqlite, store/postgres, config}` sits above the drivers,
so `store` never imports them; `setup` and `daemon` open their store through it.
`store/storetest → {store/sqlite, store/postgres, store/postgres/pgtest}` and
`store/golden → catalog` (the embed text) are test support.

Both doors into the auth flow, the browser's `/auth/{c}` pages and the admin
API's `POST /admin/auth/{c}`, go through one `auth.Flows`: `daemon` passes it
the running config's lookup, builds the pages on it, and adapts it to
`admin.Flows` by converting types only. `auth` also owns the auth link
(`Broker.Link`, which `daemon` passes to `upstream` as `Options.AuthURL`) and
the callback path (`auth.CallbackPath`, which `daemon` passes into
`httpd.Config`), so each is defined once. `daemon` builds `admin`'s one handler
(`admin.New`) from narrow interfaces over the manager and the store plus a few
values and functions, and `httpd` mounts it at `/admin/`. `cli` imports `admin`
for `admin.Client`, so each admin path and JSON body is written once, in
`admin`, for both ends. `admin` stores, deletes, and lists secrets through functions
`daemon` passes in (the resolver's `PutSecret`, the store's `DeleteSecret` and
`ListSecrets`), so
it imports neither `secrets` nor `resolve`; `resolve` owns a user secret at
rest, both sealing it and opening it.

Leaves: `redact`, `newfile`, and `paths` use only the standard library;
`newfile` and `redact` may be imported by any package. `daemonlock` is a leaf
too (the standard library and `golang.org/x/sys`): `daemon` takes the lock and
`daemonctl` probes it, so the shim and the CLI learn of a running daemon without
importing `daemon`; `cli` uses it only for `HeldError`. `selfupdate` is a leaf
as well (the standard library, `redact` for the signed download URLs in its
errors, `golang.org/x/mod` for semver, and `golang.org/x/sys` for the Rosetta
check), and only `cli` imports it. `toolsonly` imports only
the MCP SDK; `gateway` and `shim` both use it, so the two servers refuse
undeclared capabilities the same way without the shim importing `gateway`.
`httpclient` imports only the standard library and `config`. It builds the HTTP
client for a url connector's traffic, to its MCP server and to its authorization
server alike, from the connector's `[tls]` table ([§4](4-configuration.md)), so
both legs present the same client certificate and trust the same CAs. `auth`
(`BrokerOptions.Clients`, and its Cases for `ErrCAFile`), `oauthserver`
(`ErrNotPublic`; `daemon` passes it `PublicOnly()` for CIMD fetches), `upstream`
(`Options.Clients`, the base transport), `daemon` (it builds the one `Clients`
and passes it to both, the admin add's probe, and `CertificateExpiry` for the
health check), and `cli` (`connector add`'s probe) import it; it imports none of
them. `clienttoken` (over `paths`) sits at the bottom, so every reader of the
client token shares one env-over-file rule ([§3](3-paths.md)): `setup`, `daemon`
(for the `httpd` bearer, which `httpd` receives as a value), `daemonctl`, and
`shim`. No package imports one above it. Events travel upward through callbacks
that `daemon` wires up, never through imports.
