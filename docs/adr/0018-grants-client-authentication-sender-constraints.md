# 0018 — Grants, client authentication, and sender constraints

Status: Accepted (2026-11-17). Extends [0004](./0004-lazy-single-flight-refresh)
and [0007](./0007-oauth-discovery-and-registration). Amended:

- 2026-11-17 (wayfindr-9fep): the configured `token_endpoint_auth_method`
  reaches the client authentication selector through the OAuth client's
  registration, where a DCR client's registered method also sits.
- 2026-11-17 (wayfindr-6xdm): a JWT bearer grant's signing key signs its
  assertion; it is the client's credential too only when
  `token_endpoint_auth_method = "private_key_jwt"`.
- 2026-11-17 (wayfindr-5vps): the Transport doesn't choose a sender constraint;
  it sends each token as it was bound.
- 2026-11-17 (wayfindr-e86e), corrected: revoking an unattended connector also
  deletes a stored credential a hand edit left behind.

## Context

Wayfindr is an OAuth client to every upstream MCP server's authorization server.
It covers the consumer-SaaS case: the authorization code with PKCE, refresh, DCR
and CIMD, and the discovery chain of
[0007](./0007-oauth-discovery-and-registration). Three common setups don't fit
it:

- **Enterprise identity providers** (Okta, Entra, Keycloak, Ping, Auth0). The
  client authenticates with a private key JWT or a client certificate, and
  tokens are bound to the client with DPoP or the certificate.
- **Machines and workloads.** An agent host, CI, or Kubernetes, with no user to
  consent: the client credentials grant, and the JWT bearer grant with a
  workload token.
- **Headless machines.** A daemon on a remote box, where the browser can't come
  back to a loopback callback: the device code (RFC 8628).

Each adds a variation, and the variations combine: a client credentials grant
may authenticate with a private key JWT and bind its token with DPoP. Folding
them into the one authorization-code path would make every branch know about
every other.

## Decision

**Three independent axes.** How a connector's token is obtained is three
separate choices, each made once per connector by one selector:

1. The **grant**: an auth flow (authorization code or device code), with a user,
   or an **unattended grant** (client credentials or JWT bearer), without one. A
   refresh renews a grant's token; it is not a grant.
2. **Client authentication**: how Wayfindr proves it is the OAuth client at the
   token endpoint: none, a client secret (basic, post, or JWT), a private key
   JWT, or a client certificate. The method configured for the connector reaches
   its one selector the way a DCR client's registered method does, as part of
   the OAuth client's registration (amended 2026-11-17, wayfindr-9fep).
3. The **sender constraint**: what binds the token to Wayfindr: none (bearer), a
   DPoP key, or a client certificate.

Any grant combines with any client authentication and any sender constraint. In
code, each axis is one concept with one selector, the grants never call one
another, and the upstream Transport knows only where its tokens come from and
how they are sent (design §7.3, "Module layout").

The sender constraint is chosen per grant, for its token requests. The Transport
then sends each token as it was bound: with a proof of the DPoP key the token
carries, or as a bearer token when the authorization server didn't bind it
(amended 2026-11-17, wayfindr-5vps).

A JWT bearer grant's signing key belongs to its assertion. It authenticates the
client only when the connector says `private_key_jwt`; otherwise the client
authenticates with its secret, or as a public client (amended 2026-11-17,
wayfindr-6xdm).

**Unattended tokens live in memory only.** An unattended grant needs no user, so
Wayfindr gets its token whenever a call needs one and keeps it in memory. It is
never stored: such a connector has no stored credential, and a restart costs one
token request. A refused unattended grant is `auth_error`, never `needs_auth`,
since no auth flow can fix it.

**A sign-only JWS in the standard library.** Private key JWT, client secret JWT,
self-signed assertions, and DPoP proofs only sign JWTs; Wayfindr never verifies
one. Signing ES256/384, RS256, PS256, EdDSA, and HS256 is a small amount of
standard-library code, so Wayfindr carries its own sign-only JWS rather than a
JOSE dependency ([0002](./0002-go-single-binary)).

**Wayfindr's own token endpoint call.** Every grant and every refresh sends its
token request through one call Wayfindr owns, not through go-sdk's `extauth`.
That call sends the resource indicator (RFC 8707), applies the connector's
client authentication and sender constraint, reads a bounded response, and runs
it through the per-provider Inspectors, which give each failure its Case.
`extauth` would bypass the Inspectors, the resource indicator, and the Cases for
the grants it handles, so a client credentials rejection would read differently
from a refresh rejection.

**Room for Enterprise-Managed Authorization.** EMA (SEP-990) is an IdP login,
then an RFC 8693 token exchange, then the JWT bearer grant with the exchanged
token as its assertion. It is not built now. The seams it needs exist: a new
assertion source for the JWT bearer grant, and a new grant beside the others,
with no change to client authentication, sender constraints, or the Transport.

## Consequences

- A new grant, client authentication method, or sender constraint is one new
  piece, not a change to the paths beside it.
- An unattended connector has no stored credential: revoking it forgets its
  in-memory token, and switching a connector to an unattended grant leaves any
  stored token unused. Revoking such a connector also deletes a stored
  credential that a hand edit of `grant` left behind (corrected 2026-11-17,
  wayfindr-e86e).
- The token endpoint call is Wayfindr's to keep correct, including JWT signing;
  tests cover it against the fake authorization server.
- Implicit and password grants (removed in OAuth 2.1), the SAML 2.0 bearer
  grant, and UMA are not supported. Token exchange arrives with EMA.
