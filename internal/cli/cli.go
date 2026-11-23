// Package cli holds the cobra commands. Commands parse flags and delegate;
// they contain no domain logic.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/paths"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/selfupdate"
	"github.com/atomicpages/wayfindr/internal/service"
	"github.com/atomicpages/wayfindr/internal/setup"
)

// Set at build time with -ldflags "-X github.com/atomicpages/wayfindr/internal/cli.version=…".
// The one allowed kind of package-level mutable state (CODING_STANDARDS.md, State).
var (
	version = "dev"
	commit  = "unknown"
)

// Execute runs the CLI against the process's args and streams and exits.
func Execute() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}

// Host is what commands read from the machine they run on. Run uses the
// real one; tests pass fakes to RunWith so they never touch the user's home,
// data, or keychain.
type Host struct {
	// Env is the process environment and home directory.
	Env paths.Env
	// LookupEnv resolves ${env:…} config references, with os.LookupEnv
	// semantics; nil means os.LookupEnv.
	LookupEnv func(key string) (string, bool)
	// Keychain is the OS keychain; nil means none is available.
	Keychain secrets.Keychain
	// Dial probes for a running daemon; nil means a plain TCP dial.
	Dial setup.Dialer
	// Stdin is what commands read input from, such as a secret's value;
	// nil means os.Stdin.
	Stdin io.Reader
	// OpenURL opens a URL in the user's browser (`wayfindr auth login`); nil
	// means the platform's opener (open, xdg-open, or rundll32).
	OpenURL func(ctx context.Context, url string) error
	// Getwd returns the working directory, where the project scope is
	// looked up; nil means os.Getwd.
	Getwd func() (string, error)
	// RunService runs launchctl or systemctl for `wayfindr service`; nil
	// means service.ExecRunner.
	RunService service.Runner
	// Releases is where `wayfindr update` finds and downloads releases; the
	// zero value is GitHub.
	Releases selfupdate.Source
	// Executable returns the path `wayfindr update` replaces, symlinks
	// resolved; nil means service.Executable, the running binary.
	Executable func() (string, error)
	// Version is the running binary's version as `wayfindr update` compares
	// it with a release; "" means the one set at build time, so tests can
	// pick one without assigning the package's version.
	Version string
}

// executable is the binary to replace, from Executable or
// service.Executable.
func (h Host) executable() (string, error) {
	if h.Executable != nil {
		return h.Executable()
	}

	return service.Executable()
}

// version is the running binary's version, from Version or the build.
func (h Host) version() string {
	if h.Version != "" {
		return h.Version
	}

	return version
}

// workDir is the working directory, from Getwd or os.Getwd.
func (h Host) workDir() (string, error) {
	if h.Getwd != nil {
		return h.Getwd()
	}

	return os.Getwd()
}

// OSHost returns the Host backed by the real process environment and OS keychain.
func OSHost() Host {
	return Host{Env: paths.OS(), LookupEnv: os.LookupEnv, Keychain: secrets.OSKeychain{}}
}

// Run executes the CLI with the given args and streams and returns the exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	return RunWith(args, stdout, stderr, OSHost())
}

// RunWith is Run on the given host.
func RunWith(args []string, stdout, stderr io.Writer, host Host) int {
	root := newRootCmd(host)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	if host.Stdin != nil {
		root.SetIn(host.Stdin)
	}

	if err := root.Execute(); err != nil {
		return exitCode(err)
	}

	return 0
}

// Run the root's PersistentPreRunE even when a subcommand defines its own,
// so every command gets a validated --log-level and a logger. Cobra only
// offers this as a global switch (CODING_STANDARDS.md, State).
func init() {
	cobra.EnableTraverseRunHooks = true
}

// logLevels returns the accepted --log-level values, in order of verbosity.
func logLevels() []zerolog.Level {
	return []zerolog.Level{
		zerolog.TraceLevel, zerolog.DebugLevel, zerolog.InfoLevel, zerolog.WarnLevel,
		zerolog.ErrorLevel, zerolog.FatalLevel, zerolog.PanicLevel, zerolog.Disabled,
	}
}

// parseLogLevel accepts level names only; zerolog.ParseLevel would also accept numbers.
func parseLogLevel(name string) (zerolog.Level, bool) {
	for _, l := range logLevels() {
		if strings.EqualFold(name, l.String()) {
			return l, true
		}
	}

	return zerolog.NoLevel, false
}

