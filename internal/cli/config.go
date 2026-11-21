package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/paths"
)

func newConfigCmd(host Host) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show where the config file is and check it",
		Long: `Show the config file and data directory every command uses, and check the
config file the way the daemon loads it. --config and --data-dir, then
WAYFINDR_CONFIG and WAYFINDR_DATA_DIR, then the XDG defaults choose them.`,
		Args: cobra.ArbitraryArgs,
		RunE: runGroup,
	}
	cmd.AddCommand(newConfigPathCmd(host), newConfigCheckCmd(host))

	return cmd
}

// configPaths is `config path --json`.
type configPaths struct {
	ConfigFile string `json:"config_file"`
	DataDir    string `json:"data_dir"`
}

func newConfigPathCmd(host Host) *cobra.Command {
	return supportsJSON(&cobra.Command{
		Use:   "path",
		Short: "Print the config file and data directory",
		Long: `Print the config file and data directory every command uses, whether or not
they exist yet.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := paths.Resolve(host.Env)

			if err != nil {
				return err
			}

			if wantJSON(cmd) {
				return writeJSON(cmd.OutOrStdout(), configPaths{ConfigFile: p.ConfigFile, DataDir: p.DataDir})
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Config    %s\nData dir  %s\n", p.ConfigFile, p.DataDir)

			return err
		},
	})
}

func newConfigCheckCmd(host Host) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Check the config file as the daemon would load it",
		Long: `Load the config file with the checks the daemon runs at startup and report
the first problems, or how many connectors and profiles it defines. It reads
only the file: it needs no daemon, doesn't restart one that runs, and doesn't
open the store or the keychain. Exits 1 when the file is missing or invalid.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, src, err := readConfigFile(host)

			if err != nil {
				return err
			}

			cfg, err := config.Parse(src, configOptions(host, p))

			if err != nil {
				// The daemon's own wording (config.Load), which names the file.
				return fmt.Errorf("load config %s: %w", p.ConfigFile, err)
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is valid: %s, %s.\n", p.ConfigFile,
				countConnectors(len(cfg.Connectors)), count(len(cfg.Profiles), "profile", "profiles"))

			return err
		},
	}
}

// count is n with the noun's singular or plural.
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}

	return fmt.Sprintf("%d %s", n, many)
}
