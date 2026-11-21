# 0012 — Limit a client to named connectors with scoped client tokens

Status: Accepted (2026-11-11). Amends the client-auth decision of
[0003](./0003-single-daemon), and makes the client token the outer limit that
[0010](./0010-project-scopes) and [0011](./0011-named-profiles) narrow within.
Amended:

- 2026-11-12 (wayfindr-3e6f): every start of a background daemon leaves a scoped
  `WAYFINDR_CLIENT_TOKEN` out of its environment, `wayfindr start` included.

## Context

`wayfindr init` creates one client token ([0003](./0003-single-daemon)). Every
client presents it, and every client sees every connector. The daemon keeps one
digest and doesn't know which client is calling.

So an unattended agent, such as a nightly job that triages GitHub issues, can
reach Gmail and Slack just like the user's interactive client. If a prompt
injection in an issue body takes it over, everything is in reach. The same token
also opens `/admin/`, so any client can set secrets and revoke credentials.

Project scopes and profiles ([0010](./0010-project-scopes),
[0011](./0011-named-profiles)) don't close this gap. The client picks them
itself, by sending a header, and a client that leaves the header off gets
everything its token allows. Both records rely on "a scope only narrows what its
client token already allows". With one token that allows everything, those words
have no force.

## Decision

- The client token `wayfindr init` creates is the **default client token**. It
  can use every connector and the admin API, as before.
- A **scoped client token** is a named token limited to a list of connectors.
  `wayfindr token create <name> --connector …` makes one through the admin API
  and prints its value once. `wayfindr token list` shows the names, connectors,
  and when each was created and last used, never a value.
  `wayfindr token revoke <name>` deletes it.
- The limit is per connector only. Tool lists, profiles, and scope files still
  decide which of a connector's tools a client sees.
- The token is the outer limit, and the server always enforces it. A connector
  the token doesn't allow is as if it were not configured: `search_tools` leaves
  it out of results, of `unavailable`, and of the configured list in an error,
  and `call_tool` refuses it as `unknown_tool` with Case `no_connector`. The
  token is checked before the tool policy, the profile, and the scope file, so
  no `hidden_by_*` Case reveals that the connector exists.
- The admin API is for the default token only. A valid scoped token gets 403
  `This client token cannot use the admin API.` on every `/admin/*` route;
  anything else that isn't the default token gets 401.
- A scoped token's value is `wf_s_` and the base64url of 32 random bytes. The
  prefix tells it apart from the default token, for people and for secret
  scanners. Only its SHA-256 is stored, in a `client_tokens` table keyed by user
  and name. Names follow the connector-name rule, and `default` is reserved.
- At startup the daemon loads every scoped token into an in-memory registry,
  before it serves. httpd's bearer guard resolves each request's token to a
  client: the default token, compared in constant time, or a digest in the
  registry. A create or revoke updates the registry as well as the store, so a
  revoke applies from the next request on, with no restart.
- The resolved client reaches the gateway through the MCP SDK's `TokenInfo`. The
  gateway serves MCP statelessly, so no session outlives a request: each request
  is narrowed by its own token.
- The call log records the name of the token each call authenticated with, in a
  `token_name` column next to `client`. `client` stays the `clientInfo` name the
  client asserts; `token_name` is what the daemon verified. It holds a name,
  never a token value.
- A scoped value is never the default token. The daemon refuses to start when
  its default token, from `WAYFINDR_CLIENT_TOKEN` or `<data>/client-token`, is
  scoped, since it would get every connector and the admin API. `wayfindr stdio`
  leaves a scoped `WAYFINDR_CLIENT_TOKEN` out of the environment of a daemon it
  starts, so a client using a scoped token can still start the daemon, which
  reads the default token from the file. Every start of a background daemon goes
  through one starter (`internal/daemonctl`), which does the same, so
  `wayfindr start` does too (amended 2026-11-12, wayfindr-3e6f).
- A scoped token's connectors can't be edited. To change them, revoke the token
  and create a new one. The value can't be read back, so a changed scope means
  handing out a new value anyway.
- A scoped token does not narrow the OAuth scopes Wayfindr requests from an
  upstream. That is a separate layer: the token decides who may use a
  connector's credential, not what the credential can do.

## Consequences

- An unattended agent can get only the connectors its job needs, and can't use
  the admin API, whatever a prompt injection tells it.
- "A scope only narrows what its client token already allows" in
  [0010](./0010-project-scopes) and [0011](./0011-named-profiles) now holds for
  every client. A client still picks its own profile and scope file, but it
  can't escape its token.
- The default token works unchanged, so existing clients need nothing new.
- A connector removed from the config simply stops matching a token that names
  it. Re-adding it under the same name restores access, as with credentials
  ([0006](./0006-toml-config)).
- Last-used times are recorded in memory and written to the store at most once a
  minute, and at shutdown, so `wayfindr token list` can lag behind by that much.
- Tokens don't expire. Revoke one when it is no longer needed.
- Per-token tool rules, and binding a token to a profile, are possible
  follow-ups. They would reuse the rule types of tool lists and profiles.
- Team mode, with several users and OIDC, is still deferred. The digest is
  unique across users, so one value still maps to one user then.
