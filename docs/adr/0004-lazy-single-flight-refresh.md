# 0004 — Refresh OAuth tokens lazily, single-flight per credential

Status: Accepted (2026-11-03). Amended:

- 2026-11-08 (wayfindr-1dq8): a read-only check warns of credential failures it
  can predict, and never refreshes ([0015](./0015-credential-health-warnings)).
- 2026-11-17 (wayfindr-sf2z): an unattended grant's token is never refreshed or
  stored; Wayfindr runs the grant again, and a step-up re-runs it once with the
  larger scope set
  ([0018](./0018-grants-client-authentication-sender-constraints)).

## Context

Providers differ: some send no `expires_in`, some rotate refresh tokens on every
use (Google, Atlassian), and some revoke tokens early. Keeping every token "hot"
on a timer burns refreshes for connectors nobody is using.

## Decision

- Before an upstream call, if the access token is expired or within 60s of
  expiry, refresh it with the refresh token.
- On an upstream 401, refresh once and retry the call once.
- Refreshes are single-flight per credential. Concurrent callers wait for the
  in-flight refresh and use its result, so a rotated refresh token is never
  spent twice.
- If the refresh fails (`invalid_grant` or similar), the connector moves to
  `needs_auth`. `call_tool` returns an error containing a daemon URL
  (`/auth/<connector>`) that starts the auth flow.
- On 403 `insufficient_scope`, the connector moves to `needs_auth` with the
  larger scope set (step-up).
- An unattended grant has no refresh: its token lives in memory, and Wayfindr
  runs the grant again where an auth flow's token would be refreshed. Its
  step-up re-runs the grant once with the larger scope set (amended 2026-11-17,
  wayfindr-sf2z; see
  [0018](./0018-grants-client-authentication-sender-constraints)).

## Consequences

- No background refresh scheduler. A background check does read every stored
  credential every 15 minutes, and warns of a failure it can predict: a refresh
  token or client registration ending within 7 days, or a token that can't be
  refreshed expiring within a day. It never refreshes (amended 2026-11-08,
  wayfindr-1dq8; see [0015](./0015-credential-health-warnings)).
- The first call after a long idle period pays one token round trip.
- Correctness depends on the single daemon ([0003](./0003-single-daemon)).
  Multi-replica server mode will need a DB-level lock, such as a Postgres
  advisory lock.
