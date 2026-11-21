# Running the Daemon

Wayfindr is one long-running process, the daemon. It holds every upstream
connection, every credential, and the catalog. There are four ways to run it.

```bash
wayfindr start                # in the background, until `wayfindr stop`
wayfindr start --foreground   # in this terminal, until Ctrl-C
wayfindr stdio                # the shim starts it for you, in the background
wayfindr service install      # at every login, restarted if it fails
```

Each one runs the same daemon. Pick one; you can switch any time.

## In the foreground

```bash
wayfindr start --foreground
```

`-f` is short for `--foreground`. The daemon connects every connector in
parallel, syncs their tools into the catalog, and serves clients on
`server.listen` (`127.0.0.1:7777` by default). It logs JSON lines to stderr.
`--log-level` sets the detail (`info` by default; `debug` shows more), and
`--log-file` appends the log to a file instead:

```bash
wayfindr start --foreground --log-level debug --log-file ~/wayfindr.log
```

Stop it with Ctrl-C or `SIGTERM`. The daemon stops taking connections, gives
requests in flight up to 10 seconds to finish, stops every stdio connector's
process, and closes the store.

Run `wayfindr init` first. Without it, `start` stops with an error that ends in
``run `wayfindr init` first``.

## In the background

```bash
wayfindr start
```

```text
Daemon running at http://127.0.0.1:7777 (pid 4242)
Log: /Users/you/.local/share/wayfindr/wayfindr.log
```

`start` runs the same daemon in the background and returns once it answers. It
keeps running after you close the terminal. It inherits your shell's
environment, so `${env:…}` references work as they do in the foreground. If a
daemon already runs, `start` says so and starts nothing:

```text
Daemon already running at http://127.0.0.1:7777 (pid 4242)
```

If the daemon fails while starting, for example because the config is invalid,
`start` fails at once and points to the log.

Read the log, or follow it as it grows:

```bash
wayfindr logs -n 20           # the last 20 lines
wayfindr logs -f              # keep printing new lines until Ctrl-C
```

Stop the daemon:

```bash
wayfindr stop
```

```text
Stopped the daemon (pid 4242)
```

`wayfindr stop` stops the daemon on your data directory however it was started:
`start`, `start --foreground` in another terminal, the shim, or the login
service. It sends `SIGTERM`, so the daemon shuts down the same way it does on
Ctrl-C, and returns once the daemon has exited. It waits up to 15 seconds, then
fails with `the daemon (pid …) did not stop within 15s`. On Windows, which has
no `SIGTERM`, `stop` ends the daemon at once, without letting requests in flight
finish. A login service stays stopped until your next login or
`wayfindr service install`.

## Started by the shim

Clients that use `wayfindr stdio` start the daemon themselves. When the shim
starts and nothing answers on `server.listen`, it starts the daemon in the
background, detached from your client, and waits up to 5 seconds for it to
answer. The daemon keeps running after the client exits, and every other client
shares it.

The daemon it starts logs to `wayfindr.log` in the data directory
(`~/.local/share/wayfindr/wayfindr.log` by default); read it with
`wayfindr logs`. The shim's own messages go to its stderr, which your client
usually keeps in its MCP log:

```text
{"level":"info","listen":"http://127.0.0.1:7777","message":"daemon not running; starting it"}
{"level":"info","message":"daemon started"}
```

The daemon it starts inherits the shim's environment, which is your client's. If
the daemon doesn't answer in time, the shim fails with:

```text
the daemon did not answer http://127.0.0.1:7777/healthz within 5s; see <data>/wayfindr.log
```

Run `wayfindr logs` to see why. If the daemon later goes away, the next tool
call starts it again.

## As a login service

```bash
wayfindr service install
```

```text
Installed /Users/you/Library/LaunchAgents/dev.wayfindr.daemon.plist
The daemon is running and starts at login. Log: /Users/you/.local/share/wayfindr/wayfindr.log
```

