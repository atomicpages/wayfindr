# 0015 — Warn before a connector's credential fails, without refreshing it

Status: Accepted (2026-11-08). Amends the `search_tools` response of
[0001](./0001-two-meta-tools), and the no-scheduler consequence of
[0004](./0004-lazy-single-flight-refresh). Written down on 2026-11-12
(wayfindr-q2rb); the decision shipped in `1bbcb4d` and `85083eb`
(wayfindr-1dq8).

## Context

Token refresh is lazy ([0004](./0004-lazy-single-flight-refresh)): nothing
touches a credential until a call needs it. So a credential that is about to
fail fails in the middle of an agent's work, often an unattended one, as
`needs_auth`.

Some of these failures can be seen coming from what is already stored. An access
token with no refresh token stops working when it expires. A refresh token, or a
client registered with DCR, can have an end date the server reported. The user
could authorize again in time, if someone told them.

Refreshing early to find out would spend rotating refresh tokens on connectors
nobody is using, which [0004](./0004-lazy-single-flight-refresh) rejected.

## Decision

- The daemon reads each stored credential and predicts its failures. It never
  refreshes, and it makes no request to the provider.
  - `no_refresh`: the access token expires and there is no refresh token. It is
    a note until a day before the expiry, then a warning.
  - `refresh_expiring`: the refresh token ends within 7 days, or has ended. The
    end comes from `refresh_token_expires_in`, when the server sends it.
  - `registration_expiring`: the DCR client a refresh would use ends within 7
    days, or has ended.
  - `stuck`: a connector has been out of `ready` for more than a day. This one
    comes from the connector's state, not its credential.
- Each warning carries advice: a summary, an action, and the auth link when
  authorizing again fixes it.
- The daemon builds the list in one place and shows it in three:
  - `wayfindr status` lists warnings and notes for each connector.
  - Every 15 minutes the daemon checks every configured connector and logs each
    new warning once, when it starts to apply, and again only after it cleared
    and came back. Notes are not logged.
  - `search_tools` always has a `warnings` list: the credential warnings due
    within a day for the ready connectors in the search, so the agent can tell
    the user before a call fails. A connector that is not ready is under
    `unavailable` already, with its own advice.

## Consequences

- Users hear about a coming failure in time to authorize again when it suits
  them, and an agent can say so before a call fails.
- Only failures the stored credential predicts are warned of. A provider that
  revokes a token early, or ends a refresh token without saying when, still
  shows up as `needs_auth` on the next call.
- The `search_tools` response has one more field. It is always present, and
  empty when nothing is due.
- The check spans four parts of the daemon: auth reads the credentials and
  writes the advice, upstream judges `stuck`, the daemon gathers and logs them,
  and the gateway and admin API show them.
- Refresh stays lazy. The check costs a store read per connector every 15
  minutes, and no tokens.
