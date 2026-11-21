# §9 Security checklist

Part of the [Wayfindr technical design](../design.md). Go code cites this file
as "design §9".

- Credentials never cross the client boundary. Search results and errors carry
  definitions and links only.
- Constant-time comparison of the bearer token. Loopback-only bind, enforced at
  validation. Host and Origin guards as in [§7.7](7.7-httpd.md).
- The client token goes only to the daemon that holds this data directory's
  daemon lock with the configured `server.listen` (`daemonctl.Verify`,
  [§7.9](7.9-cli.md); wayfindr-zvjw). The shim and every admin command check it
  before every new connection (`Target.Transport`), not only before a session's
  first, since after a daemon restart a client simply dials again. So another
  local user who takes the port while the daemon is down gets neither the token
  nor a secret's value: the lock is in the user's 0700 data directory. A
  `/healthz` answer alone proves nothing. Residuals: the daemon exiting and the
  port being taken between a check and its dial (milliseconds); and a daemon
  that took the lock but then failed to bind a port someone else held, between
  its lock and its exit (milliseconds, but repeated each time a service manager
  restarts it; wayfindr-appz). A client set up for HTTP with the token in its
  environment trusts whatever answers; the public security guide says so.
- OAuth: `state` is single-use with a TTL. PKCE S256. Issuer checks (RFC 8414 /
  RFC 9207), opt-out only per connector and logged on every use. Resource
  indicators. No token is ever sent to a URL other than the `resource` it was
  bound to.
- Redirects: an upstream request is refused before dialing if its origin is not
  the connector's ([§7.2](7.2-upstream.md)), so no redirect or SSE `endpoint`
  event carries a connector's headers, token, or client certificate elsewhere.
  Token, device authorization, and registration requests follow no redirect at
  all: they carry secrets in the body.
- Sealed storage with row-bound AAD. Key-file fallback is mode 0600 and warned.
  The daemon refuses to generate a key over existing ciphertext.
- DPoP keys ([§7.3](7.3-auth.md), RFC 9449): an auth flow's private key is
  sealed inside its token blob (`dpop_key`), so it is replaced and deleted with
  the token and never stored on its own; an unattended grant's lives in memory
  only. Proofs carry only the public JWK. Keys and proofs are never logged, and
  the DPoP header is a header value like any other.
- Client authentication keys ([§7.3](7.3-auth.md)): a `signing_key.key` is a
  template, so `connector add`/`edit --signing-key` store it as a sealed secret
  and the config holds only its reference. It is resolved and parsed per use,
  never cached outside the Registration and never logged; a parse error never
  quotes the key. A client assertion lives 60s with a fresh `jti`.
  `client_secret_jwt` sends only an HS256 JWT, never the secret.
- Client certificates ([§4](4-configuration.md), `internal/httpclient`):
  `cert_file`/`key_file` are read from disk at each handshake after they change;
  the key file's mode is the user's to keep (the public docs say 0600; Wayfindr
  doesn't check it). A `tls.key` is a template, usually a sealed secret,
  resolved when the client is built. A certificate that fails to load is
  `tls_handshake`, a `ca_file` that fails is `tls_untrusted`; neither error
  quotes key material.
- Unattended grants: the token and its DPoP key live in `unattendedTokens`'
  memory only and are never stored, so no credentials row exists for such a
  connector (by construction: `unattendedTokens` never touches the credentials
  module). An assertion (file or value) is read per grant, never kept, and never
  quoted in an error.
- Device code: the verification URI (and the complete one) is shown or opened
  only if it is `https`, or `http` on a loopback host (`httpsOrLoopback`); any
  other fails the flow as `malformed_response`, so a hostile metadata document
  can't send the user to a phishing page for the user code.
- Upstream schemas cannot trigger fetches (no remote `$ref`).
- The web console ([§7.10](7.10-webui.md), ADR 0016): the bearer token never
  reaches the browser; a login code is single-use, lives 60s, and travels in the
  URL fragment only. The session cookie is `HttpOnly`, `SameSite=Strict`,
  `Path=/ui`, in memory. Every form carries the session's CSRF token, compared
  in constant time, on top of the same-origin `Origin` check. Pages are
  `html/template` (upstream text is escaped), sent with a CSP that allows no
  inline script or style and no framing, and `no-store`. The console only calls
  the admin API. It adds connectors (ADR 0017) and never edits or removes one. A
  sign-in redirect goes to an http(s) authorize URL only.
- Writing the config: the user, the CLI, and the daemon
  (`POST /admin/connectors`, default token only) write `wayfindr.toml`, each
  with `newfile.Replace` against the bytes it read. A command connector runs a
  program, so the console never adds one from a single form post: it shows the
  exact argv and environment and waits for a confirm carrying a single-use,
  5-minute token bound to the session. The pending spec, secret values included,
  lives only in the daemon's memory and never reaches a page; a refused form is
  shown again without its secret values. Secret values in the add body are
  stored sealed and never written to the file or logged.
- Log redaction: header values, tokens, codes, secret values, and URL query
  strings are never logged. The values of a connector's `${…}` references (url,
  headers, args, env) are masked as the reference wherever they sit, a url's
  path included: `redact.Values`, built by `config.Mask` where the templates are
  resolved, applied in upstream's and auth's error text and in discovery's logs.
  `credentials.resource` stores the url's SHA-256, and OAuth metadata naming
  such a value is not cached (wayfindr-wt2v).
- The call log keeps no arguments or results, only the error text of a call that
  did not end `ok` (ADR 0014, amended): one line, `redact.Printable` and
  `redact.URLQueries`, at most 500 bytes, deleted with its row. The console
  renders it escaped, like every upstream text.
- Usage report (ADR 0020, [§7.8](7.8-daemon.md) item 11): the only outbound
  request that goes to neither an upstream, an authorization server, nor the
  embedding provider. Off unless `[telemetry] enabled`; `DO_NOT_TRACK` and
  non-release builds block it. It carries counts and allowlisted names only
  (`internal/telemetry`): never a connector, tool, profile, scope, or token
  name, a URL or host (an embedding host is `local`, a public provider's name,
  or `other`), arguments, results, or error text. The install ID is a random
  UUIDv4 in `<data>/telemetry-id`, written only once telemetry runs. The request
  carries no credential and no header beyond `Content-Type` and `User-Agent`,
  and ignores the response body.
- A failed sync's error is stored for `status` to display, so the catalog Syncer
  redacts its URL query strings (`internal/redact`) before `ApplySync` stores it
  ([§7.4](7.4-catalog.md)).
- Single writer: only the daemon writes the store, and the daemon lock keeps it
  to one daemon per data directory ([§7.8](7.8-daemon.md)). `init` runs before
  any daemon exists. `init` refuses to run while a daemon is listening.
