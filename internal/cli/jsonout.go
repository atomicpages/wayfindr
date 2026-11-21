package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// jsonFlag is the root's persistent --json flag. Only commands that print
// data take it; supportsJSON marks them where they are built.
const jsonFlag = "json"

// jsonAnnotation is the cobra annotation key that marks a command as
// printing JSON with --json.
const jsonAnnotation = "wayfindr/json"

// supportsJSON marks cmd as one that prints its result as JSON with --json,
// and returns it. The root refuses --json on any command not marked.
func supportsJSON(cmd *cobra.Command) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}

	cmd.Annotations[jsonAnnotation] = "true"

	return cmd
}

// wantJSON reports whether --json was given to cmd.
func wantJSON(cmd *cobra.Command) bool {
	on, err := cmd.Flags().GetBool(jsonFlag)

	return err == nil && on
}

// checkJSON fails when --json was given to a command that doesn't print
// data, so a script doesn't take its text for JSON.
func checkJSON(cmd *cobra.Command) error {
	if wantJSON(cmd) && cmd.Annotations[jsonAnnotation] == "" {
		return fmt.Errorf("--json is not supported by %q", cmd.CommandPath())
	}

	return nil
}

// writeJSON prints v as indented JSON and a newline.
func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")

	if err != nil {
		return err
	}

	_, err = w.Write(append(b, '\n'))

	return err
}