This writes a user-level unit that runs
`wayfindr start --foreground --log-file <data>/wayfindr.log`, and starts it now.
No root access is needed. A daemon already running, such as one a stdio client
started, is stopped first (`Stopped the daemon (pid …); the service runs it
now.`), so the service runs the daemon from then on, with the unit's
environment.

| OS    | Unit                                                                                                       | Restarts                    |
| ----- | ---------------------------------------------------------------------------------------------------------- | --------------------------- |
| macOS | launchd agent `dev.wayfindr.daemon`, in `~/Library/LaunchAgents/dev.wayfindr.daemon.plist`                 | when it exits with an error |
| Linux | systemd user unit `wayfindr.service`, in `$XDG_CONFIG_HOME/systemd/user/` (else `~/.config/systemd/user/`) | on failure, after 5 seconds |

A clean stop stays stopped in both. So does a daemon the service starts while
another one already runs on the data directory: it logs that and exits `0`
rather than being restarted over and over. On macOS the daemon's stdout and stderr also
go to the log file. On Linux they go to the journal
(`journalctl --user -u wayfindr`).

The unit names the `wayfindr` binary by its real path, with symlinks resolved.
Run `wayfindr service install` again after you move the binary or change the
variables below; it replaces the unit and restarts the daemon.

`wayfindr service` works on macOS and Linux only. On Windows and other systems,
run `wayfindr start --foreground` under Task Scheduler or another supervisor.

To remove it:

```bash
wayfindr service uninstall
```

```text
Stopped the daemon and removed /Users/you/Library/LaunchAgents/dev.wayfindr.daemon.plist
```

### The service's environment

launchd and systemd start the daemon with their own small environment, not your
shell's. So the unit copies a few variables from the shell you run `install` in:
`PATH`, the config and data directory settings, and `DO_NOT_TRACK`. It never
copies `WAYFINDR_SECRET_KEY` or `WAYFINDR_CLIENT_TOKEN`; the service reads the
master key from the OS keychain or `<data>/master.key`, and the client token from
`<data>/client-token`.

::: warning
Variables your connectors read with `${env:NAME}` don't reach the
service either. A connector that needs one goes to `auth_error`. Store the value
with `wayfindr secret set` and use `${secret:name}` instead. See
[Secrets](./secrets).
:::

