package gateway_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/golden"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// listCatalog returns every tool it holds for any search, and records the
// last query. The connector "broken" fails to read.
type listCatalog struct {
	mu    sync.Mutex
	tools []store.Tool
	last  store.SearchQuery
}

func (c *listCatalog) GetTool(_ context.Context, _, connector, name string) (store.Tool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if connector == "broken" {
		return store.Tool{}, errors.New("disk on fire")
	}

	for _, t := range c.tools {
		if t.Connector == connector && t.Name == name {
			return t, nil
		}
	}

	return store.Tool{}, store.ErrNotFound
}

// LastSync implements gateway.Catalog: no sync is ever recorded.
func (c *listCatalog) LastSync(context.Context, string, string) (store.SyncRecord, error) {
	return store.SyncRecord{}, store.ErrNotFound
}

func (c *listCatalog) Search(_ context.Context, q store.SearchQuery) (store.SearchPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = q

	return store.SearchPage{Tools: c.tools}, nil
}

func (c *listCatalog) lastSize() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.last.Size
}

// newListed serves a gateway over cat, with the connectors in statuses
// configured.
func newListed(t *testing.T, cat *listCatalog, statuses ...upstream.ConnectorStatus) *mcp.ClientSession {
	t.Helper()
	g := gateway.New(gateway.Options{
		Upstreams: &stubUpstreams{statuses: statuses},
		Catalog:   cat,
		Logger:    zerolog.Nop(),
	})
	srv := httptest.NewServer(authed(g))
	t.Cleanup(srv.Close)

	return connect(t, srv.URL, fakeupstream.V20250618)
}

// Warnings reach the agent for the ready connectors searched, and only
// those are asked about. A connector that is not ready is under
// unavailable already, so its warning is not repeated, and one outside the
// search's connectors is left out.
func TestSearchListsWarningsOfReadyConnectors(t *testing.T) {
	var asked atomic.Pointer[[]string]
	warnings := []gateway.Warning{
		{Connector: "linear", Summary: "The access token expires at 2026-11-08T17:00:00Z and cannot be refreshed.",
			Action: "Authorize again before then to avoid an interruption.", Link: "http://127.0.0.1:7777/auth/linear"},
		{Connector: "slack", Summary: "The refresh token expires at 2026-11-08T18:00:00Z."},
		{Connector: "github", Summary: "The refresh token expires at 2026-11-08T19:00:00Z."},
	}
	g := gateway.New(gateway.Options{
		Upstreams: &stubUpstreams{statuses: []upstream.ConnectorStatus{
			ready(),
			{Connector: "slack", State: upstream.StateNeedsAuth},
			{Connector: "github", State: upstream.StateReady},
		}},
		Catalog: &listCatalog{tools: []store.Tool{linearTool(t, createIssue())}},
		Logger:  zerolog.Nop(),
		Warnings: func(_ context.Context, connectors []string) []gateway.Warning {
			var out []gateway.Warning

			for _, w := range warnings {
				if slices.Contains(connectors, w.Connector) {
					out = append(out, w)
				}
			}

			asked.Store(&connectors)

			return out
		},
	})
	srv := httptest.NewServer(authed(g))
	t.Cleanup(srv.Close)
	cs := connect(t, srv.URL, fakeupstream.V20250618)

	linear := map[string]any{
		"connector": "linear", "summary": warnings[0].Summary, "action": warnings[0].Action, "link": warnings[0].Link,
	}
	github := map[string]any{"connector": "github", "summary": warnings[2].Summary}

	for _, tt := range []struct {
		name  string
		args  map[string]any
		asked []string
		want  []any
	}{
		{"every connector", map[string]any{"query": "x"}, []string{"linear", "github"}, []any{linear, github}},
		{"some connectors", map[string]any{"query": "x", "connectors": []string{"linear", "slack"}}, []string{"linear"}, []any{linear}},
		{"by name", map[string]any{"names": []string{"linear.create_issue"}}, []string{"linear"}, []any{linear}},
	} {
		got := search(t, cs, tt.args)

		if diff := cmp.Diff(tt.want, got["warnings"]); diff != "" {
			t.Errorf("%s: warnings (-want +got):\n%s", tt.name, diff)
		}

		if diff := cmp.Diff(tt.asked, *asked.Load()); diff != "" {
			t.Errorf("%s: connectors asked about (-want +got):\n%s", tt.name, diff)
		}
	}
}