func logLevelNames() string {
	levels := logLevels()
	names := make([]string, len(levels))

	for i, l := range levels {
		names[i] = l.String()
	}

	return strings.Join(names, ", ")
}

func newRootCmd(host Host) *cobra.Command {
	var (
		logLevel string
		pf       pathFlags
	)

	// Before the commands are built, so each one's copy of host has it.
	host.Env = pf.overlay(host.Env)
	root := &cobra.Command{
		Use:          "wayfindr",
		Short:        "MCP gateway that holds credentials for many MCP servers",
		SilenceUsage: true,
		// Subcommands get the logger with zerolog.Ctx(cmd.Context()).
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkJSON(cmd); err != nil {
				return err
			}

			level, ok := parseLogLevel(logLevel)

			if !ok {
				return fmt.Errorf("invalid --log-level %q: want one of %s", logLevel, logLevelNames())
			}

			logger := zerolog.New(cmd.ErrOrStderr()).Level(level).With().Timestamp().Logger()
			cmd.SetContext(logger.WithContext(cmd.Context()))

			return pf.absolute(host.workDir)
		},
	}

	root.PersistentFlags().StringVar(&logLevel, "log-level", zerolog.InfoLevel.String(), "log level: "+logLevelNames())
	root.PersistentFlags().StringVar(&pf.config, "config", "", "config file (env "+paths.ConfigEnvVar+")")
	root.PersistentFlags().StringVar(&pf.dataDir, "data-dir", "", "data directory (env "+paths.DataDirEnvVar+")")
	root.PersistentFlags().Bool(jsonFlag, false, "print the result as JSON (commands that print data)")
	root.AddCommand(newVersionCmd(), newInitCmd(host), newStartCmd(host), newServeCmd(host))
	root.AddCommand(newStopCmd(host), newLogsCmd(host))
	root.AddCommand(newStatusCmd(host), newCallsCmd(host), newSearchCmd(host), newStdioCmd(host))
	root.AddCommand(newSecretCmd(host), newServiceCmd(host), newAuthCmd(host), newUICmd(host))
	root.AddCommand(newConnectorCmd(host), newScopeCmd(host), newTokenCmd(host))
	root.AddCommand(newConfigCmd(host), newUpdateCmd(host))
	groupCommands(root)

	return root
}

// groupCommands sorts the root's commands into the groups its help lists
// them under. The groups are assigned here, not in each command's file, so
// the root help's layout reads in one place.
func groupCommands(root *cobra.Command) {
	groups := []struct {
		id, title string
		commands  []string
	}{
		{"daemon", "Daemon:", []string{"init", "start", "stop", "logs", "service", "update"}},
		{"inspect", "Inspect:", []string{"status", "calls", "search", "scope", "ui"}},
		{"manage", "Manage:", []string{"connector", "auth", "secret", "token", "config"}},
		{"plumbing", "Plumbing:", []string{"stdio", "version"}},
	}
	byName := make(map[string]string)

	for _, g := range groups {
		root.AddGroup(&cobra.Group{ID: g.id, Title: g.title})

		for _, name := range g.commands {
			byName[name] = g.id
		}
	}

	for _, c := range root.Commands() {
		if id, ok := byName[c.Name()]; ok {
			c.GroupID = id
		}
	}

	root.SetCompletionCommandGroupID("plumbing")
	root.SetHelpCommandGroupID("plumbing")
}

// runGroup is the RunE of a command group such as `connector`: with no
// argument it prints the group's help, and with an unknown subcommand it
// fails, with suggestions, the way an unknown top-level command does. Without
// it cobra prints the help and exits 0, so a script with a typo "succeeds".
func runGroup(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}

	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())

	// Cobra sets the edit distance only in its own lookup of root commands;
	// without it SuggestionsFor matches prefixes alone, and misses "lsit".
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}

	if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n\t" + strings.Join(suggestions, "\n\t")
	}

	return fmt.Errorf("%s\n\nRun '%s --help' for usage", msg, cmd.CommandPath())
}

// versionInfo is the --json output of `version`.
type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Go      string `json:"go"`
}

func newVersionCmd() *cobra.Command {
	return supportsJSON(&cobra.Command{
		Use:   "version",
		Short: "Print the version, commit, and Go version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if wantJSON(cmd) {
				return writeJSON(cmd.OutOrStdout(), versionInfo{Version: version, Commit: commit, Go: runtime.Version()})
			}

			_, err := fmt.Fprintf(cmd.OutOrStdout(), "wayfindr %s (commit %s, %s)\n", version, commit, runtime.Version())

			return err
		},
	})
}
