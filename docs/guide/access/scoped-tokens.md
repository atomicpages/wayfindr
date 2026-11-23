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

A scoped token decides which connectors a client may use, not what a connector's
credential can do. It doesn't narrow the OAuth scopes Wayfindr was granted.
Within its connectors, every tool the connector's [tools list](./tools-lists)
exposes is in reach, so pair the two for an agent that only needs to read. A
[profile](./profiles) or project scope narrows further within the token's
connectors.

## List and revoke tokens

`wayfindr token list` shows each token's connectors, whether it is
[remote](#remote-tokens), and when it was last used; `--json` prints the daemon's
response as JSON.
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
