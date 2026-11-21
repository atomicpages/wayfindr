package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/atomicpages/wayfindr/internal/paths"
)

func TestForegroundLogFileTakesTheJSONLogWithMode0600(t *testing.T) {
	host, root := testHost(t)
	logFile := filepath.Join(root, "wayfindr.log")
	var stdout, stderr bytes.Buffer

	// No init has run, so the daemon fails at startup: the log line before
	// it shows where the log goes, and the error still reaches stderr.
	code := RunWith([]string{"start", "--foreground", "--log-file", logFile}, &stdout, &stderr, host)

	if code == 0 {
		t.Fatal("exit code = 0, want non-zero: there is no config")
	}

	if !strings.Contains(stderr.String(), "wayfindr.toml") {
		t.Errorf("stderr = %q, want the startup error naming the config file", stderr.String())
	}

	data, err := os.ReadFile(logFile) //nolint:gosec // inside the test's temp dir.

	if err != nil {
		t.Fatalf("read log file: %v", err)
	}

	if !strings.Contains(string(data), `"message":"daemon starting"`) {
		t.Errorf("log file = %q, want the JSON startup line", data)
	}

	if strings.Contains(stderr.String(), "daemon starting") {
		t.Errorf("stderr = %q, want the log in the file only", stderr.String())
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(logFile)

		if err != nil {
			t.Fatal(err)
		}

		if got := info.Mode().Perm(); got != paths.SecretFileMode {
			t.Errorf("log file mode = %v, want %v", got, paths.SecretFileMode)
		}
	}
}
