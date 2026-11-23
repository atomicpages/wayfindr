# Configuration

Every key in `wayfindr.toml`, table by table. For a walkthrough of adding
connectors, see [Connectors](../guide/connectors).

The config file lives at `~/.config/wayfindr/wayfindr.toml` unless
`WAYFINDR_CONFIG` or `XDG_CONFIG_HOME` says otherwise (see
[Paths and Environment](./paths-and-environment)). `wayfindr init` writes a
default one if there is none.

```toml
[server]
listen = "127.0.0.1:7777"

[connectors.linear]
url   = "https://mcp.linear.app/mcp"
auth  = { type = "oauth" }
tools = ["@read", "create_comment"]  # reads, plus one write

[connectors.fs]
command = "npx"
args    = ["-y", "@modelcontextprotocol/server-filesystem", "~/code"]
```

An empty file is a valid config: every table is optional, and every key has a
default or is optional.

## General rules

- **Unknown keys are errors.** A misspelled key fails the load and names the
  key, for example `connectors.linear.scope: unknown key`.
- **One load reports every problem.** Validation errors are collected and
  reported together, each with its dotted key path:
  `invalid config: <key>: <problem>; <key>: <problem>`.
- **TOML syntax and type errors** (a string where a number belongs, an unclosed
  table) are reported as the TOML parser writes them.
