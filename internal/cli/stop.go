package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/daemonctl"
	"github.com/atomicpages/wayfindr/internal/paths"
)

func newStopCmd(host Host) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		Long: `Stop the daemon that runs on this data directory, however it was started,
and wait for it to exit. It finishes requests in flight first (on Windows it
is ended at once). A daemon run as a service stays stopped until the next
login or ` + "`wayfindr service install`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := paths.Resolve(host.Env)

			if err != nil {
				return err
			}

			h, err := daemonctl.Stop(cmd.Context(), p.LockFile())

			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Stopped the daemon%s\n", pid(h.PID))

			return err
		},
	}
}
