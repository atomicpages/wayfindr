package cli

import (
	"fmt"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/shim"
)

func newStdioCmd(host Host) *cobra.Command {
	var profile string

	cmd := &cobra.Command{
		Use:   "stdio",
		Short: "Serve MCP on stdin and stdout for an MCP client (the client runs it)",
		Long: `Serve MCP on stdin and stdout for clients that only speak stdio, forwarding
tools/list and tools/call to the daemon. If the daemon is not running, start
it in the background first. Logs go to stderr; a daemon started this way logs
to wayfindr.log in the data directory.

If a .wayfindr.toml in the working directory or a parent (below your home
directory) applies, every request carries it as the project scope, which can
only narrow the connectors and tools the config exposes. A scope file that
doesn't validate stops the command; see ` + "`wayfindr scope`" + `.

With --profile, every request also names a profile, defined as
[profiles.<name>] in the config (ADR 0011). The daemon applies it as it has it
now, ANDed with the project scope; a name the config doesn't define gets every
request refused. ` + "`wayfindr scope --profile <name>`" + ` shows what it exposes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := host.workDir()

			if err != nil {
				return fmt.Errorf("working directory: %w", err)
			}

			return shim.Run(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), shim.Options{
				Env:     host.Env,
				Dir:     dir,
				Logger:  *zerolog.Ctx(cmd.Context()),
				Version: version,
				Profile: profile,
			})
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "scope every request by the config's profile `name` ([profiles.<name>])")

	return cmd
}
