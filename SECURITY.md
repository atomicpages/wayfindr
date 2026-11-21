# Security policy

Wayfindr holds the credentials for every MCP server it connects to, so we
treat security reports as our first priority.

## Reporting a vulnerability

Report it privately through GitHub:
[Report a vulnerability](https://github.com/atomicpages/wayfindr/security/advisories/new).
Don't open a public issue, discussion, or pull request for it.

Include what you can of:

- The Wayfindr version (`wayfindr version`) and your OS.
- What an attacker can do, and what they need first (for example, a web page
  open in your browser, another user on the machine, or a malicious upstream
  server).
- Steps or a proof of concept to reproduce it.

We aim to acknowledge a report within 3 working days and to agree on a fix
and a disclosure date with you. We credit reporters in the advisory unless you
ask us not to.

## In scope

- Anything that lets a client, a web page, a remote host, or an upstream
  server read secrets, OAuth tokens, or the client token.
- Ways around the client token, a scoped token's connector limit, the
  loopback-only listener, or the Host and Origin checks.
- Flaws in the OAuth flow, in encryption at rest, or in how the master key
  is stored.
- Secrets that reach logs, unit files, error messages, or tool results.
- A usage report that sends more than the
  [Telemetry](https://atomicpages.github.io/wayfindr/reference/telemetry)
  page lists, or is sent while telemetry is off.

The [security guide](https://atomicpages.github.io/wayfindr/guide/security)
describes what Wayfindr defends against and what it does not. Other processes
running as your user are out of scope: they can read the client token by
design.

## Supported versions

Only the latest release gets security fixes. Upgrade with the installer you
used to install it.
