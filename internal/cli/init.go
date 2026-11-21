package cli

import (
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/setup"
)

func newInitCmd(host Host) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create the config, master key, local user, and client token",
		Long: `Write a default config if there is none, create the master key, run the
store migrations, create the local user, and write the client token, then
print config snippets for the supported MCP clients.

Init never overwrites anything, so running it again changes nothing. It
refuses to run while a daemon is listening.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := setup.Init(cmd.Context(), setup.Options{
				Env:      host.Env,
				Keychain: host.Keychain,
				Dial:     host.Dial,
				AskTelemetry: func() bool {
					return confirm(cmd.InOrStdin(), cmd.ErrOrStderr(),
						"Send the maintainers an anonymous daily usage report (counts and versions, never connector or tool names)? [y/N] ", false)
				},
				Logger: *zerolog.Ctx(cmd.Context()),
			})

			if err != nil {
				return err
			}

			return setup.WriteReport(cmd.OutOrStdout(), res)
		},
	}
}
