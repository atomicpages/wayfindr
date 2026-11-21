# 0020 — Opt-in usage telemetry: a daily report of counts and allowlisted names, priced by proof-of-work

Status: Accepted (2026-11-18).

## Context

Nothing tells the maintainers how Wayfindr is used: which releases and platforms
are live, whether people run SQLite or Postgres, stdio or HTTP connectors, OAuth
or API keys, and how much search and call traffic goes through it. Release
testing and the work order are guesses without it.

Wayfindr holds credentials for every upstream a user has, and its users read its
network behaviour closely. [0014](./0014-call-log) keeps arguments and results
out of the call log; [0019](./0019-connector-logos-from-thesvg) keeps connector
names away from the logo CDN. Telemetry must meet the same bar, and it must not
be on by surprise.

## Decision

**Opt-in, daemon-only.** `[telemetry] enabled` defaults to `false` and applies
on reload. `DO_NOT_TRACK` set to anything but empty or `0` overrides it, and
`wayfindr service install` carries that variable into the unit. Only release
builds send: a version like `v1.2.3` (or `v1.2.3-rc.1`) and a hex commit, so
checkouts and `git describe` versions never do. CLI commands never send. Nothing
is counted, and no install ID is written, while telemetry is off. `wayfindr init`
may ask, on a terminal and only when it writes a new config, with a default of
no, so the answer is still the user's: nothing turns it on but a yes.

**One report a day.** The first goes 5 to 60 minutes after startup, at random;
then every 24 hours. A failed report keeps its counts for the next; nothing is
retried sooner or written to disk.

**Counts and allowlisted names only.** The report (schema 1) carries a random
install ID, the window it covers, build and platform, the store driver, whether
embedding is configured, connector counts by transport and by auth kind, the
number of profiles, `search_tools` calls, `call_tool` calls by result, and the
same calls by client. A client, embedding provider, or embedding model is sent
by name only when it is on an allowlist built into the binary; anything else is
`other`. An embedding host on a loopback or private address is `local`. The full
field list is the [Telemetry](/reference/telemetry) page;
`internal/telemetry/testdata/report-v1.json` is its golden copy.

**A self-hosted collector, no secret.** The report is a JSON `POST` to
`/v1/report` on a Cloudflare Worker the maintainers run, kept in its own
repository. An open-source binary can't hold a secret, so the collector can't
tell a genuine build from a script. Instead each report carries a proof-of-work:
a nonce such that SHA-256 of `wayfindr-report-v1|<install_id>|<date>|<nonce>`
has 24 leading zero bits, with `date` the sender's UTC date. The collector
checks it with one hash and refuses a `date` that isn't today or yesterday,
keeps one report per install ID and day, validates every field strictly, rejects
unknown fields and bodies over 4 KiB, rate-limits per address at the edge, and
stores no IP address or anything derived from one. The two sides share
`internal/telemetry/testdata/pow-v1.json`. A change to a field, its meaning, or
the difficulty bumps `schema`.

## Considered options

- **Opt-out with a first-run notice.** More data, but a credential-holding
  gateway that reports by default loses the trust it depends on, and packagers
  patch such defaults out.
- **A shared key in the binary.** Anyone can read it out; it stops nothing and
  needs rotating each time it leaks.
- **Per-install registration tokens.** They stop forged reports for an install
  ID someone else has, which a random UUID already makes impractical, but not
  new fake installs.
- **A hosted analytics service.** Less to run, but a third party sees every
  report and its IP address, and its SDK sends more than the report.
- **Sending from the CLI too.** Commands would slow down and touch the network;
  the daemon sees everything the report needs.

## Consequences

- The daemon makes one more kind of outbound request, only when the user opts
  in. `SECURITY.md` and the security design list it.
- Proof-of-work prices fake installs, it doesn't forbid them: at 24 bits each
  fake install costs about a CPU-second a day, and far less on GPUs. Analysis
  counts distinct installs and discounts implausible ones; the numbers are a
  guide, not a census.
- A new client, provider, or model shows as `other` until a release adds it to
  the allowlist. The collector checks names by pattern, so adding one needs no
  collector change.
- The collector's URL is a constant in the binary. `WAYFINDR_TELEMETRY_ENDPOINT`
  replaces it, for testing a collector or watching what a report holds.
