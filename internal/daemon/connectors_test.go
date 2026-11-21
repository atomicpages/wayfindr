package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/daemon"
	"github.com/atomicpages/wayfindr/internal/setup"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// addingDaemon serves a daemon on h with no connectors and returns an admin
// client for it.
func addingDaemon(t *testing.T, h host) *admin.Client {
	t.Helper()
	h.writeConfig(t, map[string]any{"server": map[string]any{"listen": freeAddr(t)}})
	refuse := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no daemon") }

	if _, err := setup.Init(t.Context(), setup.Options{Env: h.env, Keychain: h.keychain, Dial: refuse, Logger: zerolog.Nop()}); err != nil {
		t.Fatalf("setup.Init: %v", err)
	}

	token, err := clienttoken.Read(t.Context(), h.env, h.paths)

	if err != nil {
		t.Fatalf("clienttoken.Read: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d, err := daemon.New(ctx, h.options(t))

	if err != nil {
		cancel()
		t.Fatalf("daemon.New: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()
	transport := &http.Transport{}
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		cancel()

		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	return admin.NewClient("http://"+d.Addr().String(), "", &http.Client{Transport: bearer{token: token, next: transport}})
}

func readConfig(t *testing.T, h host) []byte {
	t.Helper()
	b, err := os.ReadFile(h.paths.ConfigFile)

	if err != nil {
		t.Fatal(err)
	}

	return b
}

func TestAddConnectorWritesTheFileAndConnects(t *testing.T) {
	const secretValue = "s3cr3t-never-in-the-file" //nolint:gosec // G101: a test fixture
	h := newHost(t)
	// The upstreams start first, so they close after the daemon (cleanups
	// run last in, first out).
	fake := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(as.Close)
	// The racer edits the file while the add probes it, as a person or the
	// CLI might.
	edit := []byte("\n# edited meanwhile\n")
	racer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, err := os.OpenFile(h.paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0)

		if err == nil {
			_, _ = f.Write(edit)
			_ = f.Close()
		}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(racer.Close)
	c := addingDaemon(t, h)
	before := readConfig(t, h)
	cmd := fakeupstream.StdioCommand(t, fakeupstream.V20251125)
	env := map[string]string{}

	for _, kv := range cmd.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "WAYFINDR_FAKEUPSTREAM_CONFIG" {
			env[k] = v
		}
	}

	t.Run("a command connector, with a secret variable", func(t *testing.T) {
		added, err := c.AddConnector(t.Context(), admin.ConnectorSpec{
			Name: "files", Command: cmd.Path, Args: cmd.Args[1:], Env: env, SecretEnv: map[string]string{"API_KEY": secretValue},
		})

		if err != nil {
			t.Fatalf("AddConnector: %v", err)
		}

		if added.Result.State != upstream.StateReady || added.Result.Tools == 0 {
			t.Errorf("result = %+v, want files ready with its tools synced", added.Result)
		}

		if diff := cmp.Diff([]string{"files_api_key"}, added.Secrets); diff != "" || added.OAuth != admin.OAuthNotChecked {
			t.Errorf("secrets %v, oauth %q; want [files_api_key], not_checked", added.Secrets, added.OAuth)
		}

		after := readConfig(t, h)

		if !bytes.HasPrefix(after, before) {
			t.Error("the file's existing text changed; want the table appended after it")
		}

		if bytes.Contains(after, []byte(secretValue)) || !bytes.Contains(after, []byte("${secret:files_api_key}")) {
			t.Errorf("the file holds the secret's value or lacks its reference:\n%s", after)
		}
	})

	t.Run("a URL connector, detected open", func(t *testing.T) {
		added, err := c.AddConnector(t.Context(), admin.ConnectorSpec{Name: "remote", URL: fake.URL})

		if err != nil {
			t.Fatalf("AddConnector: %v", err)
		}

		if added.Result.State != upstream.StateReady || added.OAuth != admin.OAuthOpen {
			t.Errorf("result %+v, oauth %q; want ready, open", added.Result, added.OAuth)
		}
	})

	t.Run("a taken name is 409 and changes nothing", func(t *testing.T) {
		want := readConfig(t, h)
		_, err := c.AddConnector(t.Context(), admin.ConnectorSpec{Name: "files", URL: "https://example.com/mcp", Auth: admin.AuthNone})

		if got := statusOf(err); got != http.StatusConflict || !strings.Contains(err.Error(), "already configured") {
			t.Errorf("err = %v (status %d), want 409, already configured", err, got)
		}

		if !bytes.Equal(want, readConfig(t, h)) {
			t.Error("the file changed")
		}
	})

	t.Run("an invalid name is 400", func(t *testing.T) {
		_, err := c.AddConnector(t.Context(), admin.ConnectorSpec{Name: "Not A Name", URL: "https://example.com/mcp", Auth: admin.AuthNone})

		if got := statusOf(err); got != http.StatusBadRequest {
			t.Errorf("err = %v (status %d), want 400", err, got)
		}
	})

	t.Run("a server that asks for OAuth gets an auth table", func(t *testing.T) {
		added, err := c.AddConnector(t.Context(), admin.ConnectorSpec{Name: "guarded", URL: as.URL + "/mcp"})

		if err != nil {
			t.Fatalf("AddConnector: %v", err)
		}

		if added.OAuth != admin.OAuthDetected || !bytes.Contains(readConfig(t, h), []byte(`type = "oauth"`)) {
			t.Errorf("oauth %q, want detected and an oauth auth table in the file", added.OAuth)
		}
	})

	t.Run("a file changed while adding is 409 and not overwritten", func(t *testing.T) {
		_, err := c.AddConnector(t.Context(), admin.ConnectorSpec{Name: "raced", URL: racer.URL + "/mcp"})

		if got := statusOf(err); got != http.StatusConflict || !strings.Contains(err.Error(), "changed while") {
			t.Errorf("err = %v (status %d), want 409, changed while", err, got)
		}

		after := readConfig(t, h)

		if !bytes.HasSuffix(after, edit) || bytes.Contains(after, []byte("raced")) {
			t.Errorf("the edit was overwritten or raced was written:\n%s", after)
		}
	})
}

// An add whose connect hangs is cancelled by the shutdown drain, so it
// can't hold the daemon past it (wayfindr-zqef). Before, shutdown took the
// reload lock before draining the requests, and the add held that lock for
// as long as its connect waited, up to the connect timeout.
func TestShutdownCancelsAnAddWhoseConnectHangs(t *testing.T) {
	// The drain timeout and the grace after it (design §7.8), plus a margin.
	const budget = 10*time.Second + time.Second + 3*time.Second

	// An upstream that accepts the connect and never answers it.
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}

		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(hang.Close)
	t.Cleanup(func() { close(release) })

	h := newHost(t)
	h.writeConfig(t, map[string]any{"server": map[string]any{"listen": freeAddr(t)}})
	refuse := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no daemon") }

	if _, err := setup.Init(t.Context(), setup.Options{Env: h.env, Keychain: h.keychain, Dial: refuse, Logger: zerolog.Nop()}); err != nil {
		t.Fatalf("setup.Init: %v", err)
	}

	token, err := clienttoken.Read(t.Context(), h.env, h.paths)

	if err != nil {
		t.Fatalf("clienttoken.Read: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := daemon.New(ctx, h.options(t))

	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	c := admin.NewClient("http://"+d.Addr().String(), "", &http.Client{Transport: bearer{token: token, next: transport}})

	added := make(chan struct{})
	go func() {
		defer close(added)
		// Its error is expected: shutdown cancels the request.
		_, _ = c.AddConnector(context.Background(), admin.ConnectorSpec{Name: "slow", URL: hang.URL, Auth: admin.AuthNone})
	}()

	select {
	case <-arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("the add never connected to the upstream")
	}

	start := time.Now()
	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}

		t.Logf("Serve returned %v after shutdown began", time.Since(start).Round(time.Millisecond))
	case <-time.After(budget):
		t.Fatalf("Serve did not return within %v of shutdown while an add was connecting", budget)
	}

	select {
	case <-added:
	case <-time.After(5 * time.Second):
		t.Error("the add's request never returned")
	}
}

// An add from the console or the admin API whose secret name another
// connector uses is refused before anything is stored (wayfindr-eadp).
func TestAddConnectorRefusesASecretNameInUse(t *testing.T) {
	h := newHost(t)
	c := addingDaemon(t, h)
	src := string(readConfig(t, h)) + `
[connectors.google_drive]
command = "npx"
args = ["drive-mcp"]
env = { TOKEN = "${secret:google_drive_token}" }
`

	if err := os.WriteFile(h.paths.ConfigFile, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := c.AddConnector(t.Context(), admin.ConnectorSpec{
		Name: "google", Command: "npx", Args: []string{"google-mcp"}, SecretEnv: map[string]string{"DRIVE_TOKEN": "v"},
	})

	if err == nil || !strings.Contains(err.Error(), "google_drive_token") || !strings.Contains(err.Error(), "connectors.google_drive") {
		t.Fatalf("AddConnector: %v, want a refusal naming google_drive_token and connectors.google_drive", err)
	}

	if got := string(readConfig(t, h)); got != src {
		t.Errorf("the config file changed:\n%s", got)
	}
}
