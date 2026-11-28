# Scoped Client Tokens

A scoped client token limits a client to the connectors you name, and the client
can't drop it. Use one for an agent that runs unattended.

```text
$ wayfindr token create nightly-triage --connector github --connector linear
Created client token nightly-triage. It can use: github, linear.
wf_s_6kq1…
This is the only time it is shown; store it where the client can read it.
```

## Why

The [default client token](/reference/paths-and-environment#client-token) that
`wayfindr init` writes can use every connector. Every client that holds it can
reach your mail, your chat, and your code, and can set secrets and revoke
credentials through the admin API. For your own interactive client, that's what
you want. For an agent that runs unattended, such as a nightly job that triages
GitHub issues, it isn't: a prompt injection in an issue body could make it read
your mail.

A [tools list](./tools-lists), [profile](./profiles), or
[project scope](./project-scopes) doesn't solve that on its own: a tools list
applies to every client, and the client picks its profile and scope, so one that
leaves them off gets everything its token allows. A scoped token is the limit the
client can't escape; see [how the layers combine](./#how-they-combine).

## Create one

Run `wayfindr token create` while the daemon runs. Name each connector the
client needs with `--connector`; each must be configured.

The token's name must match `[a-z0-9][a-z0-9_-]{0,62}`, the same rule as a
connector name, and `default` is reserved for the default client token. A name
that's taken is refused: revoke the token first.

The value is printed once, and Wayfindr can't show it again. Store it where the
client can read it, such as your scheduler's secret store. If you lose it,
revoke the token and create another.

## Give it to the client

A client presents a scoped token exactly as it does the default one. Set
`WAYFINDR_CLIENT_TOKEN` to its value in the environment the client runs in, and
only there:

- **HTTP:** the client sends it as `Authorization: Bearer <token>`. Every HTTP
  example in [Clients](../clients) reads `WAYFINDR_CLIENT_TOKEN`, so they work
  unchanged.
- **stdio:** `wayfindr stdio` reads `WAYFINDR_CLIENT_TOKEN` before
  `<data>/client-token`, so the shim sends the scoped token instead of the
  default one.

For example, a Claude Code `.mcp.json` for the job, which takes the value from a
variable of your own so the file doesn't hold it:

```json
{
  "mcpServers": {
    "wayfindr": {
      "command": "wayfindr",
      "args": ["stdio"],
      "env": {
        "WAYFINDR_CLIENT_TOKEN": "${TRIAGE_WAYFINDR_TOKEN}"
      }
    }
  }
}
```

If the shim has to start the daemon, it leaves a scoped `WAYFINDR_CLIENT_TOKEN`
out of the daemon's environment, so the daemon reads its default token from
`<data>/client-token`.

## What the client gets

- **Only its connectors.** Every other connector is as if it weren't configured,
  and the agent never sees a "hidden by" message for one: the token is checked
  before the tools list, the profile, and the project scope. `call_tool` on one
  of its tools gets `no connector is called gmail`, and `wayfindr calls` shows
  `rejected: no_connector`. See [what agents see](./#what-agents-see).
- **No admin API.** Every `/admin/…` request gets `403`
  `This client token cannot use the admin API.` So the agent can't set secrets,
  revoke credentials, read the call log, or make itself another token.
- **Its risky calls wait for you,** if it is a [remote token](#remote-tokens)
  made with approval entries. A call an entry matches is refused until you
  approve it. See [Approval gates](#approval-gates).

A scoped token decides which connectors a client may use, not what a connector's
credential can do. It doesn't narrow the OAuth scopes Wayfindr was granted.
Within its connectors, every tool the connector's [tools list](./tools-lists)
exposes is in reach, so pair the two for an agent that only needs to read. A
[profile](./profiles) or project scope narrows further within the token's
connectors.

## List and revoke tokens

`wayfindr token list` shows each token's connectors, whether it is
[remote](#remote-tokens), and when it was last used; `--json` prints the daemon's
response as JSON. Once some token has [approval entries](#approval-gates), an
**APPROVAL** column shows them (`-` for a token with none).
`wayfindr token revoke <name>` deletes one, and every client using it is refused
from its next request on, with no restart.

A token's connectors can't be changed: revoke it and create a new one. Scoped
tokens don't expire. See [`wayfindr token`](/reference/cli#wayfindr-token).

## Remote tokens

Every token above is for a client on your machine. A cloud agent, such as Grok
Bot or Muse, runs in a vendor's cloud and reaches Wayfindr through a tunnel you
run. A request through a tunnel gets `404` on `/mcp` with any token, unless the
token is **remote** (see [Loopback only](../security#loopback-only)). A remote
token is a scoped token made for that one case.

Make one with `--remote`, naming only the connectors the agent needs:

```text
$ wayfindr token create grok-bot --connector github --remote
Created remote client token grok-bot. It can use: github.
wf_s_9d2f…
This is the only time it is shown; store it where the client can read it.
```

The [console's Cloud agents page](../console#connecting-a-cloud-agent) makes one
for you and walks you through the rest. The
[Cloud agents guide](../cloud-agents) covers the tunnel and each agent.

- **Only a scoped token can be remote.** The default client token never is.
- **It is fixed when you create it.** Like its connectors, `remote` can't be
  changed. To make a local token remote, or the reverse, revoke it and create a
  new one.
- **It still reaches only its connectors,** and never the admin API, the
  console, or auth links. It works locally too, like any scoped token.
- **Revoke it to cut an agent off.** The next request with it gets `404` through
  a tunnel, with no restart.
- **A leaked remote token works from anywhere** until you revoke it, and it
  doesn't expire. `wayfindr calls` names the token behind each call, so you can
  read back what the agent did.

`wayfindr token list` shows `yes` in the **REMOTE** column for these, and the
console marks them with a `remote` badge.

### Approval gates

A cloud agent works while nobody watches. A remote token can hold some of its
calls until you approve them. Give it entries when you create it:

```text
$ wayfindr token create grok-bot --connector slack --connector linear --remote \
    --approval @write --approval @destructive --approval '!slack.add_reaction'
Created remote client token grok-bot. It can use: slack, linear.
Calls that need your approval: write and destructive tools, except slack.add_reaction.
wf_s_9d2f…
This is the only time it is shown; store it where the client can read it.
```

A call that an entry matches, and no `!` entry exempts, is refused until you
approve it. Calls that match nothing run as before. A token made without
`--approval` has no gate, and neither has the default token or a token made
without `--remote`, which can't take `--approval`. A remote token is held
wherever it is used, `wayfindr call --token-file` on your machine included.

| Entry                 | Holds                                                                                                                                                             |
| --------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `@write`              | Tools the upstream marks as not destructive (and not read-only). It holds only those: not `read` tools, not `destructive` ones, and not tools with no annotations. |
| `@destructive`        | Every tool that is neither read-only nor marked not destructive, which includes each tool the upstream gave no annotations. MCP's defaults make those destructive.  |
| `<connector>.<glob>`  | Tools of that connector whose name matches the glob, such as `linear.run_*`. `*` and `?` also match `/`.                                                          |
| `!<connector>.<glob>` | Lets the tools it matches through, whatever the other entries say. A class can't be exempted, and a list of only exemptions is refused.                            |

Each class holds only itself, unlike a [tools list](./tools-lists), where
`@write` also lets `@read` tools through. `@write` alone leaves the riskiest
calls, the destructive and unannotated ones, unheld, and `token create` warns
you when you give `@write` without `@destructive`. `@read` can't be held. An
entry can name only a connector the token can use. The entries are fixed when
you create the token: to change them, revoke it and create another.

Classes come from what the upstream says about its tools. An upstream that marks
a destructive tool read-only slips past `@destructive`; name the tool instead.

**What the agent sees.** The call is refused, as a `rejected` result, and the
text names the approval:

```text
slack.post_message needs the owner's approval before it runs (approval a3f2).
To fix: Wait for the owner to approve it in Wayfindr, then call it again with the same arguments. Changed arguments need a new approval.
```

**What you do.** Read the call and its arguments in the console's
[Approvals page](../console#approving-calls) or with
[`wayfindr approvals`](/reference/cli#wayfindr-approvals), then approve or deny
it by its id. The arguments shown are the ones Wayfindr holds, not the agent's
description of them. Nothing a client sends can answer: only the admin API
can, and a remote client can't reach it.

**What an approval covers.** One attempt at exactly that tool with exactly
those arguments, from that token.

- It runs on the agent's next identical call, and is used up before the
  upstream is called, even if the call then fails. A second identical call waits again. A call with changed
  arguments is a new call with its own approval.
- A waiting call lasts 24 hours. An approved one lasts 1 hour from your answer.
  A denied one is refused until it expires, 24 hours after the first call and at
  least 1 hour after the denial. You can still approve a denied call, and you
  can deny an approved one before it is used.
- A token has at most 8 calls in the queue, waiting, approved, or denied. A new
  call that would make a ninth is refused (`queue_full`) and nothing is dropped
  to make room; a repeat of a call already in the queue still gets its entry's
  answer. Entries free
  their place when they are used or expire; expiry is checked when something
  looks at the entry, so an expired entry may read as no entry at all, and the
  call waits again.
- The queue lives in memory. Restarting Wayfindr drops every entry, denials
  included, and the agent's next try waits again. A gated call that was
  answering a question from its server when Wayfindr restarted starts again
  too.
- A gated call whose arguments repeat a key in an object (keys that differ only
  in case or Unicode form, such as `ﬁle` and `file`, count as the same key), or
  hold text that isn't valid UTF-8, is
  refused (`invalid_arguments`, `ambiguous`), because you could approve one
  reading while the upstream ran another.
- A gated call whose arguments are larger than 64 KiB, or nest deeper than 32
  levels, is refused (`invalid_arguments`, `too_large`): you read the arguments
  in full before you approve them.
- `wayfindr approvals approve <id>` shows the call and asks before it approves
  it. An agent may ask you to run it; read the call it shows, not the agent's
  description.

`wayfindr calls` shows each refusal as `rejected` with `pending`, `denied`,
`expired`, `queue_full`, `ambiguous`, or `too_large`, and never the arguments.

::: warning
An approval gate protects only a remote token or an OAuth grant. A local agent
can read the default client token, which the admin API accepts, and approve its
own calls. So `--approval` without `--remote` is refused. It also doesn't stop
you from approving a bad call: you decide on the arguments you see.
:::

The same entries are on the console's page that approves an OAuth connection,
such as ChatGPT's ([Cloud agents](../cloud-agents#the-approve-page)). The
console's own form for a remote token has no approval field: create the token
with `wayfindr token create --approval` instead. For why it works this way, see
[ADR 0024](../../adr/0024-approval-gates-on-remote-clients).

`wayfindr calls` records which token each call used: `default` or the scoped
token's name. Use it to see what an unattended agent did.

## How they're stored

Only the token's SHA-256 is stored, never its value, and there is no file for
it. Scoped tokens start with `wf_s_`, so you and secret scanners can tell them
apart from the default token (`wf_…`).

## Where a scoped token doesn't work

The daemon and the `wayfindr` CLI need the default token:

| Where the scoped value is                     | What happens                                                                                                                                                                                                                     |
| --------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `WAYFINDR_CLIENT_TOKEN` for a CLI command     | Commands that use the admin API, such as `wayfindr status`, fail with `This client token cannot use the admin API.` Run them where the default token applies.                                                                    |
| `WAYFINDR_CLIENT_TOKEN` of a daemon you start | The daemon refuses to start: `client token: WAYFINDR_CLIENT_TOKEN holds a scoped client token, not the default one; unset it for the daemon`. (The shim drops it for you, as above.)                                             |
| `<data>/client-token`                         | The daemon refuses to start, naming the file: ``client token: <data>/client-token holds a scoped client token, not the default one; put the default token back, or delete the file and run `wayfindr init` to write a new one``. |
