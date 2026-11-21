# Enterprise Identity Providers

Read this when a company server sits behind an identity provider such as Okta,
Entra ID, Keycloak, Ping, or Auth0, and it wants more than a shared secret: a
private key or a client certificate to prove who Wayfindr is, or tokens bound to
Wayfindr so a stolen copy is useless. For a plain browser sign-in, see
[Authentication](./authentication).

These providers usually offer no DCR, so you register the OAuth client with the
provider and set `client_id`, as in
[A client you registered](./authentication#a-client-you-registered). Everything
below works with any grant: a browser sign-in, a device code, or an
[unattended grant](./authentication-unattended).

## Client authentication

At the token endpoint, Wayfindr proves which OAuth client it is. Leave
`token_endpoint_auth_method` out and it chooses from what you configured and what
the provider's metadata lists: a `signing_key` gives `private_key_jwt`, a
`client_secret` gives a secret method, and nothing but a `client_id` gives a
public client. The
[configuration reference](/reference/configuration#client-authentication) lists
every method, what it needs, and the full order of choice. Set the key to choose
one yourself; it is used as is.

If the provider lists methods and none fits your credential, the connector goes
to `auth_error` (`auth_method_unsupported`) before anything is sent. The two
credentials that need more than a key are below.

## Private key JWT

With `private_key_jwt`, the OAuth client proves who it is with a private key
instead of a secret. You keep the private key; the provider gets only the public
key (in the app's credentials or its JWKS, a JSON document of public keys). For
every token request, Wayfindr signs a short-lived JWT with the key.

1. Make a key pair. An EC P-256 key is the usual choice; P-384, RSA (2048 bits
   or more), and Ed25519 work too.

   ```sh
   openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out key.pem
   openssl pkey -in key.pem -pubout -out public.pem
   ```

2. Register the public key with the provider, with the redirect URI
   `http://127.0.0.1:7777/oauth/callback`. If the provider gives the key an ID,
   note it for `kid`.

3. Give Wayfindr the client ID and the private key. `--signing-key` reads the
   PEM from stdin, stores it as the secret `<name>_signing_key`, and writes the
   `[auth.signing_key]` table:

   ```sh
   wayfindr connector add corp --url https://mcp.corp.example/mcp \
     --client-id wayfindr --token-auth-method private_key_jwt --signing-key < key.pem
   rm key.pem
   ```

   Use `wayfindr connector edit corp --signing-key < key.pem` to replace the key
   later. The table it writes:

   ```toml
   [connectors.corp.auth]
   type      = "oauth"
   client_id = "wayfindr"
   token_endpoint_auth_method = "private_key_jwt"

   [connectors.corp.auth.signing_key]
   key = "${secret:corp_signing_key}"
   # kid      = "the key ID the provider gave"
   # alg      = "ES256"
   # audience = "token_endpoint"
   ```

Each JWT names your `client_id` as its issuer and subject, lives 60 seconds, and
is used once. The JWT is addressed to the provider's issuer by default. Okta and
Entra ID still want the token endpoint's URL instead: for them, set
`audience = "token_endpoint"`. How `alg` is chosen is in the
[`signing_key` reference](/reference/configuration#connectors-name-auth-signing-key).

A key Wayfindr can't read, or that can't sign the `alg` you set, puts the
connector in `auth_error` (`signing_key_invalid`). See
[OAuth errors](./troubleshooting-oauth#oauth-errors).

## The client certificate as the OAuth client's credential

Providers such as Keycloak, Okta, Entra ID, and Ping can authenticate the OAuth
client by its certificate instead of a secret (RFC 8705). Register the client
with the provider, give the connector a client certificate in its `[tls]` table
(see
[Private CAs and client certificates](./connectors#private-cas-and-client-certificates)), and set
`token_endpoint_auth_method`:

```toml
[connectors.corp.auth]
type      = "oauth"
client_id = "wayfindr"
token_endpoint_auth_method = "tls_client_auth"   # or "self_signed_tls_client_auth"

[connectors.corp.tls]
cert_file = "~/corp/client.pem"
key_file  = "~/corp/client.key"
```

Use `tls_client_auth` for a certificate from a CA the provider trusts, and
`self_signed_tls_client_auth` for one you registered with the provider. Wayfindr
sends only the `client_id` in the form; the certificate on the connection proves
it. When the provider lists mTLS endpoint aliases in its metadata
(`mtls_endpoint_aliases`), the token, device, and registration requests go
there. Wayfindr never picks these methods for you.

### Certificate-bound tokens

A provider that binds tokens to the client certificate
(`tls_client_certificate_bound_access_tokens`) issues tokens that only work over
a connection with the same certificate. Since the MCP server's connection
presents the certificate the token request did, this needs nothing more than a
`[tls]` client certificate; Wayfindr also uses the provider's mTLS aliases then.
If an MCP server rejects a fresh token while its metadata says it supports bound
tokens and the connector has no certificate, the reason in the daemon log says
to add one in `[tls]`.

## DPoP

DPoP (RFC 9449) binds a connector's tokens to a key that only Wayfindr holds, so
a stolen token is useless on its own. Each sign-in or unattended grant gets its
own key. Every token request and every call to the MCP server carries a fresh,
signed DPoP proof, and the token is sent with the `DPoP` scheme instead of
`Bearer`. When a server asks for a nonce in the proof, Wayfindr sends the
request once more with it.

You rarely need to set anything: the default, `dpop = "auto"`, uses DPoP when
the server asks for it.

| `dpop`     | What happens                                                                                                                                                                                                                                                                                                                                                     |
| ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `"auto"`   | The default. DPoP when the MCP server asks for it (its protected resource metadata sets `dpop_bound_access_tokens_required`, or its 401 challenge offers the `DPoP` scheme) and the authorization server accepts ES256 proofs (`dpop_signing_alg_values_supported`). Once a grant uses DPoP, its refreshes and re-grants keep using it. Otherwise bearer tokens. |
| `"always"` | DPoP for every grant, even when the server doesn't ask. If the authorization server lists `dpop_signing_alg_values_supported` without ES256, the connector goes to `auth_error` (`dpop_alg_unsupported`).                                                                                                                                                        |
| `"never"`  | Bearer tokens only, even when the server asks for DPoP.                                                                                                                                                                                                                                                                                                          |

```toml
[connectors.secure.auth]
type = "oauth"
dpop = "always"
```

If the authorization server returns a plain bearer token (`token_type` other
than `DPoP`), Wayfindr sends it as a bearer token, so a server that doesn't bind
tokens keeps working.

The key of a sign-in is sealed in the database together with its token: it is
encrypted with the master key, replaced when the token is, kept across
refreshes, and deleted by `wayfindr auth logout`. An unattended grant's key is
held in memory only, like its token. Wayfindr doesn't log the key, and proofs
carry only its public half. A token stored before Wayfindr supported DPoP has no key
and is still sent as a bearer token.

Each proof carries the time it was made, so DPoP needs the daemon's clock to be
right. A proof the authorization server rejects puts the connector in
`auth_error` (`dpop_rejected`). One the MCP server rejects shows as a token it
refused: `token_rejected` for an auth flow, `grant_rejected` for an unattended
grant. See [OAuth errors](./troubleshooting-oauth#oauth-errors).
