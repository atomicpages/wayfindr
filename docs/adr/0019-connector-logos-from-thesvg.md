# 0019 — Connector logos come from theSVG through jsDelivr, matched against an embedded slug list

Status: Accepted (2026-11-17). Amends the embedded connector logos of
[0016](./0016-local-web-console).

## Context

The console marks each connector with a logo. It embedded six, matched by
substring of the connector's name, so most connectors (Gmail, PostHog, Sentry)
got a letter tile. [theSVG](https://thesvg.org) publishes about 7,900 brand
logos, MIT for its tooling, each mark its owner's. Embedding them all would add
megabytes to a binary whose console assets are about 150 KiB.

## Decision

The browser loads a connector's logo from jsDelivr, pinned to one theSVG commit:
`https://cdn.jsdelivr.net/gh/glincker/thesvg@<sha>/public/icons/<slug>/default.svg`,
and `dark.svg` in dark mode for the brands that have one. The console's
`img-src` allows `https://cdn.jsdelivr.net` and nothing else from outside.

The daemon embeds only theSVG's slug list at that commit (`brands.gz`, about 37
KiB, refreshed by hand with `make brands`), and matches a connector's name
against it: the whole name, then with trailing `-parts` dropped, plus a few
aliases (`postgres` is `postgresql`). The page names a logo only when the name
matches, so the browser asks the CDN for real brands only, never for a
connector's own name.

The connector's first letter always renders under the logo. A logo that can't
load (offline, or the CDN is down) hides, and the letter shows.

## Considered options

- **Vendor a curated set of logos.** Offline and private, but every brand
  outside the list keeps a letter, and the list needs upkeep.
- **Guess the slug from the name, no index.** Nothing to embed, but every
  unmatched connector costs a 404 each time the connectors table refreshes
  (every 10 seconds), and the CDN sees every connector name.
- **Load from thesvg.org.** Unpinned, and a missing icon is a 184 KB HTML page
  cached for 10 minutes; jsDelivr pinned to a commit answers a hit with
  `immutable` and a miss with 95 bytes.

## Consequences

- The console makes requests outside the machine: to jsDelivr, for the slugs of
  brands its connectors match. Pages send `Referrer-Policy: same-origin`, so the
  console's URL is not sent with them.
- Offline, connectors show letters; the rest of the console stays embedded and
  works.
- Matching is by whole name or prefix, not substring: `github-work` matches
  GitHub, `mygithub` does not. A one-word name that is a brand's slug gets that
  brand's logo.
- Each logo carries the licence theSVG lists for it. The binary ships no logo
  files, only slugs.
- A one-colour black logo without a `dark` variant is hard to see in dark mode;
  the console no longer inverts logos.
