package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// exitCodeError ends a command with an exit code other than 1, for a
// result a script branches on rather than a failure, such as
// `status --check` finding a connector that isn't ready. The command has
// printed what it means, so there is no "Error:" line for it.
type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// exitWith ends cmd with code and no "Error:" line: cobra prints a
// returned error unless the command that ran silences it.
func exitWith(cmd *cobra.Command, code int) error {
	cmd.SilenceErrors = true

	return &exitCodeError{code: code}
}

// exitCode is the process exit code for the error a command returned: an
// exitCodeError's code, and 1 for any other error.
func exitCode(err error) int {
	if e := (*exitCodeError)(nil); errors.As(err, &e) {
		return e.code
	}

	return 1
}
