package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/paths"
)

func TestConfigPathPrintsWhereTheFilesWouldBe(t *testing.T) {
	host, root := testHost(t)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"config", "path"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want := "Config    " + filepath.Join(root, "config", "wayfindr", "wayfindr.toml") + "\n" +
		"Data dir  " + filepath.Join(root, "data", "wayfindr") + "\n"

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

// --config and --data-dir win over WAYFINDR_CONFIG and WAYFINDR_DATA_DIR,
// and a relative one is taken from the working directory.
func TestPathFlagsOverrideTheEnvironment(t *testing.T) {
	host, root := testHost(t)
	host.Env = withEnv(host.Env, map[string]string{
		"WAYFINDR_CONFIG":   "/env/wayfindr.toml",
		"WAYFINDR_DATA_DIR": "/env/data",
	})
	dataDir := filepath.Join(root, "elsewhere")
	var stdout, stderr bytes.Buffer

	args := []string{"--config", filepath.Join("work", "w.toml"), "config", "path", "--data-dir", dataDir}

	if code := RunWith(args, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want := "Config    " + filepath.Join(root, "work", "w.toml") + "\nData dir  " + dataDir + "\n"

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

func TestPathFlagsHelp(t *testing.T) {
	host, _ := testHost(t)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"--help"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	for _, want := range []string{"config file (env WAYFINDR_CONFIG)", "data directory (env WAYFINDR_DATA_DIR)"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help = %q, want it to contain %q", stdout.String(), want)
		}
	}
}

func TestConfigCheckCountsConnectorsAndProfiles(t *testing.T) {
	tests := []struct {
		name, toml, want string
	}{
		{"empty", "", "0 connectors, 0 profiles"},
		{"one each", "[connectors.github]\nurl = \"https://x.test/mcp\"\n[profiles.acme]\n", "1 connector, 1 profile"},
		{
			"several",
			"[connectors.github]\nurl = \"https://x.test/mcp\"\n[connectors.fs]\ncommand = \"npx\"\n[profiles.acme]\n[profiles.home]\n",
			"2 connectors, 2 profiles",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, _ := testHost(t)
			path := resolvePaths(t, host).ConfigFile
			writeFile(t, path, tt.toml)
			var stdout, stderr bytes.Buffer

			if code := RunWith([]string{"config", "check"}, &stdout, &stderr, host); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			if diff := cmp.Diff(path+" is valid: "+tt.want+".\n", stdout.String()); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}
		})
	}
}

func TestConfigCheckReportsAnInvalidConfig(t *testing.T) {
	host, _ := testHost(t)
	path := resolvePaths(t, host).ConfigFile
	writeFile(t, path, "[connectors.bad]\nurl = \"https://x.test/mcp\"\ncolour = \"red\"\n")
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"config", "check"}, &stdout, &stderr, host); code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}

	if msg := stderr.String(); !strings.Contains(msg, "load config "+path+":") || !strings.Contains(msg, "connectors.bad.colour") {
		t.Errorf("stderr = %q, want the path and the unknown key", msg)
	}
}

func TestConfigCheckWithoutAConfigSaysToRunInit(t *testing.T) {
	host, _ := testHost(t)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"config", "check"}, &stdout, &stderr, host); code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}

	want := "no config at " + resolvePaths(t, host).ConfigFile + ": run `wayfindr init` first"

	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestConfigCheckReadsTheConfigFlag(t *testing.T) {
	host, root := testHost(t)
	path := filepath.Join(root, "other.toml")
	writeFile(t, path, "")
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"config", "check", "--config", path}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if !strings.HasPrefix(stdout.String(), path+" is valid") {
		t.Errorf("stdout = %q, want it to check %s", stdout.String(), path)
	}
}

// start reads server.listen from the --config file: here it finds the
// daemon there rather than starting one.
func TestStartUsesTheConfigFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)

	host, root := testHost(t)
	listen := srv.Listener.Addr().String()
	path := filepath.Join(root, "other.toml")
	writeFile(t, path, fmt.Sprintf("[server]\nlisten = %q\n", listen))
	holdDaemonLock(t, root, listen)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"--config", path, "start"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if diff := cmp.Diff("Daemon already running at http://"+listen+"\n", stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

// The unit `service install` writes gives the daemon the --config and
// --data-dir of the install, made absolute, over the shell's variables.
func TestServiceInstallPassesThePathFlags(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service manager on " + runtime.GOOS)
	}

	host, root := testHost(t)
	home := filepath.Join(root, "home")
	host.Env.HomeDir = func() (string, error) { return home, nil }
	host.Env = withEnv(host.Env, map[string]string{"WAYFINDR_CONFIG": "/env/wayfindr.toml"})
	host.RunService = func(context.Context, string, ...string) error { return nil }
	var stdout, stderr bytes.Buffer

	args := []string{"--config", "w.toml", "--data-dir", "d", "service", "install"}

	if code := RunWith(args, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	unit := filepath.Join(home, "Library", "LaunchAgents", "dev.wayfindr.daemon.plist")

	if runtime.GOOS == "linux" {
		unit = filepath.Join(root, "config", "systemd", "user", "wayfindr.service")
	}

	b, err := os.ReadFile(unit) //nolint:gosec // G304: the unit file under the test's temp root

	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{filepath.Join(root, "w.toml"), filepath.Join(root, "d")} {
		if !strings.Contains(string(b), want) {
			t.Errorf("unit = %s\nwant it to contain %s", b, want)
		}
	}

	if strings.Contains(string(b), "/env/wayfindr.toml") {
		t.Errorf("unit = %s\nwant the flag, not WAYFINDR_CONFIG from the shell", b)
	}
}

// withEnv is env with vars set over it.
func withEnv(env paths.Env, vars map[string]string) paths.Env {
	getenv := env.Getenv
	env.Getenv = func(k string) string {
		if v, ok := vars[k]; ok {
			return v
		}

		return getenv(k)
	}

	return env
}

func TestConfigPathJSON(t *testing.T) {
	host, root := testHost(t)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"--json", "config", "path"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want := fmt.Sprintf("{\n  \"config_file\": %q,\n  \"data_dir\": %q\n}\n",
		filepath.Join(root, "config", "wayfindr", "wayfindr.toml"), filepath.Join(root, "data", "wayfindr"))

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}
