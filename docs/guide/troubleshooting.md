# Troubleshooting

Find your symptom in the table below, or start with `wayfindr status`. It asks
the running daemon about every connector and says what to do about each one that
needs attention:

```text
$ wayfindr status
CONNECTOR  STATE        TOOLS  LAST SYNC
broken     unreachable  0      never
fake       ready        1      2026-11-07T06:28:51Z
github     auth_error   0      never

broken: The connector's command could not be started.
  Check command in the connector's config: it must exist and be executable on the daemon's PATH.

github: The connector's config refers to secret:x, but no such secret is stored.
  Store the secret, or fix the reference in the connector's config.
```

Your agent gets the same text in the `unavailable` list of every `search_tools`
result. [`wayfindr status`](/reference/cli#wayfindr-status) in the CLI reference
explains every column and section.

## Debugging toolkit

| Command                             | Shows                                                                       |
| ----------------------------------- | --------------------------------------------------------------------------- |
| `wayfindr status`                   | Each connector's state and fix, warnings, and a config the daemon rejected. |
| `wayfindr logs -n 50`               | The daemon's last log lines, with the full reason for each state change.    |
| `wayfindr calls --errors`           | The agents' calls that didn't succeed, and why.                             |
| `wayfindr search <words> --explain` | What an agent's search returns, and why each tool ranks where it does.      |
| `wayfindr scope [connector]`        | What agents get in this directory, with every limit applied.                |

[Monitoring](./monitoring) shows each one with an example.

## Find your symptom

| Symptom                                                                                                             | See                                                                                  |
| ------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------ |
| ``run `wayfindr init` first``, master key permissions, a newer database schema, or an unsupported Postgres database | [First-run failures](#first-run-failures)                                            |
| `daemon is not running`, or the daemon won't start                                                                  | [The daemon is not running](#the-daemon-is-not-running)                              |
| `daemon already running`, or the port is in use                                                                     | ["daemon already running"](#daemon-already-running)                                  |
| `The running daemon is v…, not this command's v…`                                                                   | [The running daemon is another version](#the-running-daemon-is-another-version)      |
| You changed `server.listen` and nothing finds the daemon                                                            | [`server.listen` changed](#server-listen-changed-under-a-running-daemon)             |
| The client shows Wayfindr as failed, or can't find `wayfindr`                                                       | [The client can't find `wayfindr`](#the-client-can-t-find-wayfindr)                  |
| `401 unauthorized`, or an HTTP client asks you to sign in to Wayfindr                                               | [401 unauthorized](#_401-unauthorized)                                               |
| `403 forbidden origin`                                                                                              | [403 forbidden origin](#_403-forbidden-origin)                                       |
| The agent never calls `search_tools`                                                                                | [The agent ignores Wayfindr's tools](#the-agent-ignores-wayfindr-s-tools)            |
| A connector isn't `ready`                                                                                           | [Connector states](#connector-states)                                                |
| `Warnings:` in `wayfindr status`                                                                                    | [Warnings](#warnings)                                                                |
| `refers to secret:…` or `refers to env:…`                                                                           | [A missing secret](#a-missing-secret) or [variable](#a-missing-environment-variable) |
| A stdio connector works in your shell but not as a service                                                          | [A service doesn't see your environment](#a-service-doesn-t-see-your-environment)    |
| An OAuth or TLS error in `wayfindr status` or `wayfindr auth login`                                                 | [Troubleshooting OAuth and TLS](./troubleshooting-oauth)                             |
| `redirect_uri_mismatch`, or a Google connector asking you to sign in again                                          | [Troubleshooting OAuth and TLS](./troubleshooting-oauth)                             |
| A tool is missing from search, or the agent picks the wrong one                                                     | [Tools and search](#tools-and-search)                                                |
| `The call outlasted the connector's timeout.`                                                                       | [A call times out](#a-call-times-out)                                                |
| The agent says a tool call failed                                                                                   | [The agent says a tool call failed](#the-agent-says-a-tool-call-failed)              |
| `is hidden by …`, or `asks for the Wayfindr profile …`                                                              | [Scopes, profiles, and tokens](#scopes-profiles-and-tokens)                          |
| `needs your input, but your client …`                                                                               | [Questions from tools](#questions-from-tools)                                        |

## First-run failures

These stop `wayfindr start`, the shim, or a CLI command before anything is
connected. Each error names the cause:

| Error                                                                           | Cause and fix                                                                                                                                                                                                                                                                                                                                       |
| ------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| ``…: no such file or directory: run `wayfindr init` first``                     | There is no config, client token, or master key yet. Run `wayfindr init`; see the [quick start](./quick-start). The shim words it ``client token: …: run `wayfindr init` first``. If you did run it, check that the daemon, the CLI, and the shim resolve the same paths; see [Paths and environment](/reference/paths-and-environment#precedence). |
| `master key: master key file permissions are too open: … run: chmod 600 <path>` | `<data>/master.key` is readable by group or others. Run the `chmod 600` it names. See [Master key](/reference/paths-and-environment#master-key).                                                                                                                                                                                                    |
| `open store: …: database schema is newer than this binary`                      | An older binary found a store a newer one migrated. Upgrade the binary again. See [Upgrades and downgrades](./storage#upgrades-and-downgrades).                                                                                                                                                                                                     |
| `open store: open postgres store: unsupported postgres database: …`             | The Postgres database isn't UTF8, or the server lacks ICU. The message says which. See [Postgres](./storage#postgres).                                                                                                                                                                                                                              |

## Daemon and startup

### The daemon is not running

```text
Error: daemon is not running on 127.0.0.1:7777: start it with `wayfindr start`, or run it as a service with `wayfindr service install`
```

Start it either way. See [Running the daemon](./running). Clients that use
`wayfindr stdio` start it for you.

If `wayfindr start` or the shim couldn't start it, you see (in your client's MCP
log, for the shim) one of:

```text
the daemon exited before it answered http://127.0.0.1:7777/healthz; see <data>/wayfindr.log
the daemon did not answer http://127.0.0.1:7777/healthz within 5s; see <data>/wayfindr.log
```

Run `wayfindr logs -n 20` to see why. The usual reasons are a config error, a
missing `wayfindr init`, and a store that can't be opened; see
[First-run failures](#first-run-failures).

### "daemon already running"

```text
Error: daemon already running at 127.0.0.1:7777 (pid 98736): it holds /Users/you/.local/share/wayfindr/daemon.lock
```

Another daemon already uses this data directory, and there can be only one. Use
the running one, or stop it first with `wayfindr stop`. If you meant to run a
second, separate Wayfindr, give it its own data directory, config file, and
`server.listen`, with `--data-dir` and `--config` or `WAYFINDR_DATA_DIR` and
`WAYFINDR_CONFIG`. `wayfindr config path` shows which ones a command uses. See
[One daemon per data directory](./running#one-daemon-per-data-directory).

`127.0.0.1:7777 is in use: daemon already running?` means some other program
holds the port. Stop it or change `server.listen`.

### "does not hold the daemon lock"

```text
Error: something answers on 127.0.0.1:7777 but does not hold the daemon lock /Users/you/.local/share/wayfindr/daemon.lock: it is another program, or a daemon started with another data directory; stop it or change server.listen
```

Something answers on your `server.listen`, but it is not the daemon for your
data directory, so the CLI and the stdio shim send it nothing: not the client
token, and not a secret's value. It may be another program, a Wayfindr daemon
started with another `--data-dir`, or, on a shared machine, another user's
process. Find what holds the port (`lsof -i :7777` on macOS and Linux), stop it,
or give Wayfindr another `server.listen`.

### The running daemon is another version

After you upgrade or rebuild Wayfindr, a command may fail with:

```text
Error: the daemon could not load the new config and keeps running its last good one:
  load config …/wayfindr.toml: invalid config: connectors.gmail.auth.authorize_params: unknown key
The running daemon is v0.4.0, not this command's v0.5.0. Restart it with `wayfindr stop`, then `wayfindr start`; gmail is saved in the config file and loads then.
```

The command knows a key that the old daemon doesn't. Nothing needs redoing: the
connector is already in the config file, and the new daemon loads it when it
starts. A daemon run as a service starts again at the next login, or with
`wayfindr service install`.

### `server.listen` changed under a running daemon

The daemon applies `server.listen` only when it starts. After you edit it,
`wayfindr status` finds nothing at the new address, and the shim says
`daemon already running at …, but server.listen is now …: restart the daemon to
apply it`. Restart the daemon, then your stdio clients. See
[Changing the listen address](./running#changing-the-listen-address) for the
steps and for what else names the old address.

### A service doesn't see your environment

A daemon run by `wayfindr service install` gets only the variables copied when
you installed it; see
[What the login service sees](/reference/paths-and-environment#what-the-login-service-sees).
This shows up two ways:

- A connector that reads `${env:…}` goes to `auth_error`; see
  [A missing environment variable](#a-missing-environment-variable). Switch it
  to `${secret:…}`.
- A stdio connector goes to `unreachable` with
  `The connector's command could not be started.` because its command, such as
  `npx`, isn't on the `PATH` the unit saved. Run `wayfindr service install`
  again from a shell where `which npx` works, or give `command` an absolute
  path.

The same applies to a daemon the shim started: it has your client's environment,
which may differ from your shell's.

## The client can't connect

### The client can't find `wayfindr`

The client lists Wayfindr as failed or disconnected, and its MCP log says the
command was not found (`ENOENT`, `command not found`, or similar). Clients run
`wayfindr stdio` with their own `PATH`, which may not include the directory you
installed to, especially desktop apps started from the Dock or a launcher.

Give the client the absolute path instead, such as
`/Users/you/.local/bin/wayfindr`, as the command of its Wayfindr entry.
`which wayfindr` prints it. See
[Put it on your PATH](./installation#put-it-on-your-path).

### 401 unauthorized

The daemon answers `401 unauthorized` when a request to `/mcp` has no client
token, an empty one, or one that doesn't match. Some clients report it as an
OAuth or sign-in failure, since a 401 is how an MCP server asks for OAuth.

- **Over HTTP**, the client sends `WAYFINDR_CLIENT_TOKEN` from its own
  environment. Gemini CLI sends an empty token when the variable is unset. A
  desktop app started from the Dock doesn't see variables from your shell
  profile. Set it where the client starts, as in
  [Setting up HTTP](./clients#setting-up-http), or use the stdio shim, which
  reads the token itself.
- **After a rotation**, a client still holds the old value. Export the new one
  and restart the client. See
  [Rotating the client token](./running#rotating-the-client-token).
- **A scoped client token** stops working the moment you revoke it with
  `wayfindr token revoke`. Create a new one.
- **Through the shim**, a stale `WAYFINDR_CLIENT_TOKEN` in the client's
  environment wins over the token file. Unset it there.

To test a token, send the `initialize` request in [`/mcp`](/reference/http#mcp)
with `curl`: a `200` means the token works.

### 403 forbidden origin

The daemon refuses a request whose `Origin` header is present and not loopback
with `403 forbidden origin`. This keeps web pages in your browser away from the
daemon. A web-based client, or a client that sends a non-loopback `Origin`,
can't connect over HTTP; use a local client, or the stdio shim.

On an [auth link](./authentication#the-auth-link), the **Continue** button gets
the same `403` when the browser sends no `Origin` with the form, which some
privacy extensions strip. Allow it for `127.0.0.1`, or run
`wayfindr auth login <connector>` instead. See
[Host and Origin checks](/reference/http#host-and-origin-checks).

### The agent ignores Wayfindr's tools

The agent answers without calling `search_tools`, or says it has no tool for the
job.

1. Check that the client is connected and lists exactly two Wayfindr tools,
   `search_tools` and `call_tool`. In Claude Code, run `/mcp`.
2. Check that the tool is in reach: `wayfindr search <words>` returns what the
   agent's search would.
3. Name the tools in your prompt, such as "use wayfindr's search_tools to find a
   Linear tool", or add a line to the project's agent instructions (`CLAUDE.md`,
   `AGENTS.md`): "To reach outside services, find a tool with wayfindr's
   search_tools, then run it with call_tool."

If the client still runs the same servers directly, the agent may pick those
instead. See
[Remove the servers from the client](./connectors#remove-the-servers-from-the-client).

## Connector states

### Connector states

A connector is in exactly one state at a time.

| State         | Meaning                                                                                                                                                                                                                                                                                                       | How it's fixed                                                                                                                                                                 |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `starting`    | Connecting for the first time after startup, being added, or being reconfigured.                                                                                                                                                                                                                              | Resolves on its own. Its advice reads "The connector is still connecting."                                                                                                     |
| `ready`       | Connected and synced. Its tools can be called.                                                                                                                                                                                                                                                                | —                                                                                                                                                                              |
| `needs_auth`  | No usable OAuth token: never authorized, the refresh failed, the grant was revoked, the `url` or `client_id` changed, or the upstream needs more scopes.                                                                                                                                                      | Run `wayfindr auth login <connector>`, or open the auth link. The daemon reconnects it as soon as the token is stored.                                                         |
| `auth_error`  | Auth can't work as configured, a `${secret:…}` or `${env:…}` value is missing, an unattended grant was refused, the client authentication method isn't supported, the signing key or client certificate can't be used, the authorization server's TLS certificate isn't trusted, or it rejected a DPoP proof. | Change the config, or set the missing value. The daemon reconnects it as soon as you do. After fixing a refused grant at the provider, run `wayfindr auth logout <connector>`. |
| `unreachable` | The transport failed: the command didn't start, the connection was refused, or it timed out.                                                                                                                                                                                                                  | Wayfindr retries on the next call to one of its tools, and HTTP connectors on every poll.                                                                                      |

An `unreachable` connector that never connected has no tools in the catalog, so
nothing calls it. Fix its `url` with
[`wayfindr connector edit`](./connectors#fixing-a-connector), or its `command`
in the config; the change reconnects it at once.

For `needs_auth` and `auth_error` caused by OAuth or TLS, see
[Troubleshooting OAuth and TLS](./troubleshooting-oauth).

### Warnings

A warning says what will break and how to head it off. The daemon logs each one
once, at `warn`, when it starts to apply, and again only if it clears and comes
back. Your agent sees the OAuth and certificate warnings due within a day in the
`warnings` list of `search_tools`.

| Warning                                                                | Code                    | What to do                                                                                                                                                                                                                                      |
| ---------------------------------------------------------------------- | ----------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `The access token expires at … and cannot be refreshed.`               | `no_refresh`            | Authorize again before then (`wayfindr auth login <connector>` or the auth link). The provider issued no refresh token. For Google, see [Google asks you to sign in every hour](./troubleshooting-oauth#google-asks-you-to-sign-in-every-hour). |
| `The refresh token expires at ….`                                      | `refresh_expiring`      | Authorize again before then for a new refresh token.                                                                                                                                                                                            |
| `The OAuth client Wayfindr registered for the connector expires at ….` | `registration_expiring` | Run `wayfindr auth logout <connector>`, then authorize again. The revoke deletes the old client; the sign-in registers a new one.                                                                                                               |
| `The client certificate in [connectors.<name>.tls] expires at ….`      | `certificate_expiring`  | Replace `cert_file` and its key with a renewed certificate before then. Wayfindr presents it from the next connection, without a restart. Warned of 7 days ahead, for any HTTP connector with a client certificate, OAuth or not.               |
| `The connector has not been ready since ….`                            | `stuck`                 | Raised once a connector has been out of `ready` for more than 24 hours. Follow the connector's own advice above. If you no longer use it, remove it with `wayfindr connector remove`.                                                           |

Once the time has passed, the summary says `expired` and the warning stays until
you fix it. See
[Token refresh](./authentication#token-refresh).

### A missing secret

```text
github: The connector's config refers to secret:x, but no such secret is stored.
  Store the secret, or fix the reference in the connector's config.
```

Store it. The daemon reconnects every connector that was waiting for it and
prints how each one came out:

```text
$ wayfindr secret set x
Value for secret x:
Secret x set.
github is ready.
```

If the secret exists but can't be decrypted, the summary reads
`…but the stored secret cannot be decrypted, likely because the master key changed.`
Set it again with `wayfindr secret set <name>`. See [Secrets](./secrets).

### A missing environment variable

```text
exa: The connector's config refers to env:EXA_API_KEY, but the daemon's environment does not set it.
  Set the variable in the daemon's environment and restart it, or fix the reference.
```

The daemon reads its environment once, when it starts. Setting the variable in
your shell doesn't change a daemon that is already running. Set it where the
daemon starts, then restart it. Or store the value as a secret and reference
`${secret:name}`, which needs no restart.

## Tools and search

### A tool is missing from search

- Check that its connector is `ready` and its `TOOLS` count looks right. A count
  such as `38 (4 hidden)` means the connector's
  [tools list](./access/tools-lists) hides some tools;
  `wayfindr connector tools <connector>` shows which.
- Look at `LAST SYNC`. HTTP connectors re-sync every `refresh_interval` (default
  `1h`), or sooner when the upstream announces a change. Stdio connectors
  re-sync each time their process starts.
- Search with the tool's own name: `wayfindr search <name>`. If that finds it,
  try other words, or turn on [embeddings](./embeddings).
- Run `wayfindr scope` where the agent works: a project scope or profile may
  hide it there.

To re-sync a connector now, restart the daemon. See
[The catalog](./search#the-catalog) for when syncs happen.

### The agent picks the wrong tool

Run the agent's query yourself with `--explain`:

```sh
wayfindr search "open a ticket" --explain
```

- If the header says `Embeddings: failed (…)`, the provider is failing now, and
  agents' searches fall back to lexical results while it does. Fix the provider;
  the reason says what went wrong. See [Embeddings](./embeddings).
- If the tool you wanted has `-` under LEXICAL, none of the query's words are in
  its name, title, or descriptions. Turn on [embeddings](./embeddings), or
  expect the agent to need other words.
- If it is on page 2, the agent may not have looked there. Check its connector
  is `ready`: tools of a connector under `Unavailable:` still rank, but calling
  them fails until you fix it.

See
[Why a tool ranks where it does](./monitoring#why-a-tool-ranks-where-it-does)
for how to read the columns.

### A call times out

```text
The call outlasted the connector's timeout.
To fix: Try again, or raise timeout in the connector's config if the tool is slow.
```

Each connector's `timeout` bounds one call, `2m` by default. `wayfindr calls`
shows these as `failed: timeout`. For a slow tool, raise it:

```sh
wayfindr connector edit slow-reports --timeout 10m
```

Your client may also limit how long a tool call can take; check its MCP settings
if calls stop sooner. See [Timeouts](./connectors#timeouts).

### The agent says a tool call failed

List the calls that didn't succeed:

```sh
wayfindr calls --errors
```

RESULT says how each call ended, and the reason after the colon says why:

| RESULT                                                                                           | What happened                                                                                                                     | Fix                                                                                                    |
| ------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------ |
| `tool_error`                                                                                     | The upstream ran the tool and returned its own error.                                                                             | Read the error in the agent's transcript. Wayfindr never logs results, so it doesn't have the message. |
| `rejected: …`                                                                                    | Wayfindr refused the call without calling the upstream: an unknown tool, arguments that don't match its schema, or a hidden tool. | See [Scopes, profiles, and tokens](#scopes-profiles-and-tokens).                                       |
| `failed: timeout`                                                                                | The call outlasted the connector's timeout.                                                                                       | See [A call times out](#a-call-times-out).                                                             |
| `failed: unreachable`, `connect_refused`, `connect_timeout`, `spawn_failed`, or `auth_server`    | Wayfindr couldn't reach the upstream or start its command.                                                                        | Run `wayfindr status`; see [Connector states](#connector-states).                                      |
| `failed: needs_auth`, `no_credential`, `refresh_failed`, `token_revoked`, or another auth reason | The connector has no usable credential, or its auth setup is wrong.                                                               | Run `wayfindr status`; see [OAuth errors](./troubleshooting-oauth#oauth-errors).                       |
| `failed: secret` or `failed: env`                                                                | The connector's config refers to a secret or variable that isn't set.                                                             | See [A missing secret](#a-missing-secret) or [variable](#a-missing-environment-variable).              |
| `failed: rejected`                                                                               | The upstream refused the call with a protocol error, such as bad arguments. Not the same as the `rejected` result.                | Read the error in the agent's transcript.                                                              |
| `failed: no_client`, `client_too_old`, `no_elicitation`, `sampling`, or `concurrent_calls`       | The tool asked a question that couldn't reach you.                                                                                | See [Questions from tools](#questions-from-tools).                                                     |
| `failed: canceled`                                                                               | The client gave up before the call finished, or the daemon stopped while it ran.                                                  | Check your client's tool-call timeout.                                                                 |
| `failed: catalog`, `upstream`, or `internal`                                                     | Wayfindr itself failed.                                                                                                           | Run `wayfindr logs`.                                                                                   |

[`wayfindr calls`](/reference/cli#wayfindr-calls) lists every result.

## Scopes, profiles, and tokens

[Control what agents can use](./access/#what-agents-see) lists each refusal, the
layer behind it, and how `wayfindr calls` shows it.

### "is hidden by the tools list"

The connector's [tools list](./access/tools-lists) hides the tool the agent
called. If the agent should use it, add its name to the list; the daemon applies
the change when you save. `wayfindr connector tools <connector>` shows every
tool and whether the list exposes it.

### "is hidden by this project's Wayfindr scope"

A [project scope](./access/project-scopes), the `.wayfindr.toml` in the project
or a parent directory, hides the tool or its connector. Run `wayfindr scope` in
the project to see which file applies and what it allows, and
`wayfindr scope <connector>` for one connector's tools. To let agents use the
tool there, add it to the file, then reconnect your client to Wayfindr.

### "is hidden by the Wayfindr profile"

The [profile](./access/profiles) the client names, with
`wayfindr stdio --profile` or `?profile=` on its URL, hides the tool or its
connector. Run `wayfindr scope --profile <name>` to see what the profile allows,
and `wayfindr scope <connector> --profile <name>` for one connector's tools. To
let agents use the tool, allow it in `[profiles.<name>]` in your config. The
daemon applies the change when you save; the client doesn't need to reconnect.

### "asks for the Wayfindr profile"

The client names a profile your config doesn't have, so Wayfindr refuses every
request from it rather than give it everything. The name may be misspelled in
the client's config, or the profile may be missing from yours, for example after
you cloned a project whose committed `.mcp.json` names a teammate's profile.
`wayfindr scope --profile <name>` lists the profiles your config defines. Fix
the name in the client's config, or add the profile to yours.

If the daemon couldn't load your last edit, it is still running the config from
before it, which may not have the profile yet. `wayfindr status` shows the error
under **Config error**. A profile that names a connector your config doesn't
have is one such error.

A client that names its profile twice, such as `--profile` and `?profile=`
together, or with a name that isn't valid, is refused the same way with
`names its Wayfindr profile more than once, or with a name that isn't valid`.
Name it once. A name is lowercase letters, digits, `_`, and `-`, starts with a
letter or digit, and is at most 63 characters. On an HTTP client, check the
whole URL: a query Wayfindr can't read, such as one with a `;` or a stray `%`,
or the key spelled `Profile`, is refused the same way, since Wayfindr can't tell
which profile it meant.

### Wayfindr fails to start in one project

If your client shows Wayfindr as failed only in some projects, the project's
`.wayfindr.toml` is probably invalid: `wayfindr stdio` refuses to start rather
than run without the limits the file asks for. Run `wayfindr scope` in the
project; it prints the same error, with the key that's wrong.

### "no connector is called …" with a scoped token

```text
There is no tool called gmail.send: no connector is called gmail.
```

A client with a [scoped client token](./access/scoped-tokens) sees only the
token's connectors; any other is as if it weren't configured.
`wayfindr token list` shows each token's connectors. A token's connectors can't
be changed: revoke it and create another.

### "This client token cannot use the admin API"

A CLI command found a scoped client token in `WAYFINDR_CLIENT_TOKEN`. Only the
default client token can use the commands that talk to the daemon. Unset the
variable, or run the command where the default token applies.

## Questions from tools

Some tools ask you something during a call; see
[When a tool asks a question](./clients#when-a-tool-asks-a-question). When the
question can't reach you, the call fails with one of these, and the tool is told
you cancelled:

| Error                                                                                                                            | Fix                                                                                                                                                  |
| -------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `linear.create_issue needs your input, but your client's MCP version (2025-11-25) can't ask you.`                                | Over HTTP, only MCP 2026-07-28 clients can be asked. Update the client, or connect it with `wayfindr stdio`, which can ask clients of every version. |
| `linear.create_issue needs your input, but your client does not support elicitation.`                                            | Use a client that supports elicitation, or connect it with `wayfindr stdio`.                                                                         |
| `linear.create_issue needs your input, but your client can't ask you.`                                                           | Connect the client with `wayfindr stdio`.                                                                                                            |
| `The answer arrived too late or does not belong to this call.`                                                                   | More than 10 minutes passed, or the answer came back to the wrong call. Call the tool again.                                                         |
| `The upstream asked for input while another call to it was running, so Wayfindr could not tell which call the question was for.` | An older upstream asked during parallel calls. Call the tool again once the connector's other calls have finished.                                   |
| `This tool asks the client to sample a message with its model, which Wayfindr does not support.`                                 | Use another tool, or run this one in a client connected to the upstream directly.                                                                    |

[Compatibility](/reference/compatibility#questions-from-upstreams-elicitation)
lists which clients can be asked, and the
[meta-tools reference](/reference/meta-tools#call-tool-errors-when-the-upstream-asks-a-question)
has the exact results.
