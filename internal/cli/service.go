package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/daemonctl"
	"github.com/atomicpages/wayfindr/internal/daemonlock"
	"github.com/atomicpages/wayfindr/internal/paths"
	"github.com/atomicpages/wayfindr/internal/service"
)

func newServiceCmd(host Host) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install or remove the login service that runs the daemon",
		Long: `Manage a user-level service that runs ` + "`wayfindr start --foreground`" + ` at login and
restarts it when it fails: a launchd agent (` + service.Label + `) in
~/Library/LaunchAgents on macOS, or the systemd user unit ` + service.UnitName + `
on Linux. The daemon logs to <data>/wayfindr.log.`,
		Args: cobra.ArbitraryArgs,
		RunE: runGroup,
	}
	cmd.AddCommand(newServiceInstallCmd(host), newServiceUninstallCmd(host))

	return cmd
}

// serviceOptions is the real machine. The service package takes it
// injected so its tests never run launchctl or systemctl. host.Env carries
// --config and --data-dir, so the unit passes them to the daemon as
// WAYFINDR_CONFIG and WAYFINDR_DATA_DIR.
func serviceOptions(host Host) service.Options {
	run := host.RunService

	if run == nil {
		run = service.ExecRunner
	}

	return service.Options{
		GOOS: runtime.GOOS,
		UID:  os.Getuid(),
		Env:  host.Env,
		Run:  run,
	}
}

// stopBeforeLoad is a service.Options.BeforeLoad that stops the daemon
// holding lockFile's lock, as `wayfindr stop` does, just before the unit is
// loaded. Without it the daemon the unit starts finds that one running and
// exits cleanly, which neither service manager restarts, so the service
// would stay down (wayfindr-p9vn). `service install` and `update` both load
// the unit through it. stopped runs once a daemon has stopped; no daemon
// running is not an error.
func stopBeforeLoad(lockFile string, stopped func(daemonlock.Holder) error) func(context.Context) error {
	return func(ctx context.Context) error {
		h, err := daemonctl.Stop(ctx, lockFile)

		switch {
		case errors.Is(err, daemonctl.ErrNotRunning):
			return nil
		case err != nil:
			return fmt.Errorf("stop the running daemon before loading the service: %w", err)
		}

		return stopped(h)
	}
}

func newServiceInstallCmd(host Host) *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install and start the login service",
		Long: `Write the launchd agent or systemd user unit for this binary and (re)start
it. Running it again replaces the unit, for example after moving the binary.
A daemon already running on this data directory is stopped first, so the
service runs the daemon from then on.

The unit passes PATH, WAYFINDR_CONFIG, WAYFINDR_DATA_DIR, XDG_CONFIG_HOME,
and XDG_DATA_HOME from this shell to the daemon; --config and --data-dir
are passed as WAYFINDR_CONFIG and WAYFINDR_DATA_DIR. It never stores
WAYFINDR_SECRET_KEY or WAYFINDR_CLIENT_TOKEN: the service reads the master
key and client token from the keychain or the data directory.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := serviceOptions(host)
			binary, err := service.Executable()

			if err != nil {
				return err
			}

			opts.Binary = binary

			// The daemon running now (started by hand or by a stdio client)
			// stops once the unit is written and just before it is loaded,
			// so the one the service starts takes the lock and the unit's
			// environment; otherwise that one would find this one running
			// and exit (wayfindr-p9vn). An install that can't go ahead (no
			// service manager, a unit that can't be written) stops nothing.
			p, err := paths.Resolve(host.Env)

			if err != nil {
				return err
			}

			stopped := false
			opts.BeforeLoad = stopBeforeLoad(p.LockFile(), func(h daemonlock.Holder) error {
				stopped = true
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "Stopped the daemon (pid %d); the service runs it now.\n", h.PID)

				return err
			})

			res, err := service.Install(cmd.Context(), opts)

			if err != nil && stopped {
				return fmt.Errorf("%w\nThe daemon that was running is stopped: start it with `wayfindr start`", err)
			}

			if err != nil {
				return err
			}

			for _, w := range res.Warnings {
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+w); err != nil {
					return err
				}
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Installed %s\nThe daemon is running and starts at login. Log: %s\n", res.UnitFile, res.LogFile)

			return err
		},
	}
}

func newServiceUninstallCmd(host Host) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the login service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := service.Uninstall(cmd.Context(), serviceOptions(host))

			if err != nil {
				return err
			}

			if !res.Removed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "No service installed (%s not found); nothing to do.\n", res.UnitFile)

				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Stopped the daemon and removed %s\n", res.UnitFile)

			return err
		},
	}
}