// toolNames is the name of each tool in a search_tools response.
func toolNames(t *testing.T, resp map[string]any) []string {
	t.Helper()
	tools, _ := resp["tools"].([]any)
	names := make([]string, 0, len(tools))

	for _, tl := range tools {
		m, _ := tl.(map[string]any)
		n, _ := m["name"].(string)
		names = append(names, n)
	}

	return names
}

// ready is the linear connector's status when it is ready.
func ready() upstream.ConnectorStatus {
	return upstream.ConnectorStatus{Connector: "linear", State: upstream.StateReady}
}

func TestBriefSearchReturnsCompactEntries(t *testing.T) {
	object := map[string]any{"type": "object"}
	cat := &listCatalog{tools: []store.Tool{
		linearTool(t, createIssue()),
		linearTool(t, &mcp.Tool{
			Name:        "list_teams",
			Description: "Lists teams. Paginated.\nReturns id and name.",
			InputSchema: object,
		}),
		linearTool(t, &mcp.Tool{
			Name:        "wordy",
			Description: strings.Repeat("word ", 50),
			InputSchema: object,
		}),
		linearTool(t, &mcp.Tool{Name: "archive_team", Title: "Archive team", InputSchema: object}),
		linearTool(t, &mcp.Tool{Name: "bare", InputSchema: object}),
	}}
	cs := newListed(t, cat, ready())
	got := search(t, cs, map[string]any{"query": "x", "detail": "brief"})
	want := map[string]any{
		"tools": []any{
			map[string]any{
				"name":        "linear.create_issue",
				"title":       "Create issue",
				"summary":     "Creates a Linear issue.",
				"required":    []any{"title"},
				"annotations": map[string]any{"destructiveHint": false, "idempotentHint": false, "readOnlyHint": false},
			},
			map[string]any{"name": "linear.list_teams", "summary": "Lists teams.", "required": []any{}},
			// Cut on a word boundary to at most 160 runes, ellipsis included.
			map[string]any{"name": "linear.wordy", "summary": strings.Repeat("word ", 30) + "word…", "required": []any{}},
			map[string]any{"name": "linear.archive_team", "title": "Archive team", "summary": "Archive team", "required": []any{}},
			map[string]any{"name": "linear.bare", "required": []any{}},
		},
		"page":        float64(1),
		"size":        float64(15),
		"has_more":    false,
		"unavailable": []any{},
		"warnings":    []any{},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("brief search_tools response (-want +got):\n%s", diff)
	}

	if n := cat.lastSize(); n != 15 {
		t.Errorf("catalog searched with size %d, want the brief default 15", n)
	}
}

func TestBriefAndFullSearchReturnTheSameToolsInOrder(t *testing.T) {
	e := newEnv(t, map[string][]fakeupstream.Option{"linear": {fakeupstream.WithTools(
		createIssue(),
		&mcp.Tool{Name: "update_issue", Description: "Updates a Linear issue.", InputSchema: map[string]any{"type": "object"}},
		&mcp.Tool{Name: "list_issues", Description: "Lists Linear issues.", InputSchema: map[string]any{"type": "object"}},
	)}})
	cs := connect(t, e.url, fakeupstream.V20251125)
	full := toolNames(t, search(t, cs, map[string]any{"query": "linear issue"}))
	brief := toolNames(t, search(t, cs, map[string]any{"query": "linear issue", "detail": "brief", "size": 5}))

	if len(full) == 0 {
		t.Fatal("full search found nothing")
	}

	if diff := cmp.Diff(full, brief); diff != "" {
		t.Errorf("brief tools differ from full tools (-full +brief):\n%s", diff)
	}
}

