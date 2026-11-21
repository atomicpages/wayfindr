# Project Scopes

Your config decides what agents can use everywhere. A project scope narrows that
for one project: put a `.wayfindr.toml` in the project, and agents working there
see only the connectors and tools it allows.

```toml
# ~/src/acme-api/.wayfindr.toml
connectors = ["github", "linear"]

[tools]
github = ["@read", "create_pull_request"]
```

An agent in `~/src/acme-api` now finds GitHub and Linear tools only. Of GitHub's
tools, it gets reads plus `create_pull_request`, and only those your config's
[tools list](./tools-lists) for `github` also exposes. Every other connector is
as if it weren't configured.

You can commit the file, so everyone on the team gets the same limits.

A scope narrows within what the client's token allows; it never adds to it, and a
connector the token doesn't allow stays out of reach whatever the file says. The
client picks its scope, though, and could leave it off; see
[how the layers combine](./#how-they-combine).

## What the file can say

| Key          | What it does                                                                                                                     |
| ------------ | -------------------------------------------------------------------------------------------------------------------------------- |
| `connectors` | The connectors agents may use here. Leave it out to allow every connector; `[]` allows none.                                     |
| `[tools]`    | One tools list per connector, in the [same syntax](/reference/configuration#tools) as the config's. A tool must pass both lists. |

A connector in `[tools]` must also be in `connectors`, if you set it. Nothing
else is allowed: any other key is an error.

A scope only narrows. It can't add a connector, change a URL or command, or show
a tool your config hides. A scope file in a repository you cloned can hide your
tools from agents there, but it can't give them more.

## Where Wayfindr looks

`wayfindr stdio` looks in the directory your client starts it in, then each
parent, and uses the first `.wayfindr.toml` it finds. It stops before your home
directory, so `~/.wayfindr.toml` never applies; use your config for limits that
apply everywhere. Files are not merged: the nearest one wins.

Check what applies in a directory:

```sh
cd ~/src/acme-api
wayfindr scope
```

```
Project scope: /Users/you/src/acme-api/.wayfindr.toml

CONNECTOR  IN SCOPE  SCOPE TOOLS
github     yes       @read, create_pull_request
gmail      no        -
linear     yes       as configured
```

`wayfindr scope github` lists GitHub's tools and which ones agents in this
project get. `--json` prints either as JSON, for a script or a CI check. See the
[CLI reference](/reference/cli#wayfindr-scope).

::: tip
A connector the file names but your config doesn't have is ignored, with
a warning from `wayfindr scope`. A teammate's file may name a connector you
haven't set up.
:::

## When it applies

- **Through `wayfindr stdio` only.** The scope file comes from the shim, so your
  client must run `wayfindr stdio`, the setup in [Clients](../clients). A client
  that connects to `http://127.0.0.1:7777/mcp` directly never reads
  `.wayfindr.toml`; give it a [profile](./profiles) with `?profile=` instead.
- **When the client starts the shim.** Most clients start it in the project's
  directory. After you edit the file, reconnect the client to Wayfindr (in
  Claude Code, `/mcp`) or restart it.
- **Fails closed.** If the file has an error, `wayfindr stdio` doesn't start,
  and your client shows Wayfindr as failed. Run `wayfindr scope` in the project
  to see the error.

## What agents see

`search_tools` doesn't return what the scope hides. Naming a connector outside
the scope in `connectors` gets `No connector is called …`. A page of results can
come back with fewer tools than `size` when the scope's tools lists hide some of
them.

`call_tool` refuses a hidden tool with
`gmail.send_message is hidden by this project's Wayfindr scope (.wayfindr.toml).`,
and `wayfindr calls` shows the refusal as `rejected: hidden_by_scope`. The full
messages are in [what agents see](./#what-agents-see).

## With a profile

A client can also name a [profile](./profiles). When it does, and runs
`wayfindr stdio` in a project with a scope file, both apply: a tool must pass
your config's tools list, the profile, and the file. See
[Profiles](./profiles#with-a-scope-file).
