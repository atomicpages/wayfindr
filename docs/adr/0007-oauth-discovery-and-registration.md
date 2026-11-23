# 0007 — OAuth discovery chain, client registration order, and redirect handling

Status: Accepted (2026-11-03). Amended:

- 2026-11-04 (wayfindr-i2a): a `client_secret` without a `client_id` is a
  configuration error.
- 2026-11-06 (wayfindr-j99t): the default redirect URI follows `server.listen`
  (`http://<listen>/oauth/callback`), not a fixed port.
- 2026-11-06 (wayfindr-zgp.24): with no metadata, S256 is unknown rather than
  unsupported, so a discovered connector sends it anyway.
- 2026-11-12 (wayfindr-b05v): issuer validation ignores a terminating `/`.
- 2026-11-12 (wayfindr-b05v): a connector's `authorize_params` add query
  parameters to the authorization request.
- 2026-11-12 (wayfindr-b05v): a server that answers without a token but
  publishes Protected Resource Metadata naming an authorization server uses
  OAuth.
- 2026-11-12 (wayfindr-3e6f): a grant is bound to the OAuth client that got it;
  a manual connector whose `client_id` changed needs auth again.
- 2026-11-12 (wayfindr-q2rb), corrected: only `validate_issuer = false` logs a
  warning; `pkce = false` doesn't.
- 2026-11-17 (wayfindr-9fep): a DCR request names a `token_endpoint_auth_method`
  and an `application_type`, and Wayfindr authenticates with the method the
  provider registered
  ([0018](./0018-grants-client-authentication-sender-constraints)).
- 2026-11-17 (wayfindr-5vps): `dpop_jkt` joins the authorize parameters Wayfindr
  sets itself ([0018](./0018-grants-client-authentication-sender-constraints)).
- 2026-11-21 (wayfindr-z5q8): a request on a non-loopback `Host` also reaches `/mcp` when it
  carries a remote client token ([0021](./0021-remote-client-tokens)).

## Context

Supporting MCP servers that don't implement DCR is Wayfindr's core value.
Real-world authorization servers vary: some have no RFC 8414 metadata, some
don't advertise PKCE, some report an `issuer` that doesn't match the URL it was
discovered at (e.g. Google), and some reject loopback redirect URIs.

## Decision

**Discovery.** Explicit endpoints in config (`authorization_server`,
`authorization_endpoint`, `token_endpoint`) override any step below.

1. Protected Resource Metadata (RFC 9728). Try, in order:
   - the `resource_metadata` URL from a 401's `WWW-Authenticate` header
   - `/.well-known/oauth-protected-resource/{mcp-path}`
   - `/.well-known/oauth-protected-resource`

   This provides `authorization_servers[]` (use the first) and
   `scopes_supported`.

2. Authorization server metadata. For an issuer with a path, try in order:
   - `/.well-known/oauth-authorization-server/{path}`
   - `/.well-known/openid-configuration/{path}`
   - `{path}/.well-known/openid-configuration`

   For an issuer without a path, try the first two at the root.

3. Legacy fallback (2025-03-26): treat the MCP origin as the authorization
   server. If it has no metadata, use the default `/authorize`, `/token`, and
   `/register` endpoints.

Discovered metadata is cached in the store and refetched after an auth failure.

`wayfindr connector add` probes a URL to choose its auth. A 401 with a Bearer
challenge means OAuth. So does a success from a server that publishes Protected
Resource Metadata naming an authorization server at a well-known URL: Google's
MCP servers answer `initialize` without a token and refuse only `tools/call`
(amended 2026-11-12, wayfindr-b05v).

**Issuer validation.** By default, the metadata's `issuer` must exactly match
the discovered issuer URL (RFC 8414 §3.3), except that a terminating `/` is
ignored: RFC 8414 §3.1 drops it before building the metadata URL, so both forms
name the same document. Google's PRM lists `https://accounts.google.com/` while
its metadata says `https://accounts.google.com` (amended 2026-11-12,
wayfindr-b05v). A connector can set `validate_issuer = false`. The daemon logs a
warning each time it skips the check.

**Scopes.** Use the `scope` from `WWW-Authenticate`, else the PRM
`scopes_supported`, else the `scopes` from config. Step-up is described in
[0004](./0004-lazy-single-flight-refresh).

**Resource indicator.** The `resource` parameter (RFC 8707) is sent on authorize
and token requests. A manual connector can set `resource_indicator = false`.

