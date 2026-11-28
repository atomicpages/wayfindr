# Tools Lists

Clients see every upstream tool through one meta-tool, `call_tool`, so their own
per-tool permission rules can't tell `get_thread` from `delete_repository`. A
connector's tools list does that in Wayfindr instead. It decides which of the
connector's tools agents can find and call, everywhere.

## Limit which tools agents can use

Add `tools` to the connector's table:

```toml
[connectors.gmail]
url   = "https://gmail.example.com/mcp"
tools = ["@read", "create_draft", "update_draft"]   # read, and draft, but never send

[connectors.github]
url   = "https://api.githubcopilot.com/mcp/"
tools = ["@write", "!merge_pull_request"]          # nothing destructive, and no merging

[connectors.linear]
url   = "https://mcp.linear.app/mcp"
tools = ["list_issues", "get_issue"]               # allow exactly these two

[connectors.notion]
url   = "https://mcp.notion.com/mcp"
tools = ["!delete_*"]                              # block deletes, allow the rest
```

Leave `tools` out to expose every tool. The daemon applies a change as soon as
you save the file.

| Entry    | Picks                                                                          |
| -------- | ------------------------------------------------------------------------------ |
| `@read`  | Read-only tools, as the upstream marks them.                                   |
| `@write` | `@read` tools plus the ones the upstream marks not destructive.                |
| `name`   | The tool with that name, such as `create_draft`. Globs (`*`, `?`, `[…]`) work. |
| `!name`  | Blocks the tools it matches, whatever else matches them.                       |

A list of only `!` entries allows everything else. The exact rules are in the
[configuration reference](/reference/configuration#tools).

## See a connector's tools first

To see what a connector offers before you write its list, ask the daemon:

```sh
wayfindr connector tools gmail
```

```
TOOL           CLASS        EXPOSED  REQUIRED ARGS
create_draft   write        yes      2
delete_thread  destructive  no       1
get_thread     read         yes      1
send_message   write        no       3

2 of 4 tools exposed.
```

`wayfindr status` counts the hidden tools in the TOOLS column, as in
`38 (4 hidden)`.

## Read-only is the upstream's word

Classes come from the upstream's annotations, which are hints, not guarantees.
Many servers send none, which makes every tool `destructive`. Some mark a tool
that sends mail or deletes data as read-only or not destructive.

For anything that matters:

- Prefer `@read` plus the writes you name over `@write`.
- Check the classes with `wayfindr connector tools <name>`.
- Names match case-sensitively, so `!delete_*` doesn't block `Delete_Repo`.
- A tools list doesn't look at arguments: allowing `create_or_update_file`
  allows it on every path.

## What agents see

`search_tools` never returns a hidden tool, and `call_tool` refuses it with
`gmail.send_message is hidden by the tools list of [connectors.gmail] in your Wayfindr config.`
`wayfindr calls` shows the refusal as `rejected: hidden_by_policy`. The full
messages are in [what agents see](./#what-agents-see).

The daemon enforces the list against the running config. That holds right after
you change the list, before the connector syncs again, and when an agent answers
a question the tool asked. An agent can't reach a hidden tool through Wayfindr.

## Your client's permission prompt

Your client asks before a tool runs, but it asks about `call_tool`, never the
upstream tool behind it. Most clients show the call's arguments in the prompt,
and `name` is the qualified tool name, such as `gmail.send_message`, so you
still see which tool you're approving.

Use the two together:

1. Block what the agent must never use with the tools list. `search_tools`
   doesn't offer a hidden tool, and `call_tool` refuses it even if you approve
   the call.
2. Then choose how your client treats `call_tool`:
   - **Always allow it.** Nothing the list exposes prompts, reads or writes.
   - **Keep approving each call.** You see every call, reads too.

Wayfindr has no per-tool "ask first", so you can't let reads run freely and
still be asked about the writes the list exposes: it's fewer prompts or seeing
every call.

## Narrower in one place

A tools list applies to every client and every project. To give agents less in
one project, add a [project scope](./project-scopes) or a [profile](./profiles)
there; each can only narrow the list further. See
[how the layers combine](./#how-they-combine).
