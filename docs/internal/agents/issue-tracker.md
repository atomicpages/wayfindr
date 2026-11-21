# Issue tracker: kata

Issues and specs for this repo live in kata. Use the `kata` CLI for every
operation, with `--agent` for a one-line status and parseable details. Do not
file GitHub issues for this repo's work: `atomicpages/wayfindr` on GitHub hosts
code, not tickets. The full workflow (claim, `work.*` metadata, wait, close) is
the Kata block at the end of `AGENTS.md`; `kata quickstart --format contract`
prints the agent contract.

## Refs

- An issue's ref is its short id, such as `e36c`. Commits cite it as
  `(wayfindr-e36c)`; pass `e36c` to kata, never `wayfindr-e36c`.
- Issues from before 2026-11-06 were imported from beads. Code comments,
  commits, and docs still cite their old ids (`wayfindr-zgp.13`). Find one with
  `kata search "beads-id:wayfindr-zgp.13" --agent`; each also carries the label
  `beads-id:wayfindr-<id>`.

## Conventions

- **Search first**: `kata search "<one or two words>" --agent`. Reuse an open
  issue for the same work.
- **Create**:
  `kata create "<title>" --body "<context>" --idempotency-key <stable-key> --agent`.
  Link with `--parent <ref>`, `--related <ref>`, or, for a real prerequisite,
  `--blocked-by <ref>`.
- **Read**: `kata show <ref> --agent`.
- **List**: `kata list --label <label> --agent` (`--status all` for closed ones
  too); `kata ready --agent` lists unblocked work.
- **Comment**: `kata comment <ref> -m "..." --agent`.
- **Labels**: `kata label add <ref> <label>` /
  `kata label remove <ref> <label>`. See [triage-labels.md](triage-labels.md).
- **Close**:
  `kata close <ref> --done -m "<scope and verification>" --commit <sha> --agent`
  (or `--test`, `--pr`, `--reviewed`).
- **Never** `kata delete` or `kata purge` without the user's say-so.

## When a skill says "publish to the issue tracker"

Create an issue with `kata create`. A spec becomes a parent issue; its tickets
are children through `--parent`.

## When a skill says "fetch the relevant ticket"

`kata show <ref> --agent`.

## Groupings in this repo

- `e36c` is the parent of the feature-plan issues (`docs/internal/plans/`, local
  only).
- Milestone epics and their tasks carry the labels `m0` to `m7`
  (`kata list --status all --label m4 --agent`).
