# Installation

On macOS or Linux:

```sh
curl -fsSL https://atomicpages.github.io/wayfindr/install.sh | sh
```

On Windows, in PowerShell:

```powershell
irm https://atomicpages.github.io/wayfindr/install.ps1 | iex
```

The installer downloads the latest release for your OS and CPU from GitHub
Releases and checks it against the release's `checksums.txt`. If the checksum
doesn't match, nothing is installed. It then puts one static binary in place:

| OS           | Installs to                                         | PATH                                                                                      |
| ------------ | --------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| macOS, Linux | `~/.local/bin/wayfindr`                             | Not changed. If the directory isn't on your `PATH`, the installer prints the line to add. |
| Windows      | `%LOCALAPPDATA%\Programs\wayfindr\bin\wayfindr.exe` | Added to your user `PATH`. Open a new terminal to use it.                                 |

It needs no `sudo` and doesn't edit your shell profile. Then check it:

```sh
wayfindr version
```

```text
wayfindr v0.1.0 (commit a6fe2cf, go1.26.1)
```

It prints the version, the commit, and the Go version it was built with.

## Options

To install a specific version, pass its tag:

```sh
curl -fsSL https://atomicpages.github.io/wayfindr/install.sh | sh -s -- v0.1.0
```

```powershell
$env:WAYFINDR_VERSION = "v0.1.0"; irm https://atomicpages.github.io/wayfindr/install.ps1 | iex
```

Both installers read these environment variables:

| Variable                | Default                                                   | Effect                                                                                       |
| ----------------------- | --------------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| `WAYFINDR_VERSION`      | `latest`                                                  | Release tag to install. On macOS and Linux, an argument after `sh -s --` takes precedence.   |
| `WAYFINDR_INSTALL_DIR`  | `~/.local/bin`, or `%LOCALAPPDATA%\Programs\wayfindr\bin` | Directory for the binary.                                                                    |
| `WAYFINDR_DOWNLOAD_URL` | The GitHub release                                        | Base URL that holds the archives and `checksums.txt`, for a mirror. `https://` or `file://`. |

`latest` skips pre-releases such as `v0.2.0-rc.1`. To install one, name its tag.

## Supported platforms

| OS      | amd64 | arm64 |
| ------- | :---: | :---: |
| macOS   |  yes  |  yes  |
| Linux   |  yes  |  yes  |
| Windows |  yes  |  yes  |

The Linux binaries are static, so they run on any distribution, including
Alpine.