[What the login service sees](/reference/paths-and-environment#what-the-login-service-sees)
has the full list and the warning `install` prints for the two it skips. For a
stdio connector that works in your shell but not under the service, see
[Troubleshooting](./troubleshooting#a-service-doesn-t-see-your-environment).

## Hot reload

The daemon watches its config file. Save it and most changes apply within a
moment, with no restart; connectors you didn't touch keep their connections.
`[server]` and `[storage]` wait for a restart. If `wayfindr.toml` is a symlink
into your dotfiles, the daemon follows it: saving the file it links to applies
within about 2 seconds. [Reloading](/reference/configuration#reloading) lists
what each change does.

If the file you save is invalid, the daemon keeps running the last good config.
`wayfindr status` shows the error until you fix the file:

```text
Config error: the daemon keeps running its last good config.
  load config /Users/you/.config/wayfindr/wayfindr.toml: invalid config: connectors.broken: exactly one of url or command is allowed, not both
```

The daemon reads its own environment once, at start. A new value for a variable
that a `${env:…}` reference reads takes a restart. A new `${secret:…}` value
doesn't: `wayfindr secret set` reconnects every connector that was waiting for
it.

To restart a service-managed daemon, run `wayfindr service install` again.
Otherwise run `wayfindr stop`, then `wayfindr start`. For a daemon the shim
started, `wayfindr stop` is enough: the next tool call starts a new one.

To see what the daemon and your agents are doing, see
[Monitoring](./monitoring).

## One daemon per data directory

The running daemon holds a lock on `<data>/daemon.lock`, which the operating
system drops when it exits, even after a crash. A second daemon on the same data
directory refuses to start; see
["daemon already running"](./troubleshooting#daemon-already-running) and
[Daemon lock](/reference/paths-and-environment#daemon-lock).

Two shims that start at the same moment may both launch a daemon. One wins the
lock, the other exits, and both shims connect to the winner.

To run a second, separate Wayfindr, give it its own config file, data directory,
and `server.listen`. Pass `--config` and `--data-dir` to every command for it,
including the `stdio` command in your client's setup, or set `WAYFINDR_CONFIG`
and `WAYFINDR_DATA_DIR`:

```sh
wayfindr --config ~/work/wayfindr.toml --data-dir ~/work/wayfindr-data init
wayfindr --config ~/work/wayfindr.toml --data-dir ~/work/wayfindr-data start
wayfindr --config ~/work/wayfindr.toml --data-dir ~/work/wayfindr-data config path
```

Edit `server.listen` in the new config before you start it. A daemon that
`start` or `stdio` starts gets the same paths.

## Changing the listen address

Set it in the `[server]` table of your config:

```toml
[server]
listen = "127.0.0.1:7778"
```

`server.listen` takes effect only on a restart. Until then, the CLI and the shim
read the new address from the file, but the daemon still listens on the old one.
So after you edit it:

- `wayfindr status` looks at the new address and reports
  `daemon is not running on 127.0.0.1:7778`.
- The shim finds the old daemon through the lock and says so, rather than start
  a second daemon on the same store:

  ```text
  daemon already running at 127.0.0.1:7777 (pid 98736), but server.listen is now 127.0.0.1:7778: restart the daemon to apply it
  ```

- `wayfindr start` refuses too. In the background it says what the shim says;
  with `--foreground` it ends with
  `...; server.listen is now 127.0.0.1:7778: restart that daemon to apply it`.

Restart the daemon. If it runs as a login service, run
`wayfindr service install`, which restarts it under the service. Otherwise run
`wayfindr stop`, then `wayfindr start`: `stop` finds the old daemon through the
lock, whatever address it listens on. Then update what names the old address:

- clients that use `wayfindr stdio`: restart them, because the shim reads the
  address once, when it starts;
- clients that connect over HTTP, which use `http://<listen>/mcp`;
- OAuth apps you registered with a provider for a connector that leaves
  `redirect_uri` unset. Its redirect URI is `http://<listen>/oauth/callback`, so
  add the new one to the app.

`server.listen` must be a loopback address, such as `127.0.0.1:7777`,
`[::1]:7777`, `localhost:7777`, or another address in `127.0.0.0/8`. See
[Security](./security).

## Rotating the client token

The default [client token](/reference/paths-and-environment#client-token) is a
file that `wayfindr init` writes once and never replaces. To get a new one:

1. Quit the clients that use `wayfindr stdio`. Otherwise the shim starts the
   daemon again on its next call, and `wayfindr init` refuses to run while a
   daemon listens.
2. Stop the daemon: `wayfindr stop`.
3. Delete the token file: `rm -f ~/.local/share/wayfindr/client-token` (your
   `<data>/client-token`).
4. Run `wayfindr init`. It writes a new token and changes nothing else: your
   config, master key, secrets, and credentials stay.
5. Start the daemon: `wayfindr start`. If it runs as a login service, run
   `wayfindr service install` instead, which restarts it.
6. Update your clients. For HTTP clients, export the new value of
   `WAYFINDR_CLIENT_TOKEN` where they run. Start your stdio clients again: the
   shim reads the token when it starts.

If you set the token with `WAYFINDR_CLIENT_TOKEN` instead of the file, set a new
value everywhere it's used and restart the daemon. Rotating the default token
doesn't change scoped client tokens; revoke those with
[`wayfindr token revoke`](/reference/cli#wayfindr-token-revoke).
