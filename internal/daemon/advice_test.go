package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/store/drivers"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// asDefaultClient serves h as httpd serves /mcp to the default client
// token: every request carries the default Client in its TokenInfo, which
// the gateway refuses to serve without.
func asDefaultClient(h http.Handler) http.Handler {
	verify := func(context.Context, string, *http.Request) (*auth.TokenInfo, error) {
		return clienttoken.Default().TokenInfo(), nil
	}
	h = auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(h)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer test")
		h.ServeHTTP(w, r)
	})
}

// sameStatuses reports fixed connector states to both the admin API and
// the gateway.
type sameStatuses []upstream.ConnectorStatus

func (s sameStatuses) Status() []upstream.ConnectorStatus { return s }
func (sameStatuses) SecretChanged(context.Context, string) []upstream.ConnectorStatus {
	return nil
}
func (sameStatuses) Call(context.Context, string, string, json.RawMessage) (*mcp.CallToolResult, error) {
	return nil, &upstream.Error{Kind: upstream.KindUnknownConnector}
}
func (s sameStatuses) Continue(ctx context.Context, connector, tool string, args json.RawMessage, _ upstream.Input) (*mcp.CallToolResult, error) {
	return s.Call(ctx, connector, tool, args)
}
func (sameStatuses) Discard(string) {}

// The user reading `wayfindr status` and the agent reading search_tools'
// unavailable list are told the same thing about a connector, whatever
// state it is in and whether or not an error put it there.
func TestStatusAndSearchToolsGiveTheSameAdviceForAConnector(t *testing.T) {
	const link = "http://127.0.0.1:7777/auth/linear"
	since := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	statuses := sameStatuses{
		{Connector: "billing", State: upstream.StateAuthError, Since: since, Reason: "auth: malformed metadata"},
		{Connector: "files", State: upstream.StateUnreachable, Since: since, Reason: "dial tcp 127.0.0.1:9: connection refused",
			Kind: upstream.KindUnreachable, Case: upstream.CaseConnectRefused},
		{Connector: "github", State: upstream.StateReady, Since: since},
		{Connector: "jira", State: upstream.StateUnreachable, Since: since, Reason: "connect: i/o timeout"},
		{Connector: "linear", State: upstream.StateNeedsAuth, Since: since, Reason: "grant revoked",
			Kind: upstream.KindNeedsAuth, Case: upstream.CaseTokenRevoked, ProviderCode: "invalid_grant", Link: link},
		{Connector: "notes", State: upstream.StateAuthError, Since: since, Reason: "secret:notes is not set",
			Kind: upstream.KindMissingValue, Case: upstream.CaseSecret, Ref: "secret:notes"},
		{Connector: "search", State: upstream.StateStarting, Since: since},
	}
	st, err := drivers.Open(t.Context(), "sqlite", filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })
	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(statuses))

	for _, s := range statuses {
		names = append(names, s.Connector)
	}

	adminSrv := httptest.NewServer(admin.New(admin.Options{
		Upstreams: statuses, Store: st, UserID: user,
		Config: func() admin.ConfigState { return admin.ConfigState{Connectors: names} },
		Logger: zerolog.Nop(),
	}))
	t.Cleanup(adminSrv.Close)
	status, err := admin.NewClient(adminSrv.URL, "", adminSrv.Client()).Status(t.Context())

	if err != nil {
		t.Fatalf("admin Status: %v", err)
	}

	gwSrv := httptest.NewServer(asDefaultClient(gateway.New(gateway.Options{Upstreams: statuses, Catalog: st, UserID: user, Logger: zerolog.Nop()})))
	t.Cleanup(gwSrv.Close)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil).Connect(t.Context(),
		&mcp.StreamableClientTransport{Endpoint: gwSrv.URL, MaxRetries: -1}, nil)

	if err != nil {
		t.Fatalf("connect to the gateway: %v", err)
	}

	t.Cleanup(func() { _ = cs.Close() })
	var page struct {
		Unavailable []struct {
			Connector string `json:"connector"`
			Status    string `json:"status"`
			Summary   string `json:"summary"`
			Action    string `json:"action"`
			Link      string `json:"link"`
		} `json:"unavailable"`
	}

	if err := json.Unmarshal([]byte(call(t, cs, "search_tools", map[string]any{"query": "anything"})), &page); err != nil {
		t.Fatalf("search_tools response: %v", err)
	}

	// row is one connector's state and advice, as either side tells it.
	type row struct{ State, Summary, Action, Link string }
	fromAdmin := map[string]row{}

	for _, c := range status.Connectors {
		if c.State == upstream.StateReady {
			if c.Advice != nil {
				t.Errorf("admin advice for ready %s = %+v, want none", c.Name, c.Advice)
			}

			continue
		}

		if c.Advice == nil {
			t.Errorf("admin has no advice for %s in %s", c.Name, c.State)
			continue
		}

		fromAdmin[c.Name] = row{string(c.State), c.Advice.Summary, c.Advice.Action, c.Advice.Link}
	}

	fromGateway := map[string]row{}

	for _, u := range page.Unavailable {
		fromGateway[u.Connector] = row{u.Status, u.Summary, u.Action, u.Link}
	}

	if len(fromAdmin) != len(statuses)-1 {
		t.Errorf("admin advises %d connectors, want every one but ready github (%d)", len(fromAdmin), len(statuses)-1)
	}

	if diff := cmp.Diff(fromAdmin, fromGateway); diff != "" {
		t.Errorf("advice (-admin +search_tools):\n%s", diff)
	}

	if got := fromAdmin["linear"].Link; got != link {
		t.Errorf("linear's link = %q, want %q", got, link)
	}
}
