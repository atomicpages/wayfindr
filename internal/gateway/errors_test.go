package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// The daemon passes its upstream.Manager and its store to the gateway.
var (
	_ gateway.Upstreams = (*upstream.Manager)(nil)
	_ gateway.Catalog   = store.Store(nil)
)

// stubUpstreams fails every call with err (or returns res), and counts the
// calls that reach it. It stands in for upstream.Manager because no fake
// can make the Manager produce every Kind on demand (needs_auth needs an
// authorization server, auth_config a broken one).
type stubUpstreams struct {
	mu       sync.Mutex
	calls    int
	res      *mcp.CallToolResult
	err      error
	statuses []upstream.ConnectorStatus
}

func (s *stubUpstreams) Call(context.Context, string, string, json.RawMessage) (*mcp.CallToolResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++

	return s.res, s.err
}

func (s *stubUpstreams) Continue(ctx context.Context, connector, tool string, args json.RawMessage, _ upstream.Input) (*mcp.CallToolResult, error) {
	return s.Call(ctx, connector, tool, args)
}

func (s *stubUpstreams) Discard(string) {}

func (s *stubUpstreams) Status() []upstream.ConnectorStatus { return s.statuses }

func (s *stubUpstreams) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

// stubCatalog serves linear.create_issue; the connector "broken" fails to
// read, and so does a search for "fail".
// It stands in for the store so a test can change a definition in place.
type stubCatalog struct {
	mu   sync.Mutex
	tool store.Tool
}

func (c *stubCatalog) set(tool store.Tool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tool = tool
}

func (c *stubCatalog) GetTool(_ context.Context, _, connector, name string) (store.Tool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch {
	case connector == "broken":
		return store.Tool{}, errors.New("disk on fire")
	case connector == "linear" && name == c.tool.Name:
		return c.tool, nil
	}

	return store.Tool{}, store.ErrNotFound
}

// LastSync implements gateway.Catalog: no sync is ever recorded.
func (c *stubCatalog) LastSync(context.Context, string, string) (store.SyncRecord, error) {
	return store.SyncRecord{}, store.ErrNotFound
}

func (c *stubCatalog) Search(_ context.Context, q store.SearchQuery) (store.SearchPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if q.Text == "fail" {
		return store.SearchPage{}, errors.New("disk on fire")
	}

	return store.SearchPage{Tools: []store.Tool{c.tool}}, nil
}

// linearTool is tool as the catalog holds it for the linear connector, with
// a fixed content hash: the hash covers only names and descriptions.
func linearTool(t *testing.T, tool *mcp.Tool) store.Tool {
	t.Helper()
	def, err := json.Marshal(tool)

	if err != nil {
		t.Fatal(err)
	}

	st, err := store.NewTool("linear", def, []byte("same-hash"))

	if err != nil {
		t.Fatal(err)
	}

	return st
}

func newStubbed(t *testing.T, up *stubUpstreams) *mcp.ClientSession {
	t.Helper()

	return newStubbedCatalog(t, up, &stubCatalog{tool: linearTool(t, createIssue())})
}

func newStubbedCatalog(t *testing.T, up *stubUpstreams, cat *stubCatalog) *mcp.ClientSession {
	t.Helper()
	up.statuses = []upstream.ConnectorStatus{
		{Connector: "broken", State: upstream.StateReady},
		{Connector: "linear", State: upstream.StateReady},
	}
	g := gateway.New(gateway.Options{
		Upstreams: up,
		Catalog:   cat,
		Logger:    zerolog.Nop(),
	})
	srv := httptest.NewServer(authed(g))
	t.Cleanup(srv.Close)

	return connect(t, srv.URL, fakeupstream.V20250618)
}

