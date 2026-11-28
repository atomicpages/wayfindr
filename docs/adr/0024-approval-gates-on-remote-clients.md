# 0024 — Hold a remote client's risky calls until the owner approves them in the console or CLI

Status: Accepted (2026-11-27). Builds on the token-attached limits of
[0012](./0012-scoped-client-tokens) and the clients of
[0021](./0021-remote-client-tokens) and
[0022](./0022-authorization-server-for-remote-clients). Adds a refusal Kind to
the call log of [0014](./0014-call-log), and keeps its line on arguments.

## Context

Remote client tokens ([0021](./0021-remote-client-tokens)) and OAuth grants
([0022](./0022-authorization-server-for-remote-clients)) put cloud agents on
`/mcp`, with write access, while nobody is watching. Their permission layer, if
they have one, lives in the agent's own harness. Harness-side approval has
failed in practice:

- An MCP client advertised elicitation in headless mode and answered yes/no
  forms `accept` with no human present
  ([senpi #4185](https://github.com/code-yeongyu/senpi/issues/4185), fixed by
  declining headless elicitation). The protocol permits that answer. The
  Python SDK's own example callback auto-fills
  ([docs](https://py.sdk.modelcontextprotocol.io/client/callbacks/): "A real
  client renders a form from it; this one auto-fills").
- Prompt injection made Copilot write `"chat.tools.autoApprove": true` into the
  workspace settings (CVE-2025-53773,
  [write-up](https://embracethered.com/blog/posts/2025/github-copilot-remote-code-execution-via-prompt-injection)).
- Claude Code read its permission mode from a repository's settings before it
  showed the workspace-trust prompt, so a repository could set
  `bypassPermissions` (CVE-2026-33068,
  [advisory](https://raxe.ai/labs/advisories/RAXE-2026-040); fixed in 2.1.53).
- With "Needs approval" selected, Claude approved the creation of an email
  draft itself ([arXiv 2607.13718](https://arxiv.org/html/2607.13718), §4.3).

[arXiv 2606.02668](https://arxiv.org/html/2606.02668) (Definition 1) states the
requirement: an approval counts only if a trusted mediator renders it from the
real action, it is collected over a channel the agent can neither draw to,
overlay, nor auto-answer, and it is bound to the exact action that executes.
[arXiv 2609.21081](https://arxiv.org/abs/2609.21081) shows the binding half
failing: the user approves A and B runs.

Wayfindr can be that mediator for remote clients. It holds the real arguments,
and its admin API is a channel a remote agent can't reach: the console and the
admin API answer only loopback requests ([0016](./0016-local-web-console),
[0021](./0021-remote-client-tokens)).

The tool-policy plan dropped "confirm before calling" for local clients. The
client's own prompt already covers every `call_tool`, so a gateway prompt would
be a second one, and only for clients that can elicit. That still holds for a
user at a keyboard ([0009](./0009-tool-policy-in-the-gateway),
[0008](./0008-relay-elicitation)). It doesn't hold for a client nobody watches.

## Threat model

- **Protected against:** an agent that reaches Wayfindr only through a remote
  client token or an OAuth grant, through the tunnel. It can't reach `/admin/`
  or `/ui/`, which answer loopback requests only, so it can't approve its own
  call, however it is prompted.
- **Not protected against:** a local agent. The default token sits in a file
  the user's account can read, and `/admin/` accepts it.
- **Not protected against:** an upstream that labels a destructive tool
  read-only. Classes come from the upstream's annotations. Gating by tool name
  covers it.
- **Not protected against:** an owner who approves a bad call. The gate shows
  the real arguments, and the owner decides.

## Decision

- **Remote clients only.** A remote client token or an OAuth grant can require
  the owner's approval for some of its calls. A local agent can read the
  default token from a file its account can read, and `/admin/` accepts that
  token, so a local agent with a shell can approve anything. Gating a local
  client would promise protection Wayfindr can't give. `--approval` on a token
  that isn't remote is refused, with that reason. The admin API enforces the
  rule too, not only the CLI flag check.
- **The gate is chosen at creation.** `wayfindr token create --remote
  --approval <entry>` (repeatable) and the console page that approves an OAuth
  grant's connectors take the same entries, empty by default, so existing
  tokens and grants change nothing. An entry is `@write` or `@destructive`
  (each class gates itself only: `@write` does not include `@read`, unlike a
  tools list), `<connector>.<glob>` for tools by name, or `!<connector>.<glob>`
  to exempt. A call is gated when an entry matches and no `!` entry does.
  `@write` covers neither destructive tools nor tools without annotations,
  which are classed destructive, so `@write` alone leaves the riskiest calls
  ungated. Like the connectors list, the entries are fixed: revoke and create
  again to change them. Stored entries that no longer parse (a later release
  can tighten the grammar) fail closed: the gateway treats every call from
  that client as gated, never as ungated. Gating by name covers an upstream that labels a destructive tool
  read-only, which the class (from the upstream's annotations) can't.
- **The admin API is the only answer channel.** The owner approves or denies in
  the console (`/ui/approvals`) or with `wayfindr approvals approve|deny <id>`.
  Nothing a client sends can. A client's answer can't be told from a forged one
  (see Context), so the gateway never takes one: no elicitation form, no
  `input_required`, no link. The owner sees the arguments as the gateway holds
  them, through `redact.Visible` (the arguments through
  `redact.VisibleArguments`), never as the agent describes the call: decoded
  as the digest reads them, so an escape shows as the character it stands
  for, with keys in sorted order. An agent can still ask the owner to approve
  an id, so `wayfindr approvals approve <id>` shows the call it approves and
  asks first, or needs `--yes`. It discards what was typed or pasted before
  the question, so a `y` the agent put after the command can't answer it.
- **One-shot, bound to (client instance, tool, argument digest).** The client
  instance is the token's name and creation time, so a token created again under
  a revoked name inherits nothing. The digest is over the call's arguments.
  An approval lets the next call with the same instance, tool, and arguments
  run, and is then _spent_, even when the upstream call fails: it authorized one
  attempt. A second identical call pends again. The digest must read the
  arguments as the upstream will, so a gated call whose arguments repeat a key
  in an object, or hold text that isn't valid UTF-8 (a lone surrogate escape
  included), is refused as `invalid_arguments`, Case `ambiguous`: decoders
  resolve both differently, and the owner could approve one reading while the
  upstream runs another. Keys that look alike count as a repeat: equal ignoring
  case, or once normalized (NFKC), with the Turkish ı and İ read as i, since
  an upstream may match keys that way. So does any data after the arguments
  object. The owner reads the
  arguments in full, so a gated call whose arguments are larger than 64 KiB, or
  nest deeper than 32 levels, is refused as `invalid_arguments`, Case
  `too_large`, which also bounds what the queue holds. An entry's id is not
  given to another call for 24 hours after the entry leaves the queue. That
  holds within one process only: a restart empties the queue and starts the
  ids over. So an answer from the CLI's `approvals approve` or the console
  also names the entry's creation time, and is refused when the live entry
  with that id was created at another time: an answer applies to the call
  the owner was shown, or to nothing.
- **Two lifetimes.** A pending approval lives 24 hours from the first call,
  since the owner is at a desk and not always at it. A granted approval lives 1
  hour from the owner's answer. Expiry is checked on access (a lookup, a
  listing, a count), not swept: the call-log prune is hourly and off when
  retention is `0s` ([0014](./0014-call-log)).
- **At most 8 live entries (pending, granted, or denied) per client, failing
  closed.** When the queue can't take a new entry, the call is refused with
  Case `queue_full`. Nothing is evicted: an agent flooding the queue must not
  push out an entry the owner is about to read. Expired entries don't count,
  and making room drops them, so Case `expired` is best effort: an expired
  entry dropped while making room for another reads as no entry, and the
  retry pends again, which still fails closed.
- **Denial is sticky.** A denied entry refuses matching calls with Case
  `denied` until it expires, 24 hours from the first call and at least 1 hour
  from the denial, so an agent can't retry its way past a no, even one given
  late. The owner can still approve it later. Denying a granted entry revokes
  it before it is spent.
- **Retry-to-spend.** The agent is told to call again with the same arguments
  once the owner approves. An agent that rewrites its arguments on every try
  never matches an approval, and each rewrite creates a new pending entry
  until the bound is hit. That is the cost of exact binding; the cloud-agents
  guide says so.
- **A typed refusal.** Every client, at every protocol version, gets a
  `rejected` result with Kind `approval` and the Case `pending`, `denied`,
  `expired`, or `queue_full`. The text names the approval id and says what to
  wait for. The call log records the same Kind and Case, with no new result
  value ([0014](./0014-call-log)). A call that resumes an `input_required` with
  a valid sealed state skips the gate: only a call that already passed it, and
  reached the upstream, mints a state, and the state is bound to the client
  instance. A gated call's state is taken once: sent again, it is refused as
  `invalid_request_state`, so one approval runs the tool once however often
  the agent replays its state. Each further question of the upstream comes
  with a new state, so an elicitation of several rounds still works. The
  record of spent states is in memory, so each state also carries the random
  boot id of the process that sealed it, and whether its call was gated. A
  state that is gated now, or was when it was sealed, is taken only by the
  process that sealed it: after a restart, it is refused, and so is a state
  sealed before states carried a boot id. A state sealed gated stays
  taken-once even if the call is no longer gated when it resumes.
- **Arguments stay in memory.** The queue is in memory, behind one mutex. The
  arguments are never stored and never logged: the line of
  [0014](./0014-call-log). A daemon restart drops the queue, and the agent's
  retry pends again; a gated call's request state from before the restart is
  refused.
- **The gate sits before the upstream call.** It runs after the call is
  validated and before the connector is touched, so a parked call holds no
  upstream session.
- **Desk-bound in v1.** The owner must be at the machine, in the console or the
  CLI. A gated call waits until then, or expires. Notifications, a push channel
  that says an approval is waiting, are a follow-up, not part of this record.

## Considered options

- **Approval through elicitation or `input_required`.** The agent would get a
  form and the user would answer it. The answer comes through the channel the
  agent controls, and the gateway can't tell the user's `accept` from the
  agent's. A form invites an answer only the owner may give. Rejected.
- **A URL-mode link to approve.** The link would point at the owner's loopback
  console, which means nothing on the remote client's side, and a remote
  client can't open it. Rejected.
- **Gate every token, local ones too.** It would give a local agent with the
  default token a prompt it can answer itself. The protection would be a
  display, not a limit. Rejected; see the Threat model.
- **A separate ask-first meta-tool**, `call_tool_confirmed`, which the tool-policy
  plan named as the route if a per-tool prompt ever mattered. It would let the
  client's own prompt handle it, which is the channel that failed in Context,
  and it would amend [0001](./0001-two-meta-tools). Rejected for remote
  clients.
- **A standing approval**, such as "allow `slack.post_message` for 1 hour".
  The agent could then change what it sends while the approval is open. Binding
  to the arguments shown is the point; the cost is retry-to-spend.
- **Persist the queue.** It would survive a restart, but it would put arguments
  in the store, against [0014](./0014-call-log). A restart costs the agent one
  retry.
- **Key by token name.** A token created again under a revoked name would
  inherit the old approvals. The instance (name plus creation time) is the
  same distinction a call's sealed request state draws.
- **A sweep for expiry.** Simpler to describe, but it would run only with the
  call-log prune, hourly and off at retention `0s`. Checking on access needs no
  timer.
- **A shorter pending lifetime**, such as the 1 hour of the first draft. A
  desk-bound owner who is away for the afternoon would find every gated call
  expired, and the agent would pend again, and again. 24 hours trades a longer
  window for the owner's attention.

## Consequences

- A cloud agent works as before, except that it can't run a gated tool until
  the owner has seen its arguments and approved them. That holds however the
  agent is prompted, because the answer channel is loopback-only.
- It does not protect against a local agent: it can approve its own calls with
  the default token. This is why the gate is refused on tokens that aren't
  remote, and why the docs say so.
- It does not protect against a mistaken approval. The owner approves what they
  see, rendered from the real call, and can still approve a bad one.
- The arguments are shown in full and unredacted, in the console, the CLI, and
  `GET /admin/approvals`: a secret an agent puts in an argument is shown to the
  owner. The console and the CLI show them decoded, keys sorted: only
  invisible characters and runs of spaces are made visible, as escapes, and
  arguments that would lay out too large are shown on one line. The listing is
  as sensitive as the arguments, and only the owner can reach it, with the
  default token or a console session.
- It does not bind the upstream's effect. The approval covers one attempt at
  exactly these arguments; what the tool does with them is the upstream's.
- Gating by class trusts the upstream's annotations, as the tools list does
  ([0009](./0009-tool-policy-in-the-gateway)). Naming the tool is the exact
  form.
- An agent that rewrites its arguments on every try is never approved. It fills
  its 8 slots with entries the owner can deny, and then gets `queue_full`. A
  well-behaved agent retries with the same arguments.
- An approval is not reusable. An agent that makes the same call every hour
  needs the owner every hour.
- Pending approvals are visible in `wayfindr approvals`, the console, and
  `wayfindr status`, and bounded.
- A restart drops every entry, pending, granted, and denied. Each agent's next
  retry pends again, a denied call's included. A gated call waiting on an
  elicitation across the restart must start again too: its request state is
  refused.
- The call log shows each refusal as `rejected`, Kind `approval`, with the
  Case, and still no arguments. Once the queue has dropped an entry, the owner
  can see that an approval was asked for, not what it covered.
- The owner who is away can't approve. Until notifications exist, gated calls
  wait for the desk.
- The decision of the tool-policy plan to drop confirm-before-calling stands.
  It was about a gateway prompt for a user at a keyboard, which duplicates the
  client's own. Here nobody is at the keyboard, the owner answers out of band,
  and no gateway prompt reaches the client at all. Local clients keep their own
  prompt, and `call_tool` stays the only way to call.
