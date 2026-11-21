package cli

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/daemonctl"
	"github.com/atomicpages/wayfindr/internal/paths"
)

func newLogsCmd(host Host) *cobra.Command {
	var (
		follow bool
		lines  int
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Print the daemon's log",
		Long: `Print the JSON log of a daemon that runs in the background: one started
with ` + "`wayfindr start`" + `, by ` + "`wayfindr stdio`" + `, or as a service. It is
wayfindr.log in the data directory. With --follow, keep printing new lines
until Ctrl-C.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := paths.Resolve(host.Env)

			if err != nil {
				return err
			}

			err = daemonctl.Tail(cmd.Context(), p.LogFile(), cmd.OutOrStdout(), lines, follow)

			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("no log at %s: a daemon logs there when started with `wayfindr start`, by `wayfindr stdio`, or as a service", p.LogFile())
			}

			return err
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing lines as they are logged")
	cmd.Flags().IntVarP(&lines, "tail", "n", -1, "print only the last N lines (default all)")

	return cmd
}
