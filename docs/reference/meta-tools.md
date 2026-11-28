# Meta-Tools

Wayfindr exposes exactly two tools to clients: `search_tools` finds upstream
tools, and `call_tool` calls one. This page is their exact contract. For why
there are only two, see [ADR 0001](/adr/0001-two-meta-tools); for how ranking
works, see [How Search Works](/guide/search).

A typical session:

1. The agent calls `search_tools` with what it wants to do.
2. It reads the returned definitions and picks a tool by its qualified name,
   such as `linear.create_issue`.
3. It calls `call_tool` with that name and arguments that match the tool's
   `inputSchema`.

To survey many tools cheaply, the agent can search with `"detail": "brief"`
first, then fetch the full definitions of the tools it picked with `names`.

Every tool is addressed by its **qualified name**, `<connector>.<tool>`.
Connector names can't contain dots, so the first `.` splits the name:
`fs.read.file` is tool `read.file` on connector `fs`.

A client with a [scoped client token](/guide/access/scoped-tokens) sees only its
token's connectors. On this page, "configured" means configured and allowed by
the token: every other connector is as if it weren't configured, in results, in
`unavailable` and `warnings`, and in error messages.

## `search_tools`

Searches the catalog, the daemon's stored copy of every connector's tool
definitions. It never contacts an upstream, so it answers even when connectors
are down.

Agents see it titled **Search tools**, with this description:

> Find tools across every connected MCP server by natural language or keywords.
> Returns full tool definitions (qualified name, description, inputSchema); call
> one with call_tool. Use detail=brief to survey many tools cheaply, then
> names=[…] to get the full definitions of the ones you will call. Connectors
> that cannot be used right now are listed under unavailable, with how to fix
> them; connectors about to stop working are listed under warnings. Tell the
> user about either.

Its annotations mark it read-only (`readOnlyHint: true`) and closed-world
(`openWorldHint: false`).

### Input

