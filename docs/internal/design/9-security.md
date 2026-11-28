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
  (`POST /admin/connectors`, `PUT /admin/server/public-url`, and
  `PUT /admin/server/oauth`, default token only) write `wayfindr.toml`, each
  with `newfile.Replace` against the bytes it read. A command connector runs a
  program, so the console never adds one from a single form post: it shows the
  exact argv and environment and waits for a confirm carrying a single-use,
  5-minute token bound to the session. The pending spec, secret values included,
  lives only in the daemon's memory and never reaches a page; a refused form is
  shown again without its secret values. Secret values in the add body are
  stored sealed and never written to the file or logged.
- Testing a cloud agent's path (`POST /admin/remote/test`, default token only,
  ADR 0021): the body holds a remote token's value, which `daemon`'s
  `remoteTester` sends as a bearer to `server.public_url`'s `/mcp` and nowhere
  else. It is never stored or logged, and neither are the URL's query, a
  redirect's `Location`, or any answer's body. The test follows no redirect,
  reads at most 1 MiB of an answer, and ends after 10 seconds. A second probe
  without a token detects a proxy that makes internet traffic look local
  (`proxy_treats_remote_as_local`). The OAuth form of the test (ChatGPT)
  sends no token at all: only the tokenless probe and two GETs of the
  metadata documents, under the same limits.
- Wayfindr's authorization server (ADR 0022, [§7.11](7.11-oauthserver.md)):
  off unless `[server.oauth] enabled`, and off its routes answer as before,
  byte for byte. On, its public routes and the `/mcp` challenge answer a
  remote request only through the tunnel (the issuer's `Host`, or a loopback
  `Host` with a forwarding header), so a DNS-rebinding page, with its own
  `Host` and no forwarding header, still gets the 404 and can't read
  `public_url`. Nothing reachable through the tunnel can grant access: the
  public `/oauth/authorize` only checks a request and keeps it pending, and
  only the console's approve page, on the loopback address, with a session,
  the same-origin `Origin`, and the CSRF token, makes a code. A `client_id`
  not in `clients` is refused before any fetch or pending entry. The
  authorization endpoint accepts a request only while the user has armed it
  from the console (Connect, 10 minutes), so a stranger can't queue a request
  at other times. Nor can a stranger keep the user's out: authorize never
  consumes the arm, so a stranger's valid requests (the client ID and
  `redirect_uri` are public) only sit beside the user's, at most 8, until the
  console claims one. The claim is the user's signed-in approve page, which
  only the user's own browser reaches with the id, by following the `302`; it
  ends the arm and drops every other pending authorization, so a stranger's
  request is never answered after it. The console cookie is `SameSite=Strict`
  and the approve page without a session claims nothing, so a site that opens
  the approve page with a stranger's id claims nothing by itself; only the
  user's click on the Continue page's Continue does, and that page says to
  continue only after selecting Connect in an app and to close a tab the user
  didn't open. Should the user continue anyway, the claim fails closed: the
  server counts the requests that became pending during the arm, and with more
  than one (the stranger's and the user's own) `Approve` refuses
  (`ErrContested`, a 409) and the page offers only Deny, so a stranger's
  request costs a retry, never a grant. A stranger who floods more than 8
  requests in the seconds before the user's claim can evict the user's, which
  likewise costs a retry. Turning OAuth off or on, changing `public_url`, or
  changing `clients` disarms the server (`Disarm`): the arm, the pending and
  claimed requests, and codes not yet exchanged go, so none survives a toggle. Only a top-level navigation passes authorize:
  with `Sec-Fetch-*` present it must be `navigate` to a `document`, and a
  prefetch or prerender is refused. Consent phishing, an attacker
  sending the user a request they started themselves, is narrowed to the
  minutes after the user's own Connect; the approve page shows when the
  request started, the IP the tunnel reported (the last `X-Forwarded-For`
  entry, the one the nearest proxy appended; no vendor header, which a browser
  could send itself; shown, never trusted), and a fixed warning to deny a link
  someone sent or a tab the user didn't open. Whether the client binds `state` to the user's own
  browser is the client's affair and isn't verified. CIMD
  documents are fetched through `httpclient.PublicOnly`, whose dialer refuses
  every address that is not public (the list is in
  [§7.11](7.11-oauthserver.md)) on the resolved address at connect time, with
  no redirect and a size and time limit, and cached. PKCE S256 is required,
  client authentication other than `none` is refused, `resource` and
  `redirect_uri` match exactly, and `iss` is on every authorization response.
  Codes and refresh tokens are spent by their first use, and one presented
  again after it was spent revokes its grant (a refresh token only after a
  30-second grace, for concurrent refreshes); access tokens expire (limits in
  [§7.11](7.11-oauthserver.md)). Tokens are stored as SHA-256 digests only. A
  grant is bound to `<public_url>/mcp` and to its client being allowed, so a
  changed public URL, or the client removed from `clients`, suspends it;
  reverting the change restores it. Codes, tokens, verifiers, challenges, and
  `state` are never logged; error pages and token errors echo no input.
