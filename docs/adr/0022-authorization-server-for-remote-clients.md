# 0022 — Let cloud agents sign in to `/mcp` through an authorization server in the daemon

Status: Accepted (2026-11-22). Amends the remote-request decision of
[0021](./0021-remote-client-tokens): when `[server.oauth]` is on, a remote
request to `/mcp` without a valid token gets a 401 challenge instead of 404.
Amended:

- 2026-11-23 (wayfindr-nbp8): approval fails closed when more than one
  authorization request arrived during the arm, and turning OAuth off or on,
  changing `public_url`, or changing `clients` ends the arm and drops what it
  kept.
- 2026-11-23 (wayfindr-nbp8): the metadata, authorize, token, and the `/mcp`
  challenge answer a remote request only when it came through the tunnel, not
  a DNS-rebinding page.

## Context

ChatGPT, its always-on "dots" included, connects to a custom MCP server only
with OAuth or with no auth. Its form has no field for a header, so the remote
client tokens of [0021](./0021-remote-client-tokens) can't be given to it.
Grok Bot and Muse take a header and already work.

For ChatGPT, Wayfindr must be an OAuth authorization server for its own `/mcp`,
and `/mcp` must act as a protected resource. Until now Wayfindr has been an
OAuth client only, to its connectors' authorization servers
([0007](./0007-oauth-discovery-and-registration),
[0018](./0018-grants-client-authentication-sender-constraints)).

ChatGPT asks for (checked 2026-11-22, and it changes):

- Protected resource metadata (RFC 9728), found through the
  `resource_metadata` in a 401's `WWW-Authenticate`, naming one authorization
  server whose metadata (RFC 8414) has the same `issuer`.
- The authorization code grant with PKCE `S256`.
- A Client ID Metadata Document (CIMD): its `client_id` is an https URL,
  `https://chatgpt.com/oauth/client.json`, whose document lists its redirect
  URIs. Dynamic registration is a fallback.
- Token endpoint authentication `none` (a public client with PKCE) or
  `private_key_jwt`.
- A resource indicator (RFC 8707) on authorize and token requests: the
  canonical `/mcp` URL that the protected resource metadata names.
- Optionally, RFC 9207: an authorization server that advertises
  `authorization_response_iss_parameter_supported` must send `iss` on every
  authorization response. ChatGPT then uses its stable `client.json` and one
  redirect URI, and rejects a missing or wrong `iss`.

ChatGPT opens the authorization endpoint in the user's browser, and its own
servers call the token endpoint. Through the user's tunnel, both are on the
public URL. So the page that asks for consent is on the internet, yet nobody on
the internet may be able to grant access there. The console, the admin API,
and auth links are loopback-only, and that is the security model
([0016](./0016-local-web-console), [0021](./0021-remote-client-tokens)).

## Decision

- **Opt-in, in a `[server.oauth]` table.** It follows `[telemetry]`'s
  `enabled` pattern:

  ```toml
  [server.oauth]
  enabled = true
  clients = ["https://chatgpt.com/oauth/client.json"]  # the default when omitted
  ```

  `enabled` needs `server.public_url`. Off, every answer is byte for byte what
  it was before this record. A reload applies a change; no restart. The
  console's Cloud agents page turns `enabled` on and off; it never edits
  `clients`.