**Authorize parameters.** A connector's `authorize_params` adds query parameters
to the authorization request, for providers that need ones outside OAuth, such
as Google's `access_type = "offline"` for a refresh token. Parameters Wayfindr
sets itself (`response_type`, `client_id`, `redirect_uri`, `state`, `scope`,
PKCE's, and `resource`) are refused by validation and set after
`authorize_params`, so they can't be overridden (amended 2026-11-12,
wayfindr-b05v). So is DPoP's `dpop_jkt` (amended 2026-11-17, wayfindr-5vps).

**PKCE.**

- Discovered connectors (no `client_id` in config): use S256 if
  `code_challenge_methods_supported` advertises it. Otherwise, move the
  connector to `auth_error`. With no metadata (the 2025-03-26 legacy defaults)
  S256 is unknown rather than unsupported: send S256 anyway; only metadata that
  lacks S256 moves the connector to `auth_error` (amended 2026-11-06,
  wayfindr-zgp.24).
- Manual connectors (`client_id` set): `pkce` defaults to on, even without
  metadata. `pkce = false` is allowed only when `client_secret` is set.

**Client registration order.**

1. A pre-registered `client_id` from config. A `client_secret` without a
   `client_id` is a configuration error (amended 2026-11-04, wayfindr-i2a).
2. CIMD, only if the user sets `client_metadata_url` to a document they host and
   the authorization server advertises `client_id_metadata_document_supported`.
3. DCR via `registration_endpoint`. The request asks for `none` client
   authentication when the server lists it, else a client secret, and names
   `application_type` (`native` for a loopback redirect URI, `web` otherwise).
   Wayfindr then authenticates with the method the server registered (amended
   2026-11-17, wayfindr-9fep).
4. Otherwise, `auth_error` ("register an app and set `client_id`").

**Client binding.** A grant records the client it was obtained with: the
configured `client_id`, the CIMD URL, or the DCR client. When a manual
connector's `client_id` no longer matches the recorded client, the token is
never sent and the connector needs auth again, with Case `client_changed`. A
changed `client_secret` alone doesn't count. A row from before the binding has
no client recorded and is accepted; its next refresh records the client.
Discovered connectors aren't checked: their client is Wayfindr's own choice and
changes on re-registration, while the grant stays good until a refresh, which
fails on its own if the client is wrong (amended 2026-11-12, wayfindr-3e6f).

**Token endpoint binding.** Discovery runs again on refresh and follows what the
MCP server names, so a hostile server could point the refresh at an
authorization server of its own and collect the refresh token and the client's
credential. A grant therefore records the token endpoint it was obtained at, and
a refresh that discovers another one sends nothing: the connector needs auth
with Case `authorization_server_changed`, and the sign-in shows the user the new
server. Unattended grants keep no token to bind, so their config must name the
server with `authorization_server` or `token_endpoint` (amended 2026-11-18,
wayfindr-afb1).

**Redirect URI.** The default is `http://127.0.0.1:7777/oauth/callback` (amended
2026-11-06, wayfindr-j99t: it follows `server.listen`, as
`http://<listen>/oauth/callback`). A connector can override it with
`redirect_uri`, e.g. an ngrok URL for providers that reject loopback addresses.
The URI in effect is also the one used for DCR. On any request whose `Host` is
not loopback, the daemon serves only `/oauth/callback`; `/mcp` and `/auth/*`
refuse it. (`/mcp` serves such a request when it carries a remote client token,
and refuses it with 404 otherwise; amended 2026-11-21, wayfindr-z5q8, see
[0021](./0021-remote-client-tokens).) The callback must match a pending,
unexpired `state` issued by this daemon.

## Consequences

- Supports spec-compliant servers, 2025-03-26 servers, and bring-your-own-app
  providers.
- `validate_issuer = false` and `pkce = false` weaken security. They are opt-in,
  per connector, and logged. (Corrected 2026-11-12, wayfindr-q2rb: only
  `validate_issuer = false` logs a warning, each time the check is skipped.
  `pkce = false` shows only as `"pkce": false` on the info line that starts a
  flow.)
- Changing a manual connector's `client_id` makes its token unusable, so the
  user authorizes again rather than meeting a refresh failure later (amended
  2026-11-12, wayfindr-3e6f).
- The project hosts no CIMD document. Revisit once a project domain exists.
