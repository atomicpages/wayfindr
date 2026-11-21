# HTTP Endpoints

The daemon serves everything on one address,
[`[server] listen`](./configuration#listen), which is `127.0.0.1:7777` by
default and always loopback. This page lists every route and the checks in front
of it.

| Route                                  | Methods       | Who uses it                                      | Checks                                                                        |
| -------------------------------------- | ------------- | ------------------------------------------------ | ----------------------------------------------------------------------------- |
| [`/mcp`](#mcp)                         | `POST`        | MCP clients and the stdio shim                   | Host, Origin, bearer                                                          |
| [`/healthz`](#healthz)                 | `GET`, `HEAD` | The shim and the CLI, to see if the daemon is up | Host, Origin                                                                  |
| [`/auth/<connector>`](#auth-connector) | `GET`, `POST` | You, in a browser: the auth link                 | Host, Origin; the `POST` also needs a same-origin `Origin` and the form nonce |
| [`/oauth/callback`](#oauth-callback)   | `GET`         | The authorization server, through your browser   | The flow's `state` only                                                       |
| [`/ui/…`](#ui)                         | `GET`, `POST` | You, in a browser: the web console               | Host, Origin; a `POST` also needs a same-origin `Origin`; a session cookie    |
| [`/admin/…`](#admin)                   | various       | The `wayfindr` CLI                               | Host, Origin, bearer (the default client token only)                          |

A request for any other path gets `404`. A known path with the wrong method gets
`405` with an `Allow` header.

## Host and Origin checks

These protect the daemon from web pages in your browser, including DNS rebinding
attacks.

**Host.** Every route except `/oauth/callback` requires a `Host` header of
`127.0.0.1`, `localhost`, `[::1]`, or the loopback IP the daemon is bound to
(such as `127.0.0.2` for `listen = "127.0.0.2:7777"`), with exactly the port the
daemon listens on. Names are compared, never resolved. Any other `Host` gets
`404 page not found` for every path and method, so a foreign page can't even
learn which routes exist.

A request with a `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`,
`X-Forwarded-Proto`, `X-Real-IP`, or `CF-Connecting-IP` header is treated as
foreign whatever its `Host` says, so a proxy or tunnel that rewrites `Host` to
`127.0.0.1` still reaches only `/oauth/callback`.

**Origin.** Every route except `/oauth/callback` refuses a request whose
`Origin` header is present and not loopback, or that has more than one `Origin`
header: `403 forbidden origin`. A loopback origin is `http` or `https` on
`127.0.0.1`, `localhost`, `[::1]`, or the IP the daemon is bound to, on any
port. Requests without `Origin`, such as from the CLI, curl, and most MCP
clients, pass.

**Bearer.** `/mcp` and `/admin/` require exactly one
`Authorization: Bearer <client token>` header. The scheme is case-insensitive. A
client token is either the default client token, the one `wayfindr init` writes
(see [Paths and Environment](./paths-and-environment#client-token)), or a
[scoped client token](/guide/access/scoped-tokens) made with
[`wayfindr token create`](./cli#wayfindr-token-create). The default token is
compared in constant time; a scoped token is looked up by its SHA-256. A
missing, wrong, or revoked token gets `401 unauthorized` with
`WWW-Authenticate: Bearer`. A revoke applies from the next request on.

`/admin/` takes the default client token only. A valid scoped token there gets
`403` `This client token cannot use the admin API.`

Refused requests are logged at `debug` level with the method, path, and which
check refused them, never header values or the query string.

## `/mcp`

The MCP endpoint, served as stateless
[Streamable HTTP](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#streamable-http).

```sh
curl -s http://127.0.0.1:7777/mcp \
  -H "Authorization: Bearer $WAYFINDR_CLIENT_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'
```

```
event: message
data: {"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}},"protocolVersion":"2025-11-25","serverInfo":{"name":"wayfindr","version":"v0.1.0"}}}
```

- **Methods.** Only `POST` carries requests. `GET` and `DELETE` are routed (and
  checked) but answer `405` with `Allow: POST`, because there are no sessions to
  stream on or end.
- **Sessions.** None. The daemon never sends `Mcp-Session-Id` and accepts every
  request without one, so a client keeps working across a daemon restart.
- **Protocol versions.** 2026-07-28, 2025-11-25, and 2025-06-18, negotiated per
  request and independently of what upstreams speak. A client that asks for an
  unknown version gets 2025-11-25. See [Compatibility](./compatibility).
- **Capabilities.** `tools` only, without `listChanged`, and no `logging`. The
  two tools are [`search_tools` and `call_tool`](./meta-tools).
- **Client token.** A client with a scoped client token sees only its token's
  connectors. Every other connector is as if it weren't configured, before any
  profile or project scope applies. See
  [Scoped client tokens](/guide/access/scoped-tokens).
- **Body limit.** 4 MiB. A larger body gets `413`
  `request body exceeds 4194304 bytes`.
- **Timeouts.** The server times out only on slow request headers (10 seconds).
  There is no write or idle timeout, so long-lived response streams stay open. A
  tool call is bounded by its connector's [`timeout`](./configuration#timeout).

Clients connect here directly over HTTP, or through `wayfindr stdio`. See
[Clients](/guide/clients).

## `/healthz`

Liveness. No bearer needed.

```sh
curl -s http://127.0.0.1:7777/healthz
```

```json
{ "version": "v0.1.0" }
```

It answers `200` as soon as the daemon is listening, before connectors finish
connecting. Use [`wayfindr status`](./cli#wayfindr-status) for connector states.

## `/auth/<connector>`

The **auth link**: the page that starts a connector's OAuth auth flow in your
browser. Every `needs_auth` error and status carries it, for example
`http://127.0.0.1:7777/auth/linear`.

### `GET /auth/<connector>`

Shows an **Authorize &lt;connector&gt;** page with a **Continue** button. A
plain `GET` never starts a flow, so a link on some other web page can't push you
into a consent screen.

The form carries a **flow nonce**: single-use, bound to the connector, and valid
for 10 minutes. The daemon keeps at most 1,024 unspent nonces and drops the
oldest past that.

| Situation                           | Status | Page                                                                                                      |
| ----------------------------------- | ------ | --------------------------------------------------------------------------------------------------------- |
| An OAuth connector                  | `200`  | `Authorize <connector>` with the Continue form                                                            |
| No such connector                   | `404`  | `Cannot authorize <connector>`: `No connector named "<connector>" is configured.`                         |
| The connector has no `[auth]` table | `404`  | `Cannot authorize <connector>`: `<connector> does not use OAuth: it has no [auth] table of type "oauth".` |

### `POST /auth/<connector>`

Submitted by the Continue button. It requires `Origin: http://<Host>` exactly (a
browser always sends it on a form post) and a valid nonce in the form body (at
most 4 KiB). It spends the nonce, starts the auth flow, and redirects (`302`) to
the authorization server's authorize URL. For a connector with
`grant = "device_code"` it shows the device code instead: a page with the code
and a link to where you enter it, never a redirect.

| Situation                                                                   | Status | Page                                                                                                                 |
| --------------------------------------------------------------------------- | ------ | -------------------------------------------------------------------------------------------------------------------- |
| Flow started                                                                | `302`  | Redirect to the authorize URL                                                                                        |
| Device code flow started                                                    | `200`  | `Authorize <connector>`: the code, and a link to the provider's page (`https`, or `http` on `localhost`)             |
| `Origin` missing or not same-origin                                         | `403`  | `forbidden origin` (plain text)                                                                                      |
| Nonce missing, spent, expired, or for another connector                     | `403`  | `Authorization not started`: `This form is stale or was not opened from Wayfindr.`, with the auth link to start over |
| Unknown connector or not OAuth                                              | `404`  | As for `GET`                                                                                                         |
| The connector can't be authorized as configured, or needs auth first        | `409`  | `Cannot authorize <connector>` with the advice                                                                       |
| The authorization server is unreachable or answered with something unusable | `502`  | As above                                                                                                             |
| Internal failure                                                            | `500`  | As above                                                                                                             |

The CLI command
[`wayfindr auth login <connector>`](./cli#wayfindr-auth-login) starts the same
flow and gets the same answers.

All the browser pages are sent with
`Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'`,
`X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, and
`Cache-Control: no-store`. The "Continue" page is `Referrer-Policy: same-origin`
instead, so the browser posts its form with the page's real `Origin`.

## `/oauth/callback`

Where the authorization server sends your browser back after you consent: the
default redirect URI, `http://<listen>/oauth/callback`. If a connector sets
[`redirect_uri`](./configuration#connectors-name-auth), for example to an HTTPS
tunnel that forwards here, the browser arrives through that instead.

This is the only route served on any `Host` and through a proxy or tunnel, so a
tunnel works, and it has no Origin check. Its guard is the OAuth `state`
parameter: it must match a pending, unexpired flow this daemon started, and it
can be used once.

| Situation                                                                | Status | Page                                                                                                  |
| ------------------------------------------------------------------------ | ------ | ----------------------------------------------------------------------------------------------------- |
| Token stored                                                             | `200`  | `Authorized <connector>`: `Wayfindr is now authorized for <connector>.` `You can close this tab.`     |
| Unknown or already used `state`                                          | `400`  | `Authorization link not recognised`: `This authorization link is unknown or was already used.`        |
| The flow expired, was cancelled, was denied, or the code exchange failed | `400`  | `Authorization of <connector> failed`, with the advice and, when it helps, the auth link to try again |

## `/ui/` {#ui}

The [web console](/guide/console). Open it with
[`wayfindr ui`](./cli#wayfindr-ui), which signs your browser in.

| Route                                 | Does                                                                                                                                    |
| ------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /ui/`                            | Connectors, the ones that need you first. Refreshes every 10 seconds.                                                                   |
| `GET /ui/connectors/<name>`           | One connector and every tool its upstream serves now.                                                                                   |
| `POST /ui/connectors/<name>/sign-in`  | Start the connector's auth flow and redirect (`303`) to the authorize URL.                                                              |
| `POST /ui/connectors/<name>/sign-out` | Delete the connector's stored OAuth credential, as `wayfindr auth logout` does.                                                         |
| `GET /ui/calls`                       | The call log, `?connector=<name>` and `?result=failed` to filter.                                                                       |
| `GET /ui/search?q=<query>`            | What `search_tools` returns for the query, with each tool's scores.                                                                     |
| `GET /ui/tokens`, `POST /ui/tokens`   | List scoped client tokens; create one and show its value once.                                                                          |
| `POST /ui/tokens/<name>/revoke`       | Revoke a scoped client token.                                                                                                           |
| `GET /ui/add`, `POST /ui/add`         | The add form. A URL connector is added at once; a command connector gets a review page and is not added yet.                            |
| `POST /ui/add/confirm`                | Add the command connector the review page showed. Its token works once, for 5 minutes, in the session it was shown to; otherwise `403`. |
| `POST /ui/add/back`                   | Leave the review and show the form again, without its secret values.                                                                    |
| `GET /ui/login`, `POST /ui/login`     | Sign the browser in with the one-time code from `wayfindr ui`.                                                                          |
| `GET /ui/static/…`                    | The console's stylesheet, scripts, fonts, and logos.                                                                                    |

**Signing in.** `wayfindr ui` opens `/ui/login#<code>`. The code works once, for
60 seconds, and stays in the URL fragment, so it is never sent in a request
line. The login page posts it and gets a session cookie, `wayfindr_ui`:
`HttpOnly`, `SameSite=Strict`, `Path=/ui`. A session lasts until it is idle for
12 hours or the daemon stops. Without one, every page but the login page
redirects (`303`) there.

**Forms.** Every `POST` needs a session, `Origin: http://<Host>` exactly, and
the session's form token. A wrong or missing form token gets `403`
`This form is out of date. Reload the page and try again.`

**Headers.** Every answer has
`Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' https://cdn.jsdelivr.net; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'`,
`X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, and
`Referrer-Policy: same-origin`. Pages are `Cache-Control: no-store`. The one
outside host in the policy, `cdn.jsdelivr.net`, serves the connector logos; see
[Web console](/guide/console).

## `/admin/` {#admin}

The admin API that the `wayfindr` CLI uses to talk to the running daemon. It
sits behind the Host, Origin, and bearer checks, and takes only the default
client token: a scoped client token gets `403` on every `/admin/*` route.

```sh
curl -s http://127.0.0.1:7777/admin/status -H "Authorization: Bearer $SCOPED_TOKEN"
```

```
This client token cannot use the admin API.
```

::: warning
`/admin/*` exists for the `wayfindr` CLI and is not a stable API.
Every route below can change, or go away, in any release. Script against the CLI
instead; [`wayfindr status --json`](./cli#wayfindr-status),
[`wayfindr calls --json`](./cli#wayfindr-calls),
[`wayfindr search --json`](./cli#wayfindr-search),
[`wayfindr connector tools --json`](./cli#wayfindr-connector-tools),
[`wayfindr token list --json`](./cli#wayfindr-token-list), and
[`wayfindr secret list --json`](./cli#wayfindr-secret-list) are the supported
machine-readable output.
:::

Every route, all unstable:

| Route                                   | Used by                                                                                | Does                                                                                                                                                                                                                                                                    |
| --------------------------------------- | -------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /admin/status`                     | `status`, `scope`, and `connector add`, `edit`, `import`, and `remove` while they wait | The daemon's version, each connector's state and advice, profiles, orphaned credentials, and the config error.                                                                                                                                                          |
| `PUT /admin/secrets/{name}`             | `secret set`, `connector add`, `edit`, and `import`                                    | Store a secret, then reconnect the connectors that refer to it.                                                                                                                                                                                                         |
| `GET /admin/secrets`                    | `secret list`                                                                          | Each stored secret's name, never its value, and what in the running config refers to it (`used_by`).                                                                                                                                                                    |
| `DELETE /admin/secrets/{name}`          | `secret remove`, `connector remove`                                                    | Delete a secret.                                                                                                                                                                                                                                                        |
| `POST /admin/auth/{connector}`          | `auth login`                                                                           | Start a connector's auth flow; an optional body `{"device": true}` asks for the device code. The answer has a flow id and the authorize URL, or, for a device code flow, `user_code`, `verification_uri`, and `verification_uri_complete` (when the provider sent one). |
| `GET /admin/auth/flows/{id}`            | `auth login`                                                                           | Wait for that flow to finish, fail, or expire (`?wait=<duration>`).                                                                                                                                                                                                     |
| `DELETE /admin/credentials/{connector}` | `auth logout`, `connector remove`                                                      | Delete a connector's stored OAuth credential.                                                                                                                                                                                                                           |
| `GET /admin/calls`                      | `calls`                                                                                | The call log, filtered by `connector`, `since`, `errors`, and `limit`.                                                                                                                                                                                                  |
| `POST /admin/search`                    | `search`                                                                               | Run `search_tools` for a query, with each result's scores.                                                                                                                                                                                                              |
| `GET /admin/tools/{connector}`          | `connector tools`, `scope <connector>`                                                 | Every tool the upstream serves now, its class, its definition, and whether the tools list exposes it.                                                                                                                                                                   |
| `POST /admin/tokens`                    | `token create`                                                                         | Create a scoped client token. The answer has its value, once.                                                                                                                                                                                                           |
| `GET /admin/tokens`                     | `token list`                                                                           | List scoped client tokens, never their values.                                                                                                                                                                                                                          |
| `DELETE /admin/tokens/{name}`           | `token revoke`                                                                         | Revoke a scoped client token, from the next request on.                                                                                                                                                                                                                 |
| `POST /admin/ui/login`                  | `ui`                                                                                   | Issue a one-time login link for the web console.                                                                                                                                                                                                                        |
| `POST /admin/connectors`                | The web console                                                                        | Add a connector to the daemon's config file, store its secrets, and report it once it has connected.                                                                                                                                                                    |

A path it doesn't know gets `404`, and a known path with the wrong method `405`.
A route that refuses a request answers with a status and a one-line, plain-text
message, which the CLI prints as that sentence alone, such as
`No connector named "nope" is configured.` The bodies are the CLI's `--json`
output where it has one; see each command in the [CLI reference](./cli).
