package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/daemonlock"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

const testToken = "wf_test-token"

// writeStatusHost writes the config, naming listen as the daemon's address,
// and the client token into the test host rooted at root, and holds the
// daemon lock for a daemon on listen, as the daemon serving it would.
func writeStatusHost(t *testing.T, root, listen string) {
	t.Helper()
	writeHostFiles(t, root, listen)
	holdDaemonLock(t, root, listen)
}

// holdDaemonLock takes the daemon lock of the test host rooted at root for
// a daemon on listen until the test ends: the CLI sends the client token
// only to a daemon that holds it (wayfindr-zvjw). The holder's pid is 0, so
// nothing a test runs can signal this process.
func holdDaemonLock(t *testing.T, root, listen string) {
	t.Helper()
	lock, err := daemonlock.Acquire(t.Context(), filepath.Join(root, "data", "wayfindr", "daemon.lock"), daemonlock.Holder{Listen: listen})

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = lock.Release() })
}

// writeHostFiles writes the config, naming listen as the daemon's address,
// and the client token into the test host rooted at root.
func writeHostFiles(t *testing.T, root, listen string) {
	t.Helper()
	cfg := filepath.Join(root, "config", "wayfindr", "wayfindr.toml")
	token := filepath.Join(root, "data", "wayfindr", "client-token")

	for _, f := range []struct{ path, data string }{
		{cfg, fmt.Sprintf("[server]\nlisten = %q\n", listen)},
		{token, testToken + "\n"},
	} {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(f.path, []byte(f.data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// statusDaemon serves a daemon whose status has a ready connector, one
// that needs auth, an orphaned credential, and a config error; the table
// it prints is statusTable.
func statusDaemon(t *testing.T) string {
	t.Helper()
	since := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

	return adminDaemonOver(t,
		&fakeUpstreams{states: []upstream.ConnectorStatus{
			{Connector: "github", State: upstream.StateReady, Since: since},
			{Connector: "linear", State: upstream.StateNeedsAuth, Since: since, Reason: "no credential",
				Kind: upstream.KindNeedsAuth, Case: upstream.CaseNoCredential, Link: "http://127.0.0.1:7777/auth/linear"},
		}},
		&fakeStore{
			tools: map[string]int{"github": 12},
			syncs: map[string]store.SyncRecord{"github": {At: time.Date(2026, 11, 4, 10, 5, 0, 0, time.UTC)}},
			creds: []string{"linear", "old"},
		},
		admin.ConfigState{
			Connectors:  []string{"github", "linear"},
			ConfigError: "load config wayfindr.toml: connectors.x.url: must be an absolute URL",
		})
}

const statusTable = `CONNECTOR  STATE       TOOLS  LAST SYNC
github     ready       12     2026-11-04T10:05:00Z
linear     needs_auth  0      never

linear: The connector has not been authorized yet.
  Open the link to authorize it.
  http://127.0.0.1:7777/auth/linear

Orphaned credentials (connectors no longer in the config): old
  Remove each with ` + "`wayfindr auth logout <connector>`" + `.

Config error: the daemon keeps running its last good config.
  load config wayfindr.toml: connectors.x.url: must be an absolute URL
`

// sampleStatus has a connector in every situation the output renders.
func sampleStatus() admin.Status {
	since := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	synced := time.Date(2026, 11, 4, 10, 5, 0, 0, time.UTC)
	expires := time.Date(2026, 11, 4, 11, 0, 0, 0, time.UTC)
	noteExpires := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

	return admin.Status{
		Version: "v1.2.3",
		Connectors: []admin.Connector{
			{
				Name: "files", State: upstream.StateUnreachable, Since: since,
				Advice: &admin.Advice{
					Summary: "The upstream refused the connection.",
					Action:  "Check that the server is running at the connector's url. Wayfindr reconnects on the next call.",
				},
				LastSync: &admin.Sync{At: synced, Error: "connection refused"},
				Warnings: []admin.Warning{{Code: "stuck", Advice: admin.Advice{
					Summary: "The connector has not been ready since 2026-11-04T10:00:00Z.",
					Action:  "Check that the server is running at the connector's url. Wayfindr reconnects on the next call.",
				}}},
			},
			{
				Name: "github", State: upstream.StateReady, Since: since, Tools: 12,
				LastSync: &admin.Sync{At: synced}, TokenExpiresAt: &expires,
				Warnings: []admin.Warning{{Code: "no_refresh", Advice: admin.Advice{
					Summary: "The access token expires at 2026-11-04T11:00:00Z and cannot be refreshed.",
					Action:  "Authorize again before then to avoid an interruption.",
					Link:    "http://127.0.0.1:7777/auth/github",
				}}},
			},
			{
				Name: "linear", State: upstream.StateNeedsAuth, Since: since,
				Advice: &admin.Advice{
					Summary: "The connector has not been authorized yet.",
					Action:  "Open the link to authorize it.",
					Link:    "http://127.0.0.1:7777/auth/linear",
				},
			},
			{
				Name: "notes", State: upstream.StateUnreachable, Since: since,
				Advice: &admin.Advice{
					Summary: "The upstream cannot be reached.",
					Action:  "Check that it is running and the network. Wayfindr reconnects on the next call.",
				},
			},
			{
				Name: "search", State: upstream.StateStarting, Since: since,
				Advice: &admin.Advice{Summary: "The connector is still connecting.", Action: "Try again in a moment."},
			},
			{
				Name: "slack", State: upstream.StateReady, Since: since, Tools: 4, HiddenTools: 2,
				LastSync: &admin.Sync{At: synced}, TokenExpiresAt: &noteExpires,
				Warnings: []admin.Warning{{Code: "no_refresh", Note: true, Advice: admin.Advice{
					Summary: "The access token expires at 2026-11-11T10:00:00Z and cannot be refreshed.",
					Action:  "Authorize again before then to avoid an interruption.",
					Link:    "http://127.0.0.1:7777/auth/slack",
				}}},
			},
		},
		OrphanedCredentials: []string{"old", "older"},
		ConfigError:         "load config wayfindr.toml: connectors.x.url: must be an absolute URL",
	}
}

// The goldens pin how a Status renders, every situation included. The
// renderers get sampleStatus directly; the tests below run the command
// against the admin API.
func TestStatusOutputMatchesGolden(t *testing.T) {
	for _, tt := range []struct {
		name   string
		write  func(io.Writer, admin.Status) error
		golden string
	}{
		{"table", writeStatusTable, "status.golden"},
		{"json", writeStatusJSON, "status.json.golden"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer

			if err := tt.write(&stdout, sampleStatus()); err != nil {
				t.Fatal(err)
			}

			want, err := os.ReadFile(filepath.Join("testdata", tt.golden))

			if err != nil {
				t.Fatal(err)
			}

			if diff := cmp.Diff(string(want), stdout.String()); diff != "" {
				t.Errorf("output differs from testdata/%s (-want +got):\n%s", tt.golden, diff)
			}
		})
	}
}

func TestStatusPrintsWhatTheDaemonReports(t *testing.T) {
	host, root := testHost(t)
	writeStatusHost(t, root, statusDaemon(t))
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"status"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if diff := cmp.Diff(statusTable, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

// TestStatusJSONPrintsTheDaemonsStatus takes --json after the command or,
// as a global flag, before it.
func TestStatusJSONPrintsTheDaemonsStatus(t *testing.T) {
	for _, args := range [][]string{{"status", "--json"}, {"--json", "status"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			host, root := testHost(t)
			writeStatusHost(t, root, statusDaemon(t))
			var stdout, stderr bytes.Buffer

			code := RunWith(args, &stdout, &stderr, host)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			var got admin.Status

			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not a Status: %v\n%s", err, stdout.String())
			}

			names := []string{}

			for _, c := range got.Connectors {
				names = append(names, c.Name)
			}

			if diff := cmp.Diff([]string{"github", "linear"}, names); diff != "" {
				t.Errorf("connectors (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff([]string{"old"}, got.OrphanedCredentials); diff != "" {
				t.Errorf("orphaned credentials (-want +got):\n%s", diff)
			}
		})
	}
}

// A config the daemon can't load is when status matters most: the daemon
// keeps running its last good config and reports the error, so status reads
// only the listen address from the broken file.
func TestStatusWorksWhenTheConfigFileIsInvalid(t *testing.T) {
	host, root := testHost(t)
	writeStatusHost(t, root, statusDaemon(t))
	cfg := filepath.Join(root, "config", "wayfindr", "wayfindr.toml")
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // inside the test's temp dir

	if err != nil {
		t.Fatal(err)
	}

	// An unknown key and a connector that fails validation.
	if _, err := f.WriteString("bogus = 1\n[connectors.x]\nurl = \"not a url\"\n"); err != nil {
		t.Fatal(err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"status"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if diff := cmp.Diff(statusTable, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

// A file that doesn't parse has no listen address to trust, so status asks
// for the file to be fixed, and says whether a daemon still answers on the
// default address.
func TestStatusOnAConfigSyntaxErrorSuggestsFixingTheFile(t *testing.T) {
	for _, tt := range []struct {
		name    string
		running bool
	}{
		{"daemon running on the default address", true},
		{"no daemon", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host, root := testHost(t)
			writeStatusHost(t, root, "127.0.0.1:7777")
			cfg := filepath.Join(root, "config", "wayfindr", "wayfindr.toml")

			if err := os.WriteFile(cfg, []byte("[server\nlisten = \"127.0.0.1:7777\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			var dialed []string
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					http.NotFound(w, r)

					return
				}

				_, _ = io.WriteString(w, `{"version":"dev"}`)
			}))
			t.Cleanup(daemon.Close)
			host.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
				dialed = append(dialed, addr)

				if !tt.running {
					return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connection refused")}
				}

				var d net.Dialer

				return d.DialContext(ctx, network, daemon.Listener.Addr().String())
			}
			var stdout, stderr bytes.Buffer

			code := RunWith([]string{"status"}, &stdout, &stderr, host)

			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero; stdout: %s", stdout.String())
			}

			if diff := cmp.Diff([]string{"127.0.0.1:7777"}, dialed); diff != "" {
				t.Errorf("dialed (-want +got):\n%s", diff)
			}

			msg := stderr.String()

			for _, want := range []string{cfg, "Fix the config file"} {
				if !strings.Contains(msg, want) {
					t.Errorf("stderr = %q, want it to contain %q", msg, want)
				}
			}

			const stillRunning = "The daemon is still running on 127.0.0.1:7777 with its last good config."

			if got := strings.Contains(msg, stillRunning); got != tt.running {
				t.Errorf("stderr = %q; mentions %q = %v, want %v", msg, stillRunning, got, tt.running)
			}
		})
	}
}

func TestStatusWithNoConnectorsSaysSo(t *testing.T) {
	host, root := testHost(t)
	writeStatusHost(t, root, adminDaemonOver(t, &fakeUpstreams{}, &fakeStore{}, admin.ConfigState{}))
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"status"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if diff := cmp.Diff("No connectors are configured.\n", stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

// A process that answers on the listen address but does not hold the
// daemon lock is not the daemon (wayfindr-zvjw): the commands refuse it and
// never send it the client token or a secret's value.
func TestCommandsRefuseAListenerThatDoesNotHoldTheDaemonLock(t *testing.T) {
	var sawToken, sawValue atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawToken.Store(true)
		}

		if b, _ := io.ReadAll(r.Body); bytes.Contains(b, []byte("s3cret")) {
			sawValue.Store(true)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)
	listen := srv.Listener.Addr().String()

	for _, args := range [][]string{{"status"}, {"secret", "set", "github_token"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			host, root := testHost(t)
			writeHostFiles(t, root, listen)
			host.Stdin = strings.NewReader("s3cret\n")

			_, stderr, code := run(t, host, args...)

			if code == 0 || !strings.Contains(stderr, "does not hold the daemon lock") {
				t.Errorf("exit %d, stderr %q; want a refusal that names the daemon lock", code, stderr)
			}
		})
	}

	if sawToken.Load() || sawValue.Load() {
		t.Errorf("the listener got the client token (%v) or the secret's value (%v); want neither", sawToken.Load(), sawValue.Load())
	}
}

func TestStatusWhenTheDaemonIsNotRunningSuggestsStartingIt(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatal(err)
	}

	addr := ln.Addr().String()

	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	host, root := testHost(t)
	writeHostFiles(t, root, addr)
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"status"}, &stdout, &stderr, host)

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero; stdout: %s", stdout.String())
	}

	msg := stderr.String()

	for _, want := range []string{"daemon is not running", addr, "`wayfindr start`", "`wayfindr service install`"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr = %q, want it to contain %q", msg, want)
		}
	}

	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestStatusReportsAnAdminAPIRefusal(t *testing.T) {
	host, root := testHost(t)
	writeStatusHost(t, root, statusDaemon(t))
	// A token the daemon does not accept.
	tokenFile := filepath.Join(root, "data", "wayfindr", "client-token")

	if err := os.WriteFile(tokenFile, []byte("wf_wrong\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"status"}, &stdout, &stderr, host)

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero; stdout: %s", stdout.String())
	}

	// The daemon's sentence alone: no method, path, or status in front.
	if diff := cmp.Diff("Error: unauthorized\n", stderr.String()); diff != "" {
		t.Errorf("stderr (-want +got):\n%s", diff)
	}
}

// fakeHealth reports each connector's warnings, by name.
type fakeHealth map[string][]admin.Warning

func (h fakeHealth) Health(_ context.Context, s upstream.ConnectorStatus) (admin.ConnectorHealth, error) {
	return admin.ConnectorHealth{Warnings: h[s.Connector]}, nil
}

func TestStatusCheckExitsTwoWhenSomethingNeedsAttention(t *testing.T) {
	ready := upstream.ConnectorStatus{Connector: "github", State: upstream.StateReady}
	warning := admin.Warning{Code: "stuck", Advice: admin.Advice{Summary: "The connector has not been ready since 2026-11-04T10:00:00Z."}}
	note := admin.Warning{Code: "no_refresh", Note: true, Advice: admin.Advice{Summary: "The access token expires at 2026-11-11T10:00:00Z and cannot be refreshed."}}

	for _, tt := range []struct {
		name     string
		states   []upstream.ConnectorStatus
		health   fakeHealth
		cfgError string
		want     int
	}{
		{name: "all ready", states: []upstream.ConnectorStatus{ready}, want: 0},
		{name: "no connectors", want: 0},
		{name: "a note only", states: []upstream.ConnectorStatus{ready}, health: fakeHealth{"github": {note}}, want: 0},
		{name: "a connector not ready", want: 2, states: []upstream.ConnectorStatus{ready,
			{Connector: "linear", State: upstream.StateNeedsAuth, Kind: upstream.KindNeedsAuth, Case: upstream.CaseNoCredential}}},
		{name: "a connector starting", want: 2, states: []upstream.ConnectorStatus{{Connector: "github", State: upstream.StateStarting}}},
		{name: "a warning", states: []upstream.ConnectorStatus{ready}, health: fakeHealth{"github": {note, warning}}, want: 2},
		{name: "a config error", states: []upstream.ConnectorStatus{ready}, cfgError: "load config: bad", want: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			names := []string{}

			for _, s := range tt.states {
				names = append(names, s.Connector)
			}

			addr := adminDaemonOver(t, &fakeUpstreams{states: tt.states}, &fakeStore{},
				admin.ConfigState{Connectors: names, ConfigError: tt.cfgError},
				func(o *admin.Options) { o.Health = tt.health })

			for _, args := range [][]string{{"status", "--check"}, {"status", "--check", "--json"}} {
				host, root := testHost(t)
				writeStatusHost(t, root, addr)
				var plain, checked, stderr bytes.Buffer

				// --check prints what status alone prints.
				if code := RunWith(slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--check" }),
					&plain, &stderr, host); code != 0 {
					t.Fatalf("%v without --check: exit code = %d, want 0; stderr: %s", args, code, stderr.String())
				}

				code := RunWith(args, &checked, &stderr, host)

				if code != tt.want {
					t.Errorf("%v: exit code = %d, want %d; stderr: %s", args, code, tt.want, stderr.String())
				}

				if diff := cmp.Diff(plain.String(), checked.String()); diff != "" {
					t.Errorf("%v: stdout differs from status without --check (-without +with):\n%s", args, diff)
				}

				if stderr.Len() != 0 {
					t.Errorf("%v: stderr = %q, want nothing", args, stderr.String())
				}
			}
		})
	}
}

func TestStatusCheckExitsOneWhenTheDaemonIsNotRunning(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatal(err)
	}

	addr := ln.Addr().String()

	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	host, root := testHost(t)
	writeStatusHost(t, root, addr)
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"status", "--check"}, &stdout, &stderr, host)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}

	if msg := stderr.String(); !strings.HasPrefix(msg, "Error: daemon is not running") {
		t.Errorf("stderr = %q, want the not-running error", msg)
	}
}
