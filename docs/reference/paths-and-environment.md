# Paths and Environment

Every file Wayfindr reads or writes, and every environment variable it reads.
The layout is the same on macOS, Linux, and Windows: XDG-style paths under your
home directory.

| Item                                | Default                               | Mode   | Override                                           |
| ----------------------------------- | ------------------------------------- | ------ | -------------------------------------------------- |
| [Config file](#config-file)         | `~/.config/wayfindr/wayfindr.toml`    | `0600` | `--config`, `WAYFINDR_CONFIG`, `XDG_CONFIG_HOME`   |
| [Data directory](#data-directory)   | `~/.local/share/wayfindr/`            | `0700` | `--data-dir`, `WAYFINDR_DATA_DIR`, `XDG_DATA_HOME` |
| [SQLite database](#sqlite-database) | `<data>/wayfindr.db`                  | `0600` | [`storage.dsn`](./configuration#dsn)               |
| [Client token](#client-token)       | `<data>/client-token`                 | `0600` | `WAYFINDR_CLIENT_TOKEN` (the value)                |
| [Master key](#master-key)           | OS keychain, else `<data>/master.key` | `0600` | `WAYFINDR_SECRET_KEY` (the value)                  |
| [Daemon lock](#daemon-lock)         | `<data>/daemon.lock`                  | `0600` | none                                               |
| [Log file](#log-file)               | stderr, or `<data>/wayfindr.log`      | `0600` | `wayfindr start --log-file`                        |
| [Telemetry ID](#telemetry-id)       | `<data>/telemetry-id`                 | `0600` | none                                               |
| [Service unit](#service-unit)       | per OS                                | `0644` | none                                               |

`<data>` is the data directory. Run
[`wayfindr config path`](./cli#wayfindr-config-path) to see the resolved config
file and data directory.

## Precedence

For the config file, the first that is set wins:

1. The [`--config`](./cli#global-flags) flag, made absolute against the
   working directory.
2. `WAYFINDR_CONFIG`, used as given.
3. `$XDG_CONFIG_HOME/wayfindr/wayfindr.toml`, if `XDG_CONFIG_HOME` is an
   absolute path.
4. `~/.config/wayfindr/wayfindr.toml`.

For the data directory:

1. The [`--data-dir`](./cli#global-flags) flag, made absolute against the
   working directory.
2. `WAYFINDR_DATA_DIR`, used as given.
3. `$XDG_DATA_HOME/wayfindr`, if `XDG_DATA_HOME` is an absolute path.
4. `~/.local/share/wayfindr`.

A relative `XDG_*` value is ignored, as the XDG spec requires. The home
directory is looked up only when a default needs it, so setting both
overrides works without one.

The client token and master key each have a value override that beats the file:
see below.

::: tip
Every process that should talk to the same daemon must resolve the same
paths: the daemon, the CLI, the stdio shim your client starts, and the login
service. If you set `--config` or `--data-dir`, or `WAYFINDR_CONFIG` or
`WAYFINDR_DATA_DIR`, give them to all of them. A daemon that `start`, `stdio`,
or `connector` starts in the background gets the config file and data directory
of the command that started it, and `wayfindr service install` writes them into
the unit.
:::

## Files

### Config file

`wayfindr.toml`, documented in [Configuration](./configuration). `wayfindr init`
writes a default one (`0600`, in a directory it creates as `0700`) if none
exists, and never overwrites one. The daemon watches it and reloads on change.

### Data directory

Holds everything else Wayfindr writes. `wayfindr init`, `wayfindr start`, and
`wayfindr service install` create it as `0700` if it is missing.

### SQLite database

`<data>/wayfindr.db`, the default [`storage.dsn`](./configuration#dsn) for the
SQLite store. It holds the catalog, OAuth credentials, secrets, the discovery
cache, the call log, and the SHA-256 digests of scoped client tokens.
Credentials and secrets in it are encrypted with the master key. It runs in WAL
mode, so `wayfindr.db-wal` and `wayfindr.db-shm` sit next to it while the daemon
runs. It is created `0600`, and the `-wal` and `-shm` files take its mode. Point
`storage.dsn` elsewhere to move it; with Postgres there is no local database
file. See [Storage](/guide/storage).

### Client token

The default client token: the bearer token clients present to `/mcp`, and the
CLI to the admin API. It isn't an upstream OAuth token and gives no access to an
upstream by itself, but anyone holding it can call your tools through Wayfindr
and use the admin API, so treat it like a password.

| Source                  | Used by                       | Notes                                                                              |
| ----------------------- | ----------------------------- | ---------------------------------------------------------------------------------- |
| `WAYFINDR_CLIENT_TOKEN` | the daemon, the CLI, the shim | The token **value**. When set, it wins over the file. Use it in CI and containers. |
| `<data>/client-token`   | the daemon, the CLI, the shim | Mode `0600`. `wayfindr init` writes it if it doesn't exist and never replaces it.  |

- The token is `wf_` followed by 32 random bytes in unpadded base64url.
- Surrounding whitespace is trimmed from either source. An empty token is an
  error, never "no auth".
- The daemon refuses to start when either source holds a
  [scoped client token](/guide/access/scoped-tokens) (`wf_s_…`). The shim leaves
  a scoped value out of the environment of a daemon it starts.
- A daemon run as a [login service](#what-the-login-service-sees) never sees
  `WAYFINDR_CLIENT_TOKEN`, so it reads the file. If you set the variable for a
  daemon you start yourself, make sure its clients use the same value.

Scoped client tokens, made with
[`wayfindr token create`](./cli#wayfindr-token-create), have no file: only their
SHA-256 is kept, in the store. A client gives one in `WAYFINDR_CLIENT_TOKEN`.

The stdio shim reads the token itself. HTTP clients need the value in their
environment:

```sh
export WAYFINDR_CLIENT_TOKEN="$(cat ~/.local/share/wayfindr/client-token)"
```

To replace the default token, see
[Rotating the client token](/guide/running#rotating-the-client-token).

### Master key

The 32-byte key that encrypts secrets and OAuth credentials in the store. Looked
up in this order; the first source that holds a key wins:

1. `WAYFINDR_SECRET_KEY`: the key as standard base64 (44 characters).
2. The OS keychain: service `wayfindr`, account `master-key`. That is the macOS
   Keychain, the Secret Service on Linux (for example GNOME Keyring), or the
   Windows Credential Manager.
3. `<data>/master.key`: the key as standard base64, mode `0600`.

`wayfindr init` creates a key when no source has one: in the keychain if it is
available, else in `master.key`. It never writes the environment variable, and
it refuses to create a key when the store already holds encrypted data, because
a new key would make that data unreadable.

- A source that holds a malformed key is an error; Wayfindr doesn't move on to
  the next source.
- `master.key` must not be readable or writable by group or others. The daemon
  refuses it otherwise:
  `master key file permissions are too open: … run: chmod 600 <path>`. (Not
  checked on Windows.)
- When the key comes from `master.key`, the daemon logs a warning suggesting the
  keychain or `WAYFINDR_SECRET_KEY`.
- The keychain item belongs to your user account, not to a data directory, so
  every data directory on the account shares it.

Generate a key for `WAYFINDR_SECRET_KEY`:

```sh
head -c 32 /dev/urandom | base64
```

::: warning
Back up the master key. Without it, stored secrets and OAuth
credentials can't be decrypted: connectors go to `auth_error` or `needs_auth`
until you set the secrets again and re-authorize.
:::

### Daemon lock

`<data>/daemon.lock`, mode `0600`. The running daemon holds an OS file lock on
it and writes its pid and `listen` address into it as JSON:

```json
{ "pid": 4242, "listen": "127.0.0.1:7777" }
```

A second daemon on the same data directory refuses to start
(`daemon already running at 127.0.0.1:7777 (pid 4242): it holds <data>/daemon.lock`),
and the stdio shim waits for the running daemon instead of starting another. The
OS releases the lock when the daemon exits, even on a crash, so it is never
stale. Don't delete it.

### Log file

The daemon logs JSON lines to stderr. They go to a file when:

- you run `wayfindr start --log-file <path>` (with or without `--foreground`):
  appended, created `0600`;
- you run `wayfindr start`: `<data>/wayfindr.log` (or `--log-file`), appended,
  created `0600`;
- the stdio shim starts the daemon: `<data>/wayfindr.log`, appended, created
  `0600`;
- the login service runs it: `<data>/wayfindr.log`, through `--log-file`.

`wayfindr logs` prints `<data>/wayfindr.log`. Wayfindr doesn't rotate the log.

### Telemetry ID

`<data>/telemetry-id`, mode `0600`: the random install ID the
[usage report](./telemetry) carries. The daemon writes it the first time it
sends a report, so it exists only if you turned telemetry on. It is a random
UUID, tied to nothing about your machine or you. Delete it to start over with a
new ID.

### Service unit

Written by `wayfindr service install`, mode `0644` (launchd refuses a writable
plist), and removed by `wayfindr service uninstall`.

| OS    | Path                                                                                             |
| ----- | ------------------------------------------------------------------------------------------------ |
| macOS | `~/Library/LaunchAgents/dev.wayfindr.daemon.plist`                                               |
| Linux | `$XDG_CONFIG_HOME/systemd/user/wayfindr.service`, else `~/.config/systemd/user/wayfindr.service` |

See [`wayfindr service install`](./cli#wayfindr-service-install).

## Environment variables

Every variable Wayfindr reads:

| Variable                                | Read by                                     | Meaning                                                                                                                                                                                                        |
| --------------------------------------- | ------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `WAYFINDR_CONFIG`                       | all commands                                | Path of the config file. `--config` wins over it.                                                                                                                                                              |
| `WAYFINDR_DATA_DIR`                     | all commands                                | Path of the data directory. `--data-dir` wins over it.                                                                                                                                                         |
| `WAYFINDR_CLIENT_TOKEN`                 | daemon, CLI, shim                           | The [client token](#client-token) value. Wins over `<data>/client-token`.                                                                                                                                      |
| `WAYFINDR_SECRET_KEY`                   | daemon, `init`                              | The [master key](#master-key), standard base64 of 32 bytes. Wins over the keychain and `master.key`.                                                                                                           |
| `XDG_CONFIG_HOME`                       | all commands                                | Base for the config file, and for the systemd unit on Linux. Ignored unless absolute. `connector import` also uses it to find Claude Desktop's and VS Code's files on Linux.                                   |
| `XDG_DATA_HOME`                         | all commands                                | Base for the data directory. Ignored unless absolute.                                                                                                                                                          |
| `HOME` (`USERPROFILE` on Windows)       | all commands                                | Your home directory, for the defaults and for `~` in the config.                                                                                                                                               |
| `PATH`                                  | daemon                                      | Where stdio connectors' `command` is looked up.                                                                                                                                                                |
| Any `NAME` in `${env:NAME}`             | daemon                                      | Values for [config templates](./configuration#templates).                                                                                                                                                      |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` | daemon, `connector add`, `connector import` | A proxy for outbound HTTP: HTTP connectors, OAuth servers, and the embedding provider, and the OAuth check `add` and `import` make. The lowercase forms work too. Requests to loopback addresses never use it. |
| `CLAUDE_CONFIG_DIR`                     | `connector import`                          | Where Claude Code keeps `.claude.json` and `settings.json`; read before your home directory.                                                                                                                   |
| `DO_NOT_TRACK`                          | daemon                                      | Any value but empty or `0` keeps the [usage report](./telemetry) off, whatever `[telemetry] enabled` says.                                                                                                     |
| `CODEX_HOME`                            | `connector import`                          | Where Codex keeps `config.toml`. Default `~/.codex`.                                                                                                                                                           |
| `APPDATA`                               | `connector import`                          | On Windows, where Claude Desktop and VS Code keep their files. Default `~\AppData\Roaming`.                                                                                                                    |
| Any `VAR` in an imported `${VAR}`       | `connector import`                          | Its value in the shell you import from is stored as a secret. See [Importing from a client](/guide/connectors#importing-from-a-client).                                                                        |

`connector import` reads its variables from the shell you run it in, not from
the daemon.

### What stdio connectors see

A stdio connector's process gets the daemon's environment minus every variable
whose name starts with `WAYFINDR_` (in any case), plus the connector's own
[`env`](./configuration#env) table. So the master key and client token never
reach an upstream, unless a connector's `env` sets them explicitly.

### What the login service sees

launchd and systemd start the daemon with their own environment, not your
shell's. `wayfindr service install` copies these variables from the shell you
run it in into the unit, when they are set:

| Variable            | Why                                                                               |
| ------------------- | --------------------------------------------------------------------------------- |
| `PATH`              | So stdio connectors find `npx`, `uvx`, and the like.                              |
| `WAYFINDR_CONFIG`   | So the daemon reads the same config file. `--config` wins over the shell's value. |
| `WAYFINDR_DATA_DIR` | So it uses the same data directory. `--data-dir` wins over the shell's value.     |
| `XDG_CONFIG_HOME`   | Same, for the default config path.                                                |
| `XDG_DATA_HOME`     | Same, for the default data directory.                                             |
| `DO_NOT_TRACK`      | So the usage report stays off under the service.                                  |

It never writes `WAYFINDR_SECRET_KEY` or `WAYFINDR_CLIENT_TOKEN`, because they
hold secrets and unit files are readable by other processes. If either is set,
install warns:

```text
warning: WAYFINDR_SECRET_KEY is set in this shell but is not written to the unit, because it holds a secret; the service will not see it and uses the keychain or the files in the data directory instead
```

The service then reads the master key from the OS keychain or `master.key`, and
the client token from `client-token`. Variables your config reads with
`${env:…}` don't reach the service either; store those values with
[`wayfindr secret set`](./cli#wayfindr-secret-set) instead. Run
`service install` again after changing any of these. See
[As a login service](/guide/running#as-a-login-service).

### Development only

`WAYFINDR_TEST_PG_DSN` is read only by Wayfindr's own test suite, to run the
Postgres store tests. The daemon never reads it.

`WAYFINDR_TELEMETRY_ENDPOINT` replaces the collector the daemon sends the
[usage report](./telemetry) to: a base URL, to which it adds `/v1/report`. It is
for testing a collector, or for watching what a report holds. It sends nothing
on its own: telemetry must still be on.
