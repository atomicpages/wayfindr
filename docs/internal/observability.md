# Operator observability (not started)

Tracked as kata `hx29` (someday). Separate from the usage report (ADR 0020, kata
`y2ja`): different switch, different audience, and nothing leaves the host
unless the operator points it somewhere.

## The idea

Metrics, and maybe traces, that the person running the daemon exports to their
own collector (Prometheus, Grafana, an OTel collector, Datadog), so they can
alert on and graph their own gateway. The usage report answers "how is Wayfindr
used out there?" for the maintainers; this answers "is my gateway healthy?" for
the operator.

Today the operator has `wayfindr status`, `wayfindr calls`, the console, and the
JSON log (`docs/guide/monitoring.md`). None of it is scrapeable or has history
beyond the call log's retention.

## What it could measure

- `call_tool` latency and result per connector and tool (histogram + counter):
  the call log has all of it per row already.
- `search_tools` latency, result count, and whether the query was embedded or
  lexical only (embed timeout hits).
- Connector state transitions (`ready`, `needs_auth`, `auth_error`, `starting`,
  …) and time in each; reconnects.
- Catalog syncs: duration, tools per connector, failures, embedding-pass
  backlog.
- Token refreshes and their failures; credential health warnings.
- Call-log queue depth and drops (`callLog.dropped`); store write latency.
- Process: goroutines, memory, open fds (free with the Prometheus Go collector).

## Hooks that already exist

- `upstream.Options.OnStateChanged` (`internal/upstream/manager.go:85`) is
  defined and queued on every transition but **not wired** in `daemon.New`. It
  is the natural feed for state metrics.
- `gateway.Calls`: the daemon already wraps it (`countedCalls`) for the usage
  report; a metrics sink is one more consumer of the same `store.Call`.
- `gateway.Options.OnSearch` (added for the usage report) fires per search; it
  would need the latency and outcome to be useful here.
- `catalog.Options.OnSynced`, `auth.BrokerOptions.OnCredentialChanged`, and the
  `health` check's warnings.

Per design §2, new feeds should be callbacks `daemon` wires, not imports.

## Open choices

- **Prometheus `/metrics` vs OTel SDK.** A `/metrics` route on the loopback
  listener (behind the client token? scrapers rarely carry a bearer, so maybe
  its own opt-in listener) is small and needs only `prometheus/client_golang`.
  The OTel SDK covers traces too, and OTLP push, but is a much bigger dependency
  tree for a single static binary (design §1 and an ADR would have to justify
  it).
- **Labels.** Connector and tool names as labels are the useful part and the
  risky part: they are user data and can be high-cardinality. Since the data
  stays with the operator that is fine, but cap cardinality (tools per
  connector) and document it.
- **Config.** Likely a `[metrics]` table, off by default, live on reload.
