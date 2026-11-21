# Telemetry

Wayfindr can send its maintainers a small usage report once a day: which version
you run, on what platform, how many connectors of each kind you have, and how
many searches and calls went through it. It helps decide what to work on and
which platforms to keep testing.

**It is off unless you turn it on.** A fresh install sends nothing, and so does
every install until its config says otherwise. [`wayfindr init`](./cli#wayfindr-init)
asks when it writes a new config, on a terminal; the answer defaults to no. It
doesn't ask when its input is piped, when a config already exists, or when
`DO_NOT_TRACK` is set.

```toml
[telemetry]
enabled = true
```

The daemon applies the change as soon as it reloads the config; no restart is
needed (`DO_NOT_TRACK` is the exception, below). Turning it on starts counting from then, and the counts go out with the
next scheduled report. Set `enabled = false`, or delete the table, to stop:
nothing more is sent, not even a report that was being prepared.

## What turns it off

The report is sent only when all of these hold:

| Condition                                                    | Why                                                                                                                                     |
| ------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------- |
| [`[telemetry] enabled`](./configuration#telemetry) is `true` | Opt-in.                                                                                                                                 |
| `DO_NOT_TRACK` is unset, empty, or `0`                       | Any other value keeps it off, whatever the config says. The daemon reads it once, at start: restart it to apply a change.               |
| The binary is a release build                                | A version like `v1.2.3` or `v1.2.3-rc.1`, and a commit. Builds from a checkout (`dev`, `v1.2.3-4-gabc1234`, `v1.2.3-dirty`) never send. |
| The binary names a collector                                 | Release builds do. If one doesn't, the daemon logs the reason below.                                                                    |

`wayfindr service install` copies `DO_NOT_TRACK` into the login service, so a
value you set in your shell keeps applying there. Run it again after you change
the variable.

### Confirm the state in the log

The daemon logs one `info` line whenever the config turns telemetry on or off
(`wayfindr logs` prints them):

| Message                                                               | Meaning                                                                                                                                   |
| --------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| `usage telemetry enabled; the daemon sends a daily usage report`      | Telemetry is on and will send.                                                                                                            |
| `usage telemetry enabled in the config but stays off` (with `reason`) | The config says on, but a condition above fails: `DO_NOT_TRACK is set`, `not a release build`, or `this build has no collector endpoint`. |
| `usage telemetry disabled`                                            | You turned it off with a config reload. Nothing counted is kept.                                                                          |

A daemon that starts with telemetry off logs nothing about it.

## When it is sent

Only the daemon sends the report. CLI commands never do.

- The first report goes 5 to 60 minutes after the daemon starts, at a random
  time, so a daemon you start for a moment sends nothing.
- After that, one report every 24 hours while the daemon runs.
- If a report fails (offline, the collector is down), the counts wait for the
  next one. Nothing is retried sooner, and nothing is written to disk.

## What it holds

Every field, with an example. There are no others: the collector rejects a
report with a field it doesn't know.

```json
{
  "schema": 1,
  "install_id": "6f1c2b7e-3d4a-4c1e-9b2f-8a7d5e0c1f23",
  "window_hours": 24,
  "version": "v0.4.0",
  "commit": "2e3a922",
  "go": "go1.26.1",
  "os": "darwin",
  "arch": "arm64",
  "store": "sqlite",
  "embedding": true,
  "embedding_provider": "openai",
  "embedding_model": "text-embedding-3-small",
  "connectors": {
    "transport": { "stdio": 3, "streamable": 4, "sse": 1 },
    "auth": { "api": 2, "none": 2, "oauth": 4 }
  },
  "profiles": 2,
  "searches": 41,
  "calls": {
    "ok": 120,
    "tool_error": 3,
    "rejected": 1,
    "failed": 2,
    "input_required": 0
  },
  "clients": { "claude_code": 90, "cursor": 30, "other": 6 },
  "pow": { "date": "2026-11-18", "nonce": "00000000002b841d" }
}
```

| Field                  | What it is                                                                                                                                                                                                                                                                                                                           |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `schema`               | The report's format version.                                                                                                                                                                                                                                                                                                         |
| `install_id`           | A random UUID in [`<data>/telemetry-id`](./paths-and-environment#telemetry-id), made the first time a report is sent. It tells reports from one install apart; it says nothing about you or your machine. Delete the file for a new one.                                                                                             |
| `window_hours`         | How many hours the counts below cover: since the last report the collector accepted, or since you turned telemetry on.                                                                                                                                                                                                               |
| `version`, `commit`    | The Wayfindr release.                                                                                                                                                                                                                                                                                                                |
| `go`, `os`, `arch`     | The Go release it was built with, and the platform, as Go names them (`darwin`, `linux`, `windows`; `arm64`, `amd64`).                                                                                                                                                                                                               |
| `store`                | `sqlite` or `postgres`.                                                                                                                                                                                                                                                                                                              |
| `embedding`            | Whether [`[embedding]`](./configuration#embedding) is set.                                                                                                                                                                                                                                                                           |
| `embedding_provider`   | Left out without `[embedding]`. `openai`, `voyage`, `mistral`, `openrouter`, or `together` for those providers' public endpoints; `local` for `localhost`, a `*.localhost` or `*.local` host, or a loopback or private IP address; `other` for any other host. The host itself is never sent.                                        |
| `embedding_model`      | Left out without `[embedding]`. The model's name if it is a well-known public model (for example `text-embedding-3-small`, `nomic-embed-text`, `qwen3-embedding`), else `other`. A tag such as `:latest` and a prefix such as `BAAI/` are dropped first.                                                                             |
| `connectors.transport` | How many connectors use each transport: `stdio`, `streamable` (Streamable HTTP), `sse` (legacy HTTP+SSE).                                                                                                                                                                                                                            |
| `connectors.auth`      | How many authenticate each way: `oauth`; `api`, a URL connector with a `headers` table; `none`, everything else, stdio connectors included. Both counts add up to the number of connectors.                                                                                                                                          |
| `profiles`             | How many [profiles](../guide/access/profiles) the config defines.                                                                                                                                                                                                                                                                    |
| `searches`             | How many `search_tools` calls the window saw.                                                                                                                                                                                                                                                                                        |
| `calls`                | How many `call_tool` calls the window saw, by how each ended (the results [`wayfindr calls`](./cli#wayfindr-calls) shows).                                                                                                                                                                                                           |
| `clients`              | The same calls, by client: `claude_code`, `cursor`, `codex`, `opencode`, `omp`, or `hermes`, else `other`. The match is on the name the client announces: case-insensitive, with `_` and spaces read as `-`, and equal to the key's name or starting with it and a `-` (`cursor-vscode` is `cursor`, `codex-mcp-client` is `codex`). |
| `pow`                  | A proof-of-work; see [Abuse protection](#abuse-protection).                                                                                                                                                                                                                                                                          |

### What it never holds

- Connector names, URLs, commands, arguments, or headers.
- Tool names, call arguments, results, or error text.
- Profile, scope, secret, or client token names.
- Hostnames, usernames, file paths, or IP addresses.
- A client, embedding host, or model name that isn't on the lists above.

The counts come from memory, not from the [call log](./configuration#calls), so
they are the same with the call log off. Nothing is counted while telemetry is
off: turning it on starts from zero.

## Where it goes

The daemon sends the report over HTTPS to a collector the Wayfindr maintainers
run, at `wayfindr-collector.wingmen-unit-7h.workers.dev`, and not to a
third-party analytics service. The request is `POST /v1/report` with a JSON
body and `User-Agent: wayfindr/<version>`. It times out after 10 seconds, does
not follow redirects, and goes through `HTTPS_PROXY` if you set one. The
collector:

- stores the report with the day it was sent, and nothing finer;
- keeps one report per install per day. It refuses a second one that day, and
  the daemon keeps those counts and sends them with the next day's report;
- never stores your IP address or anything worked out from it, such as a
  country. It limits how many reports one address can send a minute: Cloudflare
  counts them under the address for that minute, and the collector never writes
  the address anywhere. An address over the limit gets `429`, and the daemon
  keeps its counts for the next report.

## Abuse protection

Anyone can send the collector a report, so each one has to cost something to
make. Before sending, the daemon finds a `nonce` such that the SHA-256 of
`wayfindr-report-v1|<install_id>|<date>|<nonce>` starts with 24 zero bits. That
takes a fraction of a second to a few seconds of one CPU core, once a day, in
the background. The collector checks it with one hash, and turns away a report
whose `date` isn't today or yesterday.

## See for yourself

To see exactly what your daemon would send, point it at a listener of your own
with `WAYFINDR_TELEMETRY_ENDPOINT` (a base URL; the daemon adds `/v1/report`),
with telemetry on, and wait for the first report, 5 to 60 minutes after the
daemon starts:

```sh
nc -l 4001 &
WAYFINDR_TELEMETRY_ENDPOINT=http://127.0.0.1:4001 wayfindr start --foreground
```

`nc` prints the request. It doesn't answer, so the daemon counts that report as
failed and keeps its counts for the next one.