- **Durations** are Go duration strings: a number and a unit, such as `"300ms"`,
  `"2s"`, `"90s"`, `"1h"`, or `"1h30m"`. Units are `ns`, `us` (or `µs`), `ms`,
  `s`, `m`, and `h`. Every duration in the config must be positive, except
  [`[calls] retention`](#retention), which may be `"0s"`.
- **URLs** in `url`, `base_url`, and the `[auth]` endpoints must be absolute
  `http` or `https` URLs.

## Reloading

The daemon watches the config file and reloads it about 250 ms after the last
change. It watches the file's directory, so editors that save by renaming a temp
file over the config work too. A config file that is a symlink, as GNU Stow,
chezmoi, and Home Manager make, is followed: the daemon checks the file it links
to every 2 seconds, so saving that file or repointing the link reloads within
about 2 seconds.

| Change                                  | Takes effect                                                                                                                                                                                     |
| --------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `[server] listen`, `[storage]`          | On restart. The daemon logs `config change takes effect on restart` with the key.                                                                                                                |
| `[server] public_url`                   | At once. Nothing else depends on it, unless `[server.oauth]` is on.                                                                                                                              |
| `[server.oauth]`                        | At once.                                                                                                                                                                                         |
| `[embedding]` added, removed, or edited | At once. The daemon swaps the embedding client and re-embeds the catalog as needed.                                                                                                              |
| `[index]`                               | At once. Every HTTP connector is rescheduled.                                                                                                                                                    |
| `[calls]`                               | At once. A new `retention` applies to calls made from then on, and to pruning from the next hourly prune.                                                                                        |
| `[telemetry]`                           | At once. Turning it off drops what was counted; turning it on starts counting from then. The `DO_NOT_TRACK` variable is read only when the daemon starts: restart the daemon to apply a change.  |
| A connector added                       | At once. It connects and syncs.                                                                                                                                                                  |
| A connector removed by hand             | At once. Its tools leave the catalog. Its stored OAuth credential is kept (see [Orphaned credentials](../guide/authentication#orphaned-credentials)). `wayfindr connector remove` deletes it.    |
| A connector's `url` changed             | At once. Its OAuth token is bound to the old URL, so the connector goes to `needs_auth` and must be authorized again.                                                                            |
| Any other connector change              | At once. The connector reconnects and syncs. A change to `url` or the `[auth]` table (subtables included), or removing the connector, also cancels any auth flow in progress (`flow_cancelled`). |
| A profile added, removed, or edited     | At once. The next request that names it gets the new profile; clients don't reconnect.                                                                                                           |

If the edited file doesn't load, the daemon keeps running its last good config
and logs the error. `wayfindr status` shows it under **Config error** until a
good config loads.

::: warning
`wayfindr status` and `wayfindr stdio` read `[server] listen` from
the file, not from the running daemon. After you change `listen`, restart the
daemon, or the CLI looks for it at the new address.
:::

## `[server]`

| Key          | Type         | Default            |
| ------------ | ------------ | ------------------ |
| `listen`     | string       | `"127.0.0.1:7777"` |
| `public_url` | string (URL) | none               |

### `listen`

The `host:port` the daemon serves HTTP on: `/mcp`, the auth link, the OAuth
callback, and the CLI's admin API.

- The host must be a loopback IP literal (such as `127.0.0.1` or `::1`) or the
  name `localhost`. Anything else is rejected:
  `host "0.0.0.0" must be a loopback address such as 127.0.0.1`. Names other
  than `localhost` are never resolved.
- Write an IPv6 address in brackets: `"[::1]:7777"`.
- The port must be from 1 to 65535.
- At startup the daemon also checks the address it actually bound and refuses to
  serve unless it is in `127.0.0.0/8` or `::1`. This catches a hosts file that
  maps `localhost` elsewhere.
- Any loopback address works. On another one, such as `127.0.0.2:7777`, the
  daemon answers requests that name that address, as well as `127.0.0.1`,
  `localhost`, and `[::1]` (see
  [HTTP Endpoints](./http#host-and-origin-checks)). Not every system routes all
  of `127.0.0.0/8` to loopback: macOS has only `127.0.0.1` until you add an
  alias, so binding `127.0.0.2` fails there.

`listen` also sets the default OAuth redirect URI,
`http://<listen>/oauth/callback`, and the auth link,
`http://<listen>/auth/<connector>`.

### `public_url`

The `https` address a tunnel serves the daemon at, such as
`"https://wayfindr.example.com"`. Leave it out when you run no tunnel. You run
the tunnel (ngrok, Cloudflare Tunnel, Tailscale Funnel, or a proxy of your own);
Wayfindr never starts or configures one.

```toml
[server]
public_url = "https://wayfindr.example.com"
```

- It must be an absolute `https` URL. The path may be empty or `/`; it can't
  have a query string, a fragment, or a username or password. Anything else is
  rejected, for example `server.public_url: must not have a query string`.
- It is informational only, unless you turn on
  [`[server.oauth]`](#server-oauth), which uses it as its issuer. Wayfindr never
  compares a request against it: no request is let in or kept out because of
  it. What lets a request through a tunnel is a
  [remote client token](./cli#wayfindr-token-create) (or, with `[server.oauth]`
  on, an OAuth grant).
- The [web console](../guide/console#connecting-a-cloud-agent) uses it for the
  text you paste into a cloud agent, and to test the connection.
- A reload applies it at once; it needs no restart.
- Set it by hand, in the console's Cloud agents page, or through the admin API
  ([`PUT /admin/server/public-url`](./http#admin)), which keeps the rest of the
  file as it was. Clearing it is refused while `server.oauth.enabled` is true.

See [Cloud agents](../guide/cloud-agents) for how to set up the tunnel and the
agent.

## `[server.oauth]`

Wayfindr's own OAuth sign-in for cloud agents that can't send a header, such as
ChatGPT. It is off unless you turn it on.

| Key       | Type             | Default                                     |
| --------- | ---------------- | ------------------------------------------- |
| `enabled` | boolean          | `false`                                     |
| `clients` | array of strings | `["https://chatgpt.com/oauth/client.json"]` |

```toml
[server]
public_url = "https://wayfindr.example.com"

[server.oauth]
enabled = true
clients = ["https://chatgpt.com/oauth/client.json"]
```

- `enabled` needs `[server] public_url`, which becomes the issuer, written
  with a lower-case scheme and host, no `:443`, and no trailing slash. Otherwise the config is rejected:
  `server.oauth.enabled: needs server.public_url`.
- `clients` are the client IDs allowed to sign in, matched exactly. Leave it out
  for ChatGPT's. An explicit `clients = []` allows none, which is rejected while
  `enabled` is true.
- Each client ID must be an absolute `https` URL with a host and a path (not just
  `/`), and no username or password, query string, or fragment. It must be in
  canonical form: a lowercase scheme and host, and no `.` or `..` path segment.
  Entries are checked even while `enabled` is false, and a repeat is rejected.
  The error names the entry by position, for example
  `server.oauth.clients: entry 2 must have a path: a client ID is the URL of its metadata document`,
  never its value.
- A reload applies a change at once; it needs no restart.
- Turn it on or off by hand, or through the admin API (`PUT /admin/server/oauth`,
  body `{"enabled": true}`; see the [admin API](./http#admin)), which writes only
  the `enabled` line.

## `[storage]`

| Key      | Type   | Default                         |
| -------- | ------ | ------------------------------- |
| `driver` | string | `"sqlite"`                      |
| `dsn`    | string | `<data>/wayfindr.db` for SQLite |

### `driver`

`"sqlite"` or `"postgres"`. Anything else is an error. See
[Storage](../guide/storage).

### `dsn`

Where the store is.

- **SQLite:** a file path. It defaults to `wayfindr.db` in the data directory. A
  leading `~` expands to your home directory.
- **Postgres:** required. A `postgres://` URL or a keyword/value string
  (`host=… dbname=…`). It is never `~`-expanded and can't hold `${…}`
  references. On first run the role must be able to create the `vector`
  (pgvector) and `pg_trgm` extensions, and the database must use UTF8 encoding
  on a server with ICU.

## `[embedding]`

Optional. Add it to turn on semantic search with any OpenAI-compatible
`/v1/embeddings` endpoint. Without it, search is fuzzy and full-text only. See
[Embeddings](../guide/embeddings).

| Key          | Type                                    | Default | Required | Description                                                                                                                                                          |
| ------------ | --------------------------------------- | ------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `base_url`   | string (URL)                            | none    | Yes      | The provider's OpenAI-compatible base, such as `"https://api.openai.com/v1"` or `"http://127.0.0.1:11434/v1"` for Ollama. Wayfindr posts to `<base_url>/embeddings`. |
| `model`      | string                                  | none    | Yes      | The model name sent with each request.                                                                                                                               |
| `dimensions` | integer                                 | none    | Yes      | Positive. Sent as the request's `dimensions` field; every vector the provider returns must have this length.                                                         |
| `api_key`    | string, [templates](#templates) allowed | none    | No       | Sent as a bearer token. Leave it out for a provider that needs none. If its reference can't be resolved, the daemon logs an error and search stays lexical only.     |
| `timeout`    | duration                                | `"2s"`  | No       | Bounds the embedding of one search query. A query that misses it is searched lexically.                                                                              |

Changing `base_url`, `model`, or `dimensions` changes the model identity, so the
catalog is embedded again.

## `[index]`

| Key                | Type     | Default |
| ------------------ | -------- | ------- |
| `refresh_interval` | duration | `"1h"`  |

### `refresh_interval`

How often the daemon re-syncs each HTTP connector's tool list. Each connector's
next poll is jittered by ±10% so they don't all poll at once. If an upstream's
last tool list carried a cache hint, the hint is used instead, bounded to
between 1 minute and 24 hours.

Stdio connectors are never polled: their tools are synced each time their
command is started. Connectors that are `starting`, `needs_auth`, or
`auth_error` are skipped; they sync when they recover.

## `[calls]`

The call log: one row per `call_tool` call, which
[`wayfindr calls`](./cli#wayfindr-calls) shows. Arguments and results are never
stored; a call that didn't end `ok` keeps its error text.

| Key         | Type     | Default  |
| ----------- | -------- | -------- |
| `retention` | duration | `"720h"` |

### `retention`

How long a call stays in the log: 30 days by default. The daemon deletes older
calls when it starts and every hour after. It must not be negative.

`"0s"` turns the call log off: no calls are recorded and none are deleted, so
the calls already in the log stay until you set a retention again.

## `[telemetry]`

The [usage report](./telemetry): once a day, the daemon sends the Wayfindr
maintainers counts and versions that say how Wayfindr is used. It is off unless
you turn it on.

```toml
[telemetry]
enabled = true
```

| Key       | Type | Default | Description                                                                                                                                                           |
| --------- | ---- | ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `enabled` | bool | `false` | `true` sends the daily usage report. The `DO_NOT_TRACK` environment variable, set to anything but empty or `0`, keeps it off whatever this says; restart to apply it. |

[Telemetry](./telemetry) lists every field the report holds and everything that
turns it off.

## `[connectors.<name>]`

One table per upstream MCP server.

```toml
[connectors.github]                     # HTTP, API key from a secret
url     = "https://api.githubcopilot.com/mcp/"
headers = { Authorization = "Bearer ${secret:github_pat}" }
timeout = "60s"

[connectors.fs]                         # stdio
command = "npx"
args    = ["-y", "@modelcontextprotocol/server-filesystem", "~/code"]
env     = { LOG_LEVEL = "warn" }
```

### Connector names

The table key is the connector's name and its stable identifier. It must match
`[a-z0-9][a-z0-9_-]{0,62}`: lowercase letters, digits, `_`, and `-`, starting
with a letter or digit, at most 63 characters. Dots are not allowed, because
tools are addressed as `<connector>.<tool>`. Renaming a connector is a remove
plus an add.

### Keys

| Key         | Type                                          | Default                                  | Applies to |
| ----------- | --------------------------------------------- | ---------------------------------------- | ---------- |
| `url`       | string, [templates](#templates) allowed       | none                                     | HTTP       |
| `command`   | string                                        | none                                     | stdio      |
| `args`      | array of strings, templates allowed           | `[]`                                     | stdio only |
| `env`       | table of strings, templates allowed           | `{}`                                     | stdio only |
| `headers`   | table of strings, templates allowed in values | `{}`                                     | HTTP only  |
| `transport` | string                                        | none (Streamable HTTP)                   | HTTP only  |
| `timeout`   | duration                                      | `"2m"`                                   | Both       |
| `tools`     | array of strings                              | none (every tool)                        | Both       |
| `auth`      | table                                         | none (no OAuth)                          | HTTP only  |
| `tls`       | table                                         | none (system CAs, no client certificate) | HTTP only  |

#### `url`

The upstream's MCP endpoint. Setting `url` makes the connector an HTTP
connector. A `url` without references must be an absolute `http` or `https` URL;
one with references is checked when it is resolved, at connect.

#### `command`

The program to run. Setting `command` makes the connector a stdio connector. It
is looked up on the daemon's `PATH`, and a leading `~` expands to your home
directory. It is not a [template](#templates): `${env:X}` in `command` stays
literal text, so put the variable in `args` or `env` instead.

#### `args`

Arguments to `command`. A leading `~` in each element expands to your home
directory; a `~` anywhere else (such as `--dir=~/x`) is kept as written.

#### `env`

Environment variables for the stdio child, set on top of the daemon's
environment. The child inherits the daemon's whole environment except variables
whose names start with `WAYFINDR_`, which are removed because they hold the
daemon's secrets. `env` can set any name, including those.

#### `headers`

HTTP headers sent with every request to the upstream. This is how you send an
API key:

```toml
headers = { "x-api-key" = "${env:EXA_API_KEY}" }
```

Only the values may hold references. Header names are used as written.

A connector with an `[auth]` table can't set an `Authorization` header (in any
letter case): Wayfindr sends the OAuth token in it, so the config is rejected.

#### `transport`

`"sse"` forces the legacy HTTP+SSE transport. Leave it out to use Streamable
HTTP. Without it, Wayfindr still falls back to HTTP+SSE when the Streamable HTTP
`initialize` POST gets a 4xx other than 401 or 403. A 401 or 403 to any request
of the connect, `server/discover` included, rules the fallback out. With an
`[auth]` table, a 401, or a 403 with `insufficient_scope`, sends the connector
to `needs_auth`. Any other refusal leaves it `unreachable`. Any other value is
an error (`must be "sse" or absent, not "…"`).

#### `timeout`

Bounds one `call_tool` on this connector. A call that runs longer returns an
error saying the call outlasted the connector's timeout; the connector's state
doesn't change. Connecting (spawning or dialing, plus the MCP handshake) has its
own fixed limit of 60 seconds; when a call triggers the connect, the call's
`timeout` applies too.

#### `tools`

Chooses which of the connector's tools agents can see and call. `search_tools`
never returns a tool it hides, and `call_tool` refuses it. Syncs leave hidden
tools out of the catalog. Leave `tools` out to expose every tool. See
[Tools lists](../guide/access/tools-lists).

```toml
tools = ["@read", "create_draft", "!delete_*"]
```

| Entry    | Matches                                                                                                                                                              |
| -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `@read`  | Tools the upstream marks read-only (`readOnlyHint: true`).                                                                                                           |
| `@write` | `@read` tools, plus tools the upstream marks not destructive (`destructiveHint: false`).                                                                             |
| `name`   | The tool with that name as the upstream sends it: `create_draft`, not `gmail.create_draft`. The glob wildcards `*`, `?`, and `[…]` work; matching is case-sensitive. |
| `!name`  | Hides the tools it matches, whatever else matches them.                                                                                                              |

A tool is exposed when no `!` entry matches it and either it matches a class or
a name, or the list has only `!` entries. A tool with no annotations is in
neither class, because MCP's defaults make it destructive. Annotations are hints
from the upstream, not guarantees; see
[Read-only is the upstream's word](../guide/access/tools-lists#read-only-is-the-upstream-s-word).

An unknown class (`"@x" is not a class; use "@read" or "@write"`), a negated
class (`a class cannot be hidden with !; hide tools by name`), a malformed glob,
or an empty entry is an error at `connectors.<name>.tools[i]`.

### Cross-key rules

A key on the wrong kind of connector is an error, and so is both or neither of
`url` and `command`:

| Rule                                                       | Error at the key                                                                                    |
| ---------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| Exactly one of `url` and `command`.                        | `exactly one of url or command is required` or `exactly one of url or command is allowed, not both` |
| `args` and `env` only with `command`.                      | `is allowed only on command (stdio) connectors`                                                     |
| `headers`, `transport`, `auth`, and `tls` only with `url`. | `is allowed only on url connectors`                                                                 |

## `[connectors.<name>.auth]`

Turns on OAuth for an HTTP connector. Without this table the connector sends no
OAuth token: it is either unauthenticated or uses an API key in `headers`. For
how to set each kind up, see [Authentication](../guide/authentication).

The smallest form uses discovery and Dynamic Client Registration:

```toml
[connectors.linear]
url  = "https://mcp.linear.app/mcp"
auth = { type = "oauth" }
```

A connector with your own registered app ("manual" connector):

```toml
[connectors.slack]
url = "https://mcp.slack.com/mcp"

[connectors.slack.auth]
type          = "oauth"
client_id     = "1234.5678"
client_secret = "${secret:slack_client_secret}"
scopes        = ["channels:read", "chat:write"]
```

The load enforces the [Auth rules](#auth-rules) below, which hold the error
messages.

### Common keys

| Key                | Type                                    | Default                                 | Description                                                                                                                                                                                                    |
| ------------------ | --------------------------------------- | --------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `type`             | string                                  | none, required                          | Must be `"oauth"`.                                                                                                                                                                                             |
| `client_id`        | string                                  | none                                    | A pre-registered OAuth client. Setting it makes this a manual connector.                                                                                                                                       |
| `client_secret`    | string, [templates](#templates) allowed | none                                    | The client's secret.                                                                                                                                                                                           |
| `scopes`           | array of strings                        | `[]`                                    | Scopes to request, used only when neither the upstream's `WWW-Authenticate` challenge nor its protected-resource metadata (`scopes_supported`) names any. An [unattended grant](#grants) asks for these first. |
| `redirect_uri`     | string (URL)                            | `http://<server.listen>/oauth/callback` | Where the authorization server sends the browser back, for example an HTTPS tunnel URL for a provider that rejects loopback redirects. Dynamic registration registers this URI too.                            |
| `authorize_params` | table of strings                        | none                                    | Extra query parameters for the authorization request, for providers that need ones outside OAuth, such as Google's `access_type = "offline"`. Parameters Wayfindr sets itself are refused.                     |

### Discovery overrides

By default Wayfindr finds the endpoints and the client itself. These keys name
them instead.

| Key                             | Type         | Default    | Description                                                                                                                                                                                                                                                           |
| ------------------------------- | ------------ | ---------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `authorization_server`          | string (URL) | discovered | The authorization server's issuer URL. Skips protected-resource metadata discovery.                                                                                                                                                                                   |
| `authorization_endpoint`        | string (URL) | discovered | The authorize endpoint, for the authorization code grant only. Set together with `token_endpoint`; skips authorization-server metadata.                                                                                                                               |
| `token_endpoint`                | string (URL) | discovered | The token endpoint. With the authorization code grant, set together with `authorization_endpoint`; with an unattended grant it may be set alone, and with the device code, with `device_authorization_endpoint`.                                                      |
| `device_authorization_endpoint` | string (URL) | discovered | The device authorization endpoint (RFC 8628), for `grant = "device_code"` or `wayfindr auth login --device`. Set it when the provider supports the device code but its metadata doesn't list the endpoint. With `token_endpoint`, it also stands in for the metadata. |
| `client_metadata_url`           | string (URL) | none       | The URL of a Client ID Metadata Document (CIMD) you host. Used when there is no `client_id` and the authorization server advertises CIMD support.                                                                                                                     |

### Security toggles

| Key                  | Type    | Default  | Description                                                                                                                                                                                                                                  |
| -------------------- | ------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `pkce`               | boolean | `true`   | Use PKCE (S256).                                                                                                                                                                                                                             |
| `validate_issuer`    | boolean | `true`   | Check that the authorization server's metadata names the issuer it was found at, and check the callback's `iss` (RFC 9207). Issuers that differ only by a trailing `/` are the same. The daemon logs a warning each time it skips the check. |
| `resource_indicator` | boolean | `true`   | Send the `resource` parameter (RFC 8707) on authorize and token requests.                                                                                                                                                                    |
| `dpop`               | string  | `"auto"` | Whether tokens are bound to a DPoP key (RFC 9449): `"auto"` when the server asks for it, `"always"`, or `"never"`. See [Enterprise Identity Providers](../guide/authentication-enterprise).                                                  |

### Grant and client-authentication keys

| Key                          | Type   | Default                | Description                                                                                                                       |
| ---------------------------- | ------ | ---------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `grant`                      | string | `"authorization_code"` | How the connector gets a token. See [Grants](#grants).                                                                            |
| `token_endpoint_auth_method` | string | chosen automatically   | How Wayfindr authenticates as the OAuth client at the token endpoint. See [Client authentication](#client-authentication).        |
| `signing_key`                | table  | none                   | A private key Wayfindr signs JWTs with. See [`[connectors.<name>.auth.signing_key]`](#connectors-name-auth-signing-key).          |
| `assertion`                  | table  | none                   | Where the `jwt_bearer` grant's assertion comes from. See [`[connectors.<name>.auth.assertion]`](#connectors-name-auth-assertion). |

### Grants

| `grant`                | Who takes part | Needs                                                                          | What happens                                                                                                                     |
| ---------------------- | -------------- | ------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------- |
| `"authorization_code"` | You            | nothing                                                                        | The default. You sign in in a browser, which comes back to the callback.                                                         |
| `"device_code"`        | You            | A provider with a device authorization endpoint                                | You enter a code at the provider's page on any device while the daemon waits. For a daemon on a remote machine or over SSH.      |
| `"client_credentials"` | Nobody         | `client_id`, `authorization_server` or `token_endpoint`, a client credential   | The OAuth client's own access, for machine-to-machine use. Wayfindr gets a token whenever a call needs one and never stores it.  |
| `"jwt_bearer"`         | Nobody         | `client_id`, `authorization_server` or `token_endpoint`, an `[auth.assertion]` | Wayfindr presents an assertion, such as a workload token from Kubernetes or a CI system, for a token. The token is never stored. |

`client_credentials` and `jwt_bearer` are **unattended grants**: there is no
sign-in, so the connector never goes to `needs_auth` and has no stored OAuth
credential. Wayfindr runs the grant when a call needs a token, holds the token
in memory only, and runs the grant again when the token is about to expire or
the server rejects it; it ignores any refresh token. A refused grant puts the
connector in `auth_error`. `wayfindr connector edit --grant` to one of them
deletes the credential the connector had. The grant sends its credential every
time, so it goes only to a server you name, never to one the MCP server's
metadata names; they also refuse the sign-in keys (see [Auth rules](#auth-rules)).
See [Machine-to-machine and workloads](../guide/authentication-unattended).

```toml
# Machine-to-machine with a private key.
[connectors.reports]
url = "https://mcp.example.com/mcp"

[connectors.reports.auth]
type                       = "oauth"
grant                      = "client_credentials"
authorization_server       = "https://auth.example.com"
client_id                  = "reports-bot"
token_endpoint_auth_method = "private_key_jwt"

[connectors.reports.auth.signing_key]
key = "${secret:reports_signing_key}"
```

### Client authentication

`token_endpoint_auth_method` is how Wayfindr proves at the token endpoint that
it is the OAuth client:

| Method                                               | What Wayfindr sends                                                                                                                                                                                                                                                                       | Needs                                                             |
| ---------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------- |
| `"none"`                                             | Only `client_id`: a public client.                                                                                                                                                                                                                                                        | nothing                                                           |
| `"client_secret_basic"`                              | `client_id` and the secret in HTTP Basic authentication.                                                                                                                                                                                                                                  | `client_secret`                                                   |
| `"client_secret_post"`                               | `client_id` and the secret in the form body.                                                                                                                                                                                                                                              | `client_secret`                                                   |
| `"client_secret_jwt"`                                | A JWT signed with HS256 over the secret (a client assertion).                                                                                                                                                                                                                             | `client_secret`, `client_id`                                      |
| `"private_key_jwt"`                                  | A JWT signed with your private key, whose public key you registered with the provider.                                                                                                                                                                                                    | `signing_key`, `client_id`                                        |
| `"tls_client_auth"`, `"self_signed_tls_client_auth"` | Only `client_id`; the client certificate the connection presents is the credential (RFC 8705). The first is for a certificate from a CA the provider trusts, the second for one you registered with the provider. Requests go to the provider's mTLS endpoint aliases when it lists them. | a [`[tls]`](#connectors-name-tls) client certificate, `client_id` |

Each client assertion is good for 60 seconds and is used once: its `iss` and
`sub` are your `client_id`. A `private_key_jwt` assertion's `aud` is set by
[`signing_key.audience`](#connectors-name-auth-signing-key). A
`client_secret_jwt` assertion is always addressed to the authorization server's
issuer, or to the token endpoint when no issuer is known (endpoints from
config).

Leave `token_endpoint_auth_method` out and Wayfindr chooses (never a `tls_*`
method: set one to use it):

1. For a client it registered dynamically, the method the provider registered it
   with.
2. Otherwise, from the credential you configured, the first method the provider
   lists in its metadata (`token_endpoint_auth_methods_supported`; metadata
   without the list means `client_secret_basic`). A `signing_key` gives
   `private_key_jwt`. A `client_secret` gives `client_secret_basic`, then
   `client_secret_post`, then `client_secret_jwt`. With no metadata at all
   (endpoints in config), it takes the first without checking.
3. With no credential, the client is public and sends only its `client_id`.

With `grant = "jwt_bearer"`, the `signing_key` belongs to the assertion, not to
the client: it signs the assertion for `assertion.subject`, and is a client
credential only when `token_endpoint_auth_method = "private_key_jwt"` says so.
Left out, such a connector authenticates with its `client_secret` if it has one,
else as a public client that sends only its `client_id`.

When the provider lists methods and none of them fits your credential, the
connector goes to `auth_error` (`auth_method_unsupported`) before any request is
sent. Set `token_endpoint_auth_method` to a method the provider accepts; it is
used as is.

When Wayfindr registers a client dynamically, it asks for `"none"` if the
provider lists it, else `"client_secret_basic"` if the provider lists that or
nothing, else `"client_secret_post"`. It sends `application_type = "native"` for
a loopback `redirect_uri` and `"web"` for any other.

### `[connectors.<name>.auth.signing_key]`

The private key for `private_key_jwt` client authentication, and for
`jwt_bearer` assertions Wayfindr signs itself (`assertion.subject`). Requires
`client_id`. See [Enterprise Identity Providers](../guide/authentication-enterprise)
for how to make one.

| Key        | Type                                    | Default        | Description                                                                                                                                                                                                                                                                                                                                                                                                                             |
| ---------- | --------------------------------------- | -------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `key`      | string, [templates](#templates) allowed | none, required | The PEM private key (EC P-256 or P-384, RSA, or Ed25519), usually `${secret:…}`.                                                                                                                                                                                                                                                                                                                                                        |
| `kid`      | string                                  | none           | The key ID put in each JWT's header.                                                                                                                                                                                                                                                                                                                                                                                                    |
| `alg`      | string                                  | chosen         | `"ES256"`, `"ES384"`, `"RS256"`, `"PS256"`, or `"EdDSA"`. Left out, a client assertion uses the first algorithm in the provider's `token_endpoint_auth_signing_alg_values_supported` that the key can sign, else the key's own (`ES256` for P-256, `ES384` for P-384, `RS256` for RSA, `EdDSA` for Ed25519); a self-signed `jwt_bearer` assertion uses the key's own. A key that can't sign the `alg` you set is `signing_key_invalid`. |
| `audience` | string                                  | `"issuer"`     | What each JWT is addressed to: the authorization server's `"issuer"`, or its `"token_endpoint"`, which Okta and Entra need.                                                                                                                                                                                                                                                                                                             |

### `[connectors.<name>.auth.assertion]`

Only with `grant = "jwt_bearer"`, which requires it. The assertion is read for
every grant and never kept, so a token rotated on disk or in the environment is
used by the next one. A file that is missing or empty, or a value that doesn't
resolve, puts the connector in `auth_error` (`unresolved_value`). Set exactly
one key:

| Key       | Type                                    | Description                                                                                                                                                                                       |
| --------- | --------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `file`    | string (path)                           | A file holding a workload token, such as one Kubernetes projects. Read again for every token. A leading `~` expands.                                                                              |
| `value`   | string, [templates](#templates) allowed | The assertion itself, usually `${env:…}`. Resolved for every token.                                                                                                                               |
| `subject` | string                                  | Wayfindr signs the assertion itself with `signing_key`, with this subject: its `iss` is `client_id`, its `aud` is set by `signing_key.audience`, and it lives 60 seconds. Requires `signing_key`. |

```toml
[connectors.cluster.auth]
type                 = "oauth"
grant                = "jwt_bearer"
authorization_server = "https://auth.example.com"
client_id            = "mcp-gateway"
assertion            = { file = "/var/run/secrets/tokens/mcp" }
```

### How the client is chosen

1. `client_id` from the config, with `client_secret` if set.
2. Otherwise CIMD, if `client_metadata_url` is set and the authorization server
   supports it.
3. Otherwise Dynamic Client Registration, if the authorization server has a
   registration endpoint.
4. Otherwise the connector goes to `auth_error`: register an app with the
   provider and set `client_id`.

A connector without `client_id` also needs an authorization server that
advertises PKCE S256 in its metadata; one whose metadata lists other methods
only puts the connector in `auth_error` (`pkce_unsupported`). A server with no
metadata at all gets S256 anyway.

### Auth rules

The Auth rules table owns every auth error message; the key tables above don't
repeat them.

| Rule                                                                                                                                                                           | Error at the key                                                                                                                     |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------ |
| `type` is set.                                                                                                                                                                 | `type: is required ("oauth")`                                                                                                        |
| `type` is `"oauth"`.                                                                                                                                                           | `type: must be "oauth", not "…"`                                                                                                     |
| `client_secret` requires `client_id`.                                                                                                                                          | `client_secret: requires client_id`                                                                                                  |
| `pkce = false` requires `client_secret` (authorization code grant).                                                                                                            | `pkce: pkce = false requires client_secret`                                                                                          |
| `resource_indicator = false` requires `client_id`.                                                                                                                             | `resource_indicator: resource_indicator = false requires client_id`                                                                  |
| `authorization_endpoint` and `token_endpoint` are set together (authorization code grant).                                                                                     | `auth: authorization_endpoint and token_endpoint must be set together`                                                               |
| Endpoint and URI keys are absolute `http`/`https` URLs.                                                                                                                        | `must be an absolute http or https URL`                                                                                              |
| `authorize_params` has no empty key.                                                                                                                                           | `authorize_params: must not have an empty key`                                                                                       |
| `authorize_params` doesn't set `response_type`, `client_id`, `redirect_uri`, `state`, `scope`, `code_challenge`, `code_challenge_method`, `resource`, or `dpop_jkt`.           | `authorize_params.<key>: is set by Wayfindr; …`, naming the setting to use instead, such as `use scopes`                             |
| The table is allowed only on `url` connectors.                                                                                                                                 | `auth: is allowed only on url connectors`                                                                                            |
| `grant`, `dpop`, `token_endpoint_auth_method`, `signing_key.alg`, and `signing_key.audience` are known values.                                                                 | `must be "…", "…", or "…", not "…"`                                                                                                  |
| `client_credentials` and `jwt_bearer` require `client_id`.                                                                                                                     | `client_id: is required with grant = "…"`                                                                                            |
| `client_credentials` and `jwt_bearer` require `authorization_server` or `token_endpoint`.                                                                                      | `grant: grant = "…" requires authorization_server or token_endpoint, so its credential goes only to the server you name`             |
| `client_credentials` and `jwt_bearer` refuse `redirect_uri`, `authorize_params`, `pkce`, `client_metadata_url`, `authorization_endpoint`, and `device_authorization_endpoint`. | `<key>: is not allowed with grant = "…", which has no user`                                                                          |
| `client_credentials` has a credential: `client_secret`, `signing_key`, or a `tls_client_auth`/`self_signed_tls_client_auth` method.                                            | `grant: grant = "client_credentials" requires a client credential: client_secret or signing_key`                                     |
| `jwt_bearer` has an `assertion` table.                                                                                                                                         | `assertion: is required with grant = "jwt_bearer"`                                                                                   |
| `assertion` is only for `jwt_bearer`.                                                                                                                                          | `assertion: is allowed only with grant = "jwt_bearer"`                                                                               |
| `assertion` sets exactly one of `file`, `value`, and `subject`.                                                                                                                | `assertion: must set one of file, value, or subject` or `exactly one of file, value, or subject is allowed`                          |
| `assertion.subject` requires `signing_key`.                                                                                                                                    | `assertion.subject: requires signing_key, which Wayfindr signs the assertion with`                                                   |
| `device_code` refuses `redirect_uri` and `authorization_endpoint`.                                                                                                             | `<key>: is not allowed with grant = "device_code", which has no authorization request or callback`                                   |
| `private_key_jwt` requires `signing_key`; `client_secret_basic`, `client_secret_post`, and `client_secret_jwt` require `client_secret`; the JWT methods require `client_id`.   | `token_endpoint_auth_method: token_endpoint_auth_method = "…" requires …`                                                            |
| `token_endpoint_auth_method = "none"` refuses `client_secret` and `signing_key`.                                                                                               | `<key>: is not allowed with token_endpoint_auth_method = "none"`                                                                     |
| `tls_client_auth` and `self_signed_tls_client_auth` require a client certificate in `[tls]`, and `client_id`.                                                                  | `token_endpoint_auth_method: token_endpoint_auth_method = "…" requires a client certificate: cert_file in the connector's tls table` |
| `signing_key` requires `client_id`, and `signing_key.key` is set.                                                                                                              | `signing_key: requires client_id: …`, `signing_key.key: is required`                                                                 |

## `[connectors.<name>.tls]`

The TLS identity and trust of an HTTP connector: the CAs it trusts besides the
system's, and the client certificate it presents. Both apply to every connection
the connector makes, to its MCP server and to its OAuth authorization server
alike, so the two see the same certificate. A connector without `[auth]` can
have one too, for a server that only takes mutual TLS. See
[Private CAs and client certificates](../guide/connectors#private-cas-and-client-certificates).

```toml
[connectors.corp]
url = "https://mcp.corp.example.com/mcp"

[connectors.corp.tls]
cert_file = "~/corp/client.pem"
key_file  = "~/corp/client.key"   # or key = "${secret:corp_tls_key}"
ca_file   = "~/corp/ca.pem"
```

| Key         | Type                                    | Description                                                                                                                                                                |
| ----------- | --------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `cert_file` | string (path)                           | A PEM file with the client certificate, then any intermediates. Presented to a server that asks for one. A leading `~` expands. Needs exactly one of `key_file` and `key`. |
| `key_file`  | string (path)                           | A PEM file with `cert_file`'s private key. A leading `~` expands.                                                                                                          |
| `key`       | string, [templates](#templates) allowed | The private key itself, as PEM, usually `${secret:…}`, instead of `key_file`. A missing secret puts the connector in `auth_error` naming it.                               |
| `ca_file`   | string (path)                           | A PEM file of CA certificates to trust in addition to the system's, for a server on a private CA. A leading `~` expands.                                                   |

The files are read when the connector connects, not when the config loads.
`cert_file` and `key_file` are read again at the next connection after either
changes on disk, so a renewed certificate is used without a reload or a restart.
`ca_file` is read once, when the connector's client is built: after replacing it
in place, edit the table (or restart the daemon) to trust the new file. The
daemon warns of a client certificate that expires within 7 days
(`certificate_expiring`); see [Warnings](../guide/troubleshooting#warnings).

| Rule                                                        | Error at the key                                                                                  |
| ----------------------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| The table sets something.                                   | `tls: must set cert_file (with key_file or key), ca_file, or both`                                |
| `cert_file` comes with exactly one of `key_file` and `key`. | `cert_file: requires key_file or key`, `tls: exactly one of key_file or key is allowed, not both` |
| `key_file` and `key` need `cert_file`.                      | `<key>: requires cert_file`                                                                       |

## `[profiles.<name>]`

A [profile](../guide/access/profiles) is a named project scope: it narrows the
connectors and tools a client gets. A client picks one by name, with
`wayfindr stdio --profile <name>` or `http://127.0.0.1:7777/mcp?profile=<name>`.

```toml
[profiles.acme]
connectors = ["github", "linear"]

[profiles.acme.tools]
github = ["@read"]            # ANDed with [connectors.github].tools
```

The keys are the same as in a `.wayfindr.toml`:

| Key          | Type                       | Default                | What it does                                                                                        |
| ------------ | -------------------------- | ---------------------- | --------------------------------------------------------------------------------------------------- |
| `connectors` | array of strings           | none (every connector) | The connectors the profile allows. `[]` allows none.                                                |
| `tools`      | table of arrays of strings | `{}`                   | One tools list per connector, in the [`tools` syntax](#tools). A tool must pass both lists to show. |

A profile only narrows: it can't add a connector or show a tool the connector's
`tools` list hides. A client that uses a profile and runs in a project with a
`.wayfindr.toml` gets only what the config, the profile, and the file all allow.

### Profile rules

| Rule                                                                    | Error at the key                                                        |
| ----------------------------------------------------------------------- | ----------------------------------------------------------------------- |
| The name matches the [connector-name rule](#connector-names).           | `profiles.<name>: profile name must match [a-z0-9][a-z0-9_-]{0,62}`     |
| Each name in `connectors` and `tools` is a valid connector name.        | `"…" is not a connector name`                                           |
| Each connector the profile names is configured.                         | `profiles.<name>: connector "…" is not configured`                      |
| A connector in `tools` is also in `connectors`, if `connectors` is set. | `profiles.<name>.tools.<connector>: connector "…" is not in connectors` |
| Each tools list follows the [`tools` rules](#tools).                    | `profiles.<name>.tools.<connector>[i]: …`                               |

Unlike a `.wayfindr.toml`, which may name a connector you haven't set up and
gets only a warning, a profile is part of your config, so a connector it names
but the config lacks is an error. As with any invalid config, a reload with that
mistake keeps the last good config running.

## Templates

These values can hold references that are filled in when Wayfindr uses them:

- `url`, `args`, `env` values, and `headers` values of a connector;
- `client_secret` in `[connectors.<name>.auth]`;
- `key` in `[connectors.<name>.auth.signing_key]` and `value` in
  `[connectors.<name>.auth.assertion]`;
- `key` in `[connectors.<name>.tls]`;
- `api_key` in `[embedding]`.

| Reference        | Reads                                                                   | Name grammar                 |
| ---------------- | ----------------------------------------------------------------------- | ---------------------------- |
| `${env:NAME}`    | The daemon's environment variable `NAME`                                | `[A-Za-z_][A-Za-z0-9_]*`     |
| `${secret:name}` | A secret stored with [`wayfindr secret set`](./cli#wayfindr-secret-set) | `[A-Za-z0-9][A-Za-z0-9_.-]*` |

```toml
headers = { Authorization = "Bearer ${secret:github_pat}" }
args    = ["--workspace", "${env:HOME}/work"]
```

A `${secret:…}` reference accepts the wider grammar above so configs from older
releases still load, but a new secret's name must follow the
[connector-name rule](#connector-names), `[a-z0-9][a-z0-9_-]{0,62}`: name new
references that way. See
[`wayfindr secret set`](./cli#wayfindr-secret-set).

- A value can mix literal text and any number of references.
- A `$` not followed by `{` is literal. There is no escape for a literal `${`,
  so a shell-style `${HOME}` is a load error: write `${env:HOME}`.
- Every `${` must open a valid reference. Malformed ones fail the load with the
  byte offset, for example
  `bad reference at offset 7: unknown reference kind "vault" (want env or secret)`.
- The load checks syntax only. References are resolved each time a connector
  connects, so a new secret or a fixed variable takes effect on the next connect
  without a reload. `[embedding] api_key` is resolved when the embedding client
  is built: at startup, when `[embedding]` changes, and when
  `wayfindr secret set` stores a secret it refers to. Resolved values are
  inserted as they are; they aren't expanded again.
- A reference that names nothing puts the connector in `auth_error` with the
  reference named, for example
  `The connector's config refers to secret:github_pat, but no such secret is stored.`
  Setting the secret with `wayfindr secret set` reconnects the waiting
  connectors at once.
- `${env:…}` reads the daemon's environment. When the daemon runs as a service,
  your shell's variables don't reach it; set them in the service's environment
  or use a secret instead (see [Running the Daemon](../guide/running)). After
  changing an environment variable, restart the daemon.

See [Secrets](../guide/secrets).

## Home directory (`~`)

A leading `~` expands to your home directory in values that hold local paths:

- `storage.dsn`, when `driver = "sqlite"`;
- a stdio connector's `command` and each element of its `args`;
- `file` in `[connectors.<name>.auth.assertion]`;
- `cert_file`, `key_file`, and `ca_file` in `[connectors.<name>.tls]`.

Only `~` alone and `~/…` expand. `~user/…` is not expanded, nor is a `~`
anywhere but the start. In `~/${env:SUB}`, the `~` expands and the reference
resolves later. If the home directory can't be found, each value that needs it
is an error at its key: `cannot expand ~: …`.

No other value is expanded; `env` values and `headers` are used as written.

## Example

A config with one of each table. Every key not shown has a default.

```toml
[server]
listen = "127.0.0.1:7777"            # loopback only; restart to apply

[storage]
driver = "sqlite"                    # or "postgres", with a dsn

[embedding]                          # optional; any OpenAI-compatible endpoint
base_url   = "http://127.0.0.1:11434/v1"
model      = "qwen3-embedding:0.6b"
dimensions = 1024

[connectors.slack]                   # OAuth with your own registered app
url     = "https://mcp.slack.com/mcp"
timeout = "60s"                      # per call_tool
auth    = { type = "oauth", client_id = "1234.5678", client_secret = "${secret:slack_client_secret}", scopes = ["channels:read", "chat:write"] }

[connectors.fs]                      # stdio: Wayfindr spawns the command
command = "npx"
args    = ["-y", "@modelcontextprotocol/server-filesystem", "~/code"]

[profiles.acme]                      # a client picks it with --profile acme
connectors = ["slack", "fs"]
tools      = { slack = ["@read"] }   # ANDed with [connectors.slack].tools
```
