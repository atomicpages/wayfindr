# Monitoring

Six commands show what Wayfindr and your agents are doing. Each asks the running
daemon, except `logs`, which reads its log file.

| You want to know                                  | Run                                    |
| ------------------------------------------------- | -------------------------------------- |
| Whether every connector works, and what to fix    | `wayfindr status`                      |
| Which tools agents called, and how each call went | `wayfindr calls`                       |
| What the daemon did, in detail                    | `wayfindr logs`                        |
| Why search ranks a tool where it does             | `wayfindr search <query> --explain`    |
| Which of a connector's tools agents can use       | `wayfindr connector tools <connector>` |
| What agents get in a project or profile           | `wayfindr scope [--profile <name>]`    |

## Is every connector working?

```sh
wayfindr status
```

```text
CONNECTOR  STATE       TOOLS          LAST SYNC             TOKEN EXPIRES
github     ready       38 (4 hidden)  2026-11-08T09:00:12Z  -
linear     ready       23             2026-11-08T09:00:14Z  2026-11-08T17:00:00Z
slack      needs_auth  0              never                 -

slack: The connector has not been authorized yet.
  Open the link to authorize it.
  http://127.0.0.1:7777/auth/slack

Warnings:
  linear: The access token expires at 2026-11-08T17:00:00Z and cannot be refreshed.
    Authorize again before then to avoid an interruption.
    http://127.0.0.1:7777/auth/linear
```

Every connector should be `ready`. Under the table, each one that isn't gets
what is wrong and how to fix it. **Warnings** lists what will break unless you
act, such as a token that can't be refreshed or a client certificate about to
expire (`certificate_expiring`, 7 days ahead), before it does. Your agent gets
the same advice in every `search_tools` result.

To check from a script or a scheduled job, add `--check`. It prints the same,
and exits `2` when a connector isn't `ready`, has a warning, or the daemon
reports a config error, `0` when all is well, and `1` when it can't ask the
daemon:

```sh
wayfindr status --check > /dev/null || echo "wayfindr needs attention"
```

