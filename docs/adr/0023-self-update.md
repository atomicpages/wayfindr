# 0023 — Update the binary from GitHub Releases with `wayfindr update`

Status: Accepted (2026-11-22).

## Context

Upgrading Wayfindr meant re-running `install.sh` or `install.ps1`, then
stopping the daemon or restarting the login service by hand. A step that is
easy to forget leaves the old binary running: the daemon
([0003](./0003-single-daemon)) keeps it until it restarts, and neither login
service unit restarts a daemon that exits cleanly.

The installers already settle what a download must pass. They fetch the
release archive for the OS and CPU from GitHub over HTTPS and check it against
the same release's `checksums.txt`. They don't verify build provenance; the
installation guide leaves that to `gh attestation verify`.

## Decision

- **`wayfindr update [version]`** installs the latest release, or the named
  one (downgrades included), in place of the running executable, symlinks
  resolved. `--check` reports whether a newer release exists and exits `2` when
  one does, like `status --check`; it never downloads an archive. A running
  daemon is not needed.
- **The installers' trust model, unchanged.** HTTPS to GitHub, and the
  archive's SHA-256 checked against `checksums.txt` from the same release
  before anything is written. That gives integrity, not authenticity: whoever
  can publish a release can publish its checksums. Provenance stays a manual
  `gh attestation verify`, as for a download by hand.
- **Latest from the `/releases/latest` redirect, not the API.** The tag comes
  from the `Location` of `https://github.com/atomicpages/wayfindr/releases/latest`,
  not followed. It has no API rate limit and needs no token, and it skips
  pre-releases and drafts, the same rule as the installers. Versions compare
  as semver; `dev` is older than any release, and a `git describe` build is
  newer than its base tag.
- **The same archive choice as the installers.** `wayfindr-<os>-<arch>`, with
  arm64 for an amd64 binary under Rosetta, and on Windows the running exe
  renamed to `wayfindr.exe.old` before the new one goes in.
- **Restart, unless `--no-restart`.** With a login service installed, the unit
  is reloaded, because `KeepAlive {SuccessfulExit: false}` and
  `Restart=on-failure` don't restart a clean exit: a stop alone would leave the
  service down until the next login. As with `service install`, a daemon that
  holds the lock is stopped just before the reload, or the unit's new daemon
  would find it running and exit, and stay down. Otherwise a running daemon is stopped, and
  the next stdio client's shim starts the new binary. A restart that fails
  after the swap is an error that says the binary was updated.
- **No mirrors.** `update` reads only GitHub; `WAYFINDR_DOWNLOAD_URL` stays an
  installer option. Mirrors and other install directories are the installer's
  job.
- **No background checks.** Nothing looks for a new release unless the user
  runs `update`. Wayfindr makes no request of its own, beyond what the
  connectors need, unless the user opts in: the stance of
  [0020](./0020-opt-in-usage-telemetry).

## Considered options

- **Only the installers.** Nothing new to maintain, but every upgrade stays two
  manual steps, and the second is the one people miss.
- **The GitHub releases API.** Richer data, but rate-limited without a token
  (60 requests an hour per address), and it would need its own pre-release
  filter to match the installers.
- **Verifying the attestation in the binary.** Authenticity, not just
  integrity, but it means a Sigstore verifier and its trust root in the binary,
  kept current. `gh` already does it.
- **Stopping the daemon even under a service.** Simpler, but the unit wouldn't
  start it again: the service would be down until the next login.

## Consequences

- An upgrade is one command. With a login service, the new code runs when it
  returns; without one, it runs once the next client (or `wayfindr start`)
  starts the daemon.
- A tampered release with matching checksums installs, as it would through the
  installers. Users who need provenance verify by hand.
- A binary in a directory the user can't write, such as `/usr/local/bin`,
  can't update itself without `sudo`; the error names the path. Under `sudo`
  it restarts nothing, since a restart as root would reach root's daemon, not
  the user's, and it says what to run as the user instead.
- Users behind a mirror, or with no route to GitHub, keep using the installer.