func TestSearchSizeLimitsDependOnDetail(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want float64 // response size; 0 means an error
	}{
		{"full default", map[string]any{"query": "x"}, 5},
		{"full detail default", map[string]any{"query": "x", "detail": "full"}, 5},
		{"full max", map[string]any{"query": "x", "size": 10}, 10},
		{"full over max", map[string]any{"query": "x", "size": 11}, 0},
		{"brief default", map[string]any{"query": "x", "detail": "brief"}, 15},
		{"brief max", map[string]any{"query": "x", "detail": "brief", "size": 25}, 25},
		{"brief over max", map[string]any{"query": "x", "detail": "brief", "size": 26}, 0},
	}
	cs := newListed(t, &listCatalog{}, ready())

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := call(t, cs, "search_tools", tt.args)

			if tt.want == 0 {
				if got := text(t, res); !res.IsError || !strings.Contains(got, "size must be") {
					t.Errorf("result (isError %v) %q, want a size error", res.IsError, got)
				}

				return
			}

			if got := search(t, cs, tt.args)["size"]; got != tt.want {
				t.Errorf("size = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNamesReturnFullDefinitionsInOrderAndListMissing(t *testing.T) {
	cat := &listCatalog{tools: []store.Tool{
		linearTool(t, createIssue()),
		linearTool(t, &mcp.Tool{Name: "update_issue", InputSchema: map[string]any{"type": "object"}}),
	}}
	cs := newListed(t, cat, ready(),
		upstream.ConnectorStatus{Connector: "slack", State: upstream.StateUnreachable},
		upstream.ConnectorStatus{Connector: "github", State: upstream.StateUnreachable},
	)
	got := search(t, cs, map[string]any{"names": []string{
		"linear.update_issue", "slack.post", "linear.create_issue", "nope", "jira.x", "linear.update_issue",
	}})

	if diff := cmp.Diff([]string{"linear.update_issue", "linear.create_issue"}, toolNames(t, got)); diff != "" {
		t.Errorf("tools (-want +got):\n%s", diff)
	}

	// Full definitions, as a search returns them.
	tools, _ := got["tools"].([]any)

	for _, tl := range tools {
		if def, _ := tl.(map[string]any); def["inputSchema"] == nil {
			t.Errorf("tool %v has no inputSchema, want the full definition", def)
		}
	}

	if diff := cmp.Diff([]any{"slack.post", "nope", "jira.x"}, got["missing"]); diff != "" {
		t.Errorf("missing (-want +got):\n%s", diff)
	}

	if got["page"] != float64(1) || got["size"] != float64(2) || got["has_more"] != false {
		t.Errorf("page, size, has_more = %v, %v, %v; want 1, 2, false", got["page"], got["size"], got["has_more"])
	}

	// Only the connectors the names mention: not github.
	unavailable, _ := got["unavailable"].([]any)

	if len(unavailable) != 1 {
		t.Fatalf("unavailable = %v, want slack only", unavailable)
	}

	if u, _ := unavailable[0].(map[string]any); u["connector"] != "slack" {
		t.Errorf("unavailable[0] = %v, want slack", u)
	}
}

// When none of the names is found and a connector they mention is not
// ready, the error says so and carries that connector's fix, not just
// "search first".
func TestNamesNoneFoundCarriesTheUnavailableConnectorsAdvice(t *testing.T) {
	const link = "http://127.0.0.1:7777/auth/slack"
	slack := upstream.ConnectorStatus{Connector: "slack", State: upstream.StateNeedsAuth, Link: link}
	cs := newListed(t, &listCatalog{}, ready(), slack)
	res := call(t, cs, "search_tools", map[string]any{"names": []string{"slack.post", "linear.nope"}})
	a := slack.Advice()

	if !res.IsError {
		t.Fatalf("result is not an error: %s", text(t, res))
	}

	got := text(t, res)

	for _, w := range []string{"None of these tools is in the catalog: slack.post, linear.nope.", "slack is needs_auth: " + a.Summary, "To fix: " + a.Action, "Link: " + link} {
		if !strings.Contains(got, w) {
			t.Errorf("text %q does not contain %q", got, w)
		}
	}
}

func TestSearchResponseHasNoMissingField(t *testing.T) {
	cs := newListed(t, &listCatalog{tools: []store.Tool{linearTool(t, createIssue())}}, ready())

	for _, detail := range []string{"full", "brief"} {
		if got := search(t, cs, map[string]any{"query": "x", "detail": detail}); got["missing"] != nil {
			t.Errorf("detail %s response has missing: %v", detail, got)
		}
	}
}

// A regression that bloats brief entries shows up here: over the golden
// fixture tools, brief entries stay under half the full bytes (about 40%
// when written). The fixtures' schemas are trimmed, so real upstreams save
// more.
func TestBriefEntriesAreMuchSmallerThanFullDefinitions(t *testing.T) {
	cat := &listCatalog{}

	for _, tools := range golden.Tools(t) {
		cat.tools = append(cat.tools, tools...)
	}

	cs := newListed(t, cat)
	full := len(text(t, call(t, cs, "search_tools", map[string]any{"query": "x"})))
	brief := len(text(t, call(t, cs, "search_tools", map[string]any{"query": "x", "detail": "brief"})))
	t.Logf("%d tools: full %d bytes, brief %d bytes (%.0f%%)", len(cat.tools), full, brief, 100*float64(brief)/float64(full))

	if brief*2 > full {
		t.Errorf("brief response is %d bytes, more than half the full %d", brief, full)
	}
}
