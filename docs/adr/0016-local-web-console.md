# 0016 — A local web console over the admin API, signed in from the CLI

Status: Accepted (2026-11-12). Amended:

- 2026-11-12 (wayfindr-sgh7): the console adds connectors, through the daemon,
  and still never edits or removes them
  ([0017](./0017-console-adds-connectors)).
- 2026-11-17 (wayfindr-8ajj): connector logos load from theSVG through jsDelivr
  instead of being embedded; the binary embeds only the slug list, and offline
  the console shows letters ([0019](./0019-connector-logos-from-thesvg)).

## Context

Everything a person does with a running daemon goes through the CLI:
`wayfindr status`, `auth`, `calls`, `search`, `token`. That works for one action
at a time. It is a poor fit for glancing at a dozen connectors, seeing which one
needs a sign-in, or trying search queries until the ranking makes sense.

The admin API already answers all of those questions for the CLI, and the
`/auth/<connector>` pages already show that the daemon can serve browser pages
safely on loopback. What a browser lacks is a way to authenticate: the admin API
takes the default client token as a bearer, and a web page can't hold that token
without exposing it to anything that can read the page's storage or history.

## Decision

- The daemon serves a web console under `/ui/`: connectors and their tools, the
  call log, a search preview, and client tokens. It reads and acts only through
  the admin API's own client, called in process, so the browser and the CLI get
  the same answers. It does what the admin API already does (sign a connector in
  or out, create or revoke a client token) and never edits the config: the TOML
  file stays the one place connectors are configured
  ([0006](./0006-toml-config)). (Amended 2026-11-12, wayfindr-sgh7: the console
  also adds connectors, which the daemon writes to the file; it still never
  edits or removes one. See [0017](./0017-console-adds-connectors).)
- `wayfindr ui` signs the browser in. It asks the admin API, with the default
  client token, for a login code, then opens `/ui/login#<code>`. The code is
  random, works once, and expires after 60 seconds. It rides in the URL
  fragment, so it never appears in a request line or a log. The login page posts
  it and gets a session cookie: `HttpOnly`, `SameSite=Strict`, scoped to `/ui`.
  The bearer token never reaches the browser.
- Sessions live in the daemon's memory only. A session ends after 12 idle hours
  or when the daemon stops.
- Every console form carries the session's CSRF token, on top of httpd's checks:
  a loopback `Host` and `Origin` on every request, and an exact same-origin
  `Origin` on every `POST`. Pages are sent with a CSP that allows only the
  console's own files (no inline script or style) and refuses framing.
- The pages are rendered on the server with `html/template`, which escapes
  everything upstreams wrote (tool names and descriptions, advice). htmx
  (vendored, 0BSD) adds partial updates; every page also works without
  JavaScript. The assets (htmx, the Geist fonts, connector logos) are embedded
  in the binary, so the console works offline and the build stays Go-only.

## Consequences

- People can see every connector at once and act on the ones that need them,
  without learning commands first.
- The daemon serves more surface to the browser. The session cookie, the CSRF
  token, the same-origin check, and the CSP each block a different attack from
  another local web page; none of them replaces the others.
- The console can only show what the admin API exposes. For example, a connector
  waiting on a secret shows the daemon's advice, but the console can't offer a
  "set secret" field, because the status doesn't name the secret.
- The console's pages send `Referrer-Policy: same-origin`, not `no-referrer`:
  under `no-referrer` a browser sends `Origin: null` on a form post, which the
  same-origin check refuses.
- The binary grows by about 150 KiB of embedded assets.
