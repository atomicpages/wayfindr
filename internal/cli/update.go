package cli

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/daemonctl"
	"github.com/atomicpages/wayfindr/internal/daemonlock"
	"github.com/atomicpages/wayfindr/internal/paths"
	"github.com/atomicpages/wayfindr/internal/selfupdate"
	"github.com/atomicpages/wayfindr/internal/service"
)

// exitUpdateAvailable is the exit code of `update --check` when a newer
// release exists, as `status --check` exits 2 when something needs
// attention; 1 stays the code for a check that could not be made.
const exitUpdateAvailable = 2

// downgradeURL is the storage guide's section on running an older binary
// against a store a newer one has migrated.
const downgradeURL = "https://atomicpages.github.io/wayfindr/guide/storage#upgrades-and-downgrades"

// updateCheck is the --json output of `update --check`.
type updateCheck struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
}

func newUpdateCmd(host Host) *cobra.Command {
	var check, noRestart bool
	cmd := &cobra.Command{
		Use:   "update [version]",
		Short: "Install the latest release, or the one named, and restart the daemon",
		Long: `Replace this binary with the latest GitHub release for this OS and CPU, or
with the version named (pre-releases and older versions included), after
checking the archive against the release's checksums.txt. Then restart the
login service if one is installed, or else stop the running daemon so the next
client starts the new binary; --no-restart leaves both as they are. Run
through sudo, it restarts nothing and says what to run as yourself.

With --check it installs nothing: it prints this version and the latest
release (or the version named) and exits 2 when that one is newer, 0
otherwise. 1 still means the check could not be made. --json works only
with --check.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if wantJSON(cmd) && !check {
				return fmt.Errorf("--json is supported by %q only with --check", cmd.CommandPath())
			}

			u := updater{host: host, cmd: cmd, current: host.version()}
			target, named, err := u.target(args)

			if err != nil {
				return err
			}

			if check {
				return u.check(target, named)
			}

			return u.install(target, named, !noRestart)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "print whether a newer release exists and exit 2 if so; install nothing")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "replace the binary but leave the daemon and the login service as they are")
	supportsJSON(cmd)

	return cmd
}

// updater is one run of `wayfindr update`.
type updater struct {
	host    Host
	cmd     *cobra.Command
	current string
}

// target is the release to compare with or install: the version named, or
// else the latest release. named reports which.
func (u updater) target(args []string) (tag string, named bool, err error) {
	if len(args) == 1 {
		tag, err := selfupdate.NormalizeTag(args[0])

		return tag, true, err
	}

	tag, err = u.host.Releases.Latest(u.cmd.Context())

	if err != nil {
		return "", false, fmt.Errorf("find the latest release: %w", err)
	}

	return tag, false, nil
}

// check prints how this version compares with target and exits 2 when
// target is newer.
func (u updater) check(target string, named bool) error {
	behind := selfupdate.Compare(u.current, target) < 0

	if wantJSON(u.cmd) {
		if err := writeJSON(u.cmd.OutOrStdout(), updateCheck{Current: u.current, Latest: target, UpdateAvailable: behind}); err != nil {
			return err
		}
	} else if err := u.say(u.compared(target, named)); err != nil {
		return err
	}

	if behind {
		return exitWith(u.cmd, exitUpdateAvailable)
	}

	return nil
}

// compared says how this version stands against target. When target is
// newer it gives the command that installs it: plain `wayfindr update`
// installs the latest release, so a named target goes in the command.
func (u updater) compared(target string, named bool) string {
	switch c := selfupdate.Compare(u.current, target); {
	case c < 0:
		install := "wayfindr update"

		if named {
			install += " " + target
		}

		return fmt.Sprintf("wayfindr %s → %s available; run `%s` to install it", u.current, target, install)
	case c > 0:
		return fmt.Sprintf("wayfindr %s is newer than %s", u.current, target)
	default:
		return fmt.Sprintf("wayfindr %s is up to date", u.current)
	}
}

// install replaces the running binary with target and restarts the daemon.
// Without a named version it installs only a newer release; a named one is
// installed whenever it differs from this version, older ones included.
func (u updater) install(target string, named, restart bool) error {
	c := selfupdate.Compare(u.current, target)

	switch {
	case c == 0 && named:
		return u.say(fmt.Sprintf("wayfindr %s is already installed; nothing to do", u.current))
	case c >= 0 && !named:
		return u.say(u.compared(target, named))
	}

	exe, err := u.host.executable()

	if err != nil {
		return fmt.Errorf("find this binary: %w", err)
	}

	goos := runtime.GOOS
	asset := selfupdate.AssetName(goos, selfupdate.HostArch(goos, runtime.GOARCH))
	bin, err := u.host.Releases.Fetch(u.cmd.Context(), target, asset)

	if err != nil {
		return fmt.Errorf("download wayfindr %s: %w", target, err)
	}

	if err := selfupdate.Replace(exe, bin, goos); err != nil {
		return fmt.Errorf("install wayfindr %s: %w", target, err)
	}

	if err := u.say(fmt.Sprintf("Updated wayfindr %s → %s at %s", u.current, target, exe)); err != nil {
		return err
	}

	if c > 0 {
		note := fmt.Sprintf("Note: %s is older than %s. An older binary can't open a store that a newer one has migrated: %s", target, u.current, downgradeURL)

		if err := u.say(note); err != nil {
			return err
		}
	}

	if !restart {
		return nil
	}

	// Under sudo the restart would act on root's data directory and
	// launchd domain, not the user's, so the user's daemon would keep
	// running the old binary while update claimed otherwise.
	if user, ok := sudoUser(os.Geteuid(), u.host.Env.Getenv); ok {
		return u.say(fmt.Sprintf("Running as root through sudo, so no daemon was restarted. As %s, run `wayfindr stop` (or `wayfindr service install` if you use the login service) so the daemon runs the new binary.", user))
	}

	return u.restart(target, exe)
}

// sudoUser returns the user who ran this command through sudo: the process
// runs as root (euid 0) and SUDO_USER names another user. That user's daemon
// is the one to restart, and a restart run as root would miss it.
func sudoUser(euid int, getenv func(string) string) (string, bool) {
	user := getenv("SUDO_USER")

	if euid != 0 || user == "" || user == "root" {
		return "", false
	}

	return user, true
}

// restart makes the new binary run: it reloads an installed login service
// (which restarts only a daemon that fails, so stopping it would leave it
// down), or else stops the running daemon so the next client starts the new
// binary. The service's reload first stops whatever daemon holds the lock,
// as `service install` does, so the unit's new daemon doesn't find it
// running and exit. The binary is already in place, so an error says so
// and how to finish by hand.
func (u updater) restart(target, exe string) error {
	ctx := u.cmd.Context()
	p, err := paths.Resolve(u.host.Env)

	if err != nil {
		return fmt.Errorf("updated wayfindr to %s at %s, but the daemon did not restart: %w\nStop it with `wayfindr stop`, or run `wayfindr service install` if you use the login service", target, exe, err)
	}

	opts := serviceOptions(u.host)
	opts.BeforeLoad = stopBeforeLoad(p.LockFile(), func(daemonlock.Holder) error { return nil })
	restarted, err := service.Restart(ctx, opts)

	if err != nil {
		return fmt.Errorf("updated wayfindr to %s at %s, but the login service did not restart: %w\nRestart it with `wayfindr service install`", target, exe, err)
	}

	if restarted {
		return u.say("Restarted the login service.")
	}

	h, err := daemonctl.Stop(ctx, p.LockFile())

	switch {
	case errors.Is(err, daemonctl.ErrNotRunning):
		return nil
	case err != nil:
		return fmt.Errorf("updated wayfindr to %s at %s, but the running daemon did not stop: %w\nStop it with `wayfindr stop`; the next client starts the new binary", target, exe, err)
	}

	return u.say(fmt.Sprintf("Stopped the daemon%s; the next client starts the new binary.", pid(h.PID)))
}

func (u updater) say(line string) error {
	_, err := fmt.Fprintln(u.cmd.OutOrStdout(), line)

	return err
}
