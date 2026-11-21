package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// fakeHealth stands in for the daemon's health: by connector.
type fakeHealth map[string]admin.ConnectorHealth

func (f fakeHealth) Health(_ context.Context, s upstream.ConnectorStatus) (admin.ConnectorHealth, error) {
	return f[s.Connector], nil
}

func TestStatusReportsEachConnectorWithAdviceToolsAndLastSync(t *testing.T) {
	ctx := t.Context()
	st, user := newStore(t)
	// A zone other than UTC: the response normalises every time to UTC.
	zone := time.FixedZone("UTC+2", 2*60*60)
	since := time.Date(2026, 11, 4, 12, 0, 0, 0, zone)
	synced := time.Date(2026, 11, 4, 12, 5, 0, 0, zone)

	if _, err := st.ApplySync(ctx, user, store.SyncOutcome{Connector: "github", At: synced,
		Tools: []store.Tool{tool(t, "github", "create_pr"), tool(t, "github", "list_prs")}}); err != nil {
		t.Fatal(err)
	}

	if _, err := st.ApplySync(ctx, user, store.SyncOutcome{Connector: "files", At: synced, Error: "connection refused"}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []string{"linear", "old"} {
		if err := st.PutCredential(ctx, store.Credential{UserID: user, Connector: c, Resource: "https://" + c + ".example/mcp"}); err != nil {
			t.Fatal(err)
		}
	}

	opts := options(t, st, user, &fakeUpstreams{states: []upstream.ConnectorStatus{
		{Connector: "files", State: upstream.StateUnreachable, Reason: "dial tcp 127.0.0.1:9: connection refused", Since: since.Add(-time.Hour),
			Kind: upstream.KindUnreachable, Case: upstream.CaseConnectRefused},
		{Connector: "github", State: upstream.StateReady, Since: since},
		{Connector: "linear", State: upstream.StateNeedsAuth, Reason: "no credential", Since: since,
			Kind: upstream.KindNeedsAuth, Case: upstream.CaseNoCredential, Link: "http://127.0.0.1:7777/auth/linear"},
		{Connector: "notes", State: upstream.StateUnreachable, Reason: "secret:notes is not set", Since: since,
			Kind: upstream.KindMissingValue, Case: upstream.CaseSecret, Ref: "secret:notes"},
		{Connector: "search", State: upstream.StateStarting, Since: since},
	}}, admin.ConfigState{Connectors: []string{"files", "github", "linear", "notes", "search"}})
	// github's OAuth token expires an hour after it synced, and cannot be
	// refreshed. files has not been ready for more than a day.
	opts.Health = fakeHealth{
		"github": {
			TokenExpiresAt: synced.Add(time.Hour),
			Warnings: []admin.Warning{{Code: "no_refresh", Advice: admin.Advice{
				Summary: "The access token expires at 2026-11-04T11:05:00Z and cannot be refreshed.",
				Action:  "Authorize again before then to avoid an interruption.",
				Link:    "http://127.0.0.1:7777/auth/github",
			}}},
		},
		"files": {Warnings: []admin.Warning{{Code: "stuck", Advice: admin.Advice{
			Summary: "The connector has not been ready since 2026-11-04T09:00:00Z.",
			Action:  "Check that the server is running at the connector's url. Wayfindr reconnects on the next call.",
		}}}},
	}
	opts.Version = "v1.2.3"
	c, srv := serve(t, opts)

	// The JSON shape is a public contract (`wayfindr status --json`), so the
	// wire is pinned byte for byte.
	golden, err := os.ReadFile(filepath.Join("testdata", "status.json"))

	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/admin/status", nil)

	if err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Client().Do(req)

	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/status = %s, want 200; body: %s", resp.Status, body)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var indented bytes.Buffer

	if err := json.Indent(&indented, body, "", "  "); err != nil {
		t.Fatalf("response is not JSON: %v; body: %s", err, body)
	}

	if diff := cmp.Diff(string(golden), indented.String()); diff != "" {
		t.Errorf("response differs from testdata/status.json (-want +got):\n%s", diff)
	}

	// The client reads the same contract back.
	var want admin.Status

	if err := json.Unmarshal(golden, &want); err != nil {
		t.Fatal(err)
	}

	got, err := c.Status(ctx)

	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Status (-want +got):\n%s", diff)
	}
}

func TestStatusReportsTheConfigErrorAndEmptyListsAsArrays(t *testing.T) {
	st, user := newStore(t)
	c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{
		ConfigError: "load config wayfindr.toml: connectors.x.url: must be an absolute URL",
	}))

	got, err := c.Status(t.Context())

	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	want := admin.Status{
		Connectors:          []admin.Connector{},
		OrphanedCredentials: []string{},
		ConfigError:         "load config wayfindr.toml: connectors.x.url: must be an absolute URL",
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Status (-want +got):\n%s", diff)
	}
}

// TestStatusReportsTheProfiles: an empty allowlist (no connector) must stay
// distinct from none (every connector) on the wire, and a profile with no
// keys narrows nothing.
func TestStatusReportsTheProfiles(t *testing.T) {
	st, user := newStore(t)
	c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{
		Profiles: map[string]*config.Scope{
			"acme": {Connectors: []string{"github"}, Tools: map[string]config.ToolPolicy{"github": {"@read"}}},
			"all":  nil,
			"none": {Connectors: []string{}},
		},
	}))

	got, err := c.Status(t.Context())

	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	want := map[string]config.Scope{
		"acme": {Connectors: []string{"github"}, Tools: map[string]config.ToolPolicy{"github": {"@read"}}},
		"all":  {},
		"none": {Connectors: []string{}},
	}

	if diff := cmp.Diff(want, got.Profiles); diff != "" {
		t.Errorf("Profiles (-want +got):\n%s", diff)
	}

	// Without profiles, the key is absent.
	none, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))
	got, err = none.Status(t.Context())

	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if got.Profiles != nil {
		t.Errorf("Profiles = %v, want none", got.Profiles)
	}
}

func TestStatusFailsWithTheDaemonsMessageWhenTheStoreFails(t *testing.T) {
	st, user := newStore(t)

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	c, _ := serve(t, options(t, st, user, &fakeUpstreams{states: []upstream.ConnectorStatus{
		{Connector: "github", State: upstream.StateReady},
	}}, admin.ConfigState{Connectors: []string{"github"}}))

	_, err := c.Status(t.Context())

	if err == nil {
		t.Fatal("Status: want an error")
	}

	if got := statusOf(err); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}

	if err.Error() != "status unavailable; see the daemon log" {
		t.Errorf("error = %q, want the daemon's message alone", err)
	}
}

func TestUnknownRoutesAndMethodsAreRefused(t *testing.T) {
	st, user := newStore(t)
	_, srv := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))

	for _, tt := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/admin/status", http.StatusMethodNotAllowed},
		{http.MethodGet, "/admin/secrets/tok", http.StatusMethodNotAllowed},
		{http.MethodGet, "/admin/nope", http.StatusNotFound},
	} {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), tt.method, srv.URL+tt.path, nil)

			if err != nil {
				t.Fatal(err)
			}

			resp, err := srv.Client().Do(req)

			if err != nil {
				t.Fatal(err)
			}

			_ = resp.Body.Close()

			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}