- Approval gates (ADR 0024, [§7.6](7.6-gateway.md)): a remote client token or an
  OAuth grant can hold some of its calls until the owner answers. *Protected:*
  an agent that reaches Wayfindr only through a remote token or grant, through
  the tunnel: it can't reach `/admin/` or `/ui/` (loopback only, §7.7), so it
  can't approve its own call, however it is prompted, and no client message,
  elicitation, or `input_required` answers one. The owner sees the arguments as
  the gateway holds them (`redact.Visible`), and an approval binds the client
  token instance (name and creation time, so a token created again under a
  revoked name inherits nothing), the tool, and a digest of the arguments; it is
  spent by the next matching call, even a failed one. A gated call whose
  arguments repeat a key (keys that look alike by case or Unicode form included) or aren't valid
  UTF-8 is refused, so the digest is of what the upstream reads, and so is one
  whose arguments are over 64 KiB or 32 levels deep, which bounds what the
  owner reads and the queue holds. A resumed `input_required` skips the gate,
  but its state is bound to the instance and taken once, and only by the
  process that sealed it (its boot id), so a restart, which forgets the spent
  states, can't replay it. Entries that no longer parse
  gate every call of the client (fail closed). At most 8 live entries per
  client, refusing the ninth rather than evicting; arguments live only in the
  in-memory queue, never stored or logged, and a restart drops it.
  *Not protected:* a local agent, which can approve its own calls with the
  default token the admin API accepts (so `--approval` is refused on a token
  that isn't remote, by the CLI and by the admin API); an upstream that labels a
  destructive tool read-only (class entries trust its annotations; name the
  tool); an owner who approves a bad call; what the upstream does with an
  approved call. The arguments are shown in full and unredacted (console, CLI,
  `GET /admin/approvals`; `redact.Visible` only escapes invisible characters
  and runs of spaces, and the console and CLI show them decoded, keys sorted),
  so a secret in one is shown and the listing is
  sensitive; only the owner reaches it (the default token, or a console
  session). *Not bound:* the entries are fixed at creation and
  never edited; expiry is checked on access, so `expired` is best effort.
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
  request that goes to neither an upstream, an authorization server, the
  embedding provider, nor an allowed client's metadata document (ADR 0022). Off
  unless `[telemetry] enabled`; `DO_NOT_TRACK` and non-release builds block it.
  It carries counts and allowlisted names only (`internal/telemetry`): never a
  connector, tool, profile, scope, or token name, a URL or host (an embedding
  host is `local`, a public provider's name, or `other`), arguments, results, or
  error text. The install ID is a random UUIDv4 in `<data>/telemetry-id`,
  written only once telemetry runs. The request carries no credential and no
  header beyond `Content-Type` and `User-Agent`, and ignores the response body.
- A failed sync's error is stored for `status` to display, so the catalog Syncer
  redacts its URL query strings (`internal/redact`) before `ApplySync` stores it
  ([§7.4](7.4-catalog.md)).
- Single writer: only the daemon writes the store, and the daemon lock keeps it
  to one daemon per data directory ([§7.8](7.8-daemon.md)). `init` runs before
  any daemon exists. `init` refuses to run while a daemon is listening.
