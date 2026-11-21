package telemetry

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/atomicpages/wayfindr/internal/newfile"
	"github.com/atomicpages/wayfindr/internal/paths"
)

// installID reads the install ID from path, writing a new random one first
// when the file is missing or holds no install ID. The ID is a lowercase
// UUIDv4, tied to nothing about the machine or the user.
func installID(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the daemon's own data file

	switch {
	case err == nil:
		if id := strings.TrimSpace(string(data)); isInstallID(id) {
			return id, nil
		}

		if err := os.Remove(path); err != nil {
			return "", fmt.Errorf("replace install ID: %w", err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("read install ID: %w", err)
	}

	id := uuid.New().String()

	if err := newfile.Create(path, []byte(id+"\n"), paths.SecretFileMode, path); err != nil {
		return "", fmt.Errorf("write install ID: %w", err)
	}

	return id, nil
}

// isInstallID reports whether s is a lowercase UUIDv4.
func isInstallID(s string) bool {
	u, err := uuid.Parse(s)

	return err == nil && u.Version() == 4 && u.String() == s
}
