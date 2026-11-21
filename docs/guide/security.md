# Security

Wayfindr holds the keys to every MCP server you use, so it is built to keep them
on your machine and inside the daemon. This page says what it protects, how, and
where the edges are.

## What it defends against

| Threat                                                       | Defense                                                                                                                                                                           |
| ------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Another machine on your network calls the daemon             | It binds to loopback only, and checks the address it actually bound.                                                                                                              |
| A web page in your browser calls `127.0.0.1:7777`            | Host and Origin checks, and the client token, which a page can't read.                                                                                                            |
| Another user on the machine takes the port while the daemon is stopped | The CLI and the stdio shim send the client token only to a daemon that holds the lock in your data directory, which other users can't open. A client you set up over HTTP, with the token in its environment, does no such check: on a shared machine, use the stdio shim. |
| DNS rebinding                                                | A request whose `Host` isn't a loopback name or IP on the daemon's port gets 404 on every path.                                                                                   |
| A tunnel for the OAuth callback exposes the daemon           | Forwarded requests reach only `/oauth/callback`, whatever `Host` they carry.                                                                                                      |
| Someone copies your database file                            | Secrets and tokens in it are encrypted, and the master key is normally stored elsewhere.                                                                                          |
| An agent or a prompt injection tries to read credentials     | No tool returns them. Agents only get tool definitions, results, and advice.                                                                                                      |
| A prompt injection takes over an unattended agent            | Give it a scoped client token: it reaches only its connectors, and no admin API.                                                                                                  |
| A token leaks to the wrong server                            | Each OAuth token is bound to its connector's URL and only sent there.                                                                                                             |
| A stolen OAuth token is replayed elsewhere                   | With DPoP or a certificate-bound token, it is useless without Wayfindr's key or certificate.                                                                                      |
| A hostile server shows you a phishing page for a device code | Wayfindr shows or opens only an `https` address, or `http` on loopback.                                                                                                           |
| An upstream's tool schema makes Wayfindr fetch something     | Schemas are validated with remote and file `$ref` loading turned off.                                                                                                             |
| Secrets end up in logs or bug reports                        | Wayfindr redacts header values, tokens, auth codes, secret values, and URL query strings and user info before it logs them. A `${…}` in a connector's URL shows as the reference. |

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

There is no setting to listen on another interface, and the daemon serves only
local clients. A request that comes through a proxy or tunnel gets `404` on
every path but `/oauth/callback`, even when the proxy rewrites `Host` to
`127.0.0.1`: the daemon refuses any request with a `Forwarded`,
`X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Real-IP`, or
`CF-Connecting-IP` header.

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
listens on, except `/oauth/callback`.

| Path                         | Needs                                                                                           |
| ---------------------------- | ----------------------------------------------------------------------------------------------- |
| `/mcp`                       | A client token, loopback `Host`, no foreign `Origin`.                                           |
| `/admin/…` (used by the CLI) | The default client token, loopback `Host`, no foreign `Origin`.                                 |
| `GET /healthz`               | Loopback `Host`, no foreign `Origin`. Returns the version only.                                 |
| `GET /auth/<connector>`      | Loopback `Host`, no foreign `Origin`. Shows a Continue page; starts nothing.                    |
| `POST /auth/<connector>`     | Loopback `Host`, a same-origin form, and a single-use nonce.                                    |
| `GET /ui/…`                  | Loopback `Host`, no foreign `Origin`. The [console](./console#security) checks its own session. |
| `POST /ui/…`                 | Loopback `Host` and a same-origin form. The console checks its own session.                     |
| `/oauth/callback`            | Nothing. Checked against a pending auth flow's `state`.                                         |

A request with the wrong token gets `401`. A scoped client token on `/admin/…`
gets `403`. A request with a foreign `Origin` gets `403`. A foreign `Host` gets
`404`, so a rebinding page can't even learn which paths exist.

The stdio shim reads the token itself, so clients that use `wayfindr stdio`
never see it.

## Limiting what an agent can reach

Give every agent that runs unattended a
[scoped client token](./access/scoped-tokens): it holds the agent to the
connectors you name and keeps it off the admin API, so a prompt injection can't
reach your other connectors. A [tools list](./access/tools-lists) hides a
connector's tools from every client, but trusts the upstream's read-only and
destructive hints, so name the tools you allow for anything that matters.
Profiles and project scopes are chosen by the client, so they keep a cooperative
agent focused and are no defense; [Control what agents can use](./access/) says
which layer holds against what.

## The OAuth callback

`/oauth/callback` is the one path served for any `Host`. That lets you route an
OAuth `redirect_uri` through a tunnel when a provider won't redirect to
`127.0.0.1`. The callback only completes an auth flow you started in the last 10
minutes: it needs that flow's `state`, and the issuer is checked (RFC 9207)
before the code is exchanged. Everything else still requires a loopback `Host`
and no forwarding headers, so the tunnel can't reach `/mcp` or the admin API,
even if it rewrites `Host`. See
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
