# §4 Configuration reference

Part of the [Wayfindr technical design](../design.md). Go code cites this file
as "design §4".

Every key, default, and rule users see is in the public reference,
[Configuration](https://atomicpages.github.io/wayfindr/reference/configuration)
(`docs/reference/configuration.md`), which the code is checked against; it is
not copied here. All durations are Go duration strings, and unknown keys are
errors (`MetaData.Undecoded()`). This section keeps how the code reads the
config.

**Templates.** `${env:NAME}` and `${secret:name}` may appear in any string value
of `url`, `args`, `env`, `headers`, `api_key`, `client_secret`, `tls.key`,
`auth.signing_key.key`, and `auth.assertion.value`. The config stores the
templates unresolved. `upstream` resolves them through a `Resolver` each time it
builds a transport, so `wayfindr secret set` takes effect on the next connect
without a reload. A missing env var or secret puts the connector in
`auth_error`, with the reference named in the reason. `embedding.api_key` is
resolved by the daemon each time it builds the embedding client: at startup, on
a changed `[embedding]` table, and on a `wayfindr secret set` of a secret the
key refers to, which rebuilds the client and so starts the embedding pass
([§7.8](7.8-daemon.md) step 5, wayfindr-9ydc). An unresolved key leaves search
lexical only.

Load checks template syntax but never resolves. Every `${` must open a valid
reference:

| Reference        | Name grammar                 |
| ---------------- | ---------------------------- |
| `${env:NAME}`    | `[A-Za-z_][A-Za-z0-9_]*`     |
| `${secret:name}` | `[A-Za-z0-9][A-Za-z0-9_.-]*` |

The secret grammar is the older, wider rule (`config.SecretRefGrammar`), kept
so configs written under it load. A new secret's name follows the
connector-name rule, `config.NameGrammar` (wayfindr-vw6h):
`config.CheckSecretName` lets `PUT /admin/secrets/{name}` keep an older-rule
name only for a secret the daemon already knows, stored or referenced by the
running config. Generated names (`config.SecretNameFor`,
`NumberedSecretNameFor`) are cut to 63 bytes.

A `$` not followed by `{` is literal. There is no escape for a literal `${`, so
a shell-style `${HOME}` in `args` is a load error.

**Home directory (`~`).** Load expands a leading `~` in the values that hold
local paths: `storage.dsn` when the driver is `sqlite` (a postgres DSN is a URL
and is never touched), a stdio connector's `command` and each element of its
`args`, and a url connector's `tls.cert_file`, `tls.key_file`, `tls.ca_file`,
and `auth.assertion.file` (`tls.key` and `auth.assertion.value` are templates,
not paths). Only `~` alone and `~/…` expand; the rest of the value is kept as
written. `~user/…` is **not** expanded (there is no lookup of other users'
homes) and stays literal, as does a `~` anywhere but the start (`--dir=~/x`). A
value starting with `${` is a template and is left alone; in `~/${env:SUB}` the
`~` expands and the reference stays unresolved. Expansion runs before
validation, so validation sees absolute paths. The home directory is injected by
the caller (`config.Options.HomeDir`, from `paths.Env`) and is looked up only
when a value needs it; if it can't be resolved, each such value is a validation
error at its key.

**Validation** happens at load time (`internal/config/validate.go`), and reports
every problem at once as a `*ValidationError` of key paths. It checks that
exactly one of `url` or `command` is set, that each transport only gets its own
fields, and that the ADR 0007 rules hold (`pkce = false` requires
`client_secret`; `client_secret` requires `client_id`;
`resource_indicator = false` requires `client_id`; `authorization_endpoint` and
`token_endpoint` are set together; the first and last apply to the authorization
code grant only). Also:

- **Tools lists** (`tools`, ADR 0009): each entry is `@read`, `@write`, a glob
  on the upstream tool name (`path.Match` syntax), or `!glob`. A class can't be
  negated, and an entry must not be blank. The same check runs on a scope file's
  and a profile's tools lists.
- **Profiles** (`[profiles.<name>]`, ADR 0011): the name follows the
  connector-name rule, the table is checked as a scope file is (connector names,
  and no tools list for a connector its `connectors` leaves out), and every
  connector it names must be configured: unlike a scope file, which a teammate
  may write, a profile is the user's own, so an unknown name is a typo.
- **`authorize_params`**: no empty key, and none of the parameters Wayfindr sets
  itself (`config.ReservedAuthorizeParam`): `response_type`, `client_id`,
  `redirect_uri`, `state`, `scope`, `code_challenge`, `code_challenge_method`,
  `resource`, and `dpop_jkt`. Each refusal names the setting that decides it
  instead (`use scopes`, `use pkce`, …). `auth` skips the same keys when it
  builds the authorize URL ([§7.3](7.3-auth.md)), so they can't reach the
  authorization server either way.
- **`[connectors.<name>.tls]`**: allowed only on url connectors, and an empty
  table is an error. `cert_file` needs exactly one of `key_file` or `key`, and
  either key needs `cert_file`; `ca_file` alone is fine. The files are not read
  at load. See TLS below.
- **Grants and client authentication** (the newer `[auth]` keys): see below.
- `index.refresh_interval` and each connector's `timeout` must be positive;
  `calls.retention` must not be negative (0 turns the call log off).

**TLS (`[connectors.<name>.tls]`).** A url connector's TLS identity and trust,
for its MCP server's connections and its authorization server's (wayfindr-eqr7):

| Key         | Type     | Meaning                                                           |
| ----------- | -------- | ----------------------------------------------------------------- |
| `cert_file` | path     | PEM client certificate, then any intermediates                    |
| `key_file`  | path     | PEM private key for `cert_file`                                   |
| `key`       | template | the same key as PEM, usually `${secret:…}`; instead of `key_file` |
| `ca_file`   | path     | PEM CA certificates trusted in addition to the system roots       |

`internal/httpclient` builds the client ([§2](2-package-map.md)). It resolves
`key` when it builds the client, so a missing secret is an error then, not a
failed handshake later; `httpclient.Clients` resolves it on every lookup and
rebuilds when the value changes, so `wayfindr secret set` applies at the next
connect. It reads `ca_file` when it builds the client, and
`cert_file`/`key_file` again at any handshake after either file's mtime or size
changes, so a certificate rotated on disk is used without a reload. `ca_file` is
read only when a client is built: a CA file edited in place is trusted once the
table changes or the daemon restarts. `tls` is part of `config.Diff`'s connector
comparison (`config.TLSEqual`), so an edited table reconnects the connector, it
is part of auth's `outdated` rule (an edited table cancels the connector's
pending auth flows, [§7.3](7.3-auth.md)), and `tls.key`'s secret is in
`Connector.Secrets()`.

