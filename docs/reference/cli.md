# CLI

Every `wayfindr` command, flag, and message. For tasks, start with the
[Quick Start](../guide/quick-start).

```sh
wayfindr init                 # once: config, master key, store, client token
wayfindr connector add linear --url https://mcp.linear.app/mcp
wayfindr start                # start the daemon in the background
wayfindr start --foreground   # or run it in this terminal
wayfindr status               # what each connector is doing
wayfindr auth login linear    # authorize an OAuth connector
```

## Commands

| Command                                                            | What it does                                                                         | Needs a running daemon, and the request timeout        |
| ------------------------------------------------------------------ | ------------------------------------------------------------------------------------ | ------------------------------------------------------ |
| [`wayfindr init`](#wayfindr-init)                                  | Create the config, master key, local user, and client token.                         | No; refuses if one is running                          |
| [`wayfindr start`](#wayfindr-start)                                | Start the daemon in the background, or with `-f` run it here.                        | No                                                     |
| [`wayfindr stop`](#wayfindr-stop)                                  | Stop the running daemon.                                                             | Stops one                                              |
| [`wayfindr logs`](#wayfindr-logs)                                  | Print the log of a daemon that runs in the background.                               | No                                                     |
| [`wayfindr stdio`](#wayfindr-stdio)                                | The stdio shim for MCP clients. Starts the daemon if needed.                         | Starts one                                             |
| [`wayfindr status`](#wayfindr-status)                              | Show each connector's state, tools, and last sync.                                   | Yes, 10 s                                              |
| [`wayfindr calls`](#wayfindr-calls)                                | Show the tools agents called, newest first.                                          | Yes, 10 s                                              |
| [`wayfindr search <query>`](#wayfindr-search)                      | Search the catalog as an agent would, and show why.                                  | Yes, 30 s                                              |
| [`wayfindr call <connector.tool>`](#wayfindr-call)                 | Run one real tool call through the daemon, as an agent would.                        | Yes, 30 s to list the tool, then the connector's       |
| [`wayfindr ui`](#wayfindr-ui)                                      | Open the web console in the browser, signed in.                                      | Yes, 10 s                                              |
| [`wayfindr scope [connector]`](#wayfindr-scope)                    | Show the project scope that applies here and what it exposes.                        | Yes, 30 s, if a scope file applies or with `--profile` |
| [`wayfindr connector add <name>`](#wayfindr-connector-add)         | Add a connector to the config file and report it.                                    | Starts one, unless `--no-wait`                         |
| [`wayfindr connector list`](#wayfindr-connector-list)              | List the connectors in the config file: kind, URL or command, and auth.              | No; reads the config file only                         |
| [`wayfindr connector edit <name>`](#wayfindr-connector-edit)       | Change keys of a connector in the config file and report it.                         | Starts one, unless the edit changes nothing            |
| [`wayfindr connector import <client>`](#wayfindr-connector-import) | Add the MCP servers a client is set up with as connectors.                           | Starts one, unless `--no-wait`                         |
| [`wayfindr connector remove <name>`](#wayfindr-connector-remove)   | Remove a connector and what it owned: its credential and secrets. Asks first.        | Starts one, if there is something to delete            |
| [`wayfindr connector tools <name> [tool]`](#wayfindr-connector-tools) | List a connector's tools, or describe one tool's arguments.                       | Yes, 30 s                                              |
| [`wayfindr auth login <connector>`](#wayfindr-auth-login)          | Run a connector's auth flow, in the browser or with a device code.                   | Yes, 60 s to start, then it waits for the flow         |
| [`wayfindr auth logout <connector>`](#wayfindr-auth-logout)        | Delete a connector's stored OAuth credential, or forget an unattended grant's token. | Yes, 90 s                                              |
| [`wayfindr secret set <name>`](#wayfindr-secret-set)               | Store a secret for `${secret:name}` references.                                      | Yes, 90 s                                              |
| [`wayfindr secret list`](#wayfindr-secret-list)                    | List stored secrets and what refers to each, never their values.                     | Yes, 10 s                                              |
| [`wayfindr secret remove <name>`](#wayfindr-secret-remove)         | Delete a stored secret.                                                              | Yes, 90 s                                              |
| [`wayfindr token create <name>`](#wayfindr-token-create)           | Create a scoped client token and print it once.                                      | Yes, 10 s                                              |
| [`wayfindr token list`](#wayfindr-token-list)                      | List scoped client tokens and OAuth grants, never their values. Alias: `token ls`.   | Yes, 10 s                                              |
| [`wayfindr token revoke <name>`](#wayfindr-token-revoke)           | Delete a scoped client token or an OAuth grant.                                      | Yes, 10 s                                              |
| [`wayfindr config path`](#wayfindr-config-path)                    | Print the config file and data directory every command uses.                         | No                                                     |
| [`wayfindr config check`](#wayfindr-config-check)                  | Check the config file as the daemon would load it.                                   | No; reads the config file only                         |
| [`wayfindr service install`](#wayfindr-service-install)            | Run the daemon at login (launchd or systemd).                                        | No                                                     |
| [`wayfindr service uninstall`](#wayfindr-service-uninstall)        | Stop and remove the login service.                                                   | No                                                     |
| [`wayfindr update [version]`](#wayfindr-update)                    | Install the latest release, or the one you name, and restart the daemon.             | No; restarts one that runs, unless `--no-restart`      |
| [`wayfindr version`](#wayfindr-version)                            | Print the version, commit, and Go version.                                           | No                                                     |
| [`wayfindr completion <shell>`](#wayfindr-completion)              | Print a shell completion script.                                                     | No                                                     |
| `wayfindr help [command]`                                          | Print help for a command. Same as `--help`.                                          | No                                                     |

`wayfindr --help` lists the commands in four groups: **Daemon** (`init`,
`start`, `stop`, `logs`, `service`, `update`), **Inspect** (`status`, `calls`, `search`,
`call`, `scope`, `ui`), **Manage** (`connector`, `auth`, `secret`, `token`,
`config`), and **Plumbing** (`stdio`, `completion`, `version`, `help`).

## Global flags

These go before or after the command, as in `wayfindr --json status` or
`wayfindr status --json`.

| Flag                  | Default | Description                                                                                                                                                                                                                                                                                                   |
| --------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--config <path>`     | none    | The [config file](./paths-and-environment#config-file). Wins over `WAYFINDR_CONFIG`.                                                                                                                                                                                                                          |
| `--data-dir <path>`   | none    | The [data directory](./paths-and-environment#data-directory). Wins over `WAYFINDR_DATA_DIR`.                                                                                                                                                                                                                  |
| `--log-level <level>` | `info`  | Log level: `trace`, `debug`, `info`, `warn`, `error`, `fatal`, `panic`, or `disabled`. Case-insensitive; numbers are not accepted.                                                                                                                                                                            |
| `--json`              | `false` | Print the result as JSON instead of text. Only the commands that print data take it: `status`, `calls`, `search`, `call`, `scope`, `connector list`, `connector tools`, `secret list`, `token list`, `config path`, `version`, and `update --check`. Each one's section says what it prints. Any other command fails. |
| `-h`, `--help`        |         | Print the command's help and exit.                                                                                                                                                                                                                                                                            |

`--config` and `--data-dir` act as if `WAYFINDR_CONFIG` and `WAYFINDR_DATA_DIR`
were set to them. A relative path is taken from the working directory. The
daemon that `start`, `stdio`, and `connector` start in the background, and the
unit that `service install` writes, get the same paths. Run
[`wayfindr config path`](#wayfindr-config-path) to see which files a command
uses.

`--log-file` is not global: only [`start`](#wayfindr-start) has it. A command
without a flag table below takes no flags besides these, and a command takes
only the arguments its row in the table above shows.

## Output and exit codes

- Results go to **stdout**.
- Logs go to **stderr** as JSON lines, one object per event, at `--log-level`.
  For example, `init` logs
  `{"level":"info","path":"…/client-token","time":"…","message":"client token written"}`.
  Use `--log-level disabled` to silence them.
- Errors go to **stderr** as `Error: <message>`, without the usage text.
- The exit code is `0` on success and `1` on any error, including a bad flag, a
  wrong number of arguments, and an unknown command or subcommand.
  [`status --check`](#wayfindr-status) also exits `2`, without an `Error:`
  line, when a connector or the config needs attention, and
  [`update --check`](#wayfindr-update) does when a newer release exists.
  [`call`](#wayfindr-call) exits `1` without an `Error:` line when the tool
  answered with an error, since it has printed the error. An unknown
  command also prints a suggestion when one is close, then
  `Run 'wayfindr --help' for usage.` (for a subcommand, the group's own
  `--help`, such as `wayfindr connector --help`).
- `--json` on a command that doesn't print data, such as `stop` or
  `connector add`, fails before the command does anything:
  `--json is not supported by "wayfindr stop"`.
- `connector`, `auth`, `secret`, `service`, `token`, or `config` on its own
  prints that group's help and exits `0`. An unknown subcommand of `auth` is taken as a
  connector to [log in to](#wayfindr-auth), not an error.
- Times are printed in UTC, RFC 3339 (`2026-11-07T06:22:06Z`).

### Finding the daemon

`status`, `calls`, `search`, `call`, `scope` (when a scope file applies, or with
`--profile`), `connector tools`, `auth login`, `auth logout`, `secret set`,
`secret list`, `secret remove`, `token`, and `connector add`, `edit`, `import`,
and `remove` once they have started it talk to the running daemon. Each reads
`[server] listen` from the config file and the
[client token](./paths-and-environment#client-token), then calls the daemon's
admin API on that address. The admin API takes only the default client token.

| Problem                                               | Error                                                                                                                                                                   |
| ----------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Nothing listens on the address                        | ``daemon is not running on 127.0.0.1:7777: start it with `wayfindr start`, or run it as a service with `wayfindr service install` ``                                    |
| No client token                                       | ``client token: read client token …/client-token: open …: no such file or directory: run `wayfindr init` first``                                                        |
| The config file doesn't parse, or `listen` is invalid | The parse error, then `Fix the config file <path>.` If a daemon answers on `127.0.0.1:7777`, the message also says it is still running there with its last good config. |
| No config file                                        | `read config: open <path>: no such file or directory`                                                                                                                   |
| The daemon answered with an error                     | The daemon's sentence alone, such as `No connector named "nope" is configured.`                                                                                         |
| `WAYFINDR_CLIENT_TOKEN` holds a scoped client token   | `This client token cannot use the admin API.` Unset the variable, or run the command where the default token applies.                                                   |

Only `[server] listen` is read, so a config that the daemon rejected for another
reason still lets `status` show why.

## `wayfindr init`

Write a default config if there is none, create the master key, run the store
migrations, create the local user, and write the client token, then print the
stdio command or config entry that connects each supported harness (Claude Code,
Codex, OpenCode, Hermes Agent, Gemini CLI, GitHub Copilot CLI, VS Code, Cursor,
Zed, Windsurf (Devin Desktop), and goose).

Init never overwrites anything, so running it again changes nothing. It refuses
to run while a daemon is listening.

What it does, in order:

1. Reads the config file. If there is none, it uses the default config (written
   in step 3). Either way it fully validates the config, so `init` is also a
   quick config check.
2. Checks for a daemon on `[server] listen` and stops if one answers.
3. Writes the default config (mode `0600`) if there was none. On a terminal it
   first asks whether to send the maintainers the anonymous daily
   [usage report](./telemetry) (`[y/N]`, default no); yes writes the config with
   `[telemetry] enabled = true`. It doesn't ask when stdin isn't a terminal, when
   `DO_NOT_TRACK` is set, or when it keeps an existing config.
4. Creates the data directory (`0700`), opens the store, and runs migrations.
5. Creates the local user if the store has none.
6. Finds the master key in `WAYFINDR_SECRET_KEY`, the OS keychain, or
   `<data>/master.key`. If there is none, it creates one in the keychain, or in
   `<data>/master.key` (`0600`) when no keychain is available. It never writes
   the environment variable.
7. Writes `<data>/client-token` (`0600`) if it doesn't exist.

**Prints:**

```
Wayfindr is set up.

  Config        /home/you/.config/wayfindr/wayfindr.toml (created)
  Data dir      /home/you/.local/share/wayfindr
  Master key    OS keychain
  Client token  /home/you/.local/share/wayfindr/client-token (created)

Add a connector
---------------

  wayfindr connector add linear --url https://mcp.linear.app/mcp
  wayfindr connector add fs -- npx -y @modelcontextprotocol/server-filesystem ~/src

The command asks an HTTP server whether it uses OAuth and offers to
authorize it. wayfindr status shows every connector's state.

Connect a client
----------------
…
```

`(created)` becomes `(kept)` for files that already existed. **Master key** is
`WAYFINDR_SECRET_KEY`, `OS keychain`, or `key file <path>`. When
`WAYFINDR_CLIENT_TOKEN` is set, a line under the token says it takes precedence
over the file. Then come two [`connector add`](#wayfindr-connector-add)
examples. If Claude Code, Claude Desktop, Cursor, Codex, or VS Code has a
user-level MCP config, a `wayfindr connector import <client> --dry-run` line
follows for each (see [`connector import`](#wayfindr-connector-import)). Then
come the stdio command or config entry for each harness, and one block for
connecting over HTTP at your `listen` address: the line
`export WAYFINDR_CLIENT_TOKEN="$(cat '<data>/client-token')"` and a link to
[Clients](../guide/clients). The output ends with a note that
[usage telemetry](./telemetry) is on, if the config turns it on, and how to turn it off,
or else that it is off by default and how to turn it on. Neither
the token nor the key is ever printed.

**Errors:**

| Situation                                          | Message                                                                                                                                               |
| -------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| A daemon is listening                              | `a daemon is listening on 127.0.0.1:7777: stop it before running init`                                                                                |
| The config is invalid                              | `config <path>: invalid config: …`                                                                                                                    |
| The store holds encrypted data but no key is found | `master key: sealed data exists but no master key was found: restore the original key via WAYFINDR_SECRET_KEY, the OS keychain, or <data>/master.key` |
| `master.key` is readable by group or others        | `master key file permissions are too open: … run: chmod 600 <path>`                                                                                   |

## `wayfindr start`

Start the daemon in the background, unless one already answers, and return once
it answers. The daemon connects every configured connector, keeps the catalog in
sync, and serves MCP clients on the configured loopback address until you run
[`wayfindr stop`](#wayfindr-stop). Run `wayfindr init` first.

| Flag                 | Default                                                             | Description                                                                           |
| -------------------- | ------------------------------------------------------------------- | ------------------------------------------------------------------------------------- |
| `-f`, `--foreground` | off                                                                 | Run the daemon in this process until SIGINT or SIGTERM, instead of in the background. |
| `--log-file <path>`  | `<data>/wayfindr.log` in the background, stderr with `--foreground` | Append the JSON log to this file (created with mode `0600`) instead of the default.   |

`wayfindr serve` is the old name of `start --foreground` and still works,
hidden, for service files and containers set up by older versions; `serve -d` is
`start`. It is deprecated: it logs a warning to stderr,
`serve is deprecated: use wayfindr start --foreground, or wayfindr start for serve -d`,
then runs as before.

### In the background

```text
$ wayfindr start
Daemon running at http://127.0.0.1:7777 (pid 4242)
Log: /Users/you/.local/share/wayfindr/wayfindr.log
```

Starts the daemon in the background, the way the [stdio shim](#wayfindr-stdio)
does, and exits `0` once `GET /healthz` answers. The daemon inherits your
shell's environment, so `${env:…}` references resolve as they do in the
foreground. Its output is appended to `<data>/wayfindr.log` (`0600`), or to
`--log-file` if you give one. `--log-level`, if you set it, is passed on to the
daemon. Read the log with [`wayfindr logs`](#wayfindr-logs) and stop the daemon
with [`wayfindr stop`](#wayfindr-stop).

If a daemon already answers, nothing is started and it exits `0`:

```text
Daemon already running at http://127.0.0.1:7777 (pid 4242)
```

| Situation                          | Message                                                                                                                        |
| ---------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| No config file                     | ``read config: …: no such file or directory: run `wayfindr init` first``                                                       |
| The daemon exited while starting   | `the daemon exited before it answered http://127.0.0.1:7777/healthz; see <data>/wayfindr.log`                                  |
| The daemon didn't come up          | `the daemon did not answer http://127.0.0.1:7777/healthz within 5s; see <data>/wayfindr.log`                                   |
| A daemon runs with an old `listen` | `daemon already running at 127.0.0.1:7777 (pid 4242), but server.listen is now 127.0.0.1:7778: restart the daemon to apply it` |

### `--foreground`

Runs the daemon in this process, with no output on stdout. This is the form for
service managers, containers, and supervisors. The JSON log goes to stderr, or
to `--log-file`. It starts with `daemon starting` and `daemon listening`, then
one `connector state changed` event per connector as it connects.

**Exit:**

- On SIGINT or SIGTERM the daemon stops accepting connections, waits up to 10
  seconds for requests in flight, closes its connectors and the store, and exits
  `0`.
- When another daemon already runs on this data directory, it logs
  `daemon not started: another one runs on this data directory`, with the
  reason `daemon already running at 127.0.0.1:7777 (pid 4242): it holds <data>/daemon.lock`,
  and exits `0`, as `wayfindr start` does. So a login service never restarts
  it in a loop. If that daemon was started with a different `listen`, the
  reason ends `; server.listen is now <new>: restart that daemon to apply it`,
  and the line is a warning.
- It exits `1` if startup fails:

| Situation                        | Message                                                                                                                                                                                                      |
| -------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| The config is invalid            | `load config <path>: invalid config: …`                                                                                                                                                                      |
| Something else holds the port    | `127.0.0.1:7777 is in use: daemon already running? …`                                                                                                                                                        |
| The bound address isn't loopback | `listen …: bound to …: listen address is not loopback`                                                                                                                                                       |
| No master key                    | ``master key: no master key: set WAYFINDR_SECRET_KEY, store one in the OS keychain (service "wayfindr", account "master-key"), or create <data>/master.key with `wayfindr init`: run `wayfindr init` first`` |
| No client token                  | ``client token: …: run `wayfindr init` first``                                                                                                                                                               |

See [Running the Daemon](../guide/running).

## `wayfindr stop`

Stop the daemon that runs on this data directory, however it was started:
`start`, `start --foreground`, the stdio shim, or the login service. It finds
the daemon through the [daemon lock](./paths-and-environment#daemon-lock), so it
also finds one still listening on an old `listen` address. It sends `SIGTERM`,
so the daemon finishes requests in flight as it does on Ctrl-C, and waits up to
15 seconds for it to exit. On Windows, which has no `SIGTERM`, the daemon is
ended at once.

```text
$ wayfindr stop
Stopped the daemon (pid 4242)
```

A daemon run as a [login service](#wayfindr-service-install) exits cleanly, so
the service leaves it stopped until your next login or
`wayfindr service install`.

| Situation                    | Message                                                                         |
| ---------------------------- | ------------------------------------------------------------------------------- |
| No daemon runs               | `daemon is not running`                                                         |
| The daemon is still starting | `the daemon is starting and has not written its pid yet: try again in a moment` |
| It didn't exit in time       | `the daemon (pid 4242) did not stop within 15s`                                 |

## `wayfindr logs`

Print `<data>/wayfindr.log`, the JSON log of a daemon that runs in the
background: one started with `start` (without `--log-file`), by the stdio shim,
or by the login service. Lines are printed as they are, one JSON object each,
ready for `jq`.

| Flag               | Default | Description                                                                               |
| ------------------ | ------- | ----------------------------------------------------------------------------------------- |
| `-f`, `--follow`   | off     | Keep printing new lines until Ctrl-C. A log that is truncated is read again from the top. |
| `-n`, `--tail <N>` | all     | Print only the last `N` lines.                                                            |

```sh
wayfindr logs -n 20               # the last 20 lines
wayfindr logs -f | jq -r .message  # follow, messages only
```

| Situation  | Message                                                                                                                         |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------- |
| No log yet | ``no log at <data>/wayfindr.log: a daemon logs there when started with `wayfindr start`, by `wayfindr stdio`, or as a service`` |

## `wayfindr stdio`

Serve MCP on stdin and stdout for clients that only speak stdio, forwarding
`tools/list` and `tools/call` to the daemon. If the daemon is not running, start
it in the background first. Logs go to stderr; a daemon started this way logs to
`wayfindr.log` in the data directory.

Your MCP client runs this command; you don't run it by hand.

| Flag               | Default | Description                                                                                                                                                      |
| ------------------ | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--profile <name>` | none    | Use the [profile](../guide/access/profiles) `[profiles.<name>]` from the config. Every request carries the name, and the daemon applies the profile it has then. |

How it starts:

1. Reads the client token and `[server] listen`. A missing token fails at once:
   ``client token: …: run `wayfindr init` first``.
2. Checks `GET /healthz`. If the daemon doesn't answer and no daemon holds the
   [daemon lock](./paths-and-environment#daemon-lock), it starts the daemon in
   the background, as `wayfindr start` does, with its output appended to
   `<data>/wayfindr.log` (`0600`), and waits up to 5 seconds for `/healthz`. If
   the lock is held, it waits for that daemon instead of starting another.
3. Connects to `/mcp` before it serves, so a daemon that won't start fails the
   shim on stderr instead of on the first call.

Before it starts the daemon, it looks for a
[project scope](../guide/access/project-scopes): the nearest `.wayfindr.toml` in
its working directory or a parent, below your home directory. When one applies,
it logs `project scope applies` with the file, and every request carries the
scope. An invalid file stops the shim with the error
[`wayfindr scope`](#wayfindr-scope) prints.

With `--profile`, the shim checks only that the name is valid; it doesn't read
the profile. The daemon looks the name up in its running config on each request,
so an edit to the profile applies without restarting the shim. A name the config
doesn't define makes every request fail with `unknown_profile`. A profile and a
scope file both apply when both are set.

It fails with the daemon-start errors of [`start`](#in-the-background) (the
daemon exited, didn't come up, or runs with an old `listen`), and with:

| Situation                         | Message                                                            |
| --------------------------------- | ------------------------------------------------------------------ |
| The project scope file is invalid | `<dir>/.wayfindr.toml: invalid project scope: …`                   |
| `--profile` isn't a valid name    | `profile "Acme": profile name must match [a-z0-9][a-z0-9_-]{0,62}` |

While it runs, results pass through unchanged. If the daemon goes away, a
`tools/call` returns an `isError` result (and `tools/list` a JSON-RPC error)
ending with
``To fix: run `wayfindr status` to check the daemon. The next call starts it again if it is not running.``
The next request reconnects, starting the daemon again if needed.

The shim answers `prompts/*`, `resources/*`, and `completion/*` with JSON-RPC
`-32601`, as the daemon does. When the client closes stdin, the shim first
answers every request it has already read, then exits `0`, so a script can pipe
requests in and close stdin at once. A tool that needs your input then can't
ask, and its call gets the `isError` result for a client that could not be
asked. If stdout fails, the shim exits non-zero.

## `wayfindr status`

Ask the running daemon for each connector's state and what to do about it, its
tool count and last sync, credentials left behind by removed connectors, and the
error of a config the daemon could not load.

| Flag      | Default | Description                                                                                                                       |
| --------- | ------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `--check` | `false` | Print the same, and exit `2` when something needs attention (see **Exit** below), so a script or a health check can branch on it. |

**Prints** one row per connector, sorted by name, then what to do about each
connector that is neither `starting` nor `ready` or whose last sync failed, then
warnings and notes, orphaned credentials, and the config error:

```
CONNECTOR   STATE       TOOLS  LAST SYNC
fake        ready       1      2026-11-07T06:22:06Z
linear      needs_auth  0      never
needsecret  auth_error  0      never

linear: The connector has not been authorized yet.
  Open the link to authorize it.
  http://127.0.0.1:7777/auth/linear

needsecret: The connector's config refers to secret:missing_one, but no such secret is stored.
  Store the secret, or fix the reference in the connector's config.
```

- **STATE** is one of `starting`, `ready`, `needs_auth`, `auth_error`, or
  `unreachable`; see [Troubleshooting](../guide/troubleshooting).
- **TOOLS** counts the connector's catalog entries, then the tools its
  [`tools` list](../guide/access/tools-lists) hid at the last sync, if any:
  `38 (4 hidden)`.
- **LAST SYNC** is `never` before the first sync, and gets ` (failed)` after a
  failed one, with `Last sync failed: <error>` under the connector.
- A **TOKEN EXPIRES** column appears once some connector has an OAuth token with
  an expiry; `-` for the others.
- **Warnings** lists what is about to break and how to head it off, one entry
  per connector and warning (see [warnings](#warnings) below). **Notes** lists
  OAuth tokens that can't be refreshed but have more than a day left.
- With no connectors: `No connectors are configured.`
- Orphaned credentials:
  `Orphaned credentials (connectors no longer in the config): old` then
  ``Remove each with `wayfindr auth logout <connector>`.``
- A config the daemon couldn't load:
  `Config error: the daemon keeps running its last good config.` and the error.

**`--json`** prints the daemon's status:

| Field                                 | Meaning                                                                                                              |
| ------------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `version`                             | The running daemon's version.                                                                                        |
| `connectors[].name`, `state`, `since` | The connector, its state, and when it entered it.                                                                    |
| `connectors[].advice`                 | `summary`, `action`, and `link` (when there is one). Absent once `ready`.                                            |
| `connectors[].tools`                  | Tools in the catalog.                                                                                                |
| `connectors[].hidden_tools`           | Tools the connector's `tools` list hid at the last successful sync. Absent when none.                                |
| `connectors[].last_sync`              | The last sync's time and, if it failed, `error`. Absent before the first sync.                                       |
| `connectors[].token_expires_at`       | The stored OAuth token's expiry. Absent when there is no token, no expiry, or the token is bound to an old `url`.    |
| `connectors[].warnings`               | `code`, `note` (`true` for a note), `summary`, `action`, and `link` (when there is one). Absent when there are none. |
| `orphaned_credentials`                | Connectors no longer in the config that still have a stored credential. Always an array.                             |
| `config_error`                        | Why the last reload failed. Absent when the running config is the file's.                                            |

Empty optional fields are omitted. `connectors` and `orphaned_credentials` are
always arrays.

### Warnings

A warning says a connector will break unless you act, before it does:

```
Warnings:
  linear: The access token expires at 2026-11-08T17:00:00Z and cannot be refreshed.
    Authorize again before then to avoid an interruption.
    http://127.0.0.1:7777/auth/linear
  jira: The connector has not been ready since 2026-11-05T10:00:00Z.
    Check that the server is running at the connector's url. Wayfindr reconnects on the next call.
```

| Code                    | When                                                                                            |
| ----------------------- | ----------------------------------------------------------------------------------------------- |
| `no_refresh`            | The OAuth access token expires within a day and there is no refresh token. Before then, a note. |
| `refresh_expiring`      | The provider said the refresh token ends within 7 days.                                         |
| `registration_expiring` | The OAuth client Wayfindr registered for the connector expires within 7 days.                   |
| `certificate_expiring`  | The client certificate in the connector's `[tls]` table expires within 7 days.                  |
| `stuck`                 | The connector has not been `ready` for more than a day.                                         |

Each also shows once it has passed, until you fix it. The daemon logs each
warning once, at `warn`, when it starts to apply; your agent sees the OAuth and
certificate ones due within a day in `search_tools`. See
[Troubleshooting](../guide/troubleshooting#warnings) for what to do about each.

**Exit:** `0` whenever the daemon answers, whatever the connectors' states. `1`
if the daemon can't be reached ([Finding the daemon](#finding-the-daemon)). With
`--check`, `2` instead of `0` when any of these holds:

- a connector is not `ready` (`starting` counts),
- a connector has a warning (a note doesn't count),
- the daemon reports a config error.

Orphaned credentials don't count. A failure to reach the daemon is still `1`,
and `2` prints no `Error:` line: the output says what needs attention.

```sh
wayfindr status --check > /dev/null || echo "wayfindr needs attention"
```

## `wayfindr calls`

Ask the running daemon for its call log: one row per `call_tool` call, newest
first. A call that didn't end `ok` is followed by the error text the agent got
back, and every row shows how big the result was. Arguments and results are
never stored. How long rows are kept is
[`[calls] retention`](./configuration#calls), 30 days by default.

| Flag          | Default | Description                                                                                                                                                                                   |
| ------------- | ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--connector` | all     | Only calls to this connector. Repeat it for several.                                                                                                                                          |
| `--since`     | `24h`   | How far back to look: a Go duration such as `90m` or `36h`, which may start with whole days, such as `7d` or `1d12h` (a day is 24 hours). `0` is the whole log. A negative value is an error. |
| `--errors`    | `false` | Only calls whose result is not `ok`.                                                                                                                                                          |
| `--limit`     | `50`    | At most this many calls, newest first. From 1 to 1000.                                                                                                                                        |

**Prints:**

```
TIME                  CLIENT       TOKEN           TOOL                  DURATION  RESULT                     SIZE
2026-11-08T09:14:02Z  claude-code  default         linear.create_issue   412ms     ok                         312 B
2026-11-08T09:13:40Z  claude-code  default         linear.search_issues  1.2s      tool_error                 48 KiB
                                                                                   Unknown filter: state=opne
2026-11-08T09:13:32Z  claude-code  default         linear.list_issues    2.3s      ok                         1.2 MiB
2026-11-08T09:12:11Z  -            nightly-triage  github.get_file       -         rejected: schema_mismatch  -
                                                                                   The arguments do not match github.get_file's inputSchema. To fix: Fix the arguments and call again.
2026-11-08T08:55:02Z  cursor       default         slack.post_message    30.0s     failed: timeout            -
```

- **TIME** is when the call arrived, in UTC.
- **CLIENT** is the name the client gave in its MCP `clientInfo`. Clients that
  use `wayfindr stdio` are always named. Clients that connect over HTTP send
  their name only from protocol version 2026-07-28 on, so before then this shows
  `-`.
- **TOKEN** is the client token the call authenticated with: `default`, or a
  [scoped client token](#wayfindr-token)'s name. The daemon checked it, so
  unlike CLIENT, a client can't choose what it shows. It is `-` for calls logged
  before Wayfindr recorded tokens.
- **TOOL** is the qualified name exactly as the agent wrote it, even when no
  such tool exists.
- **DURATION** is how long Wayfindr spent calling the upstream, connecting
  included: milliseconds under a second, then tenths of a second. It is `-` for
  a call Wayfindr refused or failed before calling the upstream. A connector
  that fails at once, such as one in `needs_auth`, shows about `0ms`.
- **RESULT** is one of the results below. A result other than `ok` is followed
  by the reason, such as `schema_mismatch` or `timeout`, and the error text on
  the next line: the upstream's own text for `tool_error`, Wayfindr's message
  otherwise. The text is on one line, at most 500 bytes, with URL query strings
  masked. Calls logged before Wayfindr kept it have none.
- **SIZE** is how big the result was that the agent got back, in binary units:
  bytes below 1 KiB, then `KiB`, `MiB`, and `GiB`, with one decimal below 10 and
  none above, such as `312 B`, `48 KiB`, and `1.2 MiB`. It is the length of the
  result's JSON encoding, so it covers its content and `structuredContent`. A
  `tool_error` result has a size too. It is `-` when no result came back from
  the upstream (a call Wayfindr refused or that failed), for an `input_required`
  call, and for calls logged before Wayfindr recorded sizes. Only the size is
  kept, never the result.

| Result           | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `ok`             | The upstream returned a result.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `tool_error`     | The upstream returned a result marked as an error (`isError`).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `rejected`       | Wayfindr refused the call without calling the upstream: a malformed name, an unknown tool (including one on a connector the client token doesn't allow, `no_connector`), a tool the config, the [profile](../guide/access/profiles), or the [project scope](../guide/access/project-scopes) hides (`hidden_by_policy`, `hidden_by_profile`, `hidden_by_scope`), a profile the config doesn't define (`unknown_profile`) or named twice or with an invalid name (`invalid_scope`, `bad_profile`), arguments that don't match the tool's schema, or an answer whose `requestState` doesn't belong to the call. |
| `failed`         | The call failed: Wayfindr couldn't read its catalog, or calling the upstream failed (auth, unreachable, timeout, a question the client can't be asked, sampling). A call cut short because the client gave up or the daemon stopped shows as `failed: canceled`. An upstream that answers with a protocol error shows as `failed: rejected`, which is not the same as the `rejected` result.                                                                                                                                                                                                                 |
| `input_required` | The upstream asked you a question, and the call returned it to the client. The client's answer is logged as a call of its own.                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |

With no matching calls it prints `No calls match.` When `retention` is `0s`, it
adds `The call log is off: [calls] retention is 0s, so no calls are recorded.`

**`--json`** prints the daemon's response:

| Field                       | Meaning                                                                                                                       |
| --------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `retention`                 | `[calls] retention` as a Go duration. `0s` means the call log is off.                                                         |
| `calls[].started_at`        | When the call arrived, in UTC.                                                                                                |
| `calls[].client`            | The client's name. Absent when unknown.                                                                                       |
| `calls[].token_name`        | The client token's name, never its value: `default` or a scoped token's. Absent for calls logged before tokens were recorded. |
| `calls[].connector`, `tool` | The name as the agent wrote it. `connector` is empty when the name has no dot; both are empty when there was no name.         |
| `calls[].duration_ms`       | How long Wayfindr spent calling the upstream. Absent when it didn't call it.                                                  |
| `calls[].result`            | `ok`, `tool_error`, `rejected`, `failed`, or `input_required`.                                                                |
| `calls[].kind`, `case`      | Why a call that isn't `ok` ended that way. Absent when unknown.                                                               |
| `calls[].result_bytes`      | The size in bytes of the result the agent got back, as SIZE counts it. Absent when unknown, as SIZE shows `-`.                |
| `calls[].error`             | The error text the agent got back from a call that isn't `ok`: one line, at most 500 bytes. Absent when there is none.        |

`calls` is always an array.

**Exit:** `0` whenever the daemon answers. `1` if a flag is out of range or the
daemon can't be reached ([Finding the daemon](#finding-the-daemon)).

## `wayfindr search`

Ask the running daemon to run `search_tools` for a query and show the page an
agent would get, in the same order. The words of the query can be separate
arguments; they are joined with spaces (`wayfindr search <query>...`). See
[Why a tool ranks where it does](../guide/monitoring#why-a-tool-ranks-where-it-does).

| Flag          | Default | Description                                           |
| ------------- | ------- | ----------------------------------------------------- |
| `--connector` | all     | Only this connector's tools. Repeat it for several.   |
| `--size`      | `5`     | Results per page, from 1 to 10.                       |
| `--page`      | `1`     | Which page of results.                                |
| `--explain`   | `false` | Show each result's lexical, vector, and fused scores. |

**Prints:**

```
Embeddings: on (text-embedding-3-small)

RANK  TOOL                 SUMMARY
1     linear.create_issue  Create a new issue in a team.
2     github.create_issue  Create a new issue in a repository.
3     jira.create_ticket   Create a Jira ticket.

Unavailable: jira (needs_auth): The connector has not been authorized yet.
More results: run the same command with --page 2
```

The first line says how this run embedded the query. An agent's earlier search
of the same words may have gone the other way: a provider can fail once and
recover.

| Header                                        | Meaning                                                                            |
| --------------------------------------------- | ---------------------------------------------------------------------------------- |
| `Embeddings: on (<model>)`                    | The query was embedded with `embedding.model`; the search is hybrid.               |
| `Embeddings: off, lexical only`               | No `[embedding]` is configured.                                                    |
| `Embeddings: failed (<reason>), lexical only` | The provider failed or missed `embedding.timeout`, so this search is lexical only. |

**RANK** counts across pages, so page 2 of size 5 starts at 6. **SUMMARY** is
the first sentence of the tool's description. Names, summaries, and the
`Unavailable:` and `Warning:` lines below can carry upstream text, so control
characters in them (the ESC that starts an escape sequence, tabs, and
bidirectional controls such as U+202E) and invalid UTF-8 print as `�`; `--json`
keeps them as sent. After the table come the connectors that are not `ready`
(`Unavailable:`), [warnings](#warnings) for the ready ones (`Warning:`), and
`More results: run the same command with --page N` when there is another page.
With no results it prints `No tools match.`

**`--explain`** replaces SUMMARY with the scores:

```
RANK  TOOL                 FUSED   LEXICAL   VECTOR
1     linear.create_issue  0.3095  #2  7.41  #1  0.712
2     github.create_issue  0.2917  #1  7.88  #3  0.655
3     jira.create_ticket   0.1429  -         #2  0.690
```

- **LEXICAL** is the tool's rank in the lexical ranking and its BM25F score.
- **VECTOR** is its rank among the 50 tools nearest the query's embedding, and
  the cosine similarity.
- **FUSED** is the sum of 1 / (5 + rank) over both rankings. Results are ordered
  by it, ties by name, except that tools whose name is the query come first
  ([How search works](../guide/search#matching)).
- `-` means the tool is not in that ranking.

Ranks are over your whole catalog, before `--connector` drops tools, so with
`--connector` they can skip numbers.

**`--json`** prints the daemon's response:

| Field                      | Meaning                                                                                 |
| -------------------------- | --------------------------------------------------------------------------------------- |
| `embedding.mode`           | `on`, `off`, or `failed`.                                                               |
| `embedding.model`          | `embedding.model`. Absent when `mode` is `off`.                                         |
| `embedding.reason`         | Why the embedding failed. Present only when `mode` is `failed`.                         |
| `tools[].name`             | The qualified name.                                                                     |
| `tools[].title`, `summary` | The tool's title and the first sentence of its description. Absent when empty.          |
| `tools[].score`            | Present only with `--explain`. A rank of `0` means the tool is not in that ranking.     |
| `page`, `size`, `has_more` | As in `search_tools`.                                                                   |
| `unavailable`, `warnings`  | As in `search_tools`: connectors not `ready`, and warnings for the ready ones searched. |

`tools`, `unavailable`, and `warnings` are always arrays.

**Exit:** `0` whenever the daemon answers, with or without results. `1` if the
query is empty, a flag is out of range, `--connector` names a connector that
isn't configured, or the daemon can't be reached
([Finding the daemon](#finding-the-daemon)).

## `wayfindr call`

Run one real call of a tool through the running daemon: the same checks,
narrowing, and [call log](../guide/monitoring) as an agent's `call_tool`. Use it
to prove a sign-in works, or to see what a scoped token's agent sees. See
[Testing a tool before your agent uses it](../guide/testing-tools).

```sh
wayfindr connector tools linear                     # pick a tool
wayfindr connector tools linear create_issue        # read its arguments
wayfindr call linear.create_issue --scaffold > args.json
wayfindr call linear.create_issue --args-file args.json
```

| Flag                  | Default | Description                                                                                                         |
| --------------------- | ------- | ------------------------------------------------------------------------------------------------------------------- |
| `--args <json>`       | `{}`    | The tool's arguments, a JSON object.                                                                                |
| `--args-file <path>`  | none    | Read the arguments from a file. `-` reads stdin. Not with `--args`.                                                 |
| `--scaffold`          | `false` | Print a JSON object of the tool's required arguments, and nothing else, then exit.                                   |
| `--token-file <path>` | none    | Run the call as the client token in this file, narrowed to its connectors. `-` reads stdin.                         |
| `--yes`               | `false` | Run a `write` or `destructive` tool without asking.                                                                 |
| `--output <file>`     | none    | Also write the whole result, as MCP's JSON, to this file.                                                           |

Without `--token-file` the call runs as you, the owner: every connector, no
profile, and no scope file. With it, the call runs as that token, and the call
log names the token. The token is read from the file, never taken as an
argument, where `ps` and your shell history would see it; the daemon neither
stores nor logs it. A token the daemon doesn't accept fails with
`The daemon accepts no client token with that value: it may be mistyped, revoked, or expired.`

**`--scaffold`** prints every required argument with its type's zero value
(`""`, `0`, `false`, `[]`, `{}`, or an enum's first value), in the order the
schema lists them, and nothing else, so it can be saved and passed back with
`--args-file`. A schema that requires nothing prints `{}`.
[`connector tools <connector> <tool>`](#wayfindr-connector-tools) describes
every argument.

```
{"team": "", "title": ""}
```

Edit the JSON and pass it back with `--args-file`.

**Before the call.** A `read` tool runs at once. A `write` or `destructive` tool
asks `linear.create_issue is a write tool. Run it? [y/N]`; a no prints
`Nothing changed.` and exits `0`. `--yes` skips the question, and off a terminal the command fails without it, before anything is
sent. A tool the connector's [`tools` list](./configuration#tools) hides is not
asked about: the daemon refuses it, as it does for an agent. A tool the
connector doesn't serve fails with
`linear serves no tool called <tool>: list them with wayfindr connector tools linear`.

If the daemon can't list the connector's tools, as when its sign-in broke, the
call still goes out, so you see the server's own error and the call is in the
call log. The command prints `linear's tools could not be listed: <reason>`, and
since the tool's class is unknown it asks as for a destructive tool:
`The class of linear.create_issue is unknown, so it may change or delete data. Run it? [y/N]`.
`--scaffold` needs the listing, and fails with its reason.

**Prints** a status line, then the result's content:

```
ok · 1.4s · 231 B
Issue ENG-481 created.
image, 48 KiB
```

The status is `ok`, or `error` when the tool or Wayfindr answered with an error;
then the time the call took and the result's size. Text prints as the upstream
sent it, except that control characters and invalid UTF-8 print as `�` and tabs
as spaces. Other content prints as one line each: `image, 48 KiB`,
`audio, …`, `link: <uri>`. A result with no content prints its structured
content. Over 4 KiB of text, the output stops with
`… cut at 4 KiB of 12 KiB; pass --output <file> for the whole result.`

**When the server asks for more.** An upstream can ask for input mid-call. A
form is asked on stdin, field by field (`n` to `Answer? [Y/n]` declines; an
empty answer skips an optional field). A URL request prints the URL and waits
for Enter, then answers that you finished. The command then sends your answers
and prints the final result. If stdin held the arguments or the token, it can't
ask, and fails.

**`--json`** prints the daemon's response, and leaves any question to you:

| Field        | Meaning                                                                                                                                                         |
| ------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `result`     | The `CallToolResult` in MCP's wire form (`content`, `structuredContent`, `isError`; for a question, `resultType: "input_required"`, `inputRequests`, `requestState`). |
| `class`      | The tool's class: `read`, `write`, or `destructive`. Absent when the catalog has no such tool.                                                                  |
| `elapsed_ms` | How long the call took, Wayfindr's checks included.                                                                                                             |

**Exit:** `0` when the tool answered, and with `--json` when it asks a question.
`1` when the tool or Wayfindr answered with an error (the error is printed, with
no `Error:` line), when the arguments are not a JSON object, the token is
unknown, or the daemon can't be reached
([Finding the daemon](#finding-the-daemon)).

## `wayfindr ui`

Open the daemon's [web console](../guide/console) in the browser, signed in. The
console shows connectors, the call log, what `search_tools` returns for a query,
and client tokens. It can [add connectors](../guide/console#adding-a-connector),
sign them in and out, and create or revoke client tokens. It never edits or
removes a connector.

The command asks the daemon for a one-time link and opens it. The link works
once, for one minute. The browser then stays signed in until it is idle for 12
hours or the daemon stops.

| Flag        | Default | Meaning                                        |
| ----------- | ------- | ---------------------------------------------- |
| `--no-open` | off     | Print the link instead of opening the browser. |

**Prints:**

```
Opened the console at http://127.0.0.1:7777/ui/.
```

With `--no-open`, or when no browser opens (then on stderr, after
`Could not open a browser (<reason>). Open this link yourself.`):

```
Open this link to use the console. It works once, until 2026-11-12T10:01:00Z:
  http://127.0.0.1:7777/ui/login#…
```

The browser opens the same way as for [`wayfindr auth login`](#wayfindr-auth-login).

**Exit:** `0` once the link is opened or printed. `1` when the daemon can't be
reached, or when it is a build without the console:
`the web console is not available`.

## `wayfindr scope`

Find the [project scope](../guide/access/project-scopes) that `wayfindr stdio`
started in this directory would send: the nearest `.wayfindr.toml` here or in a
parent, below your home directory. Check that it is valid, and show which
configured connectors it lets agents use.

| Flag               | Default | Description                                                                                                                                           |
| ------------------ | ------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--profile <name>` | none    | Also apply the [profile](../guide/access/profiles) `[profiles.<name>]`, as `wayfindr stdio --profile <name>` would. Reads it from the running daemon. |

**Prints**, without a connector, the file and one row per configured connector:

```
Project scope: /Users/you/src/acme-api/.wayfindr.toml

CONNECTOR  IN SCOPE  SCOPE TOOLS
github     yes       @read, create_pull_request
gmail      no        -
linear     yes       as configured

Warning: the scope names jira, which is not configured; it has no effect.
```

**SCOPE TOOLS** is the scope's tools list for the connector, or `as configured`
when it has none. A warning follows for each connector the scope names that the
running config doesn't have.

**With a connector**, it prints the file, then the table of
[`wayfindr connector tools`](#wayfindr-connector-tools), where **EXPOSED** is
`yes` only if both the config's tools list and the scope's expose the tool.

**With `--profile`**, it prints the profile's name first and adds a **PROFILE
TOOLS** column. A connector is **IN SCOPE** only if both the profile and the
file allow it, and with a connector, **EXPOSED** is `yes` only if the config,
the profile, and the file all expose the tool:

```
Profile: acme
Project scope: /Users/you/src/acme-api/.wayfindr.toml

CONNECTOR  IN SCOPE  PROFILE TOOLS  SCOPE TOOLS
github     yes       @read          @read, create_pull_request
gmail      no        -              -
linear     yes       as configured  as configured
```

| Situation                                             | Output                                                                                                                             |
| ----------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| No scope file applies                                 | `No project scope applies in <dir>: agents get every connector and tool the config exposes.` The daemon isn't needed.              |
| The scope file is invalid                             | `<dir>/.wayfindr.toml: invalid project scope: url: unknown key; tools.github[0]: "@admin" is not a class; use "@read" or "@write"` |
| `--profile` names a profile the config doesn't define | `profile "acme2": the running config doesn't define it; it defines acme, personal`                                                 |

**`--json`** prints, without a connector:

```json
{
  "dir": "/Users/you/src/acme-api",
  "profile": "acme",
  "scope_file": "/Users/you/src/acme-api/.wayfindr.toml",
  "connectors": [
    {
      "name": "github",
      "in_scope": true,
      "in_profile": true,
      "profile_tools": ["@read"],
      "in_scope_file": true,
      "scope_tools": ["@read", "create_pull_request"]
    },
    {
      "name": "gmail",
      "in_scope": false,
      "in_profile": false,
      "in_scope_file": false
    }
  ],
  "unconfigured": ["jira"]
}
```

| Field                                       | Meaning                                                                                                                                                                     |
| ------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `dir`                                       | The directory the scope file was looked for from.                                                                                                                           |
| `profile`                                   | `--profile`. Absent without it.                                                                                                                                             |
| `scope_file`                                | The scope file's path. Absent when none applies.                                                                                                                            |
| `connectors[].name`                         | A configured connector, in the daemon's order.                                                                                                                              |
| `connectors[].in_scope`                     | **IN SCOPE**: whether the profile and the scope file both allow it.                                                                                                         |
| `connectors[].in_profile`, `profile_tools`  | Whether the profile allows the connector, and its tools list for it. Present only with `--profile`; `profile_tools` is absent when the column shows `-` or `as configured`. |
| `connectors[].in_scope_file`, `scope_tools` | The same for the scope file. Present only when one applies.                                                                                                                 |
| `unconfigured`                              | The connectors the scope file names that aren't configured: the warnings.                                                                                                   |

Tools lists keep their entries as written; the table makes them printable.
`connectors` and `unconfigured` are always arrays.

With a connector, it prints `dir`, `profile`, and `scope_file` as above, then
what [`connector tools --json`](#wayfindr-connector-tools) prints, where
`exposed` is the **EXPOSED** column: `{"dir": …, "connector": "gmail", "tools": [{"name": "create_draft", "class": "write", "exposed": false, "required_args": 2, "definition": {…}}, …]}`.

When no scope file applies and there is no `--profile`, nothing narrows and the
daemon isn't asked: it prints `dir` with empty `connectors` and `unconfigured`,
or, with a connector, `dir`, `connector`, and empty `tools`.

**Exit:** `0` with the scope, or when none applies. `1` when the file is
invalid, `--profile` names a profile the config doesn't define, the daemon isn't
running, or the connector doesn't exist or isn't ready.

## `wayfindr connector`

Add, list, edit, import, or remove connectors, or list their tools.

- [`wayfindr connector add`](#wayfindr-connector-add): add one, by URL or command.
- [`wayfindr connector list`](#wayfindr-connector-list) (`ls`): list them, from the config file.
- [`wayfindr connector edit`](#wayfindr-connector-edit): change keys of one.
- [`wayfindr connector import`](#wayfindr-connector-import): add the servers a client already has.
- [`wayfindr connector remove`](#wayfindr-connector-remove) (`rm`): remove one and what it owned.
- [`wayfindr connector tools`](#wayfindr-connector-tools): list one's tools.

### What add, edit, and import do

The three change the config file, store any secrets, and report each connector
once it connects. In order:

1. Read the config file, build the change, and check the result as a whole with
   the config's own validation, as a [safe edit](#safe-edits). Nothing is
   written if this fails. (`import` also reads the client's files.)
2. Read the secret values.
3. Start the daemon in the background if it is not running, as
   [`wayfindr stdio`](#wayfindr-stdio) does, and store the secrets.
   `add` and `import` skip this with `--no-wait` and no secret to store; `edit`
   skips it when nothing changes.
4. Write the file.
5. For `edit`: delete the credential and the secrets the edit left unused.
6. Poll the daemon's status for up to 30 seconds, until the connector has left
   `starting` or the daemon reports a new config error. `--no-wait` skips this.

A connector that isn't ready prints its advice, as `wayfindr status` does. If it
needs auth and stdin is a terminal, the command asks `Authorize now? [Y/n]` and
runs [`wayfindr auth login`](#wayfindr-auth-login) on yes. After 30 seconds still
`starting`, it prints
``<name> is still starting; `wayfindr status` shows how it goes.``

The exit code is `0` once the file is written, whatever the connector's state.
These errors give `1`. Only the daemon's rejection comes after the file is
written; the others leave it as it was. `remove` has the first two:

| Situation                              | Message                                                                                                                                       |
| -------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| No config file                         | ``no config at <path>: run `wayfindr init` first``                                                                                            |
| The edit is unsafe                     | `could not change it safely: <reason>; edit the file by hand`                                                                                 |
| The file changed while the command ran | `the config file changed while the command ran; run the command again, or edit the file by hand`                                              |
| The daemon rejects the new config      | `the daemon could not load the new config and keeps running its last good one:` and the error                                                 |
| Two secret flags with stdin piped      | ``more than one secret value can't be piped: run this on a terminal, or set the others with `wayfindr secret set` and use --header or --env`` |

The last row is for `add` and `edit`; `import` has its own
[errors](#wayfindr-connector-import). If the file can't be written after a flag
stored a secret, the message ends with `(the secrets <names> were stored and are kept)`.

### Safe edits

`connector add`, `connector edit`, and `connector remove` edit the file's text,
not a re-encoded copy, so your comments and layout stay. Before writing, they
parse the file before and after the edit and require the two to differ only in
that one connector. Anything else is refused with `could not change it safely`,
and the file is left alone:

- a file that doesn't parse;
- connectors written as an inline table, `connectors = { … }`;
- for `remove` and `edit`, a connector written with dotted keys under
  `[connectors]` (`fs.command = "ls"`) rather than its own table;
- for `edit`, a changed key whose value spans several lines, or a connector's
  `auth` written with dotted keys (`auth.type = "oauth"`). An `auth = { … }`
  inline table is fine: `edit` rewrites it in place and keeps it inline.

The file is replaced atomically, so the daemon never reads half of it. If the
config file is a symlink, its target is replaced and the link is kept.

### `wayfindr connector add`

```
wayfindr connector add <name> --url <url> [flags]
wayfindr connector add <name> [flags] -- <command> [args...]
```

Add a `[connectors.<name>]` table to the end of the config file, then report the
connector once it connects. With `--url` it is an HTTP connector; otherwise the
command after `--` makes a stdio connector. The rest of the file, comments
included, stays as it is. A name that exists is never overwritten: `add` only
creates.

```sh
wayfindr connector add linear --url https://mcp.linear.app/mcp
wayfindr connector add github --url https://api.githubcopilot.com/mcp/ --secret-header Authorization
wayfindr connector add fs -- npx -y @modelcontextprotocol/server-filesystem ~/src
wayfindr connector add reports --url https://mcp.example.com/mcp --client-id reports-bot \
  --grant client_credentials --authorization-server https://auth.example.com \
  --token-auth-method private_key_jwt --signing-key < reports-key.pem
```

The flags are the same for [`connector edit`](#wayfindr-connector-edit),
which changes only the keys you give. The OAuth flags only write the connector's
`[auth]` table; the daemon reads the file, never the flags. A flag for the other
kind of connector is an error, and so is giving the same key twice to a `K=V`
flag. On `add`, any OAuth flag writes the `[auth]` table, so `--oauth` is only
needed on its own.

| Flag                            | Kind  | On `add`                                                                                                                                                                                                                          | On `edit`                                                                                                                                                  |
| ------------------------------- | ----- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--url <url>`                   | HTTP  | The upstream URL.                                                                                                                                                                                                                 | Set it. Not for a stdio connector.                                                                                                                         |
| `--header K=V`                  | HTTP  | A header to send. Repeatable.                                                                                                                                                                                                     | Set the header `K`; the others stay.                                                                                                                       |
| `--secret-header K`             | HTTP  | Prompt for the header's value, store it as a secret, and write a `${secret:…}` reference. Repeatable.                                                                                                                             | Prompt and replace the stored secret.                                                                                                                      |
| `--transport sse`               | HTTP  | Force the legacy HTTP+SSE transport.                                                                                                                                                                                              | Same.                                                                                                                                                      |
| `--oauth`                       | HTTP  | Write an auth table with `type = "oauth"`, without asking the server.                                                                                                                                                             | Add the auth table.                                                                                                                                        |
| `--no-oauth`                    | HTTP  | Write none, and don't ask the server.                                                                                                                                                                                             | Hidden: the same as `--unset auth`. Still works.                                                                                                           |
| `--client-id <id>`              | HTTP  | A pre-registered OAuth client.                                                                                                                                                                                                    | Set the OAuth client ID.                                                                                                                                   |
| `--client-secret`               | HTTP  | Prompt for the OAuth client's secret, store it as the secret `<name>_client_secret`, and write a reference. Needs `--client-id`.                                                                                                  | Prompt and replace the stored secret.                                                                                                                      |
| `--scope <scope>`               | HTTP  | An OAuth scope to request. Repeatable.                                                                                                                                                                                            | Replace the requested scopes with the ones given.                                                                                                          |
| `--authorize-param K=V`         | HTTP  | An extra authorization request parameter, written to `authorize_params`, such as `access_type=offline`. Repeatable.                                                                                                               | Set the parameter `K`; the others stay.                                                                                                                    |
| `--grant <grant>`               | HTTP  | The [`grant`](./configuration#grants): `authorization_code`, `device_code`, `client_credentials`, or `jwt_bearer`.                                                                                                                | Set it. `client_credentials` and `jwt_bearer` delete the stored OAuth credential.                                                                          |
| `--authorization-server <url>`  | HTTP  | The authorization server's issuer, written to `authorization_server`, used instead of the one the server's metadata names. `client_credentials` and `jwt_bearer` need it (or a `token_endpoint` set by hand).                     | Set `authorization_server`.                                                                                                                                |
| `--token-auth-method <method>`  | HTTP  | The [`token_endpoint_auth_method`](./configuration#client-authentication), such as `private_key_jwt`.                                                                                                                             | Same.                                                                                                                                                      |
| `--signing-key`                 | HTTP  | Read the OAuth client's PEM private key from stdin, store it as the secret `<name>_signing_key`, and write `[auth.signing_key]` with a reference. Needs `--client-id`. See [Private key JWT](../guide/authentication-enterprise). | Read the key from stdin and replace the stored one. Needs a `client_id`.                                                                                   |
| `--assertion-file <file>`       | HTTP  | The file holding the `jwt_bearer` grant's workload token, written to `[auth.assertion]`.                                                                                                                                          | Set `assertion.file`.                                                                                                                                      |
| `--assertion-subject <subject>` | HTTP  | The subject of a `jwt_bearer` assertion Wayfindr signs with the signing key, written to `[auth.assertion]`.                                                                                                                       | Set `assertion.subject`.                                                                                                                                   |
| `--cert-file <file>`            | HTTP  | A PEM client certificate to present, written to [`[tls]`](./configuration#connectors-name-tls). Needs `--key-file`.                                                                                                               | Set `tls.cert_file`, adding the `[tls]` table if there is none.                                                                                            |
| `--key-file <file>`             | HTTP  | The PEM private key of `--cert-file`. Needs `--cert-file`. To keep the key in a secret instead, set `key = "${secret:…}"` in `[tls]` by hand.                                                                                     | Set `tls.key_file`.                                                                                                                                        |
| `--ca-file <file>`              | HTTP  | PEM CA certificates to trust besides the system's. The OAuth check trusts them too.                                                                                                                                               | Set `tls.ca_file`.                                                                                                                                         |
| `--env K=V`                     | stdio | An environment variable. Repeatable.                                                                                                                                                                                              | Set the variable `K`; the others stay.                                                                                                                     |
| `--secret-env K`                | stdio | Prompt for the variable's value, store it as a secret, and write a reference. Repeatable.                                                                                                                                         | Prompt and replace the stored secret.                                                                                                                      |
| `--timeout <duration>`          | both  | The connector's `timeout` for one tool call, such as `5m`. Default `2m`.                                                                                                                                                          | Set it.                                                                                                                                                    |
| `--no-wait`                     | both  | Don't wait for the connector. Without secret flags, don't start the daemon either; only write the file.                                                                                                                           | Not available.                                                                                                                                             |
| `--unset <key>`                 | both  | Not available.                                                                                                                                                                                                                    | Remove a key or a table. Repeatable. `--unset auth` removes the auth table and deletes the stored OAuth credential. See [Unsetting keys](#unsetting-keys). |

**OAuth detection** (`add`). For an HTTP connector with no headers and neither
`--oauth` nor `--no-oauth`, the command first asks the server, without
credentials, whether it uses OAuth. A `401` with a `Bearer` challenge means it
does. So does a success from a server that publishes Protected Resource Metadata
for its URL at `/.well-known/oauth-protected-resource`, as Google's MCP servers
do: they answer `initialize` without a token and ask for one only when a tool is
called. Either way, the connector gets a `[connectors.<name>.auth]` table with
`type = "oauth"`:

```text
Checking https://mcp.linear.app/mcp … it uses OAuth.
```

The other answers are `it needs no auth.`, `it answered HTTP <status>.`, and
`could not reach it.`. After the last two, the connector is added without
`auth`; if it needs OAuth, run `wayfindr connector edit <name> --oauth`. A URL
with a `${…}` reference is not checked: the command says
``Not checking whether <url> uses OAuth: its URL has a reference. If it does, run `wayfindr connector edit <name> --oauth`.``
The name and the file are checked before the server is asked, so a taken name
fails without any request.

**Secrets.** `--secret-header`, `--secret-env`, `--client-secret`, and
`--signing-key` store each value as the secret `<name>_<key>`, lowercased, with
characters a secret name can't hold replaced by `_`, and cut to 63 characters:
`--secret-header
Authorization` on `github` stores `github_authorization`, and `--client-secret`
on `gmail` stores `gmail_client_secret`. Two names can make the same one
(`google` with `DRIVE_TOKEN` and `google_drive` with `TOKEN` both make
`google_drive_token`), so a name another connector or `[embedding]` already
refers to is refused before any value is read, with
`the environment variable DRIVE_TOKEN would be stored as the secret google_drive_token, which connectors.google_drive already uses: use another connector name or key`;
storing it would change what that one sends. Values are read as
[`wayfindr secret set`](#wayfindr-secret-set) reads them: prompted without echo
on a terminal, or from stdin when piped, which allows only one secret per
command. A PEM key spans lines, so pipe it: `--signing-key < key.pem`. A value
that isn't a PEM block is refused with
`the OAuth signing key is not a PEM block: pipe the key file on stdin, as in < key.pem`.

**Prints:**

```text
Stored secret github_authorization.
Added connector github.
github is ready (38 tools).
```

With `--no-wait`, the last line is ``It takes effect on the next `wayfindr start`.``,
or, when a daemon answers,
``The daemon is loading it; `wayfindr status` shows how it went.``

**Also fails (`1`) when:**

| Situation                       | Message                                                                                                                                                                                                    |
| ------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Name taken                      | ``linear already exists; change it with `wayfindr connector edit linear`, or remove it with `wayfindr connector remove linear` ``                                                                          |
| Bad name or other invalid table | `invalid config: connectors."My Server": connector name must match [a-z0-9][a-z0-9_-]{0,62}`                                                                                                               |
| Neither `--url` nor a command   | `give --url <url> for an HTTP connector, or the server's command after -- for a stdio one`                                                                                                                 |
| A flag for the other kind       | `--env is only for stdio connectors (a command after --)`                                                                                                                                                  |
| Two keys with one secret name   | `the environment variable API_KEY and the environment variable api_key would both be stored as the secret fs_api_key: use keys that differ in more than case and punctuation, or a shorter connector name` |

### `wayfindr connector list`

```
wayfindr connector list
wayfindr connector ls
```

List the connectors the config file defines, sorted by name. Only the file is
read: the daemon isn't asked, so this works when it isn't running, and says
nothing about whether a connector is connected. For that, run
[`wayfindr status`](#wayfindr-status).

**Prints:**

```text
NAME    KIND   TARGET                                               AUTH
fs      stdio  npx -y @modelcontextprotocol/server-filesystem /src  -
github  http   https://api.githubcopilot.com/mcp/                   header (secret)
linear  http   https://mcp.linear.app/mcp                           oauth
```

- **KIND** is `http` (the table has `url`) or `stdio` (it has `command`).
- **TARGET** is the URL, with its query shown as `?REDACTED` and ` (sse)` after
  it for `transport = "sse"`, or the command and its arguments as written, with
  a leading `~` expanded.
- **AUTH** is `oauth` for an `auth` table, `header (secret)` when a header
  references a `${secret:…}`, and `-` otherwise.

With no connectors it prints `No connectors are configured.`

**`--json`** prints `{"connectors": [{"name": "fs", "kind": "stdio", "target": "npx -y …", "auth": "none"}, …]}`.
`auth` is `oauth`, `secret_header`, or `none`. `connectors` is always an array.

**Exit:** `0` with the list. `1` when there is no config file
(``no config at <path>: run `wayfindr init` first``), or when it doesn't parse
or is invalid: the config's own error, then `Fix the config file <path>.`

### `wayfindr connector edit`

```
wayfindr connector edit <name> [flags]
```

Change keys of an existing connector in the config file, then report the
connector as [`connector add`](#wayfindr-connector-add) does. Use it to fix one
wrong or missing setting without removing the connector and typing every flag
again. Each flag you give changes that key and nothing else. Everything else in
the file stays as it is: other keys, their order, the table's comments, and its
position. The flags are in the table under `connector add`.

```sh
wayfindr connector edit gmail --authorize-param prompt=consent
wayfindr connector edit gmail --client-secret
wayfindr connector edit gmail --client-id 456-def.apps.googleusercontent.com --client-secret
wayfindr connector edit gmail --unset auth.authorize_params.prompt
wayfindr connector edit linear --unset auth
```

The kind is the one the table already has: `edit` can't turn a stdio connector
into an HTTP one, or the reverse. Remove it and add it again.

#### Unsetting keys

`--unset <key>` takes a dotted path inside the connector's table, such as
`timeout`, `headers.X-Team`, `auth.scopes`, or `auth.authorize_params.prompt`. A
key under `[auth]` may leave out `auth.`, as in `scopes` or
`authorize_params.prompt`. Quote a part with dots in it: `headers."X.Team"`.

A key that names a table removes the whole table: `--unset tls` removes
`[tls]`, and `--unset auth` removes the `auth` table, inline or not, and
deletes the connector's stored OAuth credential, since nothing can use it any
more. A secret only that table referenced, such as its `client_secret`, is
deleted too (see below). `--no-oauth` did this on `edit` before `--unset auth`
did; it still works, but `--help` no longer shows it.

**Secrets.** `--client-secret`, `--signing-key`, `--secret-header`, and
`--secret-env` read the new value as
[`wayfindr secret set`](#wayfindr-secret-set) does, and store it under the name
the table already references, so a connector you wrote by hand with
`${secret:google_client_secret}` keeps that name. When another connector uses
the same secret, the command says so:
`google_client_secret is also used by gdrive; it changes for both.` When the key
held a literal or an `${env:…}` reference, it gets a `${secret:<name>_<key>}`
reference, named as in [`connector add`](#wayfindr-connector-add), and refused
as there when another connector or `[embedding]` already uses that name.

A secret the edit leaves unreferenced, because a `${secret:…}` header is now a
plain value or a key was unset, is deleted if nothing else in the config
references it, and the output names it. A secret still in use elsewhere is kept.

**OAuth.** `--unset auth` deletes the stored OAuth credential, as above. A changed `scopes`, `authorize_params`, or `redirect_uri`
applies at the next sign-in: the current token stays valid but was granted under
the old values. The command says `This applies at the next sign-in.` and, on a
terminal, offers `Authorize now? [Y/n]`. A changed `client_id` makes the
connector `needs_auth`, because a token belongs to the client that obtained it,
and removing `client_id` (`--unset auth.client_id`) deletes the stored
credential; see [Authentication](../guide/authentication). `--grant
client_credentials` or `--grant jwt_bearer` deletes the stored credential too:
an unattended grant never stores or reads one, and has no sign-in to offer. A
hand edit of `grant` leaves the credential; `wayfindr auth logout` removes it.

**Prints:**

```text
Stored secret gmail_client_secret.
Updated gmail: auth.authorize_params.prompt.
This applies at the next sign-in.
gmail is ready (12 tools).
```

When the edit changes nothing, the file is left alone and the command prints
`gmail is already set that way.`

**Also fails (`1`) when:**

| Situation                                                  | Message                                                                                                                    |
| ---------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| No such connector                                          | ``no connector is called gmail; `wayfindr connector add` creates one``                                                     |
| No flags                                                   | `nothing to change: give the flags to set or --unset`                                                                      |
| A flag for the other kind, or `--url` on a stdio connector | `--oauth is only for HTTP connectors`, or `edit can't turn a stdio connector into an HTTP one; remove it and add it again` |
| `--unset` of a key the connector doesn't have              | `gmail has no auth.scopes; leave it out, or check its spelling`                                                            |
| The same key set and unset                                 | `auth.client_id is both set and unset`                                                                                     |
| The result is invalid                                      | The config's own message, such as `invalid config: connectors.gmail.auth.client_secret: requires client_id`.               |

### `wayfindr connector import`

```
wayfindr connector import <client> [flags]
```

Read the MCP servers a client is set up with and add them as connectors, in one
edit of the config file. Your client's files are only read, never changed.
`<client>` is one of:

| Client         | Argument         | Files read                                                                                                                                                                                                                                         |
| -------------- | ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Claude Code    | `claude-code`    | `~/.claude.json`: `mcpServers` (user scope) and the current directory's entry under `projects` (local scope); `./.mcp.json` (project scope, with `--include-project`). With `CLAUDE_CONFIG_DIR` set, `.claude.json` in that directory comes first. |
| Claude Desktop | `claude-desktop` | `claude_desktop_config.json` in `~/Library/Application Support/Claude` (macOS), `%APPDATA%\Claude` (Windows), or `~/.config/Claude` (Linux)                                                                                                        |
| Cursor         | `cursor`         | `~/.cursor/mcp.json`, and `./.cursor/mcp.json` with `--include-project`                                                                                                                                                                            |
| Codex          | `codex`          | `[mcp_servers.<name>]` in `$CODEX_HOME/config.toml` (default `~/.codex`), and `./.codex/config.toml` with `--include-project` when that config trusts the project                                                                                  |
| VS Code        | `vscode`         | The user profile's `mcp.json` (`~/Library/Application Support/Code/User`, `%APPDATA%\Code\User`, or `~/.config/Code/User`), and `./.vscode/mcp.json` with `--include-project`                                                                      |

```sh
wayfindr connector import claude-code --dry-run
```

```text
Read ~/.claude.json.
Not read: ~/src/app/.mcp.json (in this project; pass --include-project to read it)

  github    https://api.githubcopilot.com/mcp/  (user, ~/.claude.json)
            header Authorization → secret github_authorization
            note: header Authorization stores the value of GITHUB_TOKEN from this shell
  linear    https://mcp.linear.app/mcp  (user, ~/.claude.json)
            OAuth: asked once you confirm

Skipped:
  notes     a connector called notes already exists
  wayfindr  this is Wayfindr itself

Run again without --dry-run to add 2 connectors.
```

Each line shows the connector, its URL or command line, and the scope and file
it came from. Control characters in any of it are shown as `�`, so an entry
can't hide itself or rewrite a line above the prompt. A dry run asks no server
anything.

**Project files.** The files in the current directory (`./.mcp.json`,
`./.cursor/mcp.json`, `./.vscode/mcp.json`, `./.codex/config.toml`) come with
the repository, so whoever wrote it chose their servers and the commands they
run. They are read only with `--include-project`; without it, each one that
exists is listed as `Not read`. Pass it only in a project you trust, and check
the commands of `project` entries before you confirm.

| Flag                | Meaning                                                                                 |
| ------------------- | --------------------------------------------------------------------------------------- |
| `--dry-run`         | Show what would be added and skipped. Ask no server anything, and change nothing.       |
| `--yes`, `-y`       | Don't ask before adding. Needed when stdin is not a terminal.                           |
| `--file <path>`     | Read this one file, in the client's format, instead of the client's own files.          |
| `--plain-env K`     | Keep the environment variable `K` as plain text instead of a secret. Repeatable.        |
| `--no-oauth`        | Don't ask HTTP servers whether they use OAuth.                                          |
| `--no-wait`         | Don't wait for the connectors. Without secrets to store, don't start the daemon either. |
| `--include-project` | Also read the MCP files in the current directory. Only for a project you trust.         |

**Mapping.**

| In the client                                                               | In the connector                                                                                                                                                                                                                          |
| --------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `command`, `args`, `env`                                                    | `command`, `args`, `env`                                                                                                                                                                                                                  |
| `url`, with `type` `http`, `streamable-http`, or none                       | `url`                                                                                                                                                                                                                                     |
| `url` with `type` `sse`                                                     | `url`, `transport = "sse"`                                                                                                                                                                                                                |
| `headers`; Codex `http_headers`, `env_http_headers`, `bearer_token_env_var` | `headers`                                                                                                                                                                                                                                 |
| A literal value in `env` or `headers`                                       | The secret `<connector>_<key>`, referenced as `${secret:…}`                                                                                                                                                                               |
| `${VAR}` in `env` or `headers`, set in the shell you import from            | Its value, stored as a secret like a literal                                                                                                                                                                                              |
| `${VAR}` in `env` or `headers`, not set in that shell                       | `${env:VAR}`, with a note; literal text beside it, except an auth scheme such as `Bearer `, is stored as a secret                                                                                                                         |
| `${VAR}` in `args` or `url`                                                 | `${env:VAR}`, with a note; a `${VAR:-default}` default is dropped                                                                                                                                                                         |
| VS Code `${input:id}` (a `promptString` input)                              | Prompted for once, and stored as a secret                                                                                                                                                                                                 |
| `oauth` / `auth` client ID and scopes                                       | `[connectors.<name>.auth]` with `type = "oauth"`, `client_id`, `scopes`                                                                                                                                                                   |
| Claude Code `timeout`, Codex `tool_timeout_sec`                             | `timeout`                                                                                                                                                                                                                                 |
| A `url` with no headers or auth                                             | [OAuth detection](#wayfindr-connector-add) once you confirm, as `connector add` does; when the answer is unclear, the connector is added without OAuth, with a line saying to add `auth = { type = "oauth" }` to its table if it needs it |

A client reads `${VAR}` from the shell it runs in, but `${env:VAR}` reads the
daemon's environment, which is often different (see
[Secrets](../guide/secrets#when-references-are-resolved)). So a value the
importing shell has is stored as a secret instead. Arguments and URLs are copied
as written: if one holds a credential, such as a key in a URL's path or query,
move it to a secret yourself. A URL with a query gets a note saying so. Secret
names never clash: when `<connector>_<key>` is taken by another connector,
existing or imported, or by `[embedding]`, a number is added, as in
`github_authorization_2`.

Names are lowercased, and characters outside `[a-z0-9_-]` become `-`. When the
same name is in two scopes, the narrower one wins, as in the client: local over
project over user for Claude Code, project over user for the others.

**Skipped entries.** Each is listed under `Skipped:` with the reason, and
nothing is written for it:

- the entry is turned off in the client (`disabledMcpServers`, `disabled`, or
  Codex `enabled = false`);
- it is a Claude Code `.mcp.json` server you rejected, or haven't approved yet,
  in Claude Code (`enabledMcpjsonServers`, `disabledMcpjsonServers`,
  `enableAllProjectMcpServers`);
- it is Wayfindr itself: `wayfindr stdio`, or a URL on your `listen` address;
- a connector by that name exists, or another entry already became that name;
- it sets something a connector can't express, such as `cwd`, `envFile`,
  `headersHelper`, a VS Code sandbox, Codex `env_vars` or `enabled_tools`, or a
  VS Code or Cursor variable with no equivalent, such as `${config:…}`;
- it has both a command and a URL, or neither, or a `type` such as `ws`.

**What it does, in order:**

1. Reads the config file and the client's files (the project's only with
   `--include-project`), and maps the entries.
2. Prints the plan. With `--dry-run` it stops here, having asked no server
   anything.
3. Asks `Add <n> connectors? [Y/n]` on a terminal, unless `--yes` is given.
4. Asks each HTTP server without headers or auth whether it uses OAuth, unless
   `--no-oauth` is given.
5. Prompts for VS Code inputs, then stores the secrets and writes the file as
   [`connector add`](#wayfindr-connector-add) does: one edit, checked as a
   [safe edit](#safe-edits).
6. Reports each connector as `connector add` does, offering to authorize those
   that need it.
7. Prints how to remove the imported servers from the client, such as
   `claude mcp remove github -s user` or `codex mcp remove docs`; for other
   clients, which file to edit.

**Exit:** `0` once the file is written, or when there is nothing to import or
you answer no. `1` when:

| Situation                                                            | Message                                                                                      |
| -------------------------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| Unknown client                                                       | `unknown client "emacs": want one of claude-code, claude-desktop, cursor, codex, vscode`     |
| The client has no config files (not even a project file left unread) | `found no cursor config here or in your home directory; --file reads one file in its format` |
| A client file doesn't parse                                          | `parse <path>: …`                                                                            |
| Stdin is not a terminal and no `--yes`                               | `not a terminal, so nothing was asked or changed: pass --yes to add them`                    |
| Two VS Code inputs with stdin piped                                  | `more than one input can't be piped: run this on a terminal`                                 |

The no-config, unsafe-edit, changed-file, and rejected-config cases are the
[shared ones](#what-add-edit-and-import-do).

### `wayfindr connector remove`

```
wayfindr connector remove <name> [flags]
wayfindr connector rm <name> [flags]
```

Remove the `[connectors.<name>]` table, its subtables, and the comment lines
directly above each from the config file, then delete what the connector owned:
its stored OAuth credential and every secret it referenced that nothing else in
the config references. The rest of the file stays as it is, so removing a
connector you just added gives back the file you had. A running daemon drops the
connector when it reloads the file.

Before it changes anything, the command says what it will do and asks:

```text
Removing gmail will:
  remove [connectors.gmail] from ~/.config/wayfindr/wayfindr.toml
  delete its OAuth credential, if one is stored
  delete its secret gmail_client_secret, if it is stored
  keep slack_token: connectors.slack-2 uses it
Remove gmail? [y/N]
```

The answer defaults to no, which prints `Nothing changed.` The plan comes from
the config file alone, without the daemon, so it can't tell whether a
credential or a secret is actually stored: each delete line says "if" it is.
The credential line is there only for a connector with an OAuth `auth` table.

| Flag          | Meaning                                                                                                            |
| ------------- | ------------------------------------------------------------------------------------------------------------------ |
| `--dry-run`   | Print the plan, then `Run again without --dry-run to remove gmail.` Change nothing. Never starts the daemon.       |
| `--yes`, `-y` | Don't print the plan or ask. Needed when stdin is not a terminal: without it, the plan prints and nothing changes. |

After the answer, it starts the daemon in the background if it is not running
and there is something to delete, as [`wayfindr stdio`](#wayfindr-stdio) does.

**Prints:**

```text
Removed gmail. Deleted its OAuth credential and its secret gmail_client_secret.
```

A secret that another connector or `[embedding]` still uses is kept, and the
output says which one uses it:

```text
Removed slack. Deleted its OAuth credential.
Kept slack_token: connectors.slack-2 uses it.
```

A connector with no OAuth credential and no secrets prints `Removed linear.`

If a delete fails after the file was edited, the command names what is left and
the commands that finish the job, such as `wayfindr auth logout gmail` and
`wayfindr secret remove gmail_client_secret`, and exits non-zero.

Only this command deletes: removing a connector by hand, or commenting it out,
and letting the daemon reload keeps its credential, so putting it back needs no
new sign-in. See [ADR 0006](../adr/0006-toml-config).

**Exit:** `0` once the file is written and the cleanup is done, after a dry
run, or when you answer no. `1` when:

| Situation                              | Message                                                                    |
| -------------------------------------- | -------------------------------------------------------------------------- |
| No such connector                      | ``no connector is called fs; `wayfindr status` lists them``                |
| Stdin is not a terminal and no `--yes` | `not a terminal, so nothing was asked or changed: pass --yes to remove it` |
| The cleanup fails                      | `could not delete everything gmail owned: …` and the commands to finish    |

The no-config and unsafe-edit cases are the
[shared errors](#what-add-edit-and-import-do).

### `wayfindr connector tools`

Ask the running daemon for every tool the connector's upstream serves now, with
each tool's class, whether the connector's
[`tools` list](./configuration#tools) exposes it to agents, and how many
arguments it requires. Use it to write that list, or to pick a tool to try with
[`wayfindr call`](#wayfindr-call).

**Prints** one row per tool, sorted by name, then a count:

```
TOOL           CLASS        EXPOSED  REQUIRED ARGS
create_draft   write        yes      2
delete_thread  destructive  no       1
get_thread     read         yes      1
send_message   write        no       3

2 of 4 tools exposed.
```

**CLASS** comes from the upstream's annotations: `read` (`readOnlyHint`),
`write` (`destructiveHint: false`), or `destructive` (everything else, including
tools without annotations). **REQUIRED ARGS** counts the names in the tool's
`inputSchema.required`. **TOOL** is the name as the upstream sent it, except
that control characters (the ESC that starts an escape sequence, tabs, and
bidirectional controls such as U+202E) and invalid UTF-8 print as `�`, so a name
can't rewrite your terminal; `--json` keeps the name as sent.

**`--json`** prints
`{"connector": "gmail", "tools": [{"name": "create_draft", "class": "write", "exposed": true, "required_args": 2, "definition": {"name": "create_draft", "inputSchema": {…}, …}}, …]}`.
`definition` is the tool as the upstream serves it, in MCP's wire form
(`name`, `description`, `inputSchema`, `annotations`, and so on). Wayfindr
re-encodes it, so its keys may come in a different order than the upstream sent them.

**With a tool**, it describes that tool's arguments instead: a line with the
tool and its class (and `hidden by the tools list` when it is), a note when the
schema requires no argument, then one row per argument in the schema's order:

```
$ wayfindr connector tools linear save_issue
linear.save_issue (write)
No argument is required by the schema; its descriptions may still say what a call needs.

  NAME             TYPE         DESCRIPTION
  addLabels        string[]     Label names or IDs as a JSON array of strings (e.g. ["Bug",
                                "Urgent"]) to add. Append-only; existing labels are never removed.
  assignee         string|null  User ID, name, email, or "me". Null to remove
  …
  slaType          string|null  SLA day counting type: "all" or "onlyBusinessDays". Only use with
                                slaBreachesAt; null means unset. One of "all", "onlyBusinessDays".
  team             string       Team name or ID (required when creating)
  …
```

- **TYPE** is every type the schema allows, joined by `|` (`string|null`),
  read from `type` or from the members of `anyOf` and `oneOf`; an array shows
  its item type (`string[]`, `object[]`) when it has one, and `any` means the
  schema names no type.
- **REQUIRED** is a column only when some argument is required.
- **DESCRIPTION** is the upstream's text on one line, with the allowed values
  when there are some. On a terminal it wraps to its width, unless that leaves
  the column under 30 characters; piped, each argument stays on one line, for
  `grep`.

A tool whose schema has no properties prints `It takes no arguments.` after its
first line instead of a table.

A tool whose schema requires nothing, such as one that both creates and
updates, may still say what a create needs in its descriptions, as `team` does
above. With `--json` it prints that tool's entry, as in the list.

**Exit:** `0` with the list. `1` when the daemon isn't running, there is no such
connector or tool, or the connector isn't ready; the message says what to do.

## `wayfindr auth`

Authorize an OAuth connector, or delete its stored credential. Both
subcommands call the running daemon and take the connector's name as their one
argument. See [Authentication](../guide/authentication).

- [`wayfindr auth login`](#wayfindr-auth-login): run the connector's auth flow.
- [`wayfindr auth logout`](#wayfindr-auth-logout): delete its stored
  credential.

`wayfindr auth` on its own prints the group's help and exits `0`. The old
spellings still work, hidden from the help: `wayfindr auth <connector>` is
`auth login <connector>`, and `wayfindr auth revoke <connector>` is
`auth logout <connector>`. A connector named `login`, `logout`, or `revoke` is
authorized with `wayfindr auth login <name>`.

### `wayfindr auth login`

```
wayfindr auth login <connector> [flags]
```

Start the connector's OAuth authorization: print its URL, open it in the
browser, and wait until the browser comes back to the daemon, the token is
stored, and the connector has reconnected.

| Flag        | Default | Meaning                                                                                                                                                                                                                                                 |
| ----------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--no-open` | off     | Print the URL and never open a browser, for a machine without one (over SSH, for example). It still waits.                                                                                                                                              |
| `--device`  | off     | Sign in with a [device code](../guide/authentication) instead of a browser redirect: enter a code at the provider's page, on this or any other device. A connector with `grant = "device_code"` uses it without the flag. The provider must support it. |

**Prints:**

```
Open this URL to authorize linear:
  https://mcp.linear.app/authorize?client_id=…&state=…
Waiting for the browser (until 2026-11-07T06:32:06Z). If the page shows an error, fix it and run `wayfindr auth login linear`; linear stays configured.
Authorized linear.
linear is ready (23 tools).
```

The URL opens with `open` on macOS, `xdg-open` on Linux, or `rundll32` on
Windows, and only if it is an `http` or `https` URL. If the browser can't be
opened, stderr says
`Could not open a browser (<reason>); open the URL above yourself.` and the
command keeps waiting. With `--no-open` it prints the same lines and opens
nothing: open the URL in a browser that can reach the daemon's `/oauth/callback`
(over SSH, forward the daemon's port, as in
`ssh -L 7777:127.0.0.1:7777 <host>`).

With `--device`, or for a connector with `grant = "device_code"`, it prints the
code and where to enter it instead, and opens that page (with the code filled
in, when the provider offers such a link) unless `--no-open` is set:

```
Enter code WDJB-MJHT at https://github.com/login/device to authorize github.
Waiting for you to allow access (until 2026-11-07T06:32:06Z). If the page shows an error, fix it and run `wayfindr auth login github --device`; github stays configured.
Authorized github.
github is ready (41 tools).
```

The daemon asks the provider every few seconds until you allow or deny access,
so nothing needs to reach the daemon's `/oauth/callback`. Wayfindr only shows or
opens an `https` address, or `http` on `localhost`; any other makes the start
fail with the provider's advice, since it could be a phishing page.

If the connector comes back from its reconnect in some state other than `ready`,
the last line is its advice instead, as `status` prints it.

**Timing:** starting the flow (discovery and, for a new connector, client
registration) may take up to 60 seconds. The flow then lasts 10 minutes, or less
when the provider's device code expires sooner; the command waits until then
plus 90 seconds.

**Exit:** `0` once authorized. `1` when:

| Situation                                                                    | Output                                                                                                                                                                                                                |
| ---------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| The flow failed or expired, or you pressed Ctrl-C                            | ``<connector> was not authorized. Fix the cause, then run `wayfindr auth login <connector>`.`` and the advice, then `Error: the authorization of <connector> failed` (`expired`, or `was interrupted` after Ctrl-C)   |
| No answer in time                                                            | ``<connector> was not authorized. Fix the cause, then run `wayfindr auth login <connector>`.``, then `Error: the authorization of <connector> did not finish in time`                                                 |
| Unknown connector                                                            | `No connector named "nope" is configured.` and ``Check the connector's name with `wayfindr status`.``                                                                                                                 |
| The connector doesn't use OAuth                                              | `fake does not use OAuth: it has no [auth] table of type "oauth".` and ``Add one with `wayfindr connector edit fake --oauth` to authorize it through Wayfindr, or put its API key in headers.``                       |
| The connector uses an [unattended grant](../guide/authentication-unattended) | `reports uses an unattended grant, so there is nothing to sign in to: Wayfindr gets its token itself.` and ``Check it with `wayfindr status`. To have Wayfindr get a new token, run `wayfindr auth logout reports`.`` |
| The connector can't be authorized as configured                              | `<advice>`, for example when the authorization server doesn't advertise PKCE and there is no `client_id`, or `--device` and the provider has no device code support                                                   |
| The authorization server can't be reached                                    | `<advice>`                                                                                                                                                                                                            |

Errors from the daemon print as the daemon's sentence alone, with no request or
status in front. If the browser shows an error and never comes back, press
Ctrl-C, fix the cause, and run the command again: the connector stays
configured. See [Authentication](../guide/authentication) and
[Fixing a connector](../guide/connectors#fixing-a-connector).

### `wayfindr auth logout`

```
wayfindr auth logout <connector>
```

Delete the connector's stored OAuth token and the client Wayfindr registered for
it, and cancel its authorizations in progress. The connector then needs
`auth login` again. This also removes the orphaned credential of a connector no
longer in the config. Nothing is sent to the authorization server.

**Prints:**

```
Revoked the credential of linear.
linear: The connector has not been authorized yet.
  Open the link to authorize it.
  http://127.0.0.1:7777/auth/linear
```

For an orphaned credential:
`Revoked the credential of old, which is no longer configured.`

When there is no credential to delete, it prints
`linear has no stored credential; nothing to revoke.`

For a connector with an
[unattended grant](../guide/authentication-unattended),
it forgets the token Wayfindr holds in memory (and deletes any credential left
from an earlier sign-in), so the connector reconnects and the grant runs again
at once. That is how you retry a grant the provider refused, once you have fixed
it there: such a connector stays in `auth_error` until then.

**Exit:** `0`, including when there was no credential. The request waits up to
90 seconds for the connector to reconnect.

## `wayfindr secret`

Store, list, and delete the secrets that `${secret:name}` references resolve
to. Each subcommand calls the running daemon; `set` and `remove` take the
secret's name as their one argument. See [Secrets](../guide/secrets).

- [`wayfindr secret set`](#wayfindr-secret-set): store a secret's value.
- [`wayfindr secret list`](#wayfindr-secret-list): list stored secrets, never
  their values.
- [`wayfindr secret remove`](#wayfindr-secret-remove): delete a stored secret.

### `wayfindr secret set`

Read a secret's value from stdin and store it, sealed, in the daemon. On a
terminal the value is prompted for without echo; when piped, one trailing
newline is dropped. Connectors whose config refers to `${secret:<name>}` pick it
up on their next connect; the ones waiting on it reconnect now. When
`[embedding] api_key` refers to it, the daemon builds the embedding client again
with the new value and embeds what the catalog lacks.

```sh
wayfindr secret set slack_client_secret            # prompts: Value for secret slack_client_secret:
printf '%s' "$TOKEN" | wayfindr secret set github_pat
```

A new secret's name must match `[a-z0-9][a-z0-9_-]{0,62}`, the
[connector-name rule](./configuration#connector-names): lowercase letters,
digits, `_`, and `-`, at most 63 characters. Setting a name that already exists
replaces its value.

Names that older releases allowed, with capitals or dots such as `GitHub.Token`,
keep working. A secret already stored under one can still be set, listed, and
removed, and so can a name your config already refers to as
`${secret:GitHub.Token}`. Only a new name has to follow the rule.

**Prints** `Secret <name> set.`, then one line per connector that was waiting on
the secret, after its reconnect: `<connector> is ready.` or its advice. When
none was waiting:
`Secret <name> set. Connectors that use it pick it up on their next connect.`

**Exit:** `0` once stored. `1` when:

| Situation       | Message                                                                                       |
| --------------- | --------------------------------------------------------------------------------------------- |
| Bad name        | `secret name "Bad.Name" must match [a-z0-9][a-z0-9_-]{0,62}`                                  |
| Empty value     | `secret value is empty`                                                                       |
| Value too large | `secret value is too large: sent as JSON it is <n> bytes, over the daemon's 65536-byte limit` |

A name no secret can have (such as `_bad`) is refused before the daemon is
called. A name only the older rule allows is checked before the value is read:
it must be stored, or referenced by the config file. The daemon still decides,
against its stored secrets and running config. The request waits while waiting
connectors reconnect.

### `wayfindr secret list`

List the stored secrets, sorted by name, and what in the daemon's running config
refers to each. Values are never printed. `ls` is an alias.

```text
$ wayfindr secret list
NAME                 USED BY
embed_key            connectors.fs, embedding.api_key
github_token         connectors.github
gmail_client_secret  -
```

**USED BY** names each connector whose table refers to the secret
(`connectors.<name>`), and `embedding.api_key` when `[embedding] api_key` does,
sorted and separated by commas. It is `-` when nothing in the running config
refers to it: a secret you can delete with
[`wayfindr secret remove`](#wayfindr-secret-remove). It reflects the config the
daemon is running, so an edit shows once the daemon has reloaded it. A secret
the config refers to but that isn't stored is not listed; `wayfindr status`
shows the connector waiting on it.

With no secrets stored it prints `No secrets are stored.`

**`--json`** prints the daemon's response:

| Field               | Meaning                                                                   |
| ------------------- | ------------------------------------------------------------------------- |
| `secrets[].name`    | The secret's name.                                                        |
| `secrets[].used_by` | What in the running config refers to it, sorted. Empty when nothing does. |

`secrets` and each `used_by` are always arrays.

**Exit:** `0` whenever the daemon answers. `1` when the daemon isn't running.

### `wayfindr secret remove`

Delete a stored secret. `rm` is an alias. A connector whose config still refers
to `${secret:<name>}` goes to `auth_error` when it next needs the value, so use
it for a secret nothing references: one that
[`wayfindr secret list`](#wayfindr-secret-list) shows with `-`.
[`wayfindr connector remove`](#wayfindr-connector-remove) and
[`wayfindr connector edit`](#wayfindr-connector-edit) delete the secrets they
leave unused for you.

```sh
wayfindr secret remove gmail_client_secret
```

**Prints** `Deleted secret <name>.`

**Exit:** `0` once deleted. `1` when no secret is stored under that name, or the
daemon isn't running.

## `wayfindr token`

Manage scoped client tokens: client tokens that can use only the connectors you
name, and not the admin API. Give one to a client that runs unattended. See
[Scoped client tokens](../guide/access/scoped-tokens). With `--remote`, a token
can also reach `/mcp` through a tunnel, for a
[cloud agent](../guide/cloud-agents).

- [`wayfindr token create`](#wayfindr-token-create): create one and print it once.
- [`wayfindr token list`](#wayfindr-token-list): list them and the OAuth grants, never their values.
- [`wayfindr token revoke`](#wayfindr-token-revoke): delete one.

Each subcommand calls the running daemon's admin API with the default client
token ([Finding the daemon](#finding-the-daemon)). The default client token
itself is not managed here; to rotate it, see
[Rotating the client token](../guide/running#rotating-the-client-token).

```text
$ wayfindr token create nightly-triage --connector github --connector linear
Created client token nightly-triage. It can use: github, linear.
wf_s_6kq1…
This is the only time it is shown; store it where the client can read it.

$ wayfindr token list
NAME            KIND                 CONNECTORS      REMOTE  CREATED               LAST USED
chatgpt         oauth (chatgpt.com)  github          yes     2026-11-09T08:30:00Z  2026-11-09T09:12:41Z
nightly-triage  token                github, linear  no      2026-11-08T10:00:00Z  2026-11-08T22:00:04Z

$ wayfindr token revoke nightly-triage
Revoked nightly-triage. Clients using it are refused from now on.
```

A token for a cloud agent is made the same way, with `--remote`:

```text
$ wayfindr token create grok-bot --connector github --remote
Created remote client token grok-bot. It can use: github.
wf_s_9d2f…
This is the only time it is shown; store it where the client can read it.
```

### `wayfindr token create`

```
wayfindr token create <name> --connector <connector>... [flags]
```

Create a scoped client token and print its value once. Takes exactly one
argument, the token's name.

| Flag                      | Default | Description                                                                                                                                  |
| ------------------------- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `--connector <connector>` | none    | A connector the token can use. Repeat it for several. At least one is required.                                                              |
| `--remote`                | `false` | Let the token reach `/mcp` through a tunnel, which no other token can. It can't be changed later. See [Cloud agents](../guide/cloud-agents). |

**Prints** `Created client token <name>. It can use: <connectors>.` (`Created remote client token …` with `--remote`), then the
token on a line of its own, then
`This is the only time it is shown; store it where the client can read it.` The
value is `wf_s_` followed by 32 random bytes in base64url. Store it where the
client can read it: the daemon keeps only its SHA-256, so no command can show it
again.

- The name follows the [connector-name rule](./configuration#connector-names):
  `[a-z0-9][a-z0-9_-]{0,62}`. `default` is reserved for the default client
  token. Names are unique.
- Each `--connector` must name a connector in the daemon's running config. If
  you remove that connector later, the token simply stops matching it. Adding it
  back under the same name restores access.
- A token's connectors and its `--remote` setting can't be changed. Revoke it
  and create a new one.
- A remote token still reaches only its connectors and never the admin API, and
  it works locally like any scoped token. The default client token is never
  remote. Give a remote token only the connectors its agent needs: anyone who
  holds it can use them from anywhere until you revoke it.
- Tokens don't expire.

**Exit:** `0` once created. `1` when no `--connector` is given
(`required flag(s) "connector" not set`, before the daemon is asked), or when
the daemon refuses: the name isn't valid or is `default`, a token of that name
exists, or a connector isn't configured. The daemon's reason is the error.

### `wayfindr token list`

List the scoped client tokens and the OAuth grants that cloud agents signed in
with, sorted by name. Values are never printed; the daemon doesn't have them.
The default client token is not listed. `wayfindr token ls` is the same
command.

The columns are:

- **KIND** is `token` for a scoped client token, and `oauth (<host>)` for an
  OAuth grant, where `<host>` is the host of the client that holds it, such as
  `chatgpt.com`.
- **CONNECTORS** are the connectors the token or grant can use, as created. A
  connector removed from the config since is still listed.
- **REMOTE** is `yes` for a token created with `--remote`, `no` otherwise. Only
  a `yes` token gets through a tunnel. An OAuth grant is always `yes`.
- **LAST USED** is when a client last presented the token, or the grant's
  access token, or `-` if none has. The daemon writes it to the store at most
  once a minute, and when it stops, so it can lag by up to a minute.

With no scoped tokens or grants it prints
``No scoped client tokens. Create one with `wayfindr token create <name> --connector <connector>`.``

**`--json`** prints the daemon's response:

| Field                   | Meaning                                                                      |
| ----------------------- | ---------------------------------------------------------------------------- |
| `tokens[].name`         | The token's or grant's name.                                                 |
| `tokens[].kind`         | `token` for a scoped client token, `oauth` for an OAuth grant.               |
| `tokens[].connectors`   | The connectors it can use, as created.                                       |
| `tokens[].client`       | An OAuth grant's client ID. Absent for a token.                              |
| `tokens[].remote`       | `true` if it was created with `--remote` or is an OAuth grant, else `false`. |
| `tokens[].created_at`   | When it was created.                                                         |
| `tokens[].last_used_at` | When a client last presented it. Absent until the token is first used.       |

`tokens` is always an array.

### `wayfindr token revoke`

Delete a scoped client token or an OAuth grant, by name. A grant's access and
refresh tokens go with it, so its client has to ask for access again.

**Prints**
`Revoked <name>. Clients using it are refused from now on.` From
their next request on, clients that present it get `401`, with no restart (`404`
for a request that comes through a tunnel). Calls already logged keep its name in
the call log.

**Exit:** `0` once deleted. `1` when no scoped token or OAuth grant has that
name: the daemon's sentence.

## `wayfindr config`

Show which config file and data directory Wayfindr uses, and check the config
file. Neither subcommand needs a daemon or touches the store or the keychain.

- [`wayfindr config path`](#wayfindr-config-path): print the config file and data directory.
- [`wayfindr config check`](#wayfindr-config-check): check the config file.

```text
$ wayfindr config path
Config    /Users/you/.config/wayfindr/wayfindr.toml
Data dir  /Users/you/.local/share/wayfindr

$ wayfindr config check
/Users/you/.config/wayfindr/wayfindr.toml is valid: 3 connectors, 1 profile.
```

### `wayfindr config path`

Print the config file and data directory every command uses: from
[`--config` and `--data-dir`](#global-flags), then `WAYFINDR_CONFIG` and
`WAYFINDR_DATA_DIR`, then the defaults (see
[Paths and Environment](./paths-and-environment)). It prints them whether or not
they exist yet.

**Prints:**

```
Config    <path>
Data dir  <path>
```

**`--json`** prints `{"config_file": "<path>", "data_dir": "<path>"}`.

**Exit:** `0`.

### `wayfindr config check`

Load the config file with the same checks the daemon runs when it starts, and
report the result. It reads only the file: a running daemon keeps its config
until you restart it.

**Prints** `<path> is valid: <n> connectors, <m> profiles.`, with `connector`
and `profile` for one.

| Problem                         | Error                                                                           |
| ------------------------------- | ------------------------------------------------------------------------------- |
| No config file                  | ``no config at <path>: run `wayfindr init` first``                              |
| The file doesn't parse or check | `load config <path>: <problems>`, the same message the daemon logs and exits on |

**Exit:** `0` when the file is valid; `1` when it is missing or invalid.

## `wayfindr service`

Manage a user-level service that runs `wayfindr start --foreground` at login and
restarts it when it fails: a launchd agent (`dev.wayfindr.daemon`) in
`~/Library/LaunchAgents` on macOS, or the systemd user unit `wayfindr.service`
on Linux. The daemon logs to `<data>/wayfindr.log`.

Other operating systems fail with
``service: not supported on <os>: run `wayfindr start --foreground` under your own supervisor``.
See [Running the Daemon](../guide/running).

- [`wayfindr service install`](#wayfindr-service-install): write the unit and start it.
- [`wayfindr service uninstall`](#wayfindr-service-uninstall): stop and remove it.

### `wayfindr service install`

Write the launchd agent or systemd user unit for this binary and (re)start it.
Running it again replaces the unit, for example after moving the binary.

A daemon already running on this data directory, such as one you started with
`wayfindr start` or one a stdio client started, is stopped once the unit is
written and just before it is loaded, as [`wayfindr stop`](#wayfindr-stop)
does, so the service runs the daemon from then on, with the unit's environment.
On a system with no service manager nothing is stopped. If the service manager
then fails, the error ends
``The daemon that was running is stopped: start it with `wayfindr start` ``.

The unit passes the paths and `DO_NOT_TRACK` from this shell to the daemon, with
[`--config` and `--data-dir`](#global-flags) as `WAYFINDR_CONFIG` and
`WAYFINDR_DATA_DIR` when you give them, and never `WAYFINDR_SECRET_KEY` or
`WAYFINDR_CLIENT_TOKEN`: the service reads the master key and client token from
the keychain or the data directory. The list
is in [Paths and Environment](./paths-and-environment).

The unit runs `<binary> start --foreground --log-file <data>/wayfindr.log`,
where `<binary>` is this executable with symlinks resolved. It restarts the
daemon only when it exits with an error, so a clean stop stays stopped.

|                          | macOS                                                            | Linux                                                                                            |
| ------------------------ | ---------------------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| Unit file                | `~/Library/LaunchAgents/dev.wayfindr.daemon.plist`               | `$XDG_CONFIG_HOME/systemd/user/wayfindr.service`, else `~/.config/systemd/user/wayfindr.service` |
| Loads with               | `launchctl bootout` (if loaded), `enable`, `bootstrap gui/<uid>` | `systemctl --user daemon-reload`, `enable`, `restart`                                            |
| Restart policy           | `KeepAlive` `{SuccessfulExit = false}`                           | `Restart=on-failure`, `RestartSec=5`                                                             |
| Daemon stdout and stderr | The log file                                                     | The journal                                                                                      |

**Prints:**

```
Stopped the daemon (pid 4242); the service runs it now.
Installed /Users/you/Library/LaunchAgents/dev.wayfindr.daemon.plist
The daemon is running and starts at login. Log: /Users/you/.local/share/wayfindr/wayfindr.log
```

The first line appears only when a daemon was running.

If `WAYFINDR_SECRET_KEY` or `WAYFINDR_CLIENT_TOKEN` is set, stderr gets
`warning: <VAR> is set in this shell but is not written to the unit, because it holds a secret; the service will not see it and uses the keychain or the files in the data directory instead`.

**Exit:** `0` on success; `1` if writing the unit or `launchctl`/`systemctl`
fails, with the command and its output.

### `wayfindr service uninstall`

Stop and remove the login service.

**Prints** `Stopped the daemon and removed <unit file>`, or, with nothing
installed, `No service installed (<unit file> not found); nothing to do.` Both
exit `0`.

## `wayfindr update`

```
wayfindr update [version] [--check] [--no-restart]
```

Replace this binary with the latest release, or with the one you name, then
restart the daemon so it runs the new code. It downloads from
[GitHub Releases](https://github.com/atomicpages/wayfindr/releases) and checks
the archive the way the [installer](../guide/installation) does. It doesn't
need a running daemon.

```text
$ wayfindr update
Updated wayfindr v0.1.0 → v0.2.0 at /Users/you/.local/bin/wayfindr
```

| Flag           | Default | Description                                                                                                       |
| -------------- | ------- | ----------------------------------------------------------------------------------------------------------------- |
| `--check`      | `false` | Print how this version compares with the latest release, and exit `2` when the release is newer. Install nothing. |
| `--no-restart` | `false` | Replace the binary, but leave the daemon and the login service as they are.                                       |

**Which release.** Without an argument, `update` asks GitHub for the latest
release, which skips pre-releases and drafts, as the installer does. Then it
compares that with this binary's version:

- The same version: it prints `wayfindr v0.2.0 is up to date` and changes
  nothing.
- This binary is newer: it prints
  `wayfindr v0.2.0-3-ga6fe2cf is newer than v0.2.0` and changes nothing.
- A build that prints `dev` in [`wayfindr version`](#wayfindr-version), or a
  bare commit such as `a6fe2cf` (built from a checkout with no release tags),
  counts as older than any release.
- A build between releases, such as `v0.1.0-3-ga6fe2cf`, counts as newer than
  `v0.1.0`, so it updates only once a later release is out.

With a version, such as `wayfindr update v0.1.0` (the `v` is optional), it
installs exactly that release, a pre-release such as `v0.3.0-rc.1` or an older
one included. The version must be a full one: `wayfindr update 0.2` fails with
`invalid version "0.2": want a semantic version such as v0.2.0`. If it is the
version this binary already is, `update` prints
`wayfindr v0.2.0 is already installed; nothing to do`. When it is older, a note
follows the `Updated` line, because an older binary refuses a store that a newer
one has migrated (see [Upgrades and downgrades](../guide/storage#upgrades-and-downgrades)):

```text
$ wayfindr update v0.1.0
Updated wayfindr v0.2.0 → v0.1.0 at /Users/you/.local/bin/wayfindr
Note: v0.1.0 is older than v0.2.0. An older binary can't open a store that a newer one has migrated: https://atomicpages.github.io/wayfindr/guide/storage#upgrades-and-downgrades
```

**How it installs.** It picks the archive for this OS and CPU,
`wayfindr-<os>-<arch>.tar.gz` (`.zip` on Windows). An Intel binary running under
Rosetta on Apple silicon gets the `arm64` one, as the installer does. It
downloads the archive and the release's `checksums.txt`, and checks the
archive's SHA-256. If the checksum doesn't match, or the archive isn't listed,
nothing is written. Then it replaces the running executable in place. A symlink
is followed, so the file it points to is replaced and the link is kept. On
Windows, the running `wayfindr.exe` is renamed to `wayfindr.exe.old` first, as
the installer does, and old leftovers are removed on the next update.

`update` uses the GitHub release only. It doesn't read `WAYFINDR_DOWNLOAD_URL`;
to install from a mirror, run the installer. The checksum catches a corrupted
download. To check build provenance as well, verify the archive with
`gh attestation verify`, as in [Download by hand](../guide/installation#download-by-hand).

**Restart.** A running daemon keeps the old binary until it restarts. After the
binary is replaced, and unless you pass `--no-restart`, `update`:

1. Restarts the [login service](#wayfindr-service-install), if one is installed,
   and prints `Restarted the login service.` As
   [`service install`](#wayfindr-service-install) does, it first stops a daemon
   that runs on this data directory outside the service, such as one you
   started by hand, so the service's daemon can start. The service runs the
   binary that `service install` recorded, so this is the new one when
   `update` replaced that file.
2. Otherwise stops the daemon that runs on this data directory, as
   [`wayfindr stop`](#wayfindr-stop) does, and prints
   `Stopped the daemon (pid 4242); the next client starts the new binary.` The
   next stdio client starts the new binary, or you can run
   [`wayfindr start`](#wayfindr-start).
3. Prints nothing more when no daemon runs.

The service has to be restarted, not just stopped: it restarts the daemon only
when it fails, so a clean stop would leave it down until your next login.

Under `sudo`, as when the binary is in a directory only root can write,
`update` replaces the binary but restarts nothing, since a restart as root
would reach root's daemon and login service, not yours. It prints
`Running as root through sudo, so no daemon was restarted.` with the next
step: as your own user, run `wayfindr stop`, or `wayfindr service install` if
you use the login service.

**`--check`** compares this version with the latest release, or with the version
you name, and never downloads an archive. It prints the same up-to-date or
newer line as above, or, when the release is newer:

```text
$ wayfindr update --check
wayfindr v0.1.0 → v0.2.0 available; run `wayfindr update` to install it
```

With a version named, the command it suggests names that version, since a
plain `wayfindr update` installs the latest release:

```text
$ wayfindr update --check v0.3.0-rc.1
wayfindr v0.2.0 → v0.3.0-rc.1 available; run `wayfindr update v0.3.0-rc.1` to install it
```

**`--json`** works only with `--check`; `wayfindr update --json` fails with
`--json is supported by "wayfindr update" only with --check`. It prints:

```json
{
  "current": "v0.1.0",
  "latest": "v0.2.0",
  "update_available": true
}
```

`latest` is the version you named, if you named one. `update_available` is true
only when it is newer than `current`, the same case that exits `2`.

```sh
wayfindr update --check > /dev/null
if [ $? -eq 2 ]; then wayfindr update; fi
```

| Situation                                                       | Error                                                                                                                                                                                        |
| --------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| GitHub can't be reached                                         | `find the latest release: …`, or `download wayfindr v0.2.0: …` once the release is known. Nothing is written.                                                                                |
| The version doesn't exist                                       | `download wayfindr v9.9.9: download https://github.com/atomicpages/wayfindr/releases/download/v9.9.9/checksums.txt: 404 Not Found`. Nothing is written.                                      |
| The checksum doesn't match, or the archive isn't in the list    | `download wayfindr v0.2.0: checksum mismatch for <archive>: …`, or `… <archive> is not listed in checksums.txt of v0.2.0`. Nothing is written.                                               |
| The binary's directory isn't writable, such as `/usr/local/bin` | `install wayfindr v0.2.0: cannot write to /usr/local/bin: re-run with permission to write there (for example with sudo), or reinstall with the installer: …`. Nothing is written.            |
| The service was installed, but didn't restart                   | `updated wayfindr to v0.2.0 at <path>, but the login service did not restart: …`, then `` Restart it with `wayfindr service install` ``. The new binary stays in place.                      |
| No service, and the running daemon didn't stop                  | `updated wayfindr to v0.2.0 at <path>, but the running daemon did not stop: …`, then ``Stop it with `wayfindr stop`; the next client starts the new binary``. The new binary stays in place. |

**Exit:** `0` once the binary is replaced, and when there is nothing to update.
`1` on any error. With `--check`, `2` instead of `0` when the release is newer,
without an `Error:` line, as with [`status --check`](#wayfindr-status). A
failure to reach GitHub is still `1`.

## `wayfindr version`

Print the version, commit, and Go version.

```
wayfindr v0.5.0 (commit a6fe2cf, go1.26.1)
```

A build without version information prints `wayfindr dev (commit unknown, …)`.

**`--json`** prints
`{"version": "v0.5.0", "commit": "a6fe2cf", "go": "go1.26.1"}`.

## `wayfindr completion`

```
wayfindr completion bash|zsh|fish|powershell
```

Print a shell completion script to stdout. Run
`wayfindr completion <shell> --help` for how to load it in that shell. For
example, for zsh:

```sh
wayfindr completion zsh > "${fpath[1]}/_wayfindr"
```
