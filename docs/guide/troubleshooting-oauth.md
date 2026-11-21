# Troubleshooting OAuth and TLS

Errors from a connector's sign-in, tokens, and TLS. For anything else, or to
start from `wayfindr status`, see [Troubleshooting](./troubleshooting).

## OAuth errors

`wayfindr status` and an agent's error result word an OAuth problem one way.
`wayfindr auth login <connector>` talks to the authorization server during sign-in, so
it sometimes words the same problem with more detail; those differences are
listed below the table. A provider's own error code, when it sent one, follows in
parentheses, such as `(provider code invalid_client)`.

| `wayfindr status` says                                                                                                                                                              | Fix                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Wayfindr has no OAuth client to authorize with.` (with `(provider code …)` when the provider refused registration)                                                                 | Register an app with the provider and set `client_id` (and `client_secret`). The provider has no dynamic client registration, or refused Wayfindr. See [A client you registered](./authentication#a-client-you-registered).                                                                                                                                                                                                                                                                                                                                          |
| `The authorization server does not advertise PKCE (S256), which Wayfindr requires for a connector without a client_id.`                                                             | Register an app and set `client_id`. Wayfindr uses PKCE for every client it registers itself.                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `The authorization server does not support signing in with a code on another device (device code).`                                                                                 | The provider has no device code support, or its metadata doesn't list the endpoint. Set `device_authorization_endpoint` if it has one; otherwise sign in in a browser: drop `--device`, or set `grant = "authorization_code"`. See [Signing in without a browser on this machine](./authentication#signing-in-without-a-browser-on-this-machine-device-code).                                                                                                                                                                                                        |
| `The authorization server rejected the configured OAuth client.`                                                                                                                    | Check `client_id` and `client_secret`. Fix one with `wayfindr connector edit <connector> --client-id …` or `--client-secret`.                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `Wayfindr cannot authenticate as the connector's OAuth client: the authorization server accepts no client authentication method that fits the client's credential.`                 | The provider lists client authentication methods, and none works with the `client_secret` or `signing_key` you configured. Set `token_endpoint_auth_method` to one it accepts, with the credential it needs, for example `wayfindr connector edit <connector> --token-auth-method private_key_jwt --signing-key < key.pem`. Nothing was sent. See [Client authentication](/reference/configuration#client-authentication).                                                                                                                                           |
| `The connector's signing_key is not a private key Wayfindr can sign with, or cannot sign the configured alg.`                                                                       | Store a PEM private key (EC P-256 or P-384, RSA of 2048 bits or more, or Ed25519) with `wayfindr connector edit <connector> --signing-key < key.pem`, or fix `signing_key.alg` to one the key signs. See [Private key JWT](./authentication-enterprise#private-key-jwt).                                                                                                                                                                                                                                                                                             |
| `The connector's client_id changed since it was authorized. Authorize it again.`                                                                                                    | Run `wayfindr auth login <connector>`. The stored token belongs to the old client. See [Changing a connector's OAuth client](./authentication#changing-a-connector-s-oauth-client).                                                                                                                                                                                                                                                                                                                                                                                  |
| `The server now sends Wayfindr to a different sign-in server than the one that issued the connector's token, so the token is not refreshed there.` (`authorization_server_changed`) | The MCP server's metadata, or the connector's `token_endpoint`, now names another token endpoint than the one the token came from, so Wayfindr won't send it the refresh token. If you expected the move, run `wayfindr auth login <connector>`; the sign-in page shows the new server. If not, set `authorization_server` to the one you trust.                                                                                                                                                                                                                     |
| `The authorization server's metadata names a different issuer than the one it was found for.`                                                                                       | See [Issuer mismatch](#issuer-mismatch).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `Wayfindr does not trust the TLS certificate of the connector's authorization server, or the connector's ca_file cannot be read.` (`tls_untrusted`)                                 | See [A TLS certificate isn't trusted](#a-tls-certificate-isn-t-trusted).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `The TLS handshake with the connector's authorization server failed: …` (`tls_handshake`)                                                                                           | See [The server refuses the client certificate](#the-server-refuses-the-client-certificate).                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `Wayfindr cannot find the connector's authorization server endpoints.`                                                                                                              | Set `authorization_endpoint` and `token_endpoint`, or `authorization_server`, in the connector's `[auth]` table. See [Endpoints from config](./authentication#endpoints-from-config).                                                                                                                                                                                                                                                                                                                                                                                |
| `The authorization server no longer accepts Wayfindr's registered OAuth client.`                                                                                                    | Authorize again. Wayfindr registers a new client.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `The stored credential cannot be read, for example because the master key changed.`                                                                                                 | See [Credentials stopped working after a key change](#credentials-stopped-working-after-a-key-change).                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `The upstream needs scopes the connector was not granted.`                                                                                                                          | Authorize again; the new sign-in asks for more scopes. See [A tool needs more scopes](./authentication#a-tool-needs-more-scopes).                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `The connector's token expired and cannot be refreshed.` or `…revoked or expired the connector's grant.`                                                                            | Authorize again.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `The upstream rejected a freshly refreshed token.`                                                                                                                                  | `token_rejected`: the MCP server answered 401 to a token Wayfindr had just refreshed. Authorize again. If it comes back, see [The MCP server rejects a token](#the-mcp-server-rejects-a-token).                                                                                                                                                                                                                                                                                                                                                                      |
| `The authorization server refused the connector's unattended grant (provider code …).`                                                                                              | A `client_credentials` or `jwt_bearer` connector (`grant_rejected`): the provider refused the grant (`invalid_grant`, `invalid_scope`, `unauthorized_client`, …), or the server rejected a token fresh from it. Give the OAuth client access to the grant and its scopes at the provider, or fix the assertion (an expired workload token, the wrong audience), `client_id`, or `scopes`. A config change retries at once; after a fix at the provider, run `wayfindr auth logout <connector>`. See [Machine-to-machine and workloads](./authentication-unattended). |
| `The upstream needs scopes the connector's unattended grant asked for but was not given.`                                                                                           | The server answered `insufficient_scope`, and a new grant with the larger set of scopes didn't help (`scope_denied`). Allow the OAuth client those scopes at the provider, then run `wayfindr auth logout <connector>`, or fix `scopes`.                                                                                                                                                                                                                                                                                                                             |
| `The connector's dpop is "always", but the authorization server does not accept ES256 DPoP proofs, the only kind Wayfindr signs.`                                                   | `dpop_alg_unsupported`: the server's `dpop_signing_alg_values_supported` doesn't list ES256. Set `dpop = "auto"` or `"never"` to send bearer tokens. Nothing was sent. See [DPoP](./authentication-enterprise#dpop).                                                                                                                                                                                                                                                                                                                                                 |
| `The authorization server rejected Wayfindr's DPoP proof (provider code invalid_dpop_proof). The clock of the host the daemon runs on may be wrong.`                                | `dpop_rejected`: each proof carries the time it was made, and the server checks it. Set the host's clock right (turn on NTP). If the server still refuses the proofs, set `dpop = "never"` if it accepts bearer tokens. A config change retries at once. See [DPoP](./authentication-enterprise#dpop).                                                                                                                                                                                                                                                               |
| `A ${…} reference or file in the connector's auth setup cannot be resolved.`                                                                                                        | `unresolved_value`: a reference in `url`, `client_secret`, `signing_key.key`, `assertion.value`, or `tls.key` names no secret or variable; set it, or fix the reference. For a `jwt_bearer` connector, also when its `assertion.file` is missing or empty, or its `assertion.value` resolves to nothing. Check the file the workload token is projected to, or the variable.                                                                                                                                                                                         |

Where `wayfindr auth login` words it differently:

- **No OAuth client.** `There is no OAuth client to authorize with: no client_id is configured, and the authorization server supports neither client ID metadata documents nor dynamic client registration.` When the provider has registration but refused Wayfindr: `The authorization server rejected dynamic client registration (provider code …).` The fix is the same.
- **Issuer mismatch.** Adds `Set validate_issuer = false only if you trust this server.`
- **TLS.** `Wayfindr does not trust the authorization server's TLS certificate: …` and `The TLS handshake with the authorization server failed: …`.
- **Endpoints.** `The connector's protected resource metadata names no authorization server.` or `The authorization server publishes no metadata.`
- **Registered client gone.** `The authorization server no longer accepts the OAuth client Wayfindr registered for this connector.`
- **Unresolved value.** `A value in the connector's config cannot be resolved: a ${…} reference (url, client_secret, signing_key.key, assertion.value, or tls.key) or the assertion file.`
- **Only in `wayfindr auth login`.** `<connector> uses an unattended grant, so there is nothing to sign in to: Wayfindr gets its token itself.` (`not_interactive`). Nothing to do: a `client_credentials` or `jwt_bearer` connector has no sign-in. Check it with `wayfindr status`; `wayfindr auth logout <connector>` makes Wayfindr get a new token.
- **Only in `wayfindr status`.** The two unattended-grant rows above (`grant_rejected`, `scope_denied`): `wayfindr auth login` has no sign-in to fail for those connectors.

## The MCP server rejects a token

A token the authorization server issued, and the MCP server then refused, reads
differently from a refusal at the token endpoint:

| Grant               | Case             | `wayfindr status` says                                                              |
| ------------------- | ---------------- | ----------------------------------------------------------------------------------- |
| An auth flow        | `token_rejected` | `The upstream rejected a freshly refreshed token.` (`needs_auth`)                   |
| An unattended grant | `grant_rejected` | `The authorization server refused the connector's unattended grant.` (`auth_error`) |

Wayfindr already retried once with a fresh token before saying so. Check, in
order:

1. **DPoP.** A connector that uses DPoP sends a proof with every call. The MCP
   server checks the proof's time and its own nonce, and Wayfindr retries once
   with a new nonce when asked, so a rejection that persists usually means the
   daemon host's clock is off: set it right (turn on NTP). If the server still
   refuses, set `dpop = "never"` in the connector's `[auth]` table if it takes
   bearer tokens. The authorization server rejecting a proof is `dpop_rejected`
   instead; see [OAuth errors](#oauth-errors) and [DPoP](./authentication-enterprise#dpop).
2. **Certificate-bound tokens.** If the server binds tokens to a client
   certificate and the connector has none, the reason in `wayfindr logs` says to
   add `cert_file` and `key_file` in `[tls]`. See
   [Certificate-bound tokens](./authentication-enterprise#certificate-bound-tokens).
3. **The token itself.** The MCP server may want a token for another audience,
   or with scopes the provider didn't grant. The provider's and the server's own
   logs say which.

After a fix, an auth flow needs a new sign-in (`wayfindr auth login <connector>`); an
unattended grant retries at once after a config change, or after
`wayfindr auth logout <connector>` for a fix at the provider.

## An authorization didn't finish

`wayfindr auth login <connector>` prints why, then the fix:

```text
linear was not authorized.
linear: The authorization expired: it was not completed within 10 minutes.
  Open the link to start it again.
Error: the authorization of linear expired
```

| Summary starts with                                                                                                                                                        | What happened                                                                                                                                                                                                                                                 |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `The authorization expired: it was not completed within 10 minutes.`                                                                                                       | You didn't finish in the browser in time, or didn't enter the device code before it expired. Run it again.                                                                                                                                                    |
| `The authorization was cancelled: the connector's credential was revoked, or its url or [auth] table changed or it was removed from the config, while it was in progress.` | Something changed mid-flow. Run it again.                                                                                                                                                                                                                     |
| `The authorization was not granted`                                                                                                                                        | You declined, or the provider refused. The provider code is in parentheses. With a device code, `access_denied` means you, or the provider, denied the code.                                                                                                  |
| `The authorization server sent a response Wayfindr cannot read.`                                                                                                           | With `--device`: the provider's answer lacked the code, or named an address to enter it that is neither `https` nor `http` on a loopback address such as `localhost`, which Wayfindr never shows, since it could be a phishing page. The daemon log names it. |
| `The authorization server's answer did not name the issuer this authorization was started with, so Wayfindr did not use it. This may be a mix-up attack.`                  | Run it again. If it repeats, report it with the daemon log. No config change fixes it.                                                                                                                                                                        |

## `redirect_uri_mismatch`

The browser shows `Error 400: redirect_uri_mismatch` (Google's wording) when you
sign in. The provider doesn't have the redirect URI Wayfindr sent registered for
your OAuth client.

Register exactly `http://127.0.0.1:7777/oauth/callback`, or the `redirect_uri`
you configured, in the provider's console; for Google, as an authorized redirect
URI of a **Web application** client. Google can take from minutes to hours to
apply a change, so the error may clear on its own. Then run:

```sh
wayfindr auth login gmail
```

The connector stays configured while you wait. See
[Google](./authentication#google).

## The provider rejects the loopback redirect

Some providers refuse to register `http://127.0.0.1:7777/oauth/callback`: they
want `https`, or a host name instead of an IP address.

- If it accepts `localhost`, set
  `redirect_uri = "http://localhost:7777/oauth/callback"` in the connector's
  `[auth]` table and register that. The daemon serves the callback on any host
  name.
- If it wants `https`, run an HTTPS tunnel to the daemon and set `redirect_uri`
  to the tunnel's URL. See
  [A redirect URI through a tunnel](./authentication#a-redirect-uri-through-a-tunnel).

## Google asks you to sign in every hour

A Google connector, such as Gmail, goes to `needs_auth` about an hour after each
sign-in, and `wayfindr status` shows a `no_refresh` note or warning. Google
issued no refresh token, because the sign-in didn't ask for
`access_type=offline`. Add it, and `prompt=consent` so a later sign-in gets a
new one, then sign in again:

```sh
wayfindr connector edit gmail \
  --authorize-param access_type=offline --authorize-param prompt=consent
wayfindr auth login gmail
```

See [Google](./authentication#google).

## Google asks you to sign in every week

A Google connector goes to `needs_auth` about 7 days after each sign-in. Your
OAuth consent screen is **External** and still in **Testing**, and Google
expires a Testing app's refresh tokens after 7 days. Publish the app in the
Google Cloud console, then run `wayfindr auth login <connector>` once more. See
[Google](./authentication#google).

## A TLS certificate isn't trusted

```text
corp: Wayfindr does not trust the TLS certificate of the connector's authorization server, or the connector's ca_file cannot be read.
  If the server uses a private CA, set ca_file in the connector's [tls] table to that CA's PEM certificate. Wayfindr reconnects the connector when the config file changes.
```

The connector is in `auth_error` (`tls_untrusted`): the authorization server's
certificate was issued by a CA your system doesn't trust, or it doesn't verify
(for example, it names another host). A company server on a private CA needs
that CA in `ca_file`; see
[Client certificates and private CAs on the connectors page](./connectors).
The same message appears when `ca_file` itself can't be read or holds no
certificate. If the MCP server's own certificate isn't trusted, the connector is
`unreachable` instead, and the same `ca_file` fixes it.

## The server refuses the client certificate

```text
corp: The TLS handshake with the connector's authorization server failed: it refused the client certificate, or the lack of one, or the certificate cannot be loaded.
  Check cert_file and key_file (or key) in the connector's [tls] table: …
```

The connector is in `auth_error` (`tls_handshake`). The server asks for a client
certificate and refused the one Wayfindr presented, or none was configured; or
`cert_file` and its key can't be read or don't match. Check the `[tls]` table:
the certificate must be one the server accepts. A certificate replaced on disk
is presented from the next connection; save the config file to reconnect a
connector that is in `auth_error`.

## Issuer mismatch

```text
linear: The authorization server's metadata names a different issuer than the one it was found for.
  Check authorization_server in the connector's [auth] table.
```

The connector is in `auth_error`. The authorization server's metadata doesn't
match the URL it was discovered at, which RFC 8414 forbids. Issuers that differ
only by a trailing `/` are not a mismatch. Check `authorization_server`. If you
trust the server and it is simply misconfigured, set `validate_issuer = false`
in the connector's `[auth]` table. The daemon logs a warning every time it uses
that setting.

## Credentials stopped working after a key change

```text
linear: The stored credential cannot be read, for example because the master key changed.
  Open the link to authorize the connector again.
```

The master key that sealed the credential is gone: a new keychain, a changed
`WAYFINDR_SECRET_KEY`, or a deleted `master.key`. Authorize each OAuth connector
again and set each secret again. See
[Master key](/reference/paths-and-environment#master-key).