| Field        | Type                  | Required           | Default                          | Limits                                                          |
| ------------ | --------------------- | ------------------ | -------------------------------- | --------------------------------------------------------------- |
| `query`      | string                | Unless `names`     |                                  | Must not be blank.                                              |
| `page`       | integer               | No                 | `1`                              | At least 1.                                                     |
| `size`       | integer               | No                 | `5`, or `15` with `detail` brief | 1 to 10, or 1 to 25 with `detail` brief.                        |
| `connectors` | array of strings      | No                 | all connectors                   | Each must be a configured connector name.                       |
| `detail`     | `"full"` or `"brief"` | No                 | `"full"`                         | `"brief"` returns [brief entries](#brief-entries).              |
| `names`      | array of strings      | Instead of `query` |                                  | 1 to 10 qualified names. See [Lookup by name](#lookup-by-name). |

- `query` is natural language or keywords: `"create an issue"`,
  `"slack post message"`. Search is fuzzy and full-text, plus embedding
  similarity when an [embedding provider](./configuration#embedding) is
  configured.
- `page` and `size` must be whole numbers; `5.0` is accepted as `5`, `5.5` is
  not.
- `connectors` limits the search, and the `unavailable` and `warnings` lists, to
  those connectors. An empty array searches all of them.
- `detail` changes only how each tool is shown. Brief and full searches with the
  same arguments return the same tools in the same order.

The schema clients see:

```json
{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "What you want to do, in natural language or keywords. Required unless names is given."
    },
    "page": {
      "type": "integer",
      "minimum": 1,
      "default": 1,
      "description": "1-based page of results."
    },
    "size": {
      "type": "integer",
      "minimum": 1,
      "maximum": 25,
      "description": "Results per page: 1 to 10 (default 5), or up to 25 (default 15) with detail brief."
    },
    "connectors": {
      "type": "array",
      "items": { "type": "string" },
      "description": "Only search these connectors. Omit to search all."
    },
    "detail": {
      "type": "string",
      "enum": ["full", "brief"],
      "default": "full",
      "description": "full returns whole definitions; brief returns name, title, summary, required arguments, and annotations."
    },
    "names": {
      "type": "array",
      "items": { "type": "string" },
      "minItems": 1,
      "maxItems": 10,
      "description": "Qualified names (<connector>.<tool>) to return the full definitions of, in order. Use alone, without query."
    }
  }
}
```

The schema allows `size` up to 25 in both modes. A conditional schema
(`if`/`then` on `detail`) could express the two limits, but few clients evaluate
one, so the schema keeps the simple form and Wayfindr checks the limit itself: a
full search with `size` above 10 fails with the `size` error below.

### Result

One text content item holding a JSON object:

| Field         | Type    | Description                                                                                                                                                                                                                                                                                             |
| ------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `tools`       | array   | Full tool definitions, ranked best first. Each is the upstream's definition exactly as it sent it (`description`, `inputSchema`, `outputSchema`, `title`, `annotations`, and any other fields), except that `name` is the qualified name. With `detail` brief, [brief entries](#brief-entries) instead. |
| `page`        | integer | The page returned.                                                                                                                                                                                                                                                                                      |
| `size`        | integer | The page size used.                                                                                                                                                                                                                                                                                     |
| `has_more`    | boolean | Whether another page follows.                                                                                                                                                                                                                                                                           |
| `unavailable` | array   | Connectors that are not `ready` right now, and what to do about each. Always an array.                                                                                                                                                                                                                  |
| `warnings`    | array   | `ready` connectors whose OAuth credential stops working within a day, and how to head it off. Always an array.                                                                                                                                                                                          |
| `missing`     | array   | Only in a [lookup by name](#lookup-by-name): the names that were not found, or that the tools list, profile, or project scope hides.                                                                                                                                                                    |

Each `unavailable` entry:

| Field       | Type   | Description                                                                             |
| ----------- | ------ | --------------------------------------------------------------------------------------- |
| `connector` | string | The connector name.                                                                     |
| `status`    | string | Its state: `starting`, `needs_auth`, `auth_error`, or `unreachable`.                    |
| `summary`   | string | What is wrong.                                                                          |
| `action`    | string | What fixes it.                                                                          |
| `link`      | string | The [auth link](./http#auth-connector) that starts an auth flow. Only for `needs_auth`. |

The advice is the same text `wayfindr status` prints. A connector that isn't
`ready` may still have tools in the catalog from its last sync; they are
searched and returned as usual.

Each `warnings` entry has `connector`, `summary`, `action`, and `link` (when
there is one), the text of the matching [warning](./cli#warnings) in
`wayfindr status`. A connector is in `unavailable` or `warnings`, never both.
For example:

```json
"warnings": [
  {
    "connector": "linear",
    "summary": "The access token expires at 2026-11-08T17:00:00Z and cannot be refreshed.",
    "action": "Authorize again before then to avoid an interruption.",
    "link": "http://127.0.0.1:7777/auth/linear"
  }
]
```

### Example

Request (`tools/call` params):

```json
{
  "name": "search_tools",
  "arguments": { "query": "echo text" }
}
```

Result:

```json
{
  "content": [
    {
      "type": "text",
      "text": "{\"tools\":[…],\"page\":1,\"size\":5,\"has_more\":false,\"unavailable\":[…],\"warnings\":[]}"
    }
  ]
}
```

The `text`, decoded:

```json
{
  "tools": [
    {
      "name": "fake.echo",
      "description": "Echo the arguments back.",
      "inputSchema": {
        "type": "object",
        "properties": { "text": { "type": "string" } }
      }
    }
  ],
  "page": 1,
  "size": 5,
  "has_more": false,
  "unavailable": [
    {
      "connector": "linear",
      "status": "needs_auth",
      "summary": "The connector has not been authorized yet.",
      "action": "Open the link to authorize it.",
      "link": "http://127.0.0.1:7777/auth/linear"
    },
    {
      "connector": "github",
      "status": "auth_error",
      "summary": "The connector's config refers to secret:github_pat, but no such secret is stored.",
      "action": "Store the secret, or fix the reference in the connector's config."
    }
  ]
}
```

### Brief entries

With `"detail": "brief"`, each tool in `tools` is a brief entry instead of a
full definition. It carries enough to choose a tool, without its schemas:

```json
{
  "name": "linear.create_issue",
  "title": "Create issue",
  "summary": "Create a new issue in a Linear team.",
  "required": ["teamId", "title"],
  "annotations": { "destructiveHint": false }
}
```

| Field         | Type             | Description                                                                                                                                                                  |
| ------------- | ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `name`        | string           | The qualified name.                                                                                                                                                          |
| `title`       | string           | The tool's title. Omitted when the upstream gives none.                                                                                                                      |
| `summary`     | string           | The first sentence of the description's first line, cut to at most 160 characters on a word boundary. The title when there is no description. Omitted when there is neither. |
| `required`    | array of strings | The names of the required top-level arguments. Always an array.                                                                                                              |
| `annotations` | object           | The tool's annotations as the upstream sent them. Omitted when there are none.                                                                                               |

To call a tool found this way, get its `inputSchema` with a lookup by name
first.

### Lookup by name

With `names`, `search_tools` doesn't search: it returns the full definitions of
exactly those qualified names, in the order given. Duplicates are returned once.
`names` can't be combined with `query`, `page`, `size`, `connectors`, or
`"detail": "brief"`.

```json
{
  "name": "search_tools",
  "arguments": { "names": ["linear.create_issue", "linear.nope"] }
}
```

The result has the usual fields, plus `missing`:

```json
{
  "tools": [{ "name": "linear.create_issue", "description": "…", "inputSchema": { … } }],
  "page": 1,
  "size": 1,
  "has_more": false,
  "unavailable": [],
  "warnings": [],
  "missing": ["linear.nope"]
}
```

- A name that isn't qualified, names an unknown connector (or one the client
  token doesn't allow), isn't in the catalog, or is a tool the connector's tools
  list, the client's profile, or the project scope hides goes to `missing`. The
  lookup doesn't fail unless it finds none of the names.
- `unavailable` and `warnings` list only the connectors the names mention.

## `call_tool`

Calls one upstream tool and returns its result unchanged.

Agents see it titled **Call tool**, with this description:

> Call a tool found with search_tools. name is its qualified name
> (&lt;connector&gt;.&lt;tool&gt;); arguments must match the tool's inputSchema.
> Returns the tool's result unchanged.

It has no annotations: what it does depends on the tool it calls.

### Input

| Field       | Type   | Required | Description                                                    |
| ----------- | ------ | -------- | -------------------------------------------------------------- |
| `name`      | string | Yes      | The qualified name, `<connector>.<tool>`, from `search_tools`. |
| `arguments` | object | No       | The tool's arguments. Omitted or `null` means `{}`.            |

```json
{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "Qualified tool name from search_tools, e.g. linear.create_issue."
    },
    "arguments": {
      "type": "object",
      "description": "The tool's arguments, matching its inputSchema."
    }
  },
  "required": ["name"]
}
```

Before calling the upstream, Wayfindr:

1. Looks the tool up in the catalog.
2. Validates `arguments` against the tool's `inputSchema` (JSON Schema 2020-12
   unless the schema says otherwise). References to other documents (`$ref` to a
   URL or file) are never loaded and fail validation, and `format` is not
   enforced. If a schema doesn't compile, the daemon logs a warning and forwards
   the call without validating.
3. Holds the call if the client's token or grant has an
   [approval gate](/guide/access/scoped-tokens#approval-gates) that matches it
   and the owner hasn't approved exactly this call. The agent gets an error
   (see [the errors below](#call-tool-errors-before-the-upstream-is-called)),
   and the upstream is not touched. The default token, and tokens made without
   `--remote`, have no gate. The gate is the token's, not the machine's: a
   remote token used on your machine (`wayfindr call --token-file`) is held as
   well.
4. Calls the upstream, within the connector's
   [`timeout`](./configuration#timeout) (default 120 seconds). If the connector
   has no open connection, it connects first.

The whole request to `/mcp`, arguments included, is limited to 4 MiB.

### Result

The upstream's `CallToolResult`, exactly as it returned it: `content`,
`structuredContent`, and `isError` included. A tool's own error (`isError: true`
from the upstream) is passed through as is.

Request:

```json
{
  "name": "call_tool",
  "arguments": {
    "name": "fake.echo",
    "arguments": { "text": "hello" }
  }
}
```

Result:

```json
{
  "content": [{ "type": "text", "text": "{\"text\":\"hello\"}" }]
}
```

### Questions from the upstream

When the upstream asks you something during the call (elicitation), a client at
protocol 2026-07-28 that declares elicitation gets an `input_required` result
instead:

```json
{
  "resultType": "input_required",
  "inputRequests": {
    "linear-team": {
      "method": "elicitation/create",
      "params": {
        "message": "linear asks: Which team should own this issue?",
        "requestedSchema": {
          "type": "object",
          "properties": {
            "team": { "type": "string", "enum": ["Core", "Web"] }
          }
        }
      }
    }
  },
  "requestState": "<opaque>"
}
```

Each question's id starts with the connector name, and its message starts with
`<connector> asks:`. A URL-mode question shows the upstream's own URL. The
client asks you, then calls `call_tool` again with the same `name` and
`arguments`, plus `inputResponses` and the `requestState`. These two are fields
of the `tools/call` request, next to `name` and `arguments`, not inside the
arguments. MCP SDKs at 2026-07-28 do this for you. The upstream may ask again,
and the round repeats until it returns its result.

The `requestState` is sealed by the daemon and only works for the same tool and
arguments, sent with the same client token, for 10 minutes. A token created
under the name of a revoked one counts as another token. Clients that can't
take `input_required` get an error instead; see
[Compatibility](./compatibility#questions-from-upstreams-elicitation) for which
clients can be asked.

## Errors

Every failure, from either tool, is a normal tool result with `isError: true`
and one text item, never a JSON-RPC error. The text is:

```
<summary>
To fix: <action>
Link: <link>
```

The `Link:` line appears only when there is an auth link.

```json
{
  "content": [
    {
      "type": "text",
      "text": "size must be a whole number from 1 to 10 (default 5), or up to 25 (default 15) with detail brief, not 30.\nTo fix: Omit size for the default."
    }
  ],
  "isError": true
}
```

### `search_tools` errors

| Cause                                                        | Summary                                                                                                                    | To fix                                                                                                        |
| ------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| No `query` (or a blank one) and no `names`                   | `search_tools needs a query, or names.`                                                                                    | `Describe what you want to do in query, in words or keywords; or pass the qualified names you want in names.` |
| Wrong types, such as a number for `query`                    | `The search_tools arguments have the wrong types.`                                                                         | `query and detail are strings, page and size are integers, and connectors and names are arrays of strings.`   |
| `page` below 1 or not whole                                  | `page must be a whole number of at least 1, not 0.`                                                                        | `Omit page for the first page.`                                                                               |
| `size` outside 1–10 (1–25 with brief) or not whole           | `size must be a whole number from 1 to 10 (default 5), or up to 25 (default 15) with detail brief, not 30.`                | `Omit size for the default.`                                                                                  |
| `connectors` names an unknown connector                      | `No connector is called nope. Configured connectors: fake, linear, github.`                                                | `Use one of those names, or omit connectors to search them all.`                                              |
| `detail` is not `"full"` or `"brief"`                        | `detail must be "full" or "brief", not "short".`                                                                           | `Omit detail for full definitions.`                                                                           |
| `names` is empty, has more than 10, or a blank entry         | `names must list 1 to 10 qualified tool names, none blank.`                                                                | `Pass names such as ["linear.create_issue"]; split a longer list over several calls.`                         |
| `names` with `query`, `page`, `size`, `connectors`, or brief | `names cannot be combined with query.`                                                                                     | `Pass names alone to get full definitions, or drop names to search.`                                          |
| None of the `names` was found                                | `None of these tools is in the catalog: linear.nope, jira.x.`                                                              | `Use search_tools to find the tool and its qualified name. Then pass the names it returns.`                   |
| None found, and a connector they name is not `ready`         | The summary above, then `linear is needs_auth: The connector has not been authorized yet.`                                 | That connector's fix, such as `Open the link to authorize it.`, plus `Link:` when there is one.               |
| The catalog can't be read                                    | `Wayfindr could not read its tool catalog.`                                                                                | `Try again; if it persists, check the daemon log.`                                                            |
| This project's scope header is invalid                       | `This project's Wayfindr scope (.wayfindr.toml) is invalid, so Wayfindr refused the request.`                              | ``Tell the user; `wayfindr scope` in the project directory shows what is wrong.``                             |
| The client names a profile the config doesn't define         | `This client asks for the Wayfindr profile acme2, which the config doesn't define, so Wayfindr refused the request.`       | ``Tell the user; `wayfindr scope --profile acme2` shows the profiles the config defines.``                    |
| The client names its profile twice, or with an invalid name  | `This client names its Wayfindr profile more than once, or with a name that isn't valid, so Wayfindr refused the request.` | `Tell the user to check the profile in the client's Wayfindr settings (--profile or ?profile=).`              |

### `call_tool` errors before the upstream is called

| Cause                                                       | Summary                                                                                                                      | To fix                                                                                                                       |
| ----------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| No `name`, or not a string                                  | `call_tool needs name, the qualified name of the tool to call.`                                                              | `Use search_tools to find the tool and its qualified name.`                                                                  |
| `name` has no `.`                                           | `echo is not a qualified tool name; names look like <connector>.<tool>, for example linear.create_issue.`                    | Same as above                                                                                                                |
| No such connector, or the client token doesn't allow it     | `There is no tool called nope.echo: no connector is called nope.`                                                            | Same as above                                                                                                                |
| The connector has no such tool in the catalog               | `There is no tool called fake.nothing in the catalog.`                                                                       | `Use search_tools to find the tool and its qualified name. If the connector is listed there as unavailable, fix that first.` |
| The connector's `tools` list hides the tool                 | `gmail.send_message is hidden by the tools list of [connectors.gmail] in your Wayfindr config.`                              | `Don't use it. If the agent should, the user can add it to that tools list.`                                                 |
| The client's profile hides the tool or connector            | `github.create_pull_request is hidden by the Wayfindr profile acme.`                                                         | `Don't use it. If the agent should, the user can allow it in [profiles.acme] in the config.`                                 |
| The project scope hides the tool or connector               | `gmail.send_message is hidden by this project's Wayfindr scope (.wayfindr.toml).`                                            | `Don't use it. If the agent should, the user can allow it in that file.`                                                     |
| This project's scope header is invalid                      | `This project's Wayfindr scope (.wayfindr.toml) is invalid, so Wayfindr refused the request.`                                | ``Tell the user; `wayfindr scope` in the project directory shows what is wrong.``                                            |
| The client names a profile the config doesn't define        | `This client asks for the Wayfindr profile acme2, which the config doesn't define, so Wayfindr refused the request.`         | ``Tell the user; `wayfindr scope --profile acme2` shows the profiles the config defines.``                                   |
| The client names its profile twice, or with an invalid name | `This client names its Wayfindr profile more than once, or with a name that isn't valid, so Wayfindr refused the request.`   | `Tell the user to check the profile in the client's Wayfindr settings (--profile or ?profile=).`                             |
| `arguments` is not an object                                | `call_tool's arguments must be a JSON object.`                                                                               | `Pass the tool's arguments as an object matching its inputSchema.`                                                           |
| `arguments` fails the `inputSchema`                         | `The arguments do not match fake.echo's inputSchema:` then one `- at <JSON pointer>: <problem>` line per failure, at most 10 | `Fix the arguments and call again. Get the inputSchema with search_tools names=["fake.echo"].`                               |
| A gated call has no approval yet                            | `slack.post_message needs the owner's approval before it runs (approval a3f2).` The first call makes the approval; a repeat gets the same id. | `Wait for the owner to approve it in Wayfindr, then call it again with the same arguments. Changed arguments need a new approval.` |
| The owner denied it                                         | `The owner denied slack.post_message with these arguments (approval a3f2).`                                                  | `Don't retry it. Tell the user it was denied; a call with other arguments needs its own approval.`                           |
| Its approval expired                                        | `The owner's approval of slack.post_message with these arguments (approval a3f2) has expired.`                               | `Call it again with the same arguments to ask again, then wait for the owner to approve it in Wayfindr.`                     |
| The client has 8 live approvals already                     | `slack.post_message needs the owner's approval before it runs, but this client already has 8 open approvals (waiting, approved, or denied), so Wayfindr refused it.` (no approval id) | `A slot frees when an approved call is retried with the same arguments, or when an entry expires (within 24 hours). Don't change the arguments: each variant needs a slot of its own.` |
| A gated call's arguments repeat a key or aren't valid UTF-8 | `slack.post_message needs the owner's approval, and its arguments repeat a key in an object or hold text that isn't valid UTF-8, so the owner could approve something other than what runs. Keys that differ only in case or Unicode form count as the same key.` | `Send each key once per object, and only valid UTF-8 text, then call again.`                                                  |
| A gated call's arguments are over 64 KiB or 32 levels deep  | `slack.post_message needs the owner's approval, and its arguments are larger than 64 KiB or nest deeper than 32 levels, more than the owner can review.` | `Send smaller, flatter arguments, for example by splitting the work over several calls, then call again.`                    |

The first four of the approval rows are Kind `approval`, with the Case
`pending`, `denied`, `expired`, and `queue_full`; the last two are Kind
`invalid_arguments`, Case `ambiguous` and `too_large`. Every client at every protocol version
gets the same `isError` result: the owner is never asked through the client, so
no `input_required` and no form. See
[Approval gates](/guide/access/scoped-tokens#approval-gates) for what each state
means. A schema failure looks like this:

```
The arguments do not match fake.echo's inputSchema:
- at /text: got number, want string
To fix: Fix the arguments and call again. Get the inputSchema with search_tools names=["fake.echo"].
```

### `call_tool` errors from the connector

These come from connecting to or calling the upstream. Most also change the
connector's state, which then shows in `wayfindr status` and in `unavailable`.

| Cause                                               | Summary                                                                                                                                      | To fix                                                                                           |
| --------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| Never authorized                                    | `The connector has not been authorized yet.`                                                                                                 | `Open the link to authorize it.` plus `Link:`                                                    |
| Token expired and can't be refreshed                | `The connector's token expired and cannot be refreshed.`                                                                                     | `Open the link to authorize the connector again.` plus `Link:`                                   |
| `url` changed since authorization                   | `The connector's URL changed since it was authorized, so its token is not sent to the new URL.`                                              | `Open the link to authorize it for the new URL.` plus `Link:`                                    |
| More scopes needed (step-up)                        | `The upstream needs scopes the connector was not granted.`                                                                                   | `Open the link to authorize the connector with the larger set of scopes.` plus `Link:`           |
| Missing secret                                      | `The connector's config refers to secret:<name>, but no such secret is stored.`                                                              | `Store the secret, or fix the reference in the connector's config.`                              |
| Missing environment variable                        | `The connector's config refers to env:<NAME>, but the daemon's environment does not set it.`                                                 | `Set the variable in the daemon's environment and restart it, or fix the reference.`             |
| The call ran past `timeout`                         | `The call outlasted the connector's timeout.`                                                                                                | `Try again, or raise timeout in the connector's config if the tool is slow.`                     |
| One message of the answer is over 16 MiB            | `<connector> returned more than 16 MiB in one message.`                                                                                      | `Call it with narrower arguments or pagination. There is no setting that raises this limit.`     |
| The upstream answered with a JSON-RPC error         | `The upstream refused the request.`                                                                                                          | `Check the tool name and its arguments against the tool's schema.`                               |
| The upstream asked for sampling                     | `This tool asks the client to sample a message with its model, which Wayfindr does not support.`                                             | `Use another tool, or run this one in a client connected to the upstream directly.`              |
| A 2025-x upstream asked during another call to it   | `The upstream asked for input while another call to it was running, so Wayfindr could not tell which call the question was for.`             | `Call the tool again once the connector's other calls have finished.`                            |
| Connection refused                                  | `The upstream refused the connection.`                                                                                                       | `Check that the server is running at the connector's url. Wayfindr reconnects on the next call.` |
| The connect timed out                               | `The upstream did not answer the connect in time.`                                                                                           | `Check that it is up and not overloaded. Wayfindr reconnects on the next call.`                  |
| The command couldn't start                          | `The connector's command could not be started.`                                                                                              | `Check command in the connector's config: it must exist and be executable on the daemon's PATH.` |
| Other transport failure                             | `The upstream cannot be reached.`                                                                                                            | `Check that it is running and the network. Wayfindr reconnects on the next call.`                |
| The authorization server can't be reached           | `The connector's authorization server cannot be reached or sent an unusable answer.`                                                         | `Check the network and the provider's status. Wayfindr reconnects on the next call.`             |
| The authorization server is busy or rate limiting   | `The authorization server is temporarily unavailable or rate limiting (provider code <code>).` (the code only when there is one)             | `Wait a moment and try again.`                                                                   |
| The call was cancelled before the upstream answered | `The call was cancelled before the upstream answered.`                                                                                       | `Try again.`                                                                                     |
| The call was cancelled while connecting             | `The call was cancelled while Wayfindr was still connecting to the upstream.`                                                                | `Try again.`                                                                                     |
| The client cancelled the request (either tool)      | `The request was cancelled before Wayfindr finished it.`                                                                                     | `Try again.`                                                                                     |
| An internal failure of Wayfindr's own               | `Wayfindr's credential handling failed for this call.`, `Wayfindr failed while calling <tool>.`, or `Wayfindr failed to handle the request.` | `Try again; if it persists, check the daemon log.`                                               |
| The connector was removed during the call           | `No connector is called <name>, or it was just removed.`                                                                                     | ``Check the connector name with `wayfindr status`.``                                             |

### `call_tool` errors when the upstream asks a question

| Cause                                                                      | Summary                                                                                           | To fix                                                                                                                   |
| -------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| The client speaks a protocol version before 2026-07-28                     | `linear.create_issue needs your input, but your client's MCP version (2025-11-25) can't ask you.` | ``Update the client, or connect it with `wayfindr stdio`, which can ask clients of every version.``                      |
| The client doesn't declare elicitation                                     | `linear.create_issue needs your input, but your client does not support elicitation.`             | ``Use a client that supports elicitation, or connect it with `wayfindr stdio`, which can ask clients of every version.`` |
| The answer's `requestState` is expired, tampered with, or for another call | `The answer arrived too late or does not belong to this call.`                                    | `Call the tool again.`                                                                                                   |
| The answer's `requestState` was used already, on a call an approval gate holds | `The answer arrived too late or does not belong to this call.`                                | `Call the tool again.`                                                                                                   |

When the client can't be asked, the upstream is told the question was cancelled.

A call that an approval gate holds passes the gate before it reaches the
upstream, so a call that resumes with a valid `requestState` skips it. That
state is taken once: sent again, it gets the last row above, so one approval
runs the tool once however often the agent replays its state. Each further
question from the upstream comes with a new state.

A `needs_auth` result, in full:

```
The connector has not been authorized yet.
To fix: Open the link to authorize it.
Link: http://127.0.0.1:7777/auth/linear
```

Opening the link shows a page with a **Continue** button that starts the auth
flow; see [Authentication](/guide/authentication). The full list of connector
errors and their fixes is in [Troubleshooting](/guide/troubleshooting).

## Other MCP methods

The daemon declares only the `tools` capability, without `listChanged`, and no
`logging`.

| Method                                     | Answer                                                                                              |
| ------------------------------------------ | --------------------------------------------------------------------------------------------------- |
| `tools/list`                               | `search_tools` and `call_tool`.                                                                     |
| `tools/call`                               | As above. Any other tool name is a JSON-RPC error.                                                  |
| `prompts/*`, `resources/*`, `completion/*` | JSON-RPC `-32601` (method not found).                                                               |
| `ping`                                     | Answered.                                                                                           |
| `logging/setLevel`                         | Accepted as a no-op before protocol 2026-07-28; `-32601` from 2026-07-28. No log messages are sent. |

The [stdio shim](./cli#wayfindr-stdio) answers the same way.
