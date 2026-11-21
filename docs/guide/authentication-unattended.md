# Unattended Grants

Read this when a connector acts for a service, not for you: a reporting bot, a CI
job, a gateway in a Kubernetes cluster. Nobody can sign in for it, so Wayfindr
gets each token itself. For a connector you sign in to, see
[Authentication](./authentication).

An **unattended grant** has no user. Wayfindr gets a token whenever a call needs
one, holds it in memory only, and gets a new one when it's about to expire or the
server rejects it. It ignores any refresh token. Nothing is stored, so such a
connector has no stored credential and is never `needs_auth`.

| `grant`                | Wayfindr proves                                 | You configure                                                              |
| ---------------------- | ----------------------------------------------- | -------------------------------------------------------------------------- |
| `"client_credentials"` | that it is the OAuth client: its own access     | `client_id`, and `client_secret`, a `signing_key`, or a client certificate |
| `"jwt_bearer"`         | an assertion (a JWT) that stands for a workload | `client_id`, and where the assertion comes from                            |

Register the client with the provider first, and allow it the grant and the
scopes it needs. Both grants need a `client_id` and an authorization server you
name, with `authorization_server` (its issuer) or `token_endpoint`. Wayfindr
sends the client's secret or the assertion with every grant and nobody is there
to see where it goes, so it goes only to the server you name, never to one the
MCP server's metadata points to. The sign-in keys (`redirect_uri`, `pkce`,
`authorize_params`, and the endpoints of a browser or device sign-in) are
refused; the
[configuration reference](/reference/configuration#grants) lists the rules.

## Client credentials

With a secret:

```sh
wayfindr connector add reports --url https://mcp.example.com/mcp \
  --client-id reports-bot --client-secret --grant client_credentials \
  --authorization-server https://auth.example.com
```

```toml
[connectors.reports.auth]
type                 = "oauth"
grant                = "client_credentials"
authorization_server = "https://auth.example.com"
client_id            = "reports-bot"
client_secret        = "${secret:reports_client_secret}"
scopes               = ["reports:read"]
```

With a private key instead of a secret, as enterprise providers prefer, add
`--token-auth-method private_key_jwt --signing-key < key.pem`; see
[Private key JWT](./authentication-enterprise#private-key-jwt). With a client
certificate, see
[The client certificate as the OAuth client's credential](./authentication-enterprise#the-client-certificate-as-the-oauth-client-s-credential).

## JWT bearer

The assertion comes from exactly one place, read again for every token, so a
workload token that rotates is always fresh:

- **A file**, such as a Kubernetes projected service account token:

  ```toml
  [connectors.cluster.auth]
  type                 = "oauth"
  grant                = "jwt_bearer"
  authorization_server = "https://auth.example.com"
  client_id            = "mcp-gateway"

  [connectors.cluster.auth.assertion]
  file = "/var/run/secrets/tokens/mcp"
  ```

  ```yaml
  # in the pod spec
  volumes:
    - name: mcp-token
      projected:
        sources:
          - serviceAccountToken:
              path: mcp
              audience: https://auth.example.com # the provider's issuer
              expirationSeconds: 3600
  ```

  Kubernetes replaces the file before the token expires; Wayfindr reads it for
  every grant. `wayfindr connector add … --assertion-file <file>` writes the
  table.

- **A value**, usually from the daemon's environment, such as a GitHub Actions
  OIDC token fetched in an earlier step (the job needs
  `permissions: id-token: write`):

  ```sh
  export MCP_ID_TOKEN=$(curl -sSf -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
    "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=https://auth.example.com" | jq -r .value)
  wayfindr start
  ```

  ```toml
  [connectors.ci.auth.assertion]
  value = "${env:MCP_ID_TOKEN}"
  ```

  The daemon's environment is fixed while it runs, so this suits a short-lived
  daemon such as a CI job. For a token that rotates under a long-running daemon,
  use `file`, or a `${secret:…}` you update with `wayfindr secret set`.

- **A subject Wayfindr signs for itself** with the connector's signing key, for
  a provider that trusts your public key for that subject (`--assertion-subject`
  and `--signing-key` write it):

  ```toml
  [connectors.svc.auth.assertion]
  subject = "svc@example.com"

  [connectors.svc.auth.signing_key]
  key = "${secret:svc_signing_key}"
  ```

  The signing key belongs to the assertion here, not to the client: Wayfindr
  doesn't also authenticate the client with it unless you set
  `token_endpoint_auth_method = "private_key_jwt"`. The assertion's claims and
  signing algorithm are in the
  [configuration reference](/reference/configuration#connectors-name-auth-assertion).

A refused assertion is final: Wayfindr doesn't retry the same one, and never
falls back to another grant.

## What you see

`wayfindr status` shows the connector `ready` once a call has got a token. There
is no token expiry or sign-in to show: the token is never stored, and the
console offers no Sign in or Sign out. `wayfindr auth login <connector>` says there is
nothing to sign in to.

The grant asks for, in this order of preference:

1. the larger set a step-up named;
2. the scopes of the token Wayfindr holds, while the connector's auth config is
   unchanged, which keeps an earlier step-up's set;
3. the connector's `scopes`;
4. the scope the server's 401 challenge names;
5. none.

It never uses the protected resource metadata's `scopes_supported`, which lists
what the server knows, not what this client may have. (A browser sign-in does
use it; see [OAuth keys](./authentication#oauth-keys).) When a tool needs more
(`insufficient_scope`), Wayfindr runs the grant once more with the larger set
and retries the call once. If the server still refuses, the connector goes to
`auth_error` (`scope_denied`).

When the provider refuses, the connector goes to `auth_error`, never
`needs_auth`, since no sign-in can fix it:

```text
CONNECTOR  STATE       TOOLS  LAST SYNC  TOKEN EXPIRES
reports    auth_error  0      never      -

reports: The authorization server refused the connector's unattended grant (provider code invalid_scope).
  Fix the OAuth client's access or the assertion at the provider, or client_id, scopes, or [auth.assertion] in the connector's config. A config change retries at once; after a fix at the provider, run `wayfindr auth logout reports` to have Wayfindr run the grant again.
```

Fix it in the config, and the daemon reconnects the connector at once. Fix it at
the provider (allow the client the grant or the scopes), then run
`wayfindr auth logout <connector>`: that forgets the in-memory token and runs
the grant again. A missing or empty assertion file is `unresolved_value`. See
[OAuth errors](./troubleshooting-oauth#oauth-errors).

## Switching a connector's grant

`wayfindr connector edit <connector> --grant client_credentials` (or
`jwt_bearer`) deletes the connector's stored credential, since an unattended
grant never reads it. If you change `grant` by hand instead, the stored token is
left unused; `wayfindr auth logout <connector>` deletes it. Going back to an
auth flow, the connector is `needs_auth` until you sign in.