- [Troubleshooting](./troubleshooting#connector-states) explains each state and
  [each warning](./troubleshooting#warnings).
- [`wayfindr status`](/reference/cli#wayfindr-status) has every column, section,
  and the `--json` fields.

## What did agents call?

The daemon records every `call_tool` call in its call log: when it arrived,
which client and client token made it, which tool, how long the upstream took,
how it ended, and how big the result was. A call that didn't end `ok` also keeps
the error the agent got back, shown under its row. Arguments and results are
never stored, only the result's size.

```sh
wayfindr calls                            # the last 50 calls in 24 hours
wayfindr calls --since 7d --errors        # what failed this week
wayfindr calls --since 48h --limit 1000   # up to 1000 calls in two days
wayfindr calls --connector github --json  # one connector, as JSON
```

```text
TIME                  CLIENT       TOKEN           TOOL                  DURATION  RESULT      SIZE
2026-11-08T09:14:02Z  claude-code  default         linear.create_issue   412ms     ok          312 B
2026-11-08T09:13:40Z  claude-code  default         linear.search_issues  1.2s      tool_error  48 KiB
                                                                                   Unknown filter: state=opne
2026-11-08T09:13:32Z  claude-code  default         linear.list_issues    2.3s      ok          1.2 MiB
```

A `rejected` call never reached the upstream: the agent guessed a tool name that
doesn't exist, sent arguments that don't match the tool's schema, or called a
tool that a [tools list, profile, or project scope](./access/#what-agents-see)
hides. A `failed` call did reach it, or tried to, and the reason says why. The
client shows as `-` when it didn't send its name. The token is `default` or the
name of a [scoped client token](./access/scoped-tokens); the client can't choose
it.

SIZE is the length of the result's JSON encoding, so it shows which tools return
the most. The client receives a few bytes more, since the protocol adds its own
envelope around the result. It is `-` for a call that returned no result, such
as one that was `rejected` or `failed`, for an `input_required` call, and for
calls logged before sizes were recorded. Add
`--json` to get the exact byte count as `result_bytes`. The
[console's Calls page](./console) shows the same column.

`calls` lists at most 50 calls, newest first, and doesn't say when there are
more. Raise `--limit` up to 1000. `--since` takes days, hours, minutes, or
seconds, such as `7d`, `1d12h`, `36h`, or `90m`; `--since 0` searches the whole
log.

When nothing matches, `calls` prints `No calls match.` Calls are kept for 30
days, then deleted. Change that with
[`[calls] retention`](/reference/configuration#calls), or set it to `"0s"` to
turn the log off; `calls` then ends with
`The call log is off: [calls] retention is 0s, so no calls are recorded.` See
[`wayfindr calls`](/reference/cli#wayfindr-calls) for every result and reason.

## What is the daemon doing?

```sh
wayfindr logs -n 20               # the last 20 lines
wayfindr logs -f                  # keep printing new lines until Ctrl-C
```

This prints `wayfindr.log` in the data directory, the log of a daemon started
with `wayfindr start`, by the shim, or as a login service. A daemon run with
`wayfindr start --foreground` logs to its terminal instead.

Each line is one JSON object. Every connector state change is logged with its
reason, which has more detail than the advice `status` prints:

```json
{
  "level": "warn",
  "connector": "broken",
  "state": "unreachable",
  "reason": "unreachable (spawn_failed): start stdio child: fork/exec /nonexistent/mcp-server: no such file or directory",
  "message": "connector state changed"
}
```

For more detail, restart the daemon with `--log-level debug`. See
[`wayfindr logs`](/reference/cli#wayfindr-logs) and
[Log file](/reference/paths-and-environment#log-file).

## Why a tool ranks where it does

`wayfindr search` runs the same search an agent's `search_tools` runs and prints
the page it would get. `--explain` replaces the SUMMARY column with the scores:

```sh
wayfindr search open a ticket --explain
```

```text
Embeddings: on (text-embedding-3-small)

RANK  TOOL                 FUSED   LEXICAL   VECTOR
1     linear.create_issue  0.3095  #2  7.41  #1  0.712
2     github.create_issue  0.2917  #1  7.88  #3  0.655
3     jira.create_ticket   0.1429  -         #2  0.690
```

- **The header** says whether this run embedded the query. `off` means no
  embedding provider is configured. `failed (…)` means the provider failed just
  now, so this search is lexical only. An agent's earlier search may have gone
  the other way: a provider can fail once and recover.
- **LEXICAL** is where the tool placed on word matching, and its score. `-`
  means none of your words matched it.
- **VECTOR** is where it placed on meaning, and its cosine similarity to the
  query (1 is identical). `-` means it was not among the 50 nearest, search is
  lexical only, or the tool has no embedding from the current model yet, as just
  after you change `embedding.model`.
- **FUSED** combines the two: 1 / (5 + rank) for each ranking the tool is in. A
  tool that places well in both beats one that places first in only one, but a
  first place counts for more than two places further down. A tool whose name is
  your query comes first whatever its FUSED score
  ([How search works](./search#matching)).

Ranks count over your whole catalog. With `--connector`, the other connectors'
tools are dropped after ranking, so the ranks shown can skip numbers.
[How search works](./search) explains the rankings, and
[`wayfindr search`](/reference/cli#wayfindr-search) has every flag.

## Which tools can agents use?

```sh
wayfindr connector tools gmail
```

```text
TOOL           CLASS        EXPOSED  REQUIRED ARGS
create_draft   write        yes      2
delete_thread  destructive  no       1
get_thread     read         yes      1
send_message   write        no       3

2 of 4 tools exposed.
```

This lists every tool the upstream serves now, its class from the upstream's
annotations, whether the connector's tools list lets agents see it, and how
many arguments it requires. Use it to write a [tools list](./access/tools-lists),
or pick a tool to [try by hand](./testing-tools). `wayfindr status` shows the same
count as `38 (4 hidden)`. See
[`wayfindr connector tools`](/reference/cli#wayfindr-connector-tools).

## What do agents get in a project?

Run `wayfindr scope` in the project's directory to see which
[project scope](./access/project-scopes) applies there and which connectors it
lets agents use. Add `--profile <name>` to apply a [profile](./access/profiles)
too, and a connector name to list its tools with every layer applied:

```sh
cd ~/src/acme-api
wayfindr scope --profile acme
```

```text
Profile: acme
Project scope: /Users/you/src/acme-api/.wayfindr.toml

CONNECTOR  IN SCOPE  PROFILE TOOLS  SCOPE TOOLS
github     yes       @read          @read, create_pull_request
gmail      no        -              -
linear     yes       as configured  as configured
```

See [`wayfindr scope`](/reference/cli#wayfindr-scope).