func TestGatewayFailuresRenderAsErrorResultsWithoutCallingTheUpstream(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args map[string]any
		want []string // phrases the text must contain
	}{
		{"missing query", "search_tools", map[string]any{}, []string{"needs a query"}},
		{"blank query", "search_tools", map[string]any{"query": "  "}, []string{"needs a query"}},
		{"query not a string", "search_tools", map[string]any{"query": 7}, []string{"wrong types"}},
		{"size zero", "search_tools", map[string]any{"query": "x", "size": 0}, []string{"size must be", "not 0"}},
		{"size eleven", "search_tools", map[string]any{"query": "x", "size": 11}, []string{"from 1 to 10", "not 11"}},
		{"size fractional", "search_tools", map[string]any{"query": "x", "size": 2.5}, []string{"size must be"}},
		{"page zero", "search_tools", map[string]any{"query": "x", "page": 0}, []string{"page must be"}},
		{"unknown connector", "search_tools", map[string]any{"query": "x", "connectors": []string{"jira"}},
			[]string{"No connector is called jira", "Configured connectors: broken, linear"}},
		{"search catalog failure", "search_tools", map[string]any{"query": "fail"}, []string{"could not read its tool catalog"}},
		{"bad detail", "search_tools", map[string]any{"query": "x", "detail": "short"}, []string{`detail must be "full" or "brief", not "short"`}},
		{"detail not a string", "search_tools", map[string]any{"query": "x", "detail": 1}, []string{"wrong types"}},
		{"brief size over 25", "search_tools", map[string]any{"query": "x", "detail": "brief", "size": 26}, []string{"up to 25 (default 15) with detail brief", "not 26"}},
		{"names not an array", "search_tools", map[string]any{"names": "linear.create_issue"}, []string{"wrong types"}},
		{"empty names", "search_tools", map[string]any{"names": []string{}}, []string{"names must list 1 to 10"}},
		{"eleven names", "search_tools", map[string]any{"names": strings.Split("a.1 a.2 a.3 a.4 a.5 a.6 a.7 a.8 a.9 a.10 a.11", " ")},
			[]string{"names must list 1 to 10"}},
		{"blank name", "search_tools", map[string]any{"names": []string{"linear.create_issue", " "}}, []string{"none blank"}},
		{"names with query", "search_tools", map[string]any{"names": []string{"linear.create_issue"}, "query": "x"},
			[]string{"names cannot be combined with query"}},
		{"names with page", "search_tools", map[string]any{"names": []string{"linear.create_issue"}, "page": 1},
			[]string{"names cannot be combined with page"}},
		{"names with size", "search_tools", map[string]any{"names": []string{"linear.create_issue"}, "size": 1},
			[]string{"names cannot be combined with size"}},
		{"names with connectors", "search_tools", map[string]any{"names": []string{"linear.create_issue"}, "connectors": []string{"linear"}},
			[]string{"names cannot be combined with connectors"}},
		{"names with brief", "search_tools", map[string]any{"names": []string{"linear.create_issue"}, "detail": "brief"},
			[]string{"names cannot be combined with detail brief"}},
		{"names none found", "search_tools", map[string]any{"names": []string{"linear.delete_issue", "jira.x", "create_issue"}},
			[]string{"None of these tools is in the catalog: linear.delete_issue, jira.x, create_issue", "search_tools"}},
		{"names catalog failure", "search_tools", map[string]any{"names": []string{"broken.x"}}, []string{"could not read its tool catalog"}},
		{"missing name", "call_tool", map[string]any{}, []string{"needs name", "search_tools"}},
		{"name not a string", "call_tool", map[string]any{"name": 3}, []string{"needs name"}},
		{"unqualified name", "call_tool", map[string]any{"name": "create_issue"}, []string{"create_issue is not a qualified tool name"}},
		{"empty tool part", "call_tool", map[string]any{"name": "linear."}, []string{"not a qualified tool name"}},
		{"unconfigured connector", "call_tool", map[string]any{"name": "jira.create"}, []string{"no connector is called jira", "search_tools"}},
		{"tool not in catalog", "call_tool", map[string]any{"name": "linear.delete_issue"}, []string{"no tool called linear.delete_issue", "search_tools"}},
		{"call catalog failure", "call_tool", map[string]any{"name": "broken.x"}, []string{"could not read its tool catalog"}},
		{"arguments not an object", "call_tool", map[string]any{"name": "linear.create_issue", "arguments": []any{1}},
			[]string{"must be a JSON object"}},
		{"missing required argument", "call_tool", map[string]any{"name": "linear.create_issue"},
			[]string{"linear.create_issue's inputSchema", "- at /: missing property 'title'", `search_tools names=["linear.create_issue"]`}},
		{"wrong argument type", "call_tool", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": true}},
			[]string{"- at /title: got boolean, want string"}},
	}
	up := &stubUpstreams{res: &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}}
	cs := newStubbed(t, up)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := call(t, cs, tt.tool, tt.args)

			if !res.IsError {
				t.Fatalf("result is not an error: %s", text(t, res))
			}

			got := text(t, res)

			for _, w := range append(tt.want, "To fix: ") {
				if !strings.Contains(got, w) {
					t.Errorf("text %q does not contain %q", got, w)
				}
			}
		})
	}

	if n := up.callCount(); n != 0 {
		t.Errorf("upstream was called %d times, want 0", n)
	}
}