Windows has no [login service](./running#as-a-login-service); that page says what
to use instead.

## Download by hand

Each [release](https://github.com/atomicpages/wayfindr/releases) has one archive
per platform, named `wayfindr-<os>-<arch>.tar.gz` (`.zip` on Windows), and a
`checksums.txt`. Check the archive before you unpack it:

```sh
sha256sum --ignore-missing -c checksums.txt     # on macOS: shasum -a 256 --ignore-missing -c checksums.txt
```

The checksum catches a corrupted download. To confirm that the archive was built
from this repository by its release workflow, verify its build provenance with
the [GitHub CLI](https://cli.github.com/):

```sh
gh attestation verify wayfindr-darwin-arm64.tar.gz -R atomicpages/wayfindr
```

::: warning
The macOS binaries aren't notarized. A `curl` download, like the
installer's, runs as is. An archive downloaded with a browser is quarantined,
and Gatekeeper blocks it. Clear the flag with
`xattr -d com.apple.quarantine wayfindr`.
:::

## Build from source

With Go 1.26 or later:

```sh
git clone https://github.com/atomicpages/wayfindr.git
cd wayfindr
make build                       # writes bin/wayfindr
cp bin/wayfindr ~/.local/bin/    # or any directory on your PATH
```

`make build` sets the version and the commit. The version comes from
`git describe` over the `v*` tags, so a build between releases looks like
`v0.1.0-3-ga6fe2cf`. A plain `go build ./cmd/wayfindr` works too, but `version`
then prints `dev`.

## Requirements

The binary has no runtime dependencies. The installer needs `curl`, `tar`, and
`sha256sum` or `shasum` on macOS and Linux, and Windows PowerShell 5.1 or
PowerShell 7 on Windows. An OS keychain is optional: Wayfindr keeps its master
key in the macOS Keychain, the Linux Secret Service, or the Windows Credential
Manager, and falls back to a key file. See
[Master key](/reference/paths-and-environment#master-key).

## Put it on your PATH

Clients start Wayfindr by running `wayfindr stdio`, so the binary must be on the
`PATH` that your harness sees, not only your shell's. `~/.local/bin` and
`/usr/local/bin` are common choices.

::: tip
If a harness can't find `wayfindr`, give it the absolute path instead,
such as `/Users/you/.local/bin/wayfindr`, in the `command` of its MCP config.
:::

## Upgrade

```sh
wayfindr update --check   # is there a newer release?
wayfindr update           # install it and restart the daemon
```

```text
Updated wayfindr v0.1.0 → v0.2.0 at /Users/you/.local/bin/wayfindr
```

`update` downloads the latest release for your OS and CPU, checks it against the
release's `checksums.txt` as the installer does, and replaces the binary in
place. `--check` installs nothing and exits `2` when a newer release exists. To
install a given version, older ones included, name it:
`wayfindr update v0.1.0`. See [`wayfindr update`](../reference/cli#wayfindr-update)
for every flag.

You can also run the installer again. It replaces the binary in place too. Use
it to install from a mirror (`WAYFINDR_DOWNLOAD_URL`) or into another directory
(`WAYFINDR_INSTALL_DIR`), which `update` doesn't support.

A running daemon keeps the old binary until it restarts. `update` restarts it
for you: it restarts the [login service](./running#as-a-login-service) if you
installed one, and otherwise stops the daemon so the next client starts the new
binary. Pass `--no-restart` to skip that. Run with `sudo`, `update` restarts
nothing, since it would reach root's daemon rather than yours: run
`wayfindr stop` (or `wayfindr service install`) as yourself afterwards. The
installer doesn't restart anything: stop the daemon with `wayfindr stop` and
let the shim start the new one, or restart the service. See [Running the daemon](./running). On Windows,
both rename the running `wayfindr.exe` to `wayfindr.exe.old` and remove the
leftovers on the next upgrade.

An older binary can't open a store that a newer one has migrated. See
[Upgrades and downgrades](./storage#upgrades-and-downgrades) before you
downgrade.

## Uninstall

1. If you installed the login service, remove it: `wayfindr service uninstall`.
   It also stops the daemon. (macOS and Linux only.)
2. Otherwise stop the daemon: `wayfindr stop`.
3. Delete the binary: `~/.local/bin/wayfindr` on macOS and Linux, or the
   `wayfindr.exe` in `%LOCALAPPDATA%\Programs\wayfindr\bin` on Windows. The
   Windows installer also added that directory to your user `PATH`; remove it
   there if you want.
4. Remove Wayfindr from each client you added it to, for example
   `claude mcp remove wayfindr`.

That leaves your config and data in place, so a later install picks up where
you left off. To delete them too, remove the
[config file and data directory](/reference/paths-and-environment), by default
`~/.config/wayfindr/` and `~/.local/share/wayfindr/`. The data directory holds
the database, so it deletes your stored secrets and OAuth credentials.

::: warning
The master key may live in your OS keychain, not in the data
directory. Deleting the data directory doesn't remove it, and every data
directory on your account shares it. To remove it, delete the keychain item with
service `wayfindr` and account `master-key`; on macOS,
`security delete-generic-password -s wayfindr -a master-key`. Do this only when
you are done with every Wayfindr data directory on the account.
:::

## Next

Run through the [quick start](./quick-start).
