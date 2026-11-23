# Authentication

Wayfindr holds each connector's credential and presents it to the upstream.
Clients never see it. A connector uses one of three kinds of auth:

```toml
[connectors.deepwiki]               # none
url = "https://mcp.deepwiki.com/mcp"

[connectors.exa]                    # API key in a header
url     = "https://mcp.exa.ai/mcp"
headers = { "x-api-key" = "${secret:exa_api_key}" }

[connectors.linear]                 # OAuth
url  = "https://mcp.linear.app/mcp"
auth = { type = "oauth" }
```

Auth applies to `url` connectors only. A stdio server gets what it needs through
its `args` and `env`.

The tokens on this page are the OAuth tokens a connector presents to its
upstream. They are not the [client token](/reference/paths-and-environment#client-token)
your agents present to Wayfindr, which is covered in [Clients](./clients) and
[Scoped client tokens](./access/scoped-tokens).

## Which setup you need

Most OAuth connectors need nothing beyond `auth = { type = "oauth" }` and one
sign-in. Find your case:

| You're connecting                                                                               | What you set up                                                                                                             | See                                                                                                       |
| ----------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| Your own account on a SaaS server (Linear, Slack, Gmail, GitHub)                                | `type = "oauth"`, and a client ID if the provider has no DCR. Sign in once in a browser.                                    | [OAuth](#oauth), [Authorizing a connector](#authorizing-a-connector)                                      |
| A company server behind an enterprise identity provider (Okta, Entra ID, Keycloak, Ping, Auth0) | Your registered OAuth client, proved with a private key or a client certificate. Tokens bound with DPoP or the certificate. | [Enterprise identity providers](./authentication-enterprise)                                              |
| A server on a private CA, or one that only takes a client certificate                           | A `[tls]` table with the CA and, if needed, the certificate.                                                                | [Private CAs and client certificates](./connectors#private-cas-and-client-certificates)                   |
| A service with no user: a bot, a CI job, a workload in Kubernetes                               | An unattended grant: Wayfindr gets each token itself, and nobody signs in.                                                  | [Unattended grants](./authentication-unattended)                                                          |
| A daemon with no browser: a remote machine, a server you reach over SSH                         | A device code, or the browser sign-in through a forwarded port.                                                             | [Signing in without a browser on this machine](#signing-in-without-a-browser-on-this-machine-device-code) |

Behind these, an OAuth connector makes three choices, each with a default that
suits most servers, and any combination works:

- The **grant**: how the connector gets a token. An **auth flow** is a grant you
  take part in: the authorization code, where you sign in in a browser (the
  default), or the device code, where you enter a code on any device. An
  **unattended grant** has no user: client credentials or JWT bearer.
- **Client authentication**: how Wayfindr proves to the provider which OAuth
  client it is: no proof (a public client), a client secret, a private key JWT,
  or a client certificate.
- The **sender constraint**: what binds a token to Wayfindr so a stolen copy is
  useless: nothing (a bearer token, the default), a DPoP key, or a client
  certificate.

## No auth

Leave out `headers` and `auth`. Wayfindr sends nothing.

## API keys

Put the key in `headers`, the way the server expects it:

```toml
[connectors.exa]
url     = "https://mcp.exa.ai/mcp"
headers = { "x-api-key" = "${secret:exa_api_key}" }

[connectors.internal]
url     = "https://mcp.internal.example.com/mcp"
headers = { Authorization = "Bearer ${env:INTERNAL_MCP_TOKEN}" }
```

Don't paste keys into the config. In one step, `connector add --secret-header`
prompts for the value, stores it as a secret, and writes the reference:

```sh
wayfindr connector add exa --url https://mcp.exa.ai/mcp --secret-header x-api-key
```

Or store a secret yourself with `wayfindr secret set` and reference it as
`${secret:name}`, or read it from the environment with `${env:NAME}`. See
[Secrets](./secrets).

## OAuth

Add an `[auth]` table with `type = "oauth"`. What else goes in it depends on how
the server lets clients register.

| Server supports                     | You configure                                       |
| ----------------------------------- | --------------------------------------------------- |
| Dynamic Client Registration (DCR)   | nothing else: `auth = { type = "oauth" }`           |
| Only clients you register yourself  | `client_id`, and `client_secret` if the app has one |
| Client ID Metadata Documents (CIMD) | `client_metadata_url`                               |

Then authorize it once:

```sh
wayfindr auth login linear
```

### Discovery and DCR

```toml
[connectors.linear]
url  = "https://mcp.linear.app/mcp"
auth = { type = "oauth" }
```

With no `client_id`, Wayfindr finds the server's OAuth endpoints by itself. It
reads the server's protected resource metadata, then its authorization server's
metadata, and falls back to the server's own origin for servers that publish
neither. It then registers itself as a client through DCR and keeps that
registration for later sign-ins. How the client is chosen, and what a DCR
registration asks for, are in the
[configuration reference](/reference/configuration#how-the-client-is-chosen);
[ADR 0007](/adr/0007-oauth-discovery-and-registration) lists every lookup in
order.

A discovered connector needs PKCE (a one-time proof that the app finishing the
sign-in is the one that started it). If the authorization server's metadata
doesn't list `S256` in `code_challenge_methods_supported`, the connector goes to
`auth_error`: register an app with the provider and set `client_id`.

**Does the server offer DCR?** Run `wayfindr auth login <name>`. If there is no
`client_id` and the server offers neither DCR nor CIMD, the command stops before
it prints a URL, with:

```text
There is no OAuth client to authorize with: no client_id is configured, and the authorization server supports neither client ID metadata documents nor dynamic client registration.
```

Then register an app with the provider, as in the next section.

### A client you registered

Many providers, such as Slack, don't offer DCR. Register an OAuth app in the
provider's developer console with the redirect URI
`http://127.0.0.1:7777/oauth/callback`, then give Wayfindr its client ID:

```toml
[connectors.slack]
url = "https://mcp.slack.com/mcp"

[connectors.slack.auth]
type          = "oauth"
client_id     = "1234.5678"
client_secret = "${secret:slack_client_secret}"
scopes        = ["channels:read", "chat:write"]   # placeholders: the scopes your app and the server use
```

```sh
wayfindr secret set slack_client_secret
```

Or let `connector add` write the table and store the secret, which it prompts
for without echo:

```sh
wayfindr connector add slack --url https://mcp.slack.com/mcp \
  --client-id 1234.5678 --client-secret \
  --scope channels:read --scope chat:write
```

A connector with `client_id` is a manual connector. `client_secret` is optional:
leave it out for a public client. It requires `client_id`, and is read from its
reference each time it's needed, never copied into the store. Wayfindr sends the
secret the way the provider's metadata says it accepts; to pick the way
yourself, see
[Client authentication](/reference/configuration#client-authentication).

`scopes` applies only when the server names none (see [OAuth keys](#oauth-keys)).

### Google

Google's MCP servers, such as Gmail at `https://gmailmcp.googleapis.com/mcp/v1`,
don't offer DCR. In the Google Cloud console, enable the API for the Google
service you're connecting, set up the OAuth consent screen, and create an OAuth
client of type **Web application**. Register exactly
`http://127.0.0.1:7777/oauth/callback` as an authorized redirect URI, or the
`redirect_uri` you configured. Then:

```sh
wayfindr connector add gmail --url https://gmailmcp.googleapis.com/mcp/v1 \
  --client-id 123-abc.apps.googleusercontent.com --client-secret \
  --authorize-param access_type=offline --authorize-param prompt=consent
```

Google issues a refresh token only when the authorization request asks for
`access_type=offline`, and issues a new one on a later sign-in only with
`prompt=consent`. Without them, the connector needs signing in again each time
its access token expires, after about an hour. The flags write them to
`authorize_params`:

```toml
[connectors.gmail.auth]
type          = "oauth"
client_id     = "123-abc.apps.googleusercontent.com"
client_secret = "${secret:gmail_client_secret}"

[connectors.gmail.auth.authorize_params]
access_type = "offline"
prompt      = "consent"
```

If the consent screen's user type is **External** and its publishing status is
**Testing**, add your Google account as a test user, or Google refuses the
sign-in. Google also expires a Testing app's refresh tokens after 7 days, so the
connector needs signing in again every week. Publish the app to stop that, or
expect the weekly sign-in. An **Internal** app, on Google Workspace, has neither
limit.

Google can take from minutes to hours to apply a change to a client's redirect
URIs. A `redirect_uri_mismatch` right after you edited the client may clear on
its own: wait, then run `wayfindr auth login gmail` again. To fix a setting without
starting over, see [Fixing a connector](./connectors#fixing-a-connector).

### CIMD

With Client ID Metadata Documents, the client ID is the URL of a JSON client
metadata document that you host. Wayfindr hosts none for you.

```toml
[connectors.example.auth]
type                = "oauth"
client_metadata_url = "https://you.example.org/wayfindr-client.json"
```

Wayfindr uses it only when the authorization server advertises
`client_id_metadata_document_supported` and no `client_id` is set. Otherwise it
moves on to DCR. List Wayfindr's redirect URI in the document's `redirect_uris`.

### Endpoints from config

When discovery doesn't work for a server, name the endpoints yourself:

```toml
[connectors.example.auth]
type                   = "oauth"
client_id              = "wayfindr"
authorization_server   = "https://auth.example.com"          # skips the resource metadata
# or, to skip discovery entirely:
# authorization_endpoint = "https://auth.example.com/authorize"
# token_endpoint         = "https://auth.example.com/token"
```

For the browser sign-in, `authorization_endpoint` and `token_endpoint` go
together. A device code needs `token_endpoint` with
`device_authorization_endpoint`; an [unattended grant](./authentication-unattended)
needs `token_endpoint` alone. With endpoints from config and no metadata,
Wayfindr can't tell whether the server supports PKCE, so such a connector needs a
`client_id`.

### A redirect URI through a tunnel

The default redirect URI is `http://<listen>/oauth/callback`, which is
`http://127.0.0.1:7777/oauth/callback` unless you changed `[server] listen`.
Some providers refuse loopback or plain-`http` redirect URIs. Run an HTTPS
tunnel (ngrok, Cloudflare Tunnel, or similar) to the daemon and set
`redirect_uri`:

```toml
[connectors.picky.auth]
type         = "oauth"
client_id    = "abc123"
redirect_uri = "https://abc.ngrok.app/oauth/callback"
```

Keep the `/oauth/callback` path, and register the same URI with the provider.
The tunnel only needs to be up while you authorize. Through a tunnel the daemon
serves this callback, and `/mcp` only for a
[remote client token](./access/scoped-tokens#remote-tokens) made for a
[cloud agent](./cloud-agents); see [Loopback only](./security#loopback-only). A
callback is accepted only for a sign-in this daemon started and is still waiting
on. If you change `redirect_uri` on a DCR
connector, Wayfindr registers a new client for the new URI on the next sign-in.

### Changing a connector's OAuth client

A stored token is bound to the OAuth client that obtained it. After you change a
connector's `client_id`, with `wayfindr connector edit gmail --client-id …` or
by hand, the connector goes to `needs_auth` and the old token is never sent:

```text
gmail: The connector's client_id changed since it was authorized. Authorize it again.
  Open the link to authorize the connector again.
  http://127.0.0.1:7777/auth/gmail
```

Run `wayfindr auth login gmail`. Changing only `client_secret` keeps the token. Tokens
stored before this check have no recorded client and stay valid until your next
sign-in.

The check covers a `client_id` you set. Removing it with
`wayfindr connector edit gmail --unset auth.client_id` deletes the stored token,
since the connector then registers its own client. A client Wayfindr registered
through DCR changes when it registers again, for example after a new
`redirect_uri`; the old token keeps working until it needs a refresh.

### OAuth keys

The keys you set most often, besides `type = "oauth"`:

| Key             | Notes                                                                                                                                                                                                                            |
| --------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `client_id`     | A client you registered. Makes this a manual connector.                                                                                                                                                                          |
| `client_secret` | Requires `client_id`. Use a `${secret:…}` reference.                                                                                                                                                                             |
| `scopes`        | For a sign-in, used only when the server names none: its 401 challenge's `scope` wins, then its resource metadata's `scopes_supported`. An [unattended grant](./authentication-unattended#what-you-see) orders them differently. |
| `grant`         | `"authorization_code"` (the default), `"device_code"`, `"client_credentials"`, or `"jwt_bearer"`.                                                                                                                                |

The rest, such as `authorize_params`, `redirect_uri`, `pkce`, `validate_issuer`,
`token_endpoint_auth_method`, and `dpop`, are in the
[configuration reference](/reference/configuration#connectors-name-auth).
`pkce = false` needs a `client_secret`, and `validate_issuer = false` turns off
the issuer checks: that the authorization server's metadata names its own URL
(its issuer), and the callback's `iss`. Both weaken security. Use them only for a provider that needs them, and
only on that connector.

## Authorizing a connector

An OAuth connector with an auth flow starts in `needs_auth`. Authorize it with
the CLI:

```sh
wayfindr auth login linear
```

```text
Open this URL to authorize linear:
  https://mcp.linear.app/authorize?client_id=…
Waiting for the browser (until 2026-11-07T06:30:00Z). If the page shows an error, fix it and run `wayfindr auth login linear`; linear stays configured.
Authorized linear.
linear is ready (23 tools).
```

The command prints the provider's consent URL, opens it in your browser, and
waits. After you approve, the browser comes back to the daemon, which stores the
token, reconnects the connector, and syncs its tools. If the browser doesn't
open, copy the URL yourself. A sign-in that isn't finished within 10 minutes
expires; run the command again. If you press Ctrl-C, or the sign-in fails, the
command prints
``linear was not authorized. Fix the cause, then run `wayfindr auth login linear`.``
The connector stays configured.

You can also select **Sign in** on the connector in the [web console](./console),
or open its [auth link](#the-auth-link).

The daemon must be running. On a connector with no
`[auth]` table of type `oauth`, the command refuses, with
`<name> does not use OAuth: it has no [auth] table of type "oauth".` Run
`wayfindr connector edit <name> --oauth` to add one, or put its API key in
`headers`.

### Signing in without a browser on this machine (device code)

When the daemon runs where no browser can reach its callback, such as on a
server you reach over SSH, you have two ways in.

**A device code** (RFC 8628), if the provider supports it. Nothing needs to
reach the daemon:

```sh
wayfindr auth login github --device
```

```text
Enter code WDJB-MJHT at https://github.com/login/device to authorize github.
Waiting for you to allow access (until 2026-11-07T06:30:00Z). If the page shows an error, fix it and run `wayfindr auth login github --device`; github stays configured.
Authorized github.
github is ready (41 tools).
```

Open the address on any device, your laptop or your phone, enter the code, and
allow access. The daemon asks the provider every few seconds until you do, then
stores the token and reconnects the connector as usual. Add `--no-open` when the
machine has no browser at all. To make the device code the connector's way to
sign in, so `wayfindr auth login`, the auth link, and the web console all use it, set
`grant`:

```toml
[connectors.github.auth]
type      = "oauth"
client_id = "Iv1.abc123"
grant     = "device_code"
```

Wayfindr uses the `device_authorization_endpoint` in the authorization server's
metadata, or the one you set in the `[auth]` table. Without either, the start
fails with
`The authorization server does not support signing in with a code on another device (device code).`
Providers that support it include GitHub (turn on **Enable Device Flow** in the
app's settings), Microsoft Entra ID, Okta, and Auth0. Google supports it only
for a client of type **TVs and Limited Input devices**, and only for a few
scopes. Wayfindr shows or opens only an `https` address, or `http` on a loopback
address such as `localhost`, so a hostile server can't send you to a phishing
page that asks for the code.

**The browser sign-in through a forwarded port**, for a provider without device
code support. Forward the daemon's port from your laptop, run
`wayfindr auth login --no-open <connector>` on the server, and open the URL it prints
on your laptop:

```sh
ssh -L 7777:127.0.0.1:7777 <host>   # on your laptop
wayfindr auth login --no-open linear      # then, in that SSH session
```

`--no-open` only prints the URL and still waits. The browser comes back to
`127.0.0.1:7777` on your laptop, which the tunnel carries to the daemon.

### The auth link

Every `needs_auth` connector has an auth link:

```text
http://127.0.0.1:7777/auth/linear
```

`wayfindr status` prints it, and so do `search_tools` (in its `unavailable` and
`warnings` lists) and a failed `call_tool`, so your agent can hand it to you
mid-task. Opening it shows a page with a **Continue** button; pressing it starts
the same sign-in as `wayfindr auth login`. Opening the link alone starts nothing. Open
it in a browser on the same machine as the daemon. For a connector with
`grant = "device_code"`, the page then shows the code and a link to where you
enter it, instead of sending you to the provider.

## Revoking a credential

```sh
wayfindr auth logout linear
```

This deletes the connector's stored token and the client Wayfindr registered for
it through DCR, and cancels any sign-in in progress. The connector goes back to
`needs_auth`. Nothing is sent to the provider, so revoke the grant in the
provider's settings too if you want it gone there.

For a connector with an [unattended grant](./authentication-unattended), it
forgets the token Wayfindr holds in memory instead, and deletes any stored
credential a hand edit left behind; the grant runs again at once. If the
connector has no stored credential, there is nothing to do, and the command says
so and exits `0`: `linear has no stored credential; nothing to revoke.`

### Orphaned credentials

Removing a connector by hand, or commenting it out, keeps its credential, so
putting it back needs no new sign-in. (`wayfindr connector remove` deletes it.)
A credential whose connector is gone is orphaned. `wayfindr status` lists
orphans until `wayfindr auth logout <name>` deletes them, which says
`Revoked the credential of old-linear, which is no longer configured.`

## Token refresh

Wayfindr refreshes an access token only when a call needs it: when the token
expires within 60 seconds, or when the upstream answers 401, after which it
retries the call once. Nothing refreshes in the background, so an idle
connector's token may expire and is refreshed by its next call. An unattended
grant has no refresh: Wayfindr runs the grant again instead.

If a refresh fails, because the grant was revoked or the refresh token expired,
the connector goes to `needs_auth`, and the call's error result carries the auth
link.

Some ends are predictable: a token with no refresh token, a refresh token or
registered client with a known expiry, a client certificate about to expire.
Wayfindr warns of them in `wayfindr status`, the daemon log, and, within a day,
to your agent in `search_tools`. It only reads these dates; it never refreshes
early or signs in for you. [Warnings](./troubleshooting#warnings) lists each one
and what to do.

## A tool needs more scopes

Some servers ask for more scopes for some tools, which is called step-up. When
an upstream answers 403 with `insufficient_scope`, Wayfindr remembers the extra
scopes and moves the connector to `needs_auth`:

```text
linear: The upstream needs scopes the connector was not granted.
  Open the link to authorize the connector with the larger set of scopes.
  http://127.0.0.1:7777/auth/linear
```

Authorize again, and the new sign-in asks for the larger set. A connector with
an unattended grant needs no sign-in: Wayfindr runs the grant once more with the
larger set (see [Unattended grants](./authentication-unattended#what-you-see)).

## Problems

`wayfindr status` explains an `auth_error` and says what to change.
[OAuth errors](./troubleshooting-oauth#oauth-errors) lists each
message, as both `wayfindr status` and `wayfindr auth login` word it, with its fix.
See [Security](./security) for how credentials and keys are stored.