func TestUpstreamFailuresRenderTheirAdviceAsErrorResults(t *testing.T) {
	kinds := []upstream.Kind{
		upstream.KindUnknownConnector, upstream.KindNeedsAuth, upstream.KindAuthConfig,
		upstream.KindMissingValue, upstream.KindInteractiveInput, upstream.KindTimeout,
		upstream.KindCanceled, upstream.KindRejected, upstream.KindUnreachable,
	}

	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			e := &upstream.Error{Connector: "linear", Kind: k, Ref: "secret:linear"}
			up := &stubUpstreams{err: e}
			cs := newStubbed(t, up)
			res := call(t, cs, "call_tool", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "x"}})

			if !res.IsError {
				t.Fatalf("result is not an error: %s", text(t, res))
			}

			// The gateway renders upstream's advice; it adds no wording of
			// its own.
			a := upstream.Advise(e)
			got := text(t, res)

			for _, w := range []string{a.Summary, "To fix: " + a.Action} {
				if !strings.Contains(got, w) {
					t.Errorf("text %q does not contain %q", got, w)
				}
			}
		})
	}
}

// The upstream error carries the link: the manager fills it in from its
// Options.AuthURL when the cause had none (upstream's tests pin that).
func TestNeedsAuthFailureCarriesTheAuthLink(t *testing.T) {
	const link = "http://127.0.0.1:7777/auth/linear"
	up := &stubUpstreams{err: &upstream.Error{Connector: "linear", Kind: upstream.KindNeedsAuth, Case: upstream.CaseStepUp, Link: link}}
	cs := newStubbed(t, up)
	res := call(t, cs, "call_tool", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "x"}})

	if got := text(t, res); !res.IsError || !strings.HasSuffix(got, "Link: "+link) {
		t.Errorf("result (isError %v) %q, want an error ending %q", res.IsError, got, "Link: "+link)
	}
}

func TestFailureOutsideTheUpstreamContractRendersAsInternalError(t *testing.T) {
	up := &stubUpstreams{err: errors.New("not an *upstream.Error")}
	cs := newStubbed(t, up)
	res := call(t, cs, "call_tool", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "x"}})

	if got := text(t, res); !res.IsError || !strings.Contains(got, "Wayfindr failed") || strings.Contains(got, "not an") {
		t.Errorf("result (isError %v) %q, want a generic internal error", res.IsError, got)
	}
}

func TestUpstreamErrorResultIsReturnedUnchanged(t *testing.T) {
	up := &stubUpstreams{res: &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "issue tracker says no"}}}}
	cs := newStubbed(t, up)
	res := call(t, cs, "call_tool", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "x"}})

	if got := text(t, res); !res.IsError || got != "issue tracker says no" {
		t.Errorf("result (isError %v) %q, want the upstream's isError result", res.IsError, got)
	}
}

// Every Kind and Case has advice with an action (the exhaustive switches
// guarantee a case exists; this checks none is empty).
func TestAdviseCoversEveryKindAndCase(t *testing.T) {
	kinds := []gateway.Kind{
		gateway.KindInvalidSearch, gateway.KindInvalidName, gateway.KindUnknownTool,
		gateway.KindInvalidArguments, gateway.KindInternal, gateway.KindCanceled,
	}
	cases := []gateway.Case{
		gateway.CaseMalformedSearch, gateway.CaseMissingQuery, gateway.CaseBadPage, gateway.CaseBadSize,
		gateway.CaseUnknownConnector, gateway.CaseMissingName, gateway.CaseUnqualified, gateway.CaseNoConnector,
		gateway.CaseNotInCatalog, gateway.CaseNotObject, gateway.CaseSchemaMismatch, gateway.CaseCatalog,
		gateway.CaseUpstream, gateway.CaseBadDetail, gateway.CaseBadNames, gateway.CaseNamesConflict,
		gateway.CaseNoneFound, gateway.CaseHiddenByPolicy, gateway.CaseNoClient,
	}

	for _, k := range kinds {
		if a := gateway.Advise(&gateway.Error{Kind: k}); a.Summary == "" || a.Action == "" {
			t.Errorf("Advise(kind %s) = %+v, want a summary and an action", k, a)
		}
	}

	for _, c := range cases {
		if a := gateway.Advise(&gateway.Error{Case: c}); a.Summary == "" || a.Action == "" {
			t.Errorf("Advise(case %s) = %+v, want a summary and an action", c, a)
		}
	}

	e := &gateway.Error{Kind: gateway.KindInvalidArguments, Case: gateway.CaseSchemaMismatch, Tool: "linear.create_issue"}

	if got, ok := gateway.AsError(errors.Join(errors.New("context"), e)); !ok || got != e {
		t.Errorf("AsError did not find the wrapped *Error")
	}
}
