# Control What Agents Can Use

Every client sees your connectors through two tools, `search_tools` and
`call_tool`. Out of the box, an agent can find and call every tool of every
connector. Four settings narrow that. Each only takes away; none can give an
agent more than the others allow.

```toml
# ~/.config/wayfindr/wayfindr.toml
[connectors.gmail]
url   = "https://gmail.example.com/mcp"
tools = ["@read", "create_draft"]        # a tools list: read and draft, never send

[profiles.acme]                          # a profile a client can pick by name
connectors = ["github", "linear"]
```

```toml
# ~/src/acme-api/.wayfindr.toml          # a project scope
connectors = ["github", "linear"]
```

```sh
# a scoped client token for an unattended job
wayfindr token create nightly-triage --connector github --connector linear
```

## Which one do I use?

| You want to                                                            | Use                                        |
| ---------------------------------------------------------------------- | ------------------------------------------ |
| Block a tool everywhere, such as `send_message` or deletes             | A [tools list](./tools-lists)              |
| Allow only read-only tools of a connector                              | A [tools list](./tools-lists) with `@read` |
| Give agents in one repository fewer connectors or tools                | A [project scope](./project-scopes)        |
| Do the same for a client that connects over HTTP                       | A [profile](./profiles)                    |
| Keep limits for a project out of its repository                        | A [profile](./profiles)                    |
| Hold an unattended agent to some connectors, whatever it is told       | A [scoped client token](./scoped-tokens)   |
| Keep an agent away from the admin API (secrets, credentials, call log) | A [scoped client token](./scoped-tokens)   |
| Hold a cloud agent's risky calls until you approve them                | [Approval entries](./scoped-tokens#approval-gates) on a remote token or an OAuth grant |

## How they combine

| Layer                             | Who sets it                        | Narrows              | Can the client drop it? |
| --------------------------------- | ---------------------------------- | -------------------- | ----------------------- |
| [Client token](./scoped-tokens)   | You, with `wayfindr token create`  | Connectors           | No                      |
| [Tools list](./tools-lists)       | You, in the config                 | Tools of a connector | No                      |
| [Profile](./profiles)             | You define it; the client picks it | Connectors and tools | Yes                     |
| [Project scope](./project-scopes) | The project's `.wayfindr.toml`     | Connectors and tools | Yes                     |

[Approval gates](./scoped-tokens#approval-gates) are not a layer of this kind.
They hide nothing. A remote token or OAuth grant made with approval entries has
some of its `call_tool` calls held until you approve them, after every layer
above has allowed the tool. They are fixed at creation, like the connectors,
and the client can't drop them. They don't apply to `search_tools`. The default
token and tokens made without `--remote` can't have them, since a local client
could approve its own calls; a remote token is held wherever it is used, your
machine included.

A tool is in reach only when every layer allows it. A profile that names a
connector the token doesn't allow doesn't add it back, and a scope file can't
show a tool your tools list hides.

The client token is checked first. A connector it doesn't allow is as if it
weren't configured: it is left out of `search_tools` results, `unavailable`, and
error messages, and the agent never sees a "hidden by" message for it.

A profile and a project scope are for keeping a cooperative agent focused. The
client picks them, by a flag, a URL parameter, or the directory it starts in,
and a client that leaves them off gets everything its token allows. They are no
defense against an agent that does what a prompt injection tells it to. For
that, use a [scoped client token](./scoped-tokens).

## What agents see

`search_tools` never returns a tool a layer hides, and a lookup by `names` lists
it under `missing`. `call_tool` refuses one and names the layer, so you know
where to change it:

| Hidden by     | `call_tool` says                                                                                | `wayfindr calls` shows        |
| ------------- | ----------------------------------------------------------------------------------------------- | ----------------------------- |
| Client token  | `There is no tool called gmail.send: no connector is called gmail.`                             | `rejected: no_connector`      |
| Tools list    | `gmail.send_message is hidden by the tools list of [connectors.gmail] in your Wayfindr config.` | `rejected: hidden_by_policy`  |
| Profile       | `github.create_pull_request is hidden by the Wayfindr profile acme.`                            | `rejected: hidden_by_profile` |
| Project scope | `gmail.send_message is hidden by this project's Wayfindr scope (.wayfindr.toml).`               | `rejected: hidden_by_scope`   |
| Approval gate | `slack.post_message needs the owner's approval before it runs (approval a3f2).`                 | `rejected: pending`           |

The approval gate hides nothing: the tool shows up in search, and its call is
held. A denied, expired, or full-queue call gets its own text, and `wayfindr
calls` shows `denied`, `expired`, or `queue_full`; see
[A call waits for your approval](../troubleshooting#a-call-waits-for-your-approval).

To see what an agent gets in a directory, with every layer applied:

```sh
cd ~/src/acme-api
wayfindr scope --profile acme
```

See [`wayfindr scope`](/reference/cli#wayfindr-scope).