The daemon builds one `httpclient.Clients` and hands it to the manager
(`upstream.Options.Clients`: the connector's base transport) and the broker
(`auth.BrokerOptions.Clients`: every request to its authorization server), and
calls `Clients.Retain` on each reload, so a removed connector's client and pool
go with it. Both OAuth probes (`connector add` in the CLI,
`POST /admin/connectors` in the daemon) build the client of the connector being
added with `httpclient.For`; `connector add` and `edit`
`--cert-file/--key-file/--ca-file` writes the table. The daemon's health check
reads the certificate's `NotAfter` (`httpclient.CertificateExpiry`) for the
`certificate_expiring` warning ([§7.8](7.8-daemon.md)).

**Grants and client authentication (`[connectors.<name>.auth]`).** The keys of
ADR 0018's three axes; `auth` reads them ([§7.3](7.3-auth.md)), and
`connector add`/`edit` write them.

| Key                                    | Type                      | Default               | Meaning                                                                                                                                       |
| -------------------------------------- | ------------------------- | --------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `grant`                                | `config.Grant`            | `authorization_code`  | also `device_code`, `client_credentials`, `jwt_bearer`                                                                                        |
| `token_endpoint_auth_method`           | `config.ClientAuthMethod` | chosen by `auth`      | `none`, `client_secret_basic`, `client_secret_post`, `client_secret_jwt`, `private_key_jwt`, `tls_client_auth`, `self_signed_tls_client_auth` |
| `dpop`                                 | `config.DPoPMode`         | `auto`                | also `always`, `never`                                                                                                                        |
| `device_authorization_endpoint`        | URL                       | discovered            | RFC 8628 endpoint override                                                                                                                    |
| `signing_key.key`                      | template                  | required in the table | PEM private key, usually `${secret:…}`                                                                                                        |
| `signing_key.kid`, `signing_key.alg`   | string                    | none; `alg` chosen    | `alg`: `ES256`, `ES384`, `RS256`, `PS256`, `EdDSA`                                                                                            |
| `signing_key.audience`                 | `config.Audience`         | `issuer`              | also `token_endpoint` (Okta, Entra)                                                                                                           |
| `assertion.file`, `.value`, `.subject` | path, template, string    | none                  | the jwt_bearer grant's assertion; exactly one                                                                                                 |

`applyDefaults` fills `grant`, `dpop`, and `signing_key.audience` when the key
is absent; an explicit `""` is left for validation to refuse. The zero values
mean the defaults too, so an `Auth` built in code without them reads as the
authorization code grant with `dpop = "auto"`. `Grant.Unattended()` is the one
place that classifies a grant: `client_credentials` and `jwt_bearer` are
unattended (no user; the token is never stored).

`validate_auth.go` keeps each path's rules in one function: the rules every
table follows, then one per concept (`clientAuthRules`, `dpopRules`,
`signingKeyRules`, `assertionRules`), then one per grant (`authCodeRules`,
`deviceRules`, `clientCredentialsRules`, `jwtBearerRules`, the last two sharing
`unattendedRules`). An unknown enum value lists the known ones.

- Unattended grants need `client_id`, and refuse `redirect_uri`,
  `authorize_params`, `client_metadata_url`, `authorization_endpoint`,
  `device_authorization_endpoint`, and an explicit `pkce` (the validator asks
  `MetaData.IsDefined`, so the default `true` doesn't count). `token_endpoint`
  may be set alone.
- `client_credentials` needs `client_secret`, `signing_key`, or a `tls_*`
  method. `jwt_bearer` needs `[auth.assertion]` with exactly one of `file`,
  `value`, and `subject`, and `subject` needs `signing_key`. An assertion table
  with any other grant is refused.
- `device_code` refuses `redirect_uri` and `authorization_endpoint`: it has no
  authorization request or callback. `token_endpoint` may be set alone.
- `token_endpoint_auth_method`: `private_key_jwt` needs `signing_key`, the
  `client_secret_*` methods need `client_secret`, `none` refuses both, and the
  JWT methods need `client_id`. `tls_client_auth` and
  `self_signed_tls_client_auth` need the connector's `[tls]` client certificate
  (`cert_file`) and `client_id`; `clientAuthRules` gets the connector's tls
  table for it.
- `signing_key` needs `client_id`: a dynamically registered client gets a secret
  or none. `alg` and `audience` must be known values.

`AuthEqual` compares every new key, the two tables included, so a change cancels
pending auth flows; `Connector.Secrets()` includes the references in
`signing_key.key`, `assertion.value`, and `tls.key`. `connector edit --grant` to
an unattended grant deletes the stored OAuth credential, as
`--unset auth.client_id` does; a hand edit leaves it for `wayfindr auth logout`.

**Listen address.** `server.listen` accepts a loopback IP literal or the name
`localhost`; validation never resolves names. A hosts file can map `localhost`
anywhere, so `httpd.Listen` also checks the address it actually bound and
refuses to serve (`httpd.ErrNotLoopback`) unless it is in `127.0.0.0/8` or
`::1`.

**Public URL.** `server.public_url` (ADR 0021) is empty or the `https` address a
tunnel serves: no path but `/`, no query, fragment, or userinfo
(`validator.publicURL`; its messages never quote the value). It is
informational, so `config.Diff` ignores it and it needs no restart: the daemon's
`configState` takes it from each applied config, and `GET
/admin/server/public-url` reads it there. No guard compares a request with it.
`config.ListenAddress` reads `listen` alone and never validates it. Clearing it
while `server.oauth.enabled` is true is refused (below).

**Authorization server.** `[server.oauth]` (ADR 0022) is `config.OAuth` at
`Server.OAuth`; its default, its validation rules, `Server.Issuer()`, and the
`SetPublicURL` refusal while it is on are in [§7.11](7.11-oauthserver.md).
Like `public_url`, `config.Diff` ignores the table: a reload applies it
through the daemon's token service and the authorization server, which reads
the running config on every request.

`config.Diff(old, new)` produces per-connector `Added`, `Removed`, `URLChanged`,
`Changed`, and `Unchanged` sets, plus `EmbeddingChanged`. It also lists
`RestartRequired` fields (`server.*`, `storage.*`). A connector whose tools list
changed is `Changed`, so it reconnects and its new session's sync applies the
list ([§7.4](7.4-catalog.md)). Profiles are not diffed: the gateway reads each
request's profile from the running config ([§7.6](7.6-gateway.md)), so an edit
applies from the next request.

**Editing the file.** `connector add`, `edit`, and `remove` change the user's
file in place with `config.AppendConnector`, `EditConnector`, and
`RemoveConnector`. Each works on the bytes, keeps the rest of the file byte for
byte, re-parses the result, and refuses (`*config.Error`, Kinds `conflict`,
`unsafe`, `invalid`) any edit that would read differently than written or change
another connector; the CLI writes the result with `newfile.Replace`. The daemon
is the third writer: `POST /admin/connectors` appends a table the same way, for
the web console (ADR 0017, [§7.8](7.8-daemon.md)), `PUT
/admin/server/public-url` sets or clears `server.public_url` with
`config.SetPublicURL`, which rewrites that one line (or adds a `[server]` table)
under the same checks and refuses a `server` written as a dotted key or inline
table (`CaseServerNotTable`). `PUT /admin/server/oauth` turns
`server.oauth.enabled` on or off with `config.SetOAuthEnabled` the same way
([§7.11](7.11-oauthserver.md)); it never edits `clients`. All three daemon
writes share one lock. Every
writer passes the bytes
it read as `newfile.Replace`'s `expect`, so a write that would lose another's
edit fails with `newfile.ErrChanged` instead; the check and the rename are two
steps, so two writes in the same instant can still lose one.
