# Security

Wayfindr holds the keys to every MCP server you use, so it is built to keep them
on your machine and inside the daemon. This page says what it protects, how, and
where the edges are.

## What it defends against

| Threat                                                                 | Defense                                                                                                                                                                                                                                                                    |
| ---------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Another machine on your network calls the daemon                       | It binds to loopback only, and checks the address it actually bound.                                                                                                                                                                                                       |
| A web page in your browser calls `127.0.0.1:7777`                      | Host and Origin checks, and the client token, which a page can't read.                                                                                                                                                                                                     |
| Another user on the machine takes the port while the daemon is stopped | The CLI and the stdio shim send the client token only to a daemon that holds the lock in your data directory, which other users can't open. A client you set up over HTTP, with the token in its environment, does no such check: on a shared machine, use the stdio shim. |
| DNS rebinding                                                          | A request whose `Host` isn't a loopback name or IP on the daemon's port gets 404 on every path but the OAuth callback and, with a remote client token, `/mcp`. The sign-in paths answer only your public URL's `Host`, or a request a tunnel forwarded ([Loopback only](#loopback-only)), so a rebinding page can't learn your public URL. |
| A tunnel for the OAuth callback exposes the daemon                     | A tunnel reaches only the callback and, with a remote client token, `/mcp`. See [Loopback only](#loopback-only).                                                                                                                                                           |
| A cloud agent's remote client token leaks                              | It reaches only its connectors, never the admin API, the console, or auth links. Revoke it, and the next request with it gets `404`.                                                                                                                                       |
| Turning on ChatGPT's sign-in exposes more through the tunnel               | Only the sign-in information (public by design), `/oauth/authorize`, and `/oauth/token`, and only while `server.oauth.enabled` is true. None of them grants access by itself: see [Cloud agents that sign in](#cloud-agents-that-sign-in). Off, every answer is what it was before. |
| A stranger starts a sign-in to your public address and sends you the link | You must have selected **Connect a client** in the last 10 minutes, or the endpoint answers an error page and keeps nothing. When you open the approve page for your own request, every other waiting request is dropped. The page shows when the request started and the IP your tunnel saw, and tells you to deny a link someone sent you. This narrows the risk and does not rule it out: it still depends on you. |
| A web page you have open opens the approve page for a stranger's request while you connect | The page claims nothing until you select **Continue**, and says to continue only if you just selected Connect in an app. If you continue anyway, your own request arrives too: when more than one request arrived after you selected **Connect a client**, the approve page offers only Deny. You connect again, so it costs you a retry, never a grant. |
| A stranger makes Wayfindr fetch a web address or fill its queue         | Only the client IDs in `server.oauth.clients` are looked at (ChatGPT's, by default). Any other gets an error page before Wayfindr fetches or keeps anything. |
| An approval comes from somewhere other than your computer              | The approve page is part of the console, which answers only on your computer's loopback address, and needs a console session and its form token. No request through the tunnel reaches it. |
| An OAuth grant for a cloud agent leaks                                 | It reaches only the connectors you ticked, never the admin API or the console. `wayfindr token revoke <name>` deletes the grant, and the next request with its token is refused. Its tokens are random values stored only as hashes. |
| Someone copies your database file                                      | Secrets and tokens in it are encrypted, and the master key is normally stored elsewhere.                                                                                                                                                                                   |
| An agent or a prompt injection tries to read credentials               | No tool returns them. Agents only get tool definitions, results, and advice.                                                                                                                                                                                               |
| A prompt injection takes over an unattended agent                      | Give it a scoped client token: it reaches only its connectors, and no admin API.                                                                                                                                                                                           |
| A token leaks to the wrong server                                      | Each OAuth token is bound to its connector's URL and only sent there.                                                                                                                                                                                                      |
| A stolen OAuth token is replayed elsewhere                             | With DPoP or a certificate-bound token, it is useless without Wayfindr's key or certificate.                                                                                                                                                                               |
| A hostile server shows you a phishing page for a device code           | Wayfindr shows or opens only an `https` address, or `http` on loopback.                                                                                                                                                                                                    |
| An upstream's tool schema makes Wayfindr fetch something               | Schemas are validated with remote and file `$ref` loading turned off.                                                                                                                                                                                                      |
| Secrets end up in logs or bug reports                                  | Wayfindr redacts header values, tokens, auth codes, secret values, and URL query strings and user info before it logs them. A `${…}` in a connector's URL shows as the reference.                                                                                          |

What it does not defend against: other processes running as your user. Any
program that can read your home directory can read the default client token and
talk to the daemon, just as it could read your shell's API keys. Run untrusted
code under another account or in a container.

## Loopback only

`server.listen` must be a loopback address: an IP in `127.0.0.0/8`, `::1`, or
the name `localhost`. Anything else is a config error:

```text
Error: load config /Users/you/.config/wayfindr/wayfindr.toml: invalid config: server.listen: host "0.0.0.0" must be a loopback address such as 127.0.0.1
```

Wayfindr never resolves names to check them. Because a hosts file can map
`localhost` anywhere, the daemon also checks the address it actually bound, and
refuses to serve with `listen address is not loopback` if it isn't.

There is no setting to listen on another interface. Wayfindr treats a request as
coming through a tunnel or proxy (a _remote_ request) when its `Host` isn't a
loopback name with the daemon's port, or when it carries a `Forwarded`, `Via`,
`X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Real-IP`, or
`CF-Connecting-IP` header. A proxy that rewrites `Host` to `127.0.0.1` doesn't
change that: the forwarding header still marks the request.

**What answers through a tunnel.** Exactly two things:

- `/oauth/callback`, with no token, for a sign-in you started in the last 10
  minutes. See [The OAuth callback](#the-oauth-callback).
- `/mcp`, only with a scoped client token you created as
  [remote](./access/scoped-tokens#remote-tokens), for a
  [cloud agent](./cloud-agents), and then only that token's connectors. With
  ChatGPT's sign-in on, an OAuth grant you approved works here the same way.

While `server.oauth.enabled` is true, five more paths answer, with no token:
`/.well-known/oauth-protected-resource`, the same with `/mcp` after it,
`/.well-known/oauth-authorization-server`, `/oauth/authorize`, and
`/oauth/token`. They answer only a request whose `Host` is your public URL's
host, or one with a loopback `Host` and a forwarding header (a tunnel that
rewrites `Host`). Any other `Host`, such as a DNS-rebinding page's, gets `404`
on them, and on `/mcp` without a remote token, so it can't learn your public
URL. See [Cloud agents that sign in](#cloud-agents-that-sign-in).

Everything else gets `404 page not found`, whatever token it carries: the admin
API, the console, auth links, and `/healthz`. So does `/mcp` with no token, a
wrong or revoked token, or any token that isn't remote, the default client token
included. With sign-in off it is never `401`, so a request from the internet
can't learn which tokens exist. With sign-in on, those requests to `/mcp`
through your tunnel get `401` with a challenge that names the sign-in
information, which ChatGPT needs;
it is the same answer for every token, so it still tells a stranger nothing about
which tokens exist.

A tunnel must add one of the headers above, or its requests look local and the
default client token would work through it; ngrok, Cloudflare Tunnel, and
Tailscale Funnel add them. The [Cloud agents guide](./cloud-agents) sets up a
tunnel and a remote token step by step, and says which header each proxy needs.
Through a tunnel the token is the only defense, so give it only the connectors
its agent needs, keep it out of chats and logs, and revoke it when you stop using
the agent. See [ADR 0021](../adr/0021-remote-client-tokens).

## Client tokens

Clients authenticate with a client token as a bearer token:

```http
Authorization: Bearer wf_…
```

The **default client token**, which `wayfindr init` writes, is 32 random bytes
and can use every connector and the admin API. The daemon compares tokens in
constant time. Where it lives and how to override it are in
[Paths and environment](/reference/paths-and-environment#client-token). To
replace it, see
[Rotating the client token](./running#rotating-the-client-token).

Every route also needs a `Host` that names a loopback name (`127.0.0.1`,
`localhost`, `[::1]`, or the address the daemon bound) with the port the daemon
listens on, except what a tunnel may reach (see
[Loopback only](#loopback-only)).

| Path                         | Needs                                                                                           |
| ---------------------------- | ----------------------------------------------------------------------------------------------- |
| `/mcp`                       | A client token, loopback `Host`, no foreign `Origin`. Through a tunnel: a remote client token.  |
| `/admin/…` (used by the CLI) | The default client token, loopback `Host`, no foreign `Origin`.                                 |
| `GET /healthz`               | Loopback `Host`, no foreign `Origin`. Returns the version only.                                 |
| `GET /auth/<connector>`      | Loopback `Host`, no foreign `Origin`. Shows a Continue page; starts nothing.                    |
| `POST /auth/<connector>`     | Loopback `Host`, a same-origin form, and a single-use nonce.                                    |
| `GET /ui/…`                  | Loopback `Host`, no foreign `Origin`. The [console](./console#security) checks its own session. |
| `POST /ui/…`                 | Loopback `Host` and a same-origin form. The console checks its own session.                     |
| `/oauth/callback`            | Nothing. Checked against a pending auth flow's `state`.                                         |
| `/.well-known/oauth-…`       | Nothing, and only while `server.oauth.enabled` is true. Public sign-in information.             |
| `/oauth/authorize`           | Nothing, and only while sign-in is on and you armed it with **Connect a client**. Keeps the request pending. |
| `/oauth/token`               | Nothing, and only while sign-in is on. Needs a code you approved, or a refresh token.           |
| `/ui/oauth/approve`          | Loopback `Host`, and a console session. Never through a tunnel.                                 |

A request with the wrong token gets `401`. A scoped client token on `/admin/…`
gets `403`. A request with a foreign `Origin` gets `403`. A foreign `Host` gets
`404` on every path but `/oauth/callback` and, with a remote client token,
`/mcp`; the sign-in paths answer only your public URL's `Host` or a request a
tunnel forwarded (see [Loopback only](#loopback-only)). So a rebinding page
can't even learn which paths exist, or your public URL.

The stdio shim reads the token itself, so clients that use `wayfindr stdio`
never see it.

## Limiting what an agent can reach

Give every agent that runs unattended a
[scoped client token](./access/scoped-tokens): it holds the agent to the
connectors you name and keeps it off the admin API, so a prompt injection can't
reach your other connectors. A cloud agent gets a
[remote token](./access/scoped-tokens#remote-tokens), which is a scoped token
that may also come in through a tunnel. A [tools list](./access/tools-lists) hides a
connector's tools from every client, but trusts the upstream's read-only and
destructive hints, so name the tools you allow for anything that matters.
Profiles and project scopes are chosen by the client, so they keep a cooperative
agent focused and are no defense; [Control what agents can use](./access/) says
which layer holds against what.

## Cloud agents that sign in

ChatGPT can't send a token, so with `[server.oauth]` on, Wayfindr is its own sign-in
server (OAuth) for `/mcp`. It is off unless you turn it on, in the console's Cloud
agents page or with
[`server.oauth.enabled`](../reference/configuration#server-oauth). Off, every
answer is what it was before.

**What it opens through the tunnel.** The sign-in information (two documents that
name your public address and its sign-in and token addresses), `/oauth/authorize`,
and `/oauth/token`. The documents hold nothing secret. The other two start and
finish a sign-in, and neither can grant access alone:

- **You arm it.** `/oauth/authorize` keeps a request only in the 10 minutes after
  you selected **Connect a client** in the console. With no arm it answers an error page
  and fetches nothing. There is one arm at a time, and it ends when you open the
  approve page for your own request. Turning sign-in off or on, changing
  `public_url`, or changing `server.oauth.clients` ends it too, and drops every
  waiting request: select **Connect a client** again.
- **Only listed clients.** Only the client IDs in `server.oauth.clients`, by
  default ChatGPT's, are looked at. Wayfindr fetches one client's document from
  its address and requires the redirect address to be in it.
- **You approve on your computer.** The authorize address doesn't approve
  anything. It sends your browser to the approve page in the console on the
  loopback address, which needs your console session and its form token, and where
  you pick the connectors. Only then does the browser go back to the client with a
  one-time code, valid for 60 seconds. Nobody approves from another device.
- **The token address wants that code.** A client trades it, with the secret it
  made before it started, for an access token (one hour) and a refresh token.
  They are random, stored only as hashes, and work only for the connectors you
  ticked and only for `<public_url>/mcp`.

**Consent phishing is narrowed, not ruled out.** The client's address and
redirect address are public, so anyone can start a sign-in to your public address
from their own ChatGPT account and send you the link. If you approved it, their
account would get the connectors you tick. The arm limits that to the minutes
after you chose **Connect a client**, and when you open your own request's approve page,
every other waiting request is dropped, so a stranger's can't sit in front of
yours. The page shows when the request started and the IP address your tunnel
saw, and says: approve only if you clicked Connect in the client in the last few
minutes; if someone sent you this link, or the page opened in a tab you didn't
open, choose Deny.

A web page you have open can also open the approve page with a stranger's request
in a new tab while you are connecting. That tab first shows a page with a
**Continue** button, as your own request does, which says to continue only if you
just selected Connect in an app. Should you continue anyway, Wayfindr still won't
approve it: it counts the requests that arrived after you selected
**Connect a client**, and only one, yours, is expected. When more than one
arrived, the approve page says how many, shows no Approve button, and tells you
to choose Deny, then select **Connect a client** and try again in the client. A
stranger's request costs you a retry, never a grant.

What is left is a stranger's request that is the only one to arrive, which you
approve despite the page, and whether the client ties its sign-in to your
browser, which Wayfindr can't check.

**Cutting it off.** `wayfindr token revoke <name>` deletes a grant and its tokens,
and the next request is refused. Turning sign-in off, changing `public_url`, or
removing the client from `server.oauth.clients` suspends every affected grant:
none of its tokens work until you undo the change. The call log names the grant
behind each call. The decision and its other options are in
[ADR 0022](../adr/0022-authorization-server-for-remote-clients).

## The OAuth callback

`/oauth/callback` is served for any `Host` without a token. That lets you route
an OAuth `redirect_uri` through a tunnel when a provider won't redirect to
`127.0.0.1`. The callback only completes an auth flow you started in the last 10
minutes: it needs that flow's `state`, and the issuer is checked (RFC 9207)
before the code is exchanged. The rest of a tunnel's reach is in
[Loopback only](#loopback-only). See
[A redirect URI through a tunnel](./authentication#a-redirect-uri-through-a-tunnel).

## Encryption at rest

Secrets you store with `wayfindr secret set`, OAuth tokens with their DPoP keys,
and the clients Wayfindr registers with authorization servers are sealed with
AES-256-GCM. Each value is bound to its row, so a sealed value moved to another
row fails to open. Tool definitions, the catalog, sync records, and the call log
are not encrypted. They hold no credentials.

The master key is kept outside the store: in `WAYFINDR_SECRET_KEY`, the OS
keychain, or `<data>/master.key`, looked up in that order. See
[Master key](/reference/paths-and-environment#master-key) for each source.

The `master.key` fallback sits in the data directory, next to the database. Don't
back the two up together, or anyone with the backup can read your secrets. A
`master.key` that group or others can read is refused (`chmod 600` it). The
daemon logs a warning at every start that reads the key from the file; prefer the
keychain or `WAYFINDR_SECRET_KEY`. If the key is lost, the stored secrets and
tokens can't be read; see [The master key](./secrets#the-master-key).

## Credentials stay in the daemon

The daemon adds credentials to upstream requests itself. Agents get tool
definitions, results, and advice. An error carries a summary, a fix, and, for
`needs_auth`, an auth link such as `http://127.0.0.1:7777/auth/linear`, which
starts a sign-in in your browser and carries no token. `${secret:…}` and
`${env:…}` references stay unresolved in the config, and `wayfindr status` names
a missing reference, never a value; see [Secrets](./secrets#references).

An OAuth token is bound to the connector's `url` (RFC 8707). If you change the
`url`, the connector goes to `needs_auth` and the old token is never sent to the
new address.

## Keys and certificates

Connectors behind an enterprise identity provider hold more than a token. See
[Enterprise identity providers](./authentication-enterprise) for how to set each
one up.

- **Signing keys.** `--signing-key` stores the private key as a secret, so it is
  sealed like any other, and the config holds only its `${secret:…}` reference.
  Only the JWTs it signs leave the daemon: each lives 60 seconds and is used
  once. Your provider gets only the public key. With `client_secret_jwt`, the
  secret never leaves the daemon either; only a JWT signed with it does.
- **DPoP keys.** Where each is kept, and when it's replaced or deleted, is in
  [DPoP](./authentication-enterprise#dpop).
- **Client certificates.** The certificate itself is public. Its private key is
  read from `key_file` at each new connection, so keep that file mode 0600 in a
  directory only you can read; Wayfindr doesn't check its mode. Or store the key
  as a secret and set `key = "${secret:…}"` in `[tls]`, so it is sealed.
- **Unattended grants.** A `client_credentials` or `jwt_bearer` token, and its
  DPoP key, are held in memory only and never stored, so a restart costs one
  token request. A workload token in `[auth.assertion]` is read for each grant
  and never kept.

Wayfindr doesn't log these keys, tokens, or assertions, and doesn't put them in
the errors it builds. The device-code address rule is in
[Signing in without a browser](./authentication#signing-in-without-a-browser-on-this-machine-device-code).

## Logs

The daemon logs JSON. Before anything is written, Wayfindr keeps out header
values, access and refresh tokens, authorization codes, secret values, and the
query strings and user info of URLs. An embedding provider's error body is
scrubbed of the API key. Logs do name your connectors, their URLs (without query
strings), and the names of your secrets and variables. Read a log before you
share it.

Log files Wayfindr creates are mode 0600.

## The call log

The daemon keeps a row for every `call_tool` call, which
[`wayfindr calls`](/reference/cli#wayfindr-calls) shows: when it arrived, which
client token it used, the client's name, the tool name as the agent wrote it,
how long the upstream took, and the result. It never stores the call's arguments
or the upstream's result, so what an agent sent or read stays out of it. The one
exception is a call that didn't end `ok`: it keeps the error text the agent got
back, on one line, cut to 500 bytes, with URL query strings masked. An
upstream's error can quote what the agent sent, such as a search query, and it
is deleted with its call after the retention period. A name is cut to 256 bytes,
and control and bidirectional characters in it are replaced, so printing the log
can't send escape sequences to your terminal or reorder its text.

The client's name is whatever the client says it is. The token is what the
daemon checked: `default` or a scoped token's name. Use the token to see what an
unattended agent did.

Rows are deleted after [`[calls] retention`](/reference/configuration#calls), 30
days by default. Set it to `"0s"` to record nothing.

## Service units

`wayfindr service install` writes a launchd plist or a systemd unit, which other
processes can read (launchd requires that). So it never writes
`WAYFINDR_SECRET_KEY` or `WAYFINDR_CLIENT_TOKEN` into it, and warns if they are
set. The service reads the master key from the keychain or `master.key`, and the
token from `client-token`. See [Running the daemon](./running).

## Files

| File                            | Mode                                 |
| ------------------------------- | ------------------------------------ |
| Data directory                  | 0700                                 |
| `<data>/client-token`           | 0600                                 |
| `<data>/master.key`             | 0600                                 |
| `<data>/daemon.lock`            | 0600                                 |
| `<data>/wayfindr.log`           | 0600                                 |
| `wayfindr.toml`, made by `init` | 0600                                 |
| `[tls]` `key_file`              | Yours to set: keep it 0600           |
| `<data>/wayfindr.db`            | 0600, inside the 0700 data directory |
| Service unit file               | 0644                                 |

If you move the database with `storage.dsn`, put it in a directory only you can
read.
