package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/daemon"
	"github.com/atomicpages/wayfindr/internal/daemonctl"
	"github.com/atomicpages/wayfindr/internal/daemonlock"
	"github.com/atomicpages/wayfindr/internal/paths"
)

func newStartCmd(host Host) *cobra.Command {
	var (
		logFile    string
		foreground bool
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the daemon in the background",
		Long: `Start the daemon in the background, unless one already answers, and return
once it answers. The daemon connects every configured connector, keeps the
catalog in sync, and serves MCP clients on the configured loopback address.
Its log goes to wayfindr.log in the data directory, or to --log-file; read it
with ` + "`wayfindr logs`" + ` and stop the daemon with ` + "`wayfindr stop`" + `. Run
` + "`wayfindr init`" + ` first.

With --foreground, run the daemon in this process instead, logging to stderr
or --log-file, until SIGINT or SIGTERM: for a service manager or a container.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if foreground {
				return runForeground(cmd, host, logFile)
			}

			return startDetached(cmd, host, logFile)
		},
	}

	cmd.Flags().BoolVarP(&foreground, "foreground", "f", false, "run the daemon in this process until SIGINT or SIGTERM")
	cmd.Flags().StringVar(&logFile, "log-file", "", "append the JSON log to this file (mode 0600) instead of the default")

	return cmd
}

// newServeCmd is `serve`, the name `start --foreground` had before
// (wayfindr-3e6f): service files and container commands that older versions
// wrote run it, so it stays, hidden, with its old meaning: the daemon in
// this process, or in the background with --detach.
func newServeCmd(host Host) *cobra.Command {
	var (
		logFile string
		detach  bool
	)

	cmd := &cobra.Command{
		Use:    "serve",
		Short:  "Run the daemon in this process (the old name of start --foreground)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// On stderr only: stdout may be what a service or container reads.
			zerolog.Ctx(cmd.Context()).Warn().
				Msg("serve is deprecated: use wayfindr start --foreground, or wayfindr start for serve -d")

			if detach {
				return startDetached(cmd, host, logFile)
			}

			return runForeground(cmd, host, logFile)
		},
	}

	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "run the daemon in the background and return once it answers")
	cmd.Flags().StringVar(&logFile, "log-file", "", "append the JSON log to this file (mode 0600) instead of stderr")

	return cmd
}

// runForeground runs the daemon in this process until ctx ends, logging to
// logFile if it is set.
func runForeground(cmd *cobra.Command, host Host, logFile string) error {
	logger := *zerolog.Ctx(cmd.Context())

	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, paths.SecretFileMode) //nolint:gosec // the path is the user's own flag

		if err != nil {
			return fmt.Errorf("open --log-file: %w", err)
		}

		defer func() { _ = f.Close() }()
		logger = logger.Output(f)
	}

	logger.Info().Str("version", version).Msg("daemon starting")

	err := daemon.Run(cmd.Context(), daemon.Options{
		Env:       host.Env,
		LookupEnv: host.LookupEnv,
		Keychain:  host.Keychain,
		Logger:    logger,
		Version:   version,
		Commit:    commit,
	})

	// A daemon already running on this data directory is not this one's
	// failure, as `wayfindr start` reports it too. The login service runs
	// this command, and a non-zero exit would make the service manager
	// start it again, every few seconds, for ever (wayfindr-p9vn). One
	// started with another server.listen (refused says so) is a warning.
	if held, ok := errors.AsType[*daemonlock.HeldError](err); ok {
		ev := logger.Info()

		if err.Error() != held.Error() {
			ev = logger.Warn()
		}

		ev.Str("reason", err.Error()).Msg("daemon not started: another one runs on this data directory")

		return nil
	}

	return err
}

// startDetached is `start`: the daemon is started in the background, as
// the shim starts it, unless one already answers.
func startDetached(cmd *cobra.Command, host Host, logFile string) error {
	p, err := paths.Resolve(host.Env)

	if err != nil {
		return err
	}

	target, err := daemonTarget(cmd.Context(), p)

	if err != nil {
		return err
	}

	if logFile != "" {
		if logFile, err = filepath.Abs(logFile); err != nil {
			return fmt.Errorf("--log-file: %w", err)
		}
	}

	var args []string

	if f := cmd.Flag("log-level"); f != nil && f.Changed {
		args = append(args, "--log-level", f.Value.String())
	}

	running, err := daemonctl.Ensure(cmd.Context(), target, daemonctl.StartOptions{Args: args, LogFile: logFile})

	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()

	if !running.Started {
		_, err = fmt.Fprintf(out, "Daemon already running at %s%s\n", target.URL(), pid(running.Holder.PID))

		return err
	}

	if _, err = fmt.Fprintf(out, "Daemon running at %s%s\n", target.URL(), pid(running.Holder.PID)); err != nil || running.LogFile == "" {
		return err
	}

	_, err = fmt.Fprintf(out, "Log: %s\n", running.LogFile)

	return err
}

// daemonTarget is the daemon of p, for starting it in the background. Only
// problems are logged: the commands that start it print the outcome.
func daemonTarget(ctx context.Context, p paths.Paths) (daemonctl.Target, error) {
	logger := *zerolog.Ctx(ctx)
	logger = logger.Level(max(logger.GetLevel(), zerolog.WarnLevel))
	target, err := daemonctl.NewTarget(p, logger)

	if errors.Is(err, fs.ErrNotExist) {
		return daemonctl.Target{}, fmt.Errorf("%w: run `wayfindr init` first", err)
	}

	return target, err
}

// pid formats a daemon's pid for output: nothing when it is not known.
func pid(n int) string {
	if n == 0 {
		return ""
	}

	return fmt.Sprintf(" (pid %d)", n)
}
