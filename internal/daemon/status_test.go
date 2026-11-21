package daemon_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/daemon"
	"github.com/atomicpages/wayfindr/internal/setup"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/drivers"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// TestAdminStatusReportsConnectorsToolCountsAndOrphanedCredentials also
// pins that the daemon mounts the admin API behind its guards.
func TestAdminStatusReportsConnectorsToolCountsAndOrphanedCredentials(t *testing.T) {
	h := newHost(t)
	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"files": stdioConnector(t, fakeupstream.V20250618)},
	})
	refuse := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no daemon") }

	if _, err := setup.Init(t.Context(), setup.Options{Env: h.env, Keychain: h.keychain, Dial: refuse, Logger: zerolog.Nop()}); err != nil {
		t.Fatalf("setup.Init: %v", err)
	}

	token, err := clienttoken.Read(t.Context(), h.env, h.paths)

	if err != nil {
		t.Fatalf("clienttoken.Read: %v", err)
	}

	// A credential left behind by a connector no longer in the config.
	st, err := drivers.Open(t.Context(), "sqlite", h.paths.DBFile())

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatal(err)
	}

	if err := st.PutCredential(t.Context(), store.Credential{UserID: user, Connector: "gone", Resource: "https://gone.example/mcp"}); err != nil {
		t.Fatal(err)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
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
	client := &http.Client{Transport: bearer{token: token, next: transport}}

	// The catalog fills in the background after startup.
	var got admin.Status
	deadline := time.Now().Add(15 * time.Second)

	for {
		got = getStatus(t, client, "http://"+d.Addr().String())

		if len(got.Connectors) == 1 && got.Connectors[0].State == upstream.StateReady && got.Connectors[0].Tools == 1 && got.Connectors[0].LastSync != nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("files never became ready with its one tool synced; last status: %+v", got)
		}

		time.Sleep(50 * time.Millisecond)
	}

	if diff := cmp.Diff([]string{"gone"}, got.OrphanedCredentials); diff != "" {
		t.Errorf("orphaned credentials (-want +got):\n%s", diff)
	}

	if got.ConfigError != "" {
		t.Errorf("config error = %q, want none", got.ConfigError)
	}

	if c := got.Connectors[0]; c.Name != "files" || c.Advice != nil || c.LastSync.Error != "" {
		t.Errorf("connector = %+v, want files, without advice, last synced successfully", c)
	}

	// /admin is mounted behind the guards of design §7.7.
	plain := &http.Client{Transport: transport}

	for _, tt := range []struct {
		name string
		set  func(*http.Request)
		want int
	}{
		{"no bearer", func(*http.Request) {}, http.StatusUnauthorized},
		{"a wrong bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wf_wrong") }, http.StatusUnauthorized},
		{"a foreign Origin", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Origin", "https://evil.example")
		}, http.StatusForbidden},
		{"a foreign Host", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Host = "evil.example"
		}, http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+d.Addr().String()+"/admin/status", nil)

			if err != nil {
				t.Fatal(err)
			}

			tt.set(req)
			resp, err := plain.Do(req)

			if err != nil {
				t.Fatal(err)
			}

			_ = resp.Body.Close()

			if resp.StatusCode != tt.want {
				t.Errorf("GET /admin/status with %s: status = %d, want %d", tt.name, resp.StatusCode, tt.want)
			}
		})
	}

	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}
}

// TestAdminStatusShowsNoFalseOrphansDuringTheStartupApply pins that the
// status knows the configured connectors while the startup apply is still
// connecting them: a configured connector's credential is never listed as
// orphaned.
func TestAdminStatusShowsNoFalseOrphansDuringTheStartupApply(t *testing.T) {
	// An upstream that answers nothing until the test ends holds the
	// startup apply in its connect.
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}

		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(hung.Close)
	h := newHost(t)
	initHost(t, h, map[string]any{
		"server":     map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"slow": map[string]any{"url": hung.URL}},
	})
	putCredential(t, h, "slow")
	putCredential(t, h, "gone")

	r := start(t, h, h.options(t))
	// Registered after start, so it runs before the daemon's shutdown.
	t.Cleanup(func() { close(release) })

	got := r.waitStatus(t, "slow connecting", func(s admin.Status) bool {
		c, ok := connector(s, "slow")

		return ok && c.State == upstream.StateStarting
	})

	if diff := cmp.Diff([]string{"gone"}, got.OrphanedCredentials); diff != "" {
		t.Errorf("orphaned credentials while the startup apply connects (-want +got):\n%s", diff)
	}
}

// TestAdminStatusReportsTheProfilesTheRunningConfigHas: a reload that
// changes the profiles shows in the status, and one that fails keeps the
// running config's.
func TestAdminStatusReportsTheProfilesTheRunningConfigHas(t *testing.T) {
	h := newHost(t)
	listen := freeAddr(t)
	files := stdioConnector(t, fakeupstream.V20250618)
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files},
		"profiles":   map[string]any{"acme": map[string]any{"connectors": []string{"files"}}},
	})
	got := r.waitStatus(t, "files synced", synced("files"))

	if diff := cmp.Diff(map[string]config.Scope{"acme": {Connectors: []string{"files"}}}, got.Profiles); diff != "" {
		t.Errorf("profiles at startup (-want +got):\n%s", diff)
	}

	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files},
		"profiles": map[string]any{
			"acme": map[string]any{"tools": map[string]any{"files": []string{"@read"}}},
			"none": map[string]any{"connectors": []string{}},
		},
	})
	want := map[string]config.Scope{
		"acme": {Tools: map[string]config.ToolPolicy{"files": {"@read"}}},
		"none": {Connectors: []string{}},
	}
	r.waitStatus(t, "the reloaded profiles", func(s admin.Status) bool { return cmp.Equal(want, s.Profiles) })

	// A profile naming a connector the config lacks fails the reload, and
	// the running profiles stay.
	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files},
		"profiles":   map[string]any{"acme": map[string]any{"connectors": []string{"gmail"}}},
	})
	got = r.waitStatus(t, "a config error", func(s admin.Status) bool { return s.ConfigError != "" })

	if diff := cmp.Diff(want, got.Profiles); diff != "" {
		t.Errorf("profiles after a failed reload (-want +got):\n%s", diff)
	}
}

// getStatus reads the status through the admin client of the daemon at
// base. client presents the bearer.
func getStatus(t *testing.T, client *http.Client, base string) admin.Status {
	t.Helper()
	s, err := admin.NewClient(base, "", client).Status(t.Context())

	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	return s
}
