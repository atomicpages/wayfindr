# 0021 — Let cloud agents reach `/mcp` through a tunnel with remote client tokens

Status: Accepted (2026-11-21). Amends the client-auth decision of
[0003](./0003-single-daemon), the scoped client tokens of
[0012](./0012-scoped-client-tokens), and the redirect handling of
[0007](./0007-oauth-discovery-and-registration): a request through a tunnel can
reach `/mcp`, with a remote client token only. Amended:

- 2026-11-22 (wayfindr-z5q8): the go-sdk's own localhost `Host` check is turned
  off, so the host guard alone decides which requests are remote.

## Context

Some agents run in a vendor's cloud, never on the user's machine: Grok Bot and
Meta Muse now, ChatGPT later. They are always on, and they connect to MCP
servers over HTTPS. None of them has an API Wayfindr could call, so the only
way to use them with Wayfindr is as clients.

Today they can't reach the daemon. It binds to loopback only
([0003](./0003-single-daemon)). A request through a proxy or tunnel counts as
foreign whatever its `Host` says, because it carries a forwarding header
(wayfindr-rrvq), and a foreign request gets 404 on every path but
`/oauth/callback` ([0007](./0007-oauth-discovery-and-registration)). A user who
points a tunnel at the daemon gets the callback and nothing else.

Opening `/mcp` to every request through a tunnel would put the default client
token on the internet, with every connector behind it. Every client token today
was made for a local client. The user should choose which token may come in
from outside, and that token should reach only the connectors its agent needs.

The vendors authenticate differently. Grok Bot (v0.79 and later) takes a custom
connector with an `Authorization` header. Muse asks for a credential when a
custom connector is created in chat, and keeps it. ChatGPT takes no header: a
connector there uses OAuth or no auth.

## Decision

- Cloud agents are clients. Wayfindr adds no upstream adapter for them.
- Wayfindr stays local. The user runs the tunnel (ngrok, Cloudflare Tunnel,
  Tailscale Funnel, or a proxy of their own) and points it at the daemon.
  Wayfindr never starts, stops, or configures one.
- A scoped client token can be **remote**. The console makes one, and so does
  `wayfindr token create <name> --connector … --remote`. Only a remote token
  lets a request in through a tunnel. In every other way it is a scoped token
  ([0012](./0012-scoped-client-tokens)): it reaches only its connectors, and the
  admin API refuses it. Its value keeps the `wf_s_` prefix. `remote` is stored
  with its digest, and `wayfindr token list` shows it.
- Only a scoped token can be remote. The default client token never is.
- `remote` is set when the token is created and can't be changed, like its
  connectors. To change it, revoke the token and create another.
- A remote token works locally too, like any scoped token.
- Remote tokens don't expire, like every client token. A token is never taken
  from the URL: query strings end up in tunnel, proxy, and vendor logs.
- **Remote request.** A request is remote when it carries a forwarding header,
  or its `Host` isn't a loopback name with the daemon's port. That is the test
  the host guard applied before this record, with `Via` added (below). The
  guard alone makes this decision (amended 2026-11-22, wayfindr-z5q8): the
  go-sdk's own localhost `Host` check is turned off, since it would refuse every
  request whose `Host` isn't loopback.
  The guard then decides:
  - On `/mcp`, a remote request proceeds if and only if its bearer resolves to
    a remote scoped token, under the same one-header rule as a local request.
    From there it gets every check a local request gets: a present,
    non-loopback `Origin` is still 403, the token narrows it, and `GET` and
    `DELETE` still get 405.
  - Any other remote request to `/mcp` gets the same `404 page not found` a
    foreign request gets today: no token, a wrong or revoked one, the default
    token, or a scoped token that isn't remote. There is no 401 and no
    `WWW-Authenticate`, so a scanner learns nothing about the path, and a
    client that would start OAuth discovery isn't invited to.
  - Every other route is unchanged. `/admin/`, `/ui/`, `/auth/<connector>`, and
    `/healthz` stay loopback-only, and a remote request gets 404 there whatever
    token it carries. `/oauth/callback` is still served for any `Host`.
  - A local request is unchanged. A missing or wrong token on `/mcp` still gets
    401 with `WWW-Authenticate: Bearer`.
- **A tunnel must add a forwarding header** for its requests to count as
  remote. ngrok, cloudflared, and Tailscale Funnel do. A proxy that rewrites
  `Host` to the loopback address and adds none looks local, and then gets
  everything a local client gets (wayfindr-gz1y). The guide says which header
  to add for each proxy that needs one.
- **`Via` counts as a forwarding header.** RFC 9110 has every proxy add it, but
  many don't: Squid and some CDNs do, nginx and Apache don't unless configured.
  A local client sends none, unless it goes through a debugging proxy that adds
  one. Each mistake costs something different. A local request taken for
  remote fails closed: it gets 404, or needs a remote token. A remote request
  taken for local fails open: the default token, the console, and auth links
  are in reach. So the guard errs toward remote, and `Via` joins the list with
  the guard change for this record. It doesn't close wayfindr-gz1y: a plain
  nginx `proxy_pass` adds no `Via` either.
- **`server.public_url`** is the `https` address the tunnel serves. It is
  stored in the config and is informational only: the guard never compares a
  request against it. The token is what lets a request in, and a tunnel can
  rewrite `Host` anyway. The console uses it for setup text and to test the
  connection. A future authorization server in the daemon would use it as its
  issuer.
- `call_tool` keeps no `readOnlyHint` ([0001](./0001-two-meta-tools)). A cloud
  agent may ask before every call. The guide documents what each vendor
  confirms, rather than the tool changing for them.
- A team bot shares its connectors with everyone on the team, so they could
  all use the user's connectors through its token. The guide says to use a
  personal bot. Wayfindr can't tell the two apart and doesn't try.
- ChatGPT waits for an OAuth authorization server in the daemon (wayfindr-73at),
  with its own record. Then an unauthenticated remote request to `/mcp` would
  get 401 with `WWW-Authenticate: Bearer resource_metadata=…`, so the client
  can find the server, instead of 404.

## Consequences

- An always-on cloud agent that can send a bearer header can use Wayfindr
  through a tunnel the user runs, limited to its token's connectors.
- Nothing changes until the user creates a remote token. A tunnel already set up
  for the OAuth callback still reaches only the callback with every existing
  token.
- Through a tunnel, the token is the only defense. The Host and Origin checks
  stop web pages in a local browser; they do nothing against a client holding
  the token. A leaked remote token works from anywhere until it is revoked, and
  a revoke applies from the next request on. Its value is 32 random bytes, and
  the call log names the token behind each call, so what the agent did can be
  read back.
- Managing Wayfindr stays local: the admin API, the console, and auth links are
  never reachable through a tunnel, whatever the token.
- A wrong token through a tunnel gets the same 404 as a wrong URL. That is less
  helpful to someone setting up an agent, so the console's connection test and
  the guide have to cover it.
- Whether a request counts as remote depends on the tunnel adding a header.
  The guide's tunnel recipes, and its checklist for other proxies, name the
  header and how to check for it.
- ChatGPT can't connect until the authorization server exists.
