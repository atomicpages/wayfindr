# Wayfindr — Technical Design

This is the implementation companion to the [ADRs](../adr/) and the glossary in
[GLOSSARY.md](../../GLOSSARY.md). The ADRs record _why_ each decision was made;
the design records _how_ the code does it. User-facing contracts (config keys,
commands, HTTP endpoints and bodies) live in the public reference, which the
design links rather than copies. When the design and the code disagree, the code
wins and the design gets a fix.

The design is one file per section in [`design/`](design/). Go code cites a
section as "design §N", and the file for §N is `design/N-<topic>.md`, so
`ls docs/internal/design/7.5-*` finds §7.5. Section numbers and file names stay
put; add a subsection inside a file rather than renumbering.

| §    | File                                       | What it covers                                                                        |
| ---- | ------------------------------------------ | ------------------------------------------------------------------------------------- |
| 1    | [Stack](design/1-stack.md)                 | Go version and every dependency.                                                      |
| 2    | [Package map](design/2-package-map.md)     | Each `internal/` package and the one-way import graph.                                |
| 3    | [Paths and identity](design/3-paths.md)    | Config, data, token, key, lock, and log file locations; `.wayfindr.toml` lookup.      |
| 4    | [Configuration](design/4-configuration.md) | Load, templates and when they resolve, validation, and the config file editor.        |
| 5    | [Data model](design/5-data-model.md)       | Tables and migrations for both drivers.                                               |
| 6    | [Store interface](design/6-store.md)       | What the `Store` interface promises and why.                                          |
| 7.1  | [`secrets`](design/7.1-secrets.md)         | Master key sourcing and the sealer.                                                   |
| 7.2  | [`upstream`](design/7.2-upstream.md)       | Transports, the connector manager, states, errors and advice, elicitation relay.      |
| 7.3  | [`auth`](design/7.3-auth.md)               | OAuth discovery, registration, flows, the broker and refresh, health, client binding. |
| 7.4  | [`catalog`](design/7.4-catalog.md)         | Sync, embed text, the tools-list filter, hidden tools.                                |
| 7.5  | [Search algorithm](design/7.5-search.md)   | Tokenizing, BM25F, typo expansion, embeddings, and fusion.                            |
| 7.6  | [`gateway`](design/7.6-gateway.md)         | `search_tools`, `call_tool`, request narrowing, `input_required`, call recording.     |
| 7.7  | [`httpd`](design/7.7-httpd.md)             | Listener, guards, client tokens, the auth pages, and the admin client.                |
| 7.8  | [`daemon`](design/7.8-daemon.md)           | Startup, reload, polling, the call log, health checks, and shutdown, step by step.    |
| 7.9  | [CLI](design/7.9-cli.md)                   | How commands reach the daemon, the shim, `daemonctl`, and error handling.             |
| 7.10 | [`webui`](design/7.10-webui.md)            | The web console: routes, login codes and sessions, rendering, htmx, CSP.              |
| 8    | [Key sequences](design/8-sequences.md)     | End-to-end flows across packages.                                                     |
| 9    | [Security checklist](design/9-security.md) | Invariants every change must keep, including redaction.                               |
| 10   | [Testing](design/10-testing.md)            | Fakes, suites, golden files, and what CI runs.                                        |
| 11   | Milestones                                 | Below.                                                                                |

## 11. Milestones

The build was planned as milestones M0 (foundations) to M7 (Postgres), each
ending in a working, tested state and a `gate/m<N>` tag. They are kata epics,
and their tasks carry the labels `m0` to `m7`:
`kata list --status all --label m4 --agent` lists one milestone's work, and
`kata show <ref> --agent` gives an epic's exit criteria. Work after v1 is
grouped under `e36c` (feature plans) or filed as ordinary issues.
