# Architecture Decisions

These records explain why Wayfindr is built the way it is. Each one gives the
context, the decision, and what follows from it. A record is not rewritten when
the code changes: a later decision amends or replaces it.

| ADR                                                            | Decision                                                                             |
| -------------------------------------------------------------- | ------------------------------------------------------------------------------------ |
| [0001](./0001-two-meta-tools)                                  | Expose exactly two meta-tools: `search_tools` and `call_tool`.                       |
| [0002](./0002-go-single-binary)                                | Implement in Go, shipped as a single static binary.                                  |
| [0003](./0003-single-daemon)                                   | One daemon owns all upstreams; clients connect over local HTTP.                      |
| [0004](./0004-lazy-single-flight-refresh)                      | Refresh OAuth tokens lazily, single-flight per credential.                           |
| [0005](./0005-store-owns-hybrid-search)                        | Each store implements hybrid search; SQLite computes cosine in Go.                   |
| [0006](./0006-toml-config)                                     | Declarative TOML config; secrets live encrypted in the store.                        |
| [0007](./0007-oauth-discovery-and-registration)                | OAuth discovery chain, client registration order, and redirect handling.             |
| [0008](./0008-relay-elicitation)                               | Relay elicitation to clients.                                                        |
| [0009](./0009-tool-policy-in-the-gateway)                      | Choose which upstream tools agents see with a per-connector tools list.              |
| [0010](./0010-project-scopes)                                  | Narrow a project's connectors and tools with a project scope file.                   |
| [0011](./0011-named-profiles)                                  | Pick a scope by name with profiles in the config.                                    |
| [0012](./0012-scoped-client-tokens)                            | Limit a client to named connectors with scoped client tokens.                        |
| [0013](./0013-stateless-mcp-server)                            | Serve `/mcp` statelessly at every protocol version.                                  |
| [0014](./0014-call-log)                                        | Record every `call_tool` call in a call log, without arguments or results.           |
| [0015](./0015-credential-health-warnings)                      | Warn before a connector's credential fails, without refreshing it.                   |
| [0016](./0016-local-web-console)                               | Serve a local web console over the admin API, signed in from the CLI.                |
| [0017](./0017-console-adds-connectors)                         | Let the daemon add connectors for the console; review commands before they run.      |
| [0018](./0018-grants-client-authentication-sender-constraints) | Model upstream OAuth as three axes: grant, client authentication, sender constraint. |
| [0019](./0019-connector-logos-from-thesvg)                     | Load connector logos from theSVG on jsDelivr; embed only the slug list.              |
| [0020](./0020-opt-in-usage-telemetry)                          | Send an opt-in daily usage report of counts and allowlisted names.                   |
| [0021](./0021-remote-client-tokens)                            | Let cloud agents reach `/mcp` through a tunnel with remote client tokens.            |
| [0022](./0022-authorization-server-for-remote-clients)         | Let cloud agents sign in to `/mcp` through an authorization server in the daemon.    |
| [0023](./0023-self-update)                                     | Update the binary from GitHub Releases with `wayfindr update`.                       |

0001 to 0007 were accepted on 2026-11-03, 0013 on 2026-11-04, 0014 and 0015 on
2026-11-08, 0008 and 0009 on 2026-11-09, 0010 on 2026-11-10, 0011 and 0012 on
2026-11-11, 0016 and 0017 on 2026-11-12, 0018 and 0019 on 2026-11-17, 0020 on
2026-11-18, 0021 on 2026-11-21, and 0022 and 0023 on 2026-11-22. 0013 to 0015
were written down on 2026-11-12, after their decisions shipped.
