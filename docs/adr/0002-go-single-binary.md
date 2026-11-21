# 0002 — Implement in Go, shipped as a single static binary

Status: Accepted (2026-11-03). Amended:

- 2026-11-03: search doesn't use SQLite's FTS5
  ([0005](./0005-store-owns-hybrid-search)).
- 2026-11-07 (wayfindr-sknr): there is no Homebrew tap. A release is checksummed
  archives, which the one-line installers download and verify.

## Context

The gateway has to speak MCP 2025-06-18, 2025-11-25, and 2026-07-28 to clients
and to upstreams. 2026-07-28 removes the initialize handshake and sessions. SDK
support for the newest spec revision therefore matters a lot. A single binary
was a stated preference.

## Decision

Use Go with the official Tier-1 MCP Go SDK, which supports 2026-07-28. The Rust
SDK supports it only in beta. Build without cgo:

- SQLite via `modernc.org/sqlite` (pure Go, includes FTS5; search doesn't use
  it, amended 2026-11-03).
- Postgres via `pgx` with `pgvector`.
- Logging via `zerolog`.

Distribute with GoReleaser (darwin, linux, and windows; amd64 and arm64) and a
Homebrew tap (amended 2026-11-07, wayfindr-sknr: there is no tap. Each release
has a `tar.gz` archive per target, `zip` on Windows, and `checksums.txt`.
`install.sh` and `install.ps1`, served from the docs site, download the archive
for the machine and check it against `checksums.txt`).

## Consequences

- One downloadable binary, with trivial cross-compilation.
- No SQLite loadable extensions such as sqlite-vec. Vector search on SQLite is
  done in Go. See [0005](./0005-store-owns-hybrid-search).
- Wayfindr depends on the Go SDK's pace for future spec revisions.
