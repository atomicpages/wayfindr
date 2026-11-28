# 0014 — Record every `call_tool` call in a call log, without arguments or results

Status: Accepted (2026-11-08). Written down on 2026-11-12 (wayfindr-q2rb); the
decision shipped in `7d7967f` to `57a9398` (wayfindr-9br6). Amended:

- 2026-11-09 (wayfindr-7a8p): a call that returns a question to the client ends
  as `input_required` ([0008](./0008-relay-elicitation)).
- 2026-11-11 (wayfindr-5rbg): each call also records the name of the client
  token it authenticated with ([0012](./0012-scoped-client-tokens)).
- 2026-11-12 (wayfindr-e1ky): a call that does not end `ok` also records the
  error text the agent got back, cleaned and cut to 500 bytes.

## Context

Agents call every upstream tool through one meta-tool
([0001](./0001-two-meta-tools)), so a harness's own history shows `call_tool`,
not which upstream tool ran. Users want to see what their agents did: which
tools, from which client, and whether each call worked.

A call's arguments and results hold the user's data: email bodies, documents,
and whatever an upstream echoes back. Storing them would make the store a copy
of everything an agent touched.

Recording a call must not slow it down or make it fail.

## Decision

- Every `call_tool` call gets one row in a `calls` table: when it started, the
  connector and tool as the agent wrote them, the calling client, how long the
  upstream call took, and how the call ended (`ok`, `tool_error`, `rejected`, or
  `failed`), with the error's Kind and Case. A call that returns a question ends
  as `input_required` (amended 2026-11-09, wayfindr-7a8p).
- Arguments and results are never stored. The exception is the error text of a
  call that does not end `ok` (amended 2026-11-12, wayfindr-e1ky): for a
  `tool_error`, the text of the upstream's `isError` result; otherwise the
  message Wayfindr returned. It is kept on one line, with control characters
  replaced, URL query strings masked, and at most 500 bytes. An `ok` or
  `input_required` call stores none.
- A call that returned a result also records its size in bytes, the length of
  the result's JSON encoding, in `result_bytes` (amended 2026-11-25,
  wayfindr-690r). The size is metadata, so the result itself is still not
  stored.
- The client is the `clientInfo` name the client asserts. The stdio shim
  forwards its own client's name. A `token_name` column holds the name of the
  client token the daemon verified, `default` or a scoped token's name, never a
  value (amended 2026-11-11, wayfindr-5rbg).
- `calls.retention` sets how long a call is kept, 30 days by default. Every
  hour, calls older than that are deleted. `0s` turns the log off: nothing is
  recorded or deleted.
- The gateway hands each call to a queue of 256 entries and returns. One writer
  stores the calls off the request path. When the queue is full, the call is
  dropped, and the daemon log says how many were dropped. At shutdown the writer
  stores what is queued for up to 2 seconds.
- `wayfindr calls` lists the log, through the admin API.

## Consequences

- Users can see what agents did, by client and by token, without a second copy
  of their data.
- The log can't say what an agent sent or got back from a call that worked. The
  upstream's own records, if any, have that.
- A failed call says why, after the fact, in the upstream's own words. That text
  can quote what the agent sent, such as a search query, so it is cut short and
  deleted with its call.
- Under a burst of calls, the log may miss some rather than slow them down. The
  daemon log says when it did.
- A refusal is recorded with its own Case, so a call hidden by a tools list, a
  project scope, a profile, or a client token can be told apart
  ([0009](./0009-tool-policy-in-the-gateway), [0010](./0010-project-scopes),
  [0011](./0011-named-profiles), [0012](./0012-scoped-client-tokens)).
- The client name is what the client claims. Only `token_name` is verified.
- With `0s`, calls already in the log stay until the retention is set again.
