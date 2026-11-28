//go:build !(darwin || linux)

package cli

import (
	"os"
	"testing"
)

// openTerminal skips the test: it opens pseudo-terminals on Darwin and
// Linux only.
func openTerminal(t *testing.T) (keys, tty *os.File) {
	t.Helper()
	t.Skip("pseudo-terminals are opened on Darwin and Linux only")

	return nil, nil
}