- **Allowed clients.** `clients` lists the client IDs, CIMD URLs, that may start
  an authorization, matched exactly, with no patterns. The entries are checked
  at load (the rules are in the [technical
  design](https://github.com/atomicpages/wayfindr/blob/main/docs/internal/design/7.11-oauthserver.md)).
  A `client_id` that isn't listed is refused before anything else, even a
  repeated parameter: an error page, and no outbound fetch, no pending
  authorization, no redirect. A stranger can't make the daemon fetch a URL or
  fill its queue of pending authorizations, except under an allowed ID, whose
  document is cached. Removing a client from `clients` suspends its grants:
  their access tokens stop working and their refreshes are refused until it is
  listed again.
- **Registration is CIMD only.** The daemon fetches an allowed client's
  document, requires its `client_id` to be the URL it came from, and requires
  the `redirect_uri` to be listed in it exactly. Token endpoint authentication
  is `none` with PKCE `S256`: the token endpoint refuses any other client
  authentication. There is no dynamic registration endpoint.
- **Consent is a loopback redirect.** The public authorization endpoint only
  checks the request and keeps it pending. It then redirects the browser to the
  console on the daemon's loopback address, where the user approves it: with a
  console session and its CSRF token, as for every console form. The page shows
  the client's name and host, the host the browser will return to, when the
  request started, and the IP address the tunnel reported for the browser, and
  the user picks the connectors the grant may use. Approve or Deny then
  redirects the browser to the client's `redirect_uri` with a `code` or an
  `error`, the `state`, and `iss`. The browser on the Wayfindr machine must be
  the one that opens the approve page; there is no approval from another
  device. The console lists no pending authorizations.
- **Connecting is armed from the console.** The authorization endpoint accepts
  a request only while the user has armed it: Connect, in the console's Cloud
  agents page, with a session and CSRF token. An arm lasts 10 minutes and there
  is one at a time. While armed, every request that passes every check is kept
  pending, at most 8 (the oldest goes); the endpoint never consumes the arm.
  The console does: the user's signed-in approve page claims the request it was
  opened for, which ends the arm and drops every other pending authorization.
  The client ID and its `redirect_uri` are public, so anyone can send a valid
  request; were the first one to consume the arm, a stranger polling the
  endpoint would take it within a second of the user's Connect and keep the
  user's own request out. Under the claim, a stranger's requests only sit beside
  the user's, and only the user's browser, following the redirect, brings an id
  to the console. Without an arm the endpoint answers an error page and fetches
  nothing. A browser request that is not a top-level page navigation (a
  `no-cors` request, an iframe, a prefetch or prerender) is refused. Turning
  `[server.oauth]` off or on, changing `public_url`, or changing `clients` ends
  the arm and drops the pending and claimed requests and any code not yet
  exchanged, so none outlives the settings it was made under (amended
  2026-11-23, wayfindr-nbp8).
- **Consent phishing is narrowed, not ruled out.** A stranger can start an
  authorization from their own ChatGPT account against the user's public URL
  and send the user the link. Approving it would give the stranger's account
  a grant. The arm limits this to the minutes after the user chose Connect,
  and the user's claim of their own request drops the stranger's. The approve
  page says when the request started and from which IP the
  tunnel saw it, and always warns: approve only if you clicked Connect in the
  client in the last few minutes; if someone sent you this link, deny it.
  Whether the client binds `state` to the browser that started the flow,
  which would stop this on the client's side, is the client's affair and
  can't be checked from Wayfindr.

  It is narrowed further (amended 2026-11-23, wayfindr-nbp8): a page the user
  has open can open the approve page with a stranger's request id in a new
  tab while the user is armed, and the user, expecting that page, selects
  Continue as in the real flow, which claims the stranger's request. So the
  daemon counts the requests that became pending during the arm, and only one
  is expected, the user's own. With more than one, approving fails closed:
  the approve page says how many arrived and offers only Deny, and the user
  arms again and retries. A stranger's request then costs the user a retry,
  never a grant. What is left is a stranger's request that is the only one in
  the arm and that the user approves despite the page; the "Continue" page and
  the approve page both say to deny a link someone sent or a tab the user
  didn't open.
- **What a grant reaches: the connectors picked.** There are no OAuth scopes,
  and the metadata names none (`scopes_supported` is left out), since ChatGPT
  asks for every advertised scope by default.
- **Grants are client tokens to the user.** A grant has a name in the name
  space scoped client tokens use, and its access token resolves to a remote
  scoped client: its name, its connectors, `Remote` set. So it reaches `/mcp`
  through a tunnel the way a remote client token does, and the admin API
  refuses it. `wayfindr token list` and the console's Tokens page show grants
  next to tokens, as kind `oauth` with the client's host. `wayfindr token
  revoke <name>` deletes a grant and its tokens, and refuses it from the next
  request on. The call log names the grant behind each call.
- **Tokens are opaque.** Access and refresh tokens are random values with the
  scoped token's `wf_s_` prefix. Only their SHA-256 is stored. Wayfindr is both
  the authorization server and the resource server, so there are no JWTs and
  no JWKS.
- **Lifetimes.** An access token lasts 1 hour. A refresh token doesn't expire,
  and is spent by its use: a refresh returns a new refresh token with the new
  access token, and the grant's earlier access token stops working. A code
  lasts 60 seconds and is spent by its first presentation. A spent refresh
  token is kept for 30 days and a spent code remembered for 10 minutes, and
  either one presented again in that time revokes the whole grant, except
  that a refresh token presented within 30 seconds of its spend is only
  refused, so concurrent refreshes by one client don't cost it the grant.
  Refresh tokens get no idle limit (see Considered options).
- **The resource is `<public_url>/mcp`, matched exactly.** The protected
  resource metadata names it, and authorization and token requests must
  carry it. A grant is bound to the resource it was approved for. When
  `public_url` changes, the grant is suspended: its tokens stop working and
  its refreshes are refused until `public_url` is set back. A client that
  can't wait connects again, which makes a new grant. Turning
  `[server.oauth]` off suspends every grant the same way until it is turned on
  again.
- **The issuer is `public_url` without a trailing slash**, the same string in
  the metadata, in `authorization_servers`, and in every `iss`.
- **RFC 9207.** The metadata advertises
  `authorization_response_iss_parameter_supported`, and every authorization
  response that goes to a `redirect_uri`, errors included, carries `iss`.
- **The tunnel is the user's.** The guide covers ngrok, Cloudflare Tunnel, and
  Tailscale Funnel, as for [0021](./0021-remote-client-tokens). OpenAI's
  Secure MCP Tunnel is documented as unsupported.
- **What the tunnel reaches with OAuth on.** The two protected resource metadata
  paths (`/.well-known/oauth-protected-resource` and the same with `/mcp`
  appended), `/.well-known/oauth-authorization-server`, `/oauth/authorize`, and
  `/oauth/token` are served to remote requests, with no token. A remote request
  to `/mcp` without a valid remote token gets 401 with
  `WWW-Authenticate: Bearer resource_metadata="…"`, naming
  `<public_url>/.well-known/oauth-protected-resource/mcp`, and
  `error="invalid_token"` when it carried a token. Everything else is as
  [0021](./0021-remote-client-tokens) decided: the console, the admin API, and
  auth links stay loopback-only, and every other remote request gets 404. A
  local request to `/mcp` is unchanged: a missing or wrong token gets 401 with
  `WWW-Authenticate: Bearer` and no parameters.

  Only a remote request that came through the tunnel gets these routes and the
  401 (amended 2026-11-23, wayfindr-nbp8): its `Host` is the public URL's
  host, or it is a loopback `Host` with the daemon's port and a forwarding
  header, as from a tunnel that rewrites `Host`. A DNS-rebinding page's
  request, its own `Host` and no forwarding header, is remote but gets the 404
  on these too, so it can't read `public_url` from the metadata or the
  challenge. The remote-token admission of [0021](./0021-remote-client-tokens)
  doesn't look at `Host` and is unchanged.

## Considered options

- **A match code, as in device flows.** The public page would show a short
  code, and the console would list pending authorizations with the same code to
  approve. It relies on the user comparing the codes: a stranger's look-alike
  request is stopped only if the user checks, and a stranger can fill the
  list. The loopback redirect sends the code only to the browser that
  approved, and the arm keeps strangers' requests out except in the minutes
  after the user's own Connect. Neither removes the user's judgement entirely
  (see the consent-phishing decision); the approve page's start time, reported
  IP, and warning support it. A match code also costs cross-device approval,
  which v1 doesn't need.
- **An idle limit on refresh tokens**, such as ending a grant unused for 30
  days. Cloud agents are always on, and some are used rarely (a dot that runs
  weekly or monthly); a limit would end their grants with no security event
  and make the user connect again. Rejected. The residual risk: a refresh
  token copied out of the client keeps working until the grant is revoked, or
  until reuse detection sees both copies used.
- **A password on the public page.** An internet-facing login, open to brute
  force and phishing, guarding everything the grant reaches.
- **A loopback authorization endpoint.** ChatGPT likely refuses a non-public
  endpoint in the metadata, and it breaks for a browser on another device
  even when the redirect is to that device.
- **Any https CIMD client.** Consent is local and shows who is asking, but
  every stranger could make the daemon fetch a URL and fill its pending queue.
  An allowlist stops both, at the cost of a config edit for each new client.
- **Dynamic client registration.** ChatGPT prefers CIMD. Registration is an
  open write endpoint on the internet, with storage and expiry to manage, for
  no client that needs it now.
- **JWT access tokens.** The resource server is the same process. A JWT adds a
  signing key to keep, rotate, and publish, and can't be revoked before it
  expires; an opaque token is revoked by deleting a row.
- **OpenAI's Secure MCP Tunnel.** It would replace the user's tunnel with one
  OpenAI runs, for one vendor. Wayfindr keeps one way in for every cloud agent.

## Consequences

- ChatGPT can use Wayfindr through the user's tunnel, limited to the
  connectors approved for it. Turning `[server.oauth]` on is the only step
  beyond setting the public URL.
- With OAuth on, the tunnel shows that a Wayfindr `/mcp` is there: the 401 and
  the metadata documents answer anyone. Without a grant they reach nothing.
  With OAuth off, nothing changes.
- The daemon now makes one more kind of outbound request: a GET of an allowed
  client's metadata document, through a dialer that refuses every address that
  isn't public (loopback, private, link-local, unspecified, reserved and shared
  ranges, and the NAT64 and 6to4 ranges that can embed them; the full list is in
  the [technical
  design](https://github.com/atomicpages/wayfindr/blob/main/docs/internal/design/7.11-oauthserver.md)),
  following no redirect, with a size and time limit. It is never made for a
  client that isn't allowed.
- Adding a client other than ChatGPT means adding its CIMD URL to `clients`.
  A client that can only register dynamically, or authenticate with
  `private_key_jwt` alone, can't connect.
- Connecting needs the Wayfindr machine's browser, signed in to the console.
  A user who starts the connection elsewhere can't approve it.
- Connecting takes one more step: choose Connect in the console before adding
  the connector in the client, and finish within 10 minutes. A stranger who
  sends authorization requests in that window can't use up the arm or reach
  the console; the user's own request is claimed and the stranger's dropped.
  When a stranger's request arrived in the same arm, the user's claim can't be
  approved and the user connects again (amended 2026-11-23, wayfindr-nbp8).
  A stranger who floods more than 8 requests in the seconds between the
  user's redirect and the approve page can evict the user's, and the user
  starts again in the client. Outside that window a stranger's request gets an
  error page and costs the daemon nothing.
- A client that presents a spent refresh token more than 30 seconds after
  its spend, even by retrying a request that timed out, loses its grant and
  must connect again. Reuse is seen for 30 days after a refresh token is
  spent and 10 minutes after a code is; later, the old value is only unknown.
  Within the 30-second grace a reuse isn't caught: a thief who refreshes
  first and is followed by the client within 30 seconds keeps the grant until
  the client's next attempt with the old token revokes it.
- Changing `public_url` suspends every OAuth grant until it is set back.
- Access tokens expire, unlike client tokens. A grant is revoked like a token,
  and lasts until it is.
