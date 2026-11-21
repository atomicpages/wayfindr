# How Search Works

Your agent sees two tools: `search_tools` and `call_tool`. It finds a tool with
the first and runs it with the second. Here is one exchange.

The agent searches in plain words:

```json
{
  "name": "search_tools",
  "arguments": { "query": "create an issue", "size": 2 }
}
```

Wayfindr answers with full tool definitions, ranked best first:

```json
{
  "tools": [
    {
      "name": "linear.create_issue",
      "description": "Create a new issue in a Linear team.",
      "inputSchema": {
        "type": "object",
        "properties": {
          "teamId": { "type": "string" },
          "title": { "type": "string" }
        },
        "required": ["teamId", "title"]
      }
    },
    {
      "name": "github.create_issue",
      "description": "Open an issue in a GitHub repository.",
      "inputSchema": { "type": "object", "properties": { "…": {} } }
    }
  ],
  "page": 1,
  "size": 2,
  "has_more": true,
  "unavailable": [],
  "warnings": []
}
```

The agent then calls the tool by its qualified name:

```json
{
  "name": "call_tool",
  "arguments": {
    "name": "linear.create_issue",
    "arguments": { "teamId": "ENG", "title": "Fix the login page" }
  }
}
```

Wayfindr checks the arguments against the tool's `inputSchema`, calls the
upstream, and returns its result unchanged.

The exact arguments and result fields are in the
[meta-tools reference](/reference/meta-tools).

## Qualified names

Every tool is named `<connector>.<tool>`, such as `linear.create_issue`. The
connector part is the name of the `[connectors.<name>]` table in your config.
Connector names can't contain dots, so the name always splits cleanly.

Each result is the tool's definition as the upstream sent it, with only its
`name` rewritten. Annotations such as `readOnlyHint` and `destructiveHint` pass
through, so the agent can see them. Your client's permission prompts can't use
them: the client sees every call as `call_tool`, so a rule in the client allows
or blocks all upstream tools together. To choose which tools agents get, give
the connector a [`tools` list](./access/tools-lists).

## Searching, briefly

`search_tools` takes a `query`, and optionally `size`, `page`, `connectors`,
`detail`, and `names`. Ranking is deterministic, so page 2 gives the results
right after page 1. Two arguments keep an agent's context small:

- `"detail": "brief"` returns a short entry per tool (name, title, a
  one-sentence summary, the arguments it requires, and its annotations) without
  schemas, so a page holds more.
- `names` returns the full definitions of exactly the qualified names the agent
  picked, without searching. Names it can't find come back in `missing`.

Brief and full searches return the same tools in the same order; only the shape
of each entry differs. The default is `"full"`, so agents that don't know about
`detail` work as before. Every argument, limit, result field, and error is in
the [meta-tools reference](/reference/meta-tools#search-tools).

## The catalog

Search never asks the upstreams. It reads the **catalog**: Wayfindr's stored
copy of every connector's tool definitions. That keeps search fast, and it works
for a connector that is down right now.

Wayfindr **syncs** a connector, fetching its tool list into the catalog:

- at startup, when each connector connects;
- on a config reload that adds or changes the connector;
- after you authorize it, or set a secret it was waiting on;
- when the upstream sends `notifications/tools/list_changed`;
- every `refresh_interval` (default `1h`) for HTTP connectors;
- each time a stdio connector's process starts.

Stdio connectors are not polled. Wayfindr stops an idle stdio process after 10
minutes and starts it again on the next call, and that new process syncs.

HTTP connectors are polled when they are `ready` or `unreachable`. Each poll
lands within ±10% of `refresh_interval`, so connectors don't all poll at once.
If the upstream sends a cache hint (`ttlMs`) with its tool list, Wayfindr polls
when the hint runs out instead. It clamps the hint to between 1 minute and 24
hours, then adds up to 20% of random delay, so the poll comes a little after the
hint, never before.

The catalog is kept in the [store](./storage), so it survives a restart. When
you remove a connector from the config, its tools leave the catalog at once.
`wayfindr status` shows each connector's tool count and last sync.

## Matching

Without embeddings, search is lexical. It matches your words against each tool's
qualified name, title, description, parameter names, and parameter descriptions.
A match in the name counts most, and a match in a parameter description counts
least. Words that appear in many tools count less than rare ones.

It forgives the usual differences:

- **Word splitting.** `createIssue`, `create_issue`, and `create-issue` all read
  as `create issue`.
- **Plurals.** `invoice` finds `list_invoices`, and `queries` finds `query`.
- **Typos.** A word of four or more letters that matches nothing is compared
  with the words in your catalog by shared letter triples. `creat_isue` finds
  `create_issue`.

If your query is a tool's name, that tool comes first. `list issues`,
`list_issues`, and `listIssues` all put every `list_issues` tool at the top, and
`github.list_issues` puts GitHub's first. The words must be the name's words, in
the same order, with nothing added: `list issue` and `list the issues` are
ranked as usual. This holds with or without embeddings.

## Hybrid search

With an [embedding provider](./embeddings) configured, each search runs two
rankings and merges them:

- the lexical ranking above;
- the 50 tools whose embeddings are closest in meaning to the query.

Wayfindr fuses the two with reciprocal-rank fusion: each list adds 1 / (5 +
rank) to a tool's score. A tool high in either list ranks high, and a tool in
both ranks higher. The top places count most, so a tool that one list puts first
can beat tools that both lists put in the middle. So
`send a message to the team` can find `slack.chat_post_message` even though the
words differ.

Embeddings only add results. If the provider is slow or down, the search runs
lexically and still answers. See [Embeddings](./embeddings).

## Check what an agent sees

`wayfindr search open a ticket` runs the same search an agent's `search_tools`
runs and prints the page it would get. Add `--explain` to see why each tool
ranks where it does; see
[Monitoring](./monitoring#why-a-tool-ranks-where-it-does).

## Unavailable connectors

Every result has an `unavailable` list: each connector that is not `ready`, with
its state and what to do about it, the same text `wayfindr status` prints. An
agent can pass the `action` and `link` on to you. A `warnings` list names each
`ready` connector whose OAuth credential stops working within a day. Both are
described in the
[meta-tools reference](/reference/meta-tools#result); the states are in
[Troubleshooting](./troubleshooting#connector-states).

A connector in `unavailable` may still have tools in the catalog from an earlier
sync, so they can show up in results. Calling one returns the same advice as an
error, with the link when there is one.
