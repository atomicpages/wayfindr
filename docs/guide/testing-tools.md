# Testing a tool before your agent uses it

`wayfindr status` says a connector is `ready`, but a wrong API key header or a
half-working sign-in can still fail the first real call, deep inside an agent's
task. Run that call yourself first. It goes through the same checks, narrowing,
and call log as an agent's `call_tool`.

```sh
wayfindr call linear.search_issues --args '{"query":"login bug"}'
```

```text
ok · 0.8s · 1 KiB
[{"id":"ENG-412","title":"Login fails on Safari"}, …]
```

You can also run it from the [web console](./console): open a connector, then
**Try it** beside a tool.

## Pick a tool

List what the connector offers, with each tool's class and how many arguments it
requires:

```sh
wayfindr connector tools linear
```

```text
TOOL           CLASS  EXPOSED  REQUIRED ARGS
create_issue   write  yes      2
search_issues  read   yes      1

2 of 2 tools exposed.
```

Start with a `read` tool: it runs at once and changes nothing. You can only
call a tool the connector's [tools list](./access/tools-lists) exposes. Wayfindr
refuses a hidden one, as it does for an agent.

## Fill in the arguments

Read what the tool takes:

```sh
wayfindr connector tools linear create_issue
```

```text
linear.create_issue (write)

  NAME    TYPE      REQUIRED  DESCRIPTION
  team    string    required  The team's key.
  title   string    required
  labels  string[]            Label names.
```

Then ask for a starting point. `--scaffold` prints the required arguments as
JSON, and nothing else:

```sh
wayfindr call linear.create_issue --scaffold > args.json
```

Fill in `args.json`, then pass it back:

```sh
wayfindr call linear.create_issue --args-file args.json
```

```text
linear.create_issue is a write tool. Run it? [y/N] y
ok · 1.4s · 231 B
Issue ENG-481 created.
```

A `write` or `destructive` tool asks first. In a script, pass `--yes`; without
it, the command fails before it sends anything.

## Prove a sign-in works

Right after [`wayfindr auth login`](./authentication#authorizing-a-connector),
call a `read` tool. An `ok` result proves the stored credential works against
the real server. If the call fails, the error is the same one an agent would
get, with what to do about it, and the call is in `wayfindr calls`. That holds
even when the sign-in is so broken that Wayfindr can't list the connector's
tools: the call still goes out, and since the tool's class is then unknown,
`wayfindr call` asks first, as for a destructive tool.

## See what a scoped token's agent sees

Run the call as a [scoped client token](./access/scoped-tokens) to check what
that token allows. Keep the token in a file, never on the command line, where
`ps` and your shell history would see it:

```sh
wayfindr call github.list_repos --token-file ./nightly.token
```

A connector the token leaves out is refused as if it weren't configured. The
call log names the token.

## When the server asks for more

Some tools ask for input mid-call. Wayfindr asks you on the terminal: a form
field by field, or a link to open, after which you press Enter. With `--json`,
the command prints the question and leaves it to you.

## Read the whole result

The output stops at 4 KiB of text. Images and other content show as one line,
such as `image, 48 KiB`. Write the whole result to a file with
`--output result.json`, or print the daemon's full response with `--json`.

See [`wayfindr call`](/reference/cli#wayfindr-call) for every flag and message.
