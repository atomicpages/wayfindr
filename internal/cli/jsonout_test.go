package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestVersionJSON(t *testing.T) {
	version, commit = "1.2.3", "abc1234"
	t.Cleanup(func() { version, commit = "dev", "unknown" })

	var stdout, stderr bytes.Buffer

	if code := Run([]string{"version", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	var got map[string]string

	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, stdout.String())
	}

	want := map[string]string{"version": "1.2.3", "commit": "abc1234", "go": runtime.Version()}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("version (-want +got):\n%s", diff)
	}
}

// TestJSONIsRefusedByCommandsThatPrintNoData checks that --json fails a
// command that has no JSON output before it does anything, wherever the
// flag is placed.
func TestJSONIsRefusedByCommandsThatPrintNoData(t *testing.T) {
	t.Run("init", func(t *testing.T) {
		host, root := testHost(t)
		stdout, stderr, code := run(t, host, "init", "--json")

		wantRefused(t, "wayfindr init", stdout, stderr, code)

		for _, dir := range []string{"config", "data"} {
			if _, err := os.Stat(filepath.Join(root, dir)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("init --json created %s (stat: %v), want nothing written", dir, err)
			}
		}
	})

	t.Run("stop", func(t *testing.T) {
		host, _ := testHost(t)
		stdout, stderr, code := run(t, host, "--json", "stop")

		wantRefused(t, "wayfindr stop", stdout, stderr, code)
	})

	t.Run("connector add", func(t *testing.T) {
		host, path, before := connectorHost(t, "127.0.0.1:1")
		stdout, stderr, code := run(t, host, "connector", "add", "fs", "--no-wait", "--json", "--", "npx", "server")

		wantRefused(t, "wayfindr connector add", stdout, stderr, code)

		if diff := cmp.Diff(before, readFile(t, path)); diff != "" {
			t.Errorf("config file changed (-before +after):\n%s", diff)
		}
	})
}

func wantRefused(t *testing.T, path, stdout, stderr string, code int) {
	t.Helper()

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}

	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}

	want := "Error: --json is not supported by \"" + path + "\"\n"

	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

func TestJSONIsAGlobalFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := Run([]string{"status", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	_, global, ok := strings.Cut(stdout.String(), "Global Flags:\n")

	if !ok || !strings.Contains(global, "--json") || !strings.Contains(global, "print the result as JSON (commands that print data)") {
		t.Errorf("help does not list --json under Global Flags:\n%s", stdout.String())
	}
}
