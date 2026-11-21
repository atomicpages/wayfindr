package config_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/atomicpages/wayfindr/internal/config"
)

// writeFile writes data as a config file in a temp dir and returns its path.
func writeFile(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wayfindr.toml")

	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestListenAddressReadsServerListen(t *testing.T) {
	path := writeFile(t, "[server]\nlisten = \"127.0.0.1:9000\"\n")

	got, err := config.ListenAddress(path)

	if err != nil {
		t.Fatalf("ListenAddress: %v", err)
	}

	if got != "127.0.0.1:9000" {
		t.Errorf("ListenAddress = %q, want %q", got, "127.0.0.1:9000")
	}
}

// A file that Load rejects still yields its listen address, so the CLI can
// reach a daemon that keeps running its last good config.
func TestListenAddressIgnoresProblemsOutsideServerListen(t *testing.T) {
	for name, data := range map[string]string{
		"unknown key in server": "[server]\nlisten = \"127.0.0.1:9000\"\nport = 1\n",
		"unknown table":         "[server]\nlisten = \"127.0.0.1:9000\"\n[bogus]\nx = 1\n",
		"wrong type elsewhere":  "[server]\nlisten = \"127.0.0.1:9000\"\n[storage]\ndriver = 5\n[index]\nrefresh_interval = 3\n",
		"rule violation elsewhere": "[server]\nlisten = \"127.0.0.1:9000\"\n" +
			"[connectors.x]\nurl = \"not a url\"\ncommand = \"also-set\"\n[connectors.x.auth]\ntype = \"basic\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := config.ListenAddress(writeFile(t, data))

			if err != nil {
				t.Fatalf("ListenAddress: %v", err)
			}

			if got != "127.0.0.1:9000" {
				t.Errorf("ListenAddress = %q, want %q", got, "127.0.0.1:9000")
			}
		})
	}
}

// A TOML syntax error anywhere fails the read: the parser can't say where
// [server] ends in a file it can't parse, so a guessed address would be a
// guess.
func TestListenAddressFailsOnAMalformedListenOrASyntaxError(t *testing.T) {
	for name, data := range map[string]string{
		"listen not a string":        "[server]\nlisten = 7777\n",
		"listen not host:port":       "[server]\nlisten = \"7777\"\n",
		"listen not loopback":        "[server]\nlisten = \"0.0.0.0:7777\"\n",
		"syntax error elsewhere":     "[server]\nlisten = \"127.0.0.1:9000\"\n[connectors.x]\nurl = \"unterminated\n",
		"server is not a table":      "server = \"127.0.0.1:9000\"\n",
		"syntax error before server": "oops\n[server]\nlisten = \"127.0.0.1:9000\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := config.ListenAddress(writeFile(t, data))

			if err == nil {
				t.Fatalf("ListenAddress = %q, want an error", got)
			}
		})
	}
}

func TestListenAddressFailsOnAnUnreadableFile(t *testing.T) {
	_, err := config.ListenAddress(filepath.Join(t.TempDir(), "missing.toml"))

	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ListenAddress error = %v, want fs.ErrNotExist", err)
	}
}

func TestListenAddressDefaultsWhenServerListenIsAbsent(t *testing.T) {
	for name, data := range map[string]string{
		"empty file":      "",
		"no server table": "[index]\nrefresh_interval = \"1h\"\n",
		"no listen key":   "[server]\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := config.ListenAddress(writeFile(t, data))

			if err != nil {
				t.Fatalf("ListenAddress: %v", err)
			}

			if got != "127.0.0.1:7777" {
				t.Errorf("ListenAddress = %q, want the default %q", got, "127.0.0.1:7777")
			}
		})
	}
}
