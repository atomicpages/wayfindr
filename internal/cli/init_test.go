package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/paths"
	"github.com/atomicpages/wayfindr/internal/testutil/fakekeychain"
)

// testHost is an isolated machine rooted at a temp dir: XDG dirs inside it,
// no home directory, a fake keychain, and no daemon listening.
func testHost(t *testing.T) (Host, string) {
	t.Helper()
	root := t.TempDir()
	vars := map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_DATA_HOME":   filepath.Join(root, "data"),
	}

	return Host{
		Env: paths.Env{
			Getenv:  func(k string) string { return vars[k] },
			HomeDir: func() (string, error) { return "", errors.New("tests must not use the home directory") },
		},
		Keychain: &fakekeychain.Keychain{},
		// Never a terminal, so init does not stop to ask about telemetry.
		Stdin: strings.NewReader(""),
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("connection refused")
		},
		// The temp root, so no scope file around the repository applies.
		Getwd: func() (string, error) { return root, nil },
	}, root
}

func TestInitPrintsWhatItDidAndTheHarnessSnippets(t *testing.T) {
	host, root := testHost(t)
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"init"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want, err := os.ReadFile(filepath.Join("testdata", "init.golden"))

	if err != nil {
		t.Fatal(err)
	}

	// The golden spells paths with forward slashes; Windows prints them with
	// backslashes.
	got := strings.ReplaceAll(filepath.ToSlash(stdout.String()), filepath.ToSlash(root), "<root>")

	if diff := cmp.Diff(string(want), got); diff != "" {
		t.Errorf("stdout differs from testdata/init.golden (-want +got):\n%s", diff)
	}

	// Design §9: the token is never printed; the snippets reference it.
	token, err := os.ReadFile(filepath.Join(root, "data", "wayfindr", "client-token")) //nolint:gosec // inside the test's temp dir.

	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(stdout.String()+stderr.String(), string(token)) {
		t.Error("output contains the client token")
	}
}

func TestSecondInitReportsEverythingKept(t *testing.T) {
	host, _ := testHost(t)

	if code := RunWith([]string{"init"}, &bytes.Buffer{}, &bytes.Buffer{}, host); code != 0 {
		t.Fatalf("first init exit code = %d", code)
	}

	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"init"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if out := stdout.String(); strings.Contains(out, "(created)") || strings.Count(out, "(kept)") != 2 {
		t.Errorf("stdout = %q, want the config and client token reported as kept", out)
	}
}

func TestInitFailsWhileADaemonIsListening(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })
	host, root := testHost(t)
	host.Dial = nil // the real TCP probe
	cfg := filepath.Join(root, "config", "wayfindr", "wayfindr.toml")

	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(cfg, fmt.Appendf(nil, "[server]\nlisten = %q\n", ln.Addr().String()), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"init"}, &stdout, &stderr, host)

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero; stdout: %s", stdout.String())
	}

	if !strings.Contains(stderr.String(), "daemon is listening") {
		t.Errorf("stderr = %q, want it to say a daemon is listening", stderr.String())
	}
}

// TestInitSuggestsImport: a client config in the home directory gets a
// line suggesting `wayfindr connector import`.
func TestInitSuggestsImport(t *testing.T) {
	host, root := testHost(t)
	home := filepath.Join(root, "home")

	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(home, ".cursor", "mcp.json"), []byte(`{"mcpServers": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	host.Env.HomeDir = func() (string, error) { return home, nil }
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"init"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr.String())
	}

	want := "importing them would add:\n\n  wayfindr connector import cursor --dry-run\n"

	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout does not contain %q:\n%s", want, stdout.String())
	}
}
