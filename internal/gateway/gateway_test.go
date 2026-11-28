package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/catalog"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// createIssue is the linear fake's tool: one required string parameter.
func createIssue() *mcp.Tool {
	return &mcp.Tool{
		Name:        "create_issue",
		Title:       "Create issue",
		Description: "Creates a Linear issue.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"title": map[string]any{"type": "string", "description": "Issue title"}},
			"required":   []any{"title"},
		},
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(bool)},
	}
}

// env is a gateway wired the way the daemon wires it: a real
// upstream.Manager over fakeupstream connectors, and a SQLite catalog the
// Syncer filled from them.
type env struct {
	url     string
	manager *upstream.Manager
	gateway *gateway.Gateway
	// options are the gateway's, so a test can start another over the
	// same upstreams, catalog, and master key, as a restart would.
	options gateway.Options
}

// envOptions configures newEnvWith. embedder, when set, embeds the catalog
// before the gateway starts; gateway supplies the query-embedding options
// (Embedder, EmbedTimeout), the rest being filled in by newEnvWith.
type envOptions struct {
	fakes    map[string][]fakeupstream.Option
	down     []string
	embedder catalog.Embedder
	gateway  gateway.Options
	// upstreamVersion pins the fakes' protocol version; empty means
	// 2025-11-25.
	upstreamVersion string
	// syncPolicy is the tool policy the catalog was synced under; nil
	// exposes every tool. gateway.Policy is the one call_tool checks.
	syncPolicy func(connector string) config.ToolPolicy
	// logs, when set, receives the gateway's log as JSON lines; nil
	// discards it.
	logs io.Writer
}

// newEnv starts one Streamable fake per connector in fakes (connector name
// to fake options), applies them, syncs each into a temp SQLite catalog,
// and serves the gateway over httptest. Connectors in down are configured
// at a URL nothing listens on, so they end up unreachable.
func newEnv(t *testing.T, fakes map[string][]fakeupstream.Option, down ...string) env {
	t.Helper()

	return newEnvWith(t, envOptions{fakes: fakes, down: down})
}

// newEnvWith is newEnv with an optional embedding provider.
func newEnvWith(t *testing.T, o envOptions) env {
	t.Helper()
	fakes, down := o.fakes, o.down
	ctx := context.Background()
	cfg := &config.Config{Connectors: config.Connectors{}}

	version := o.upstreamVersion

	if version == "" {
		version = fakeupstream.V20251125
	}

	for name, opts := range fakes {
		srv := fakeupstream.StartStreamable(t, version, opts...)
		cfg.Connectors[name] = config.Connector{Name: name, URL: config.Template(srv.URL)}
	}

	for _, name := range down {
		dead := httptest.NewServer(nil)
		dead.Close()
		cfg.Connectors[name] = config.Connector{Name: name, URL: config.Template(dead.URL)}
	}

	m := upstream.New(upstream.Options{Logger: zerolog.Nop()})
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Errorf("Manager.Close: %v", err)
		}
	})
	m.Apply(ctx, cfg.Connectors, config.Diff(nil, cfg))

	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })
	user, err := st.EnsureUser(ctx)

	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	syncer := catalog.New(catalog.Options{
		Lister: m, Store: st, Embedder: o.embedder, UserID: user, Logger: zerolog.Nop(), Policy: o.syncPolicy,
	})
	t.Cleanup(func() { _ = syncer.Close(context.Background()) })

	for name := range fakes {
		if err := syncer.Sync(ctx, name); err != nil {
			t.Fatalf("Sync %s: %v", name, err)
		}
	}

	if o.embedder != nil {
		if err := syncer.EmbedPending(ctx); err != nil {
			t.Fatalf("EmbedPending: %v", err)
		}
	}

	opts := o.gateway
	opts.Upstreams, opts.Catalog, opts.UserID, opts.Logger = m, st, user, zerolog.Nop()

	if o.logs != nil {
		opts.Logger = zerolog.New(o.logs)
	}

	if opts.Sealer == nil {
		sealer, err := secrets.NewSealer(make([]byte, secrets.KeySize))

		if err != nil {
			t.Fatalf("NewSealer: %v", err)
		}

		opts.Sealer = sealer
	}

	g := gateway.New(opts)
	srv := httptest.NewServer(authed(g))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})

	return env{url: srv.URL, manager: m, gateway: g, options: opts}
}

// connect opens an SDK client session to the gateway at version, and
// checks the gateway negotiated it.
func connect(t *testing.T, url, version string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url, MaxRetries: -1},
		&mcp.ClientSessionOptions{ProtocolVersion: version})

	if err != nil {
		t.Fatalf("Connect at %s: %v", version, err)
	}

	t.Cleanup(func() { _ = cs.Close() })

	if got := cs.InitializeResult().ProtocolVersion; got != version {
		t.Fatalf("negotiated protocol version %s, want %s", got, version)
	}

	return cs
}

func TestEveryClientVersionListsExactlyTheTwoMetaTools(t *testing.T) {
	e := newEnv(t, map[string][]fakeupstream.Option{"linear": {fakeupstream.WithTools(createIssue())}})

	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			cs := connect(t, e.url, version)
			res, err := cs.ListTools(t.Context(), nil)

			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}

			var names []string

			for _, tool := range res.Tools {
				names = append(names, tool.Name)
			}

			if diff := cmp.Diff([]string{"call_tool", "search_tools"}, names); diff != "" {
				t.Errorf("tools (-want +got):\n%s", diff)
			}

			caps, err := json.Marshal(cs.InitializeResult().Capabilities)

			if err != nil {
				t.Fatal(err)
			}

			// Tools without listChanged, and no logging: a stateless
			// handler can deliver neither.
			if got, want := string(caps), `{"tools":{}}`; got != want {
				t.Errorf("capabilities = %s, want %s", got, want)
			}
		})
	}
}

// The gateway declares only tools, so every request of the prompts,
// resources, and completions capabilities is "method not found", as from a
// server that never implemented them, rather than an empty list (design
// §7.6).
func TestEveryClientVersionAnswersUndeclaredCapabilitiesWithMethodNotFound(t *testing.T) {
	e := newEnv(t, map[string][]fakeupstream.Option{"linear": {fakeupstream.WithTools(createIssue())}})
	requests := []struct {
		method string
		do     func(context.Context, *mcp.ClientSession) error
	}{
		{"prompts/list", func(ctx context.Context, cs *mcp.ClientSession) error {
			_, err := cs.ListPrompts(ctx, nil)

			return err
		}},
		{"prompts/get", func(ctx context.Context, cs *mcp.ClientSession) error {
			_, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greeting"})

			return err
		}},
		{"resources/list", func(ctx context.Context, cs *mcp.ClientSession) error {
			_, err := cs.ListResources(ctx, nil)

			return err
		}},
		{"resources/templates/list", func(ctx context.Context, cs *mcp.ClientSession) error {
			_, err := cs.ListResourceTemplates(ctx, nil)

			return err
		}},
		{"resources/read", func(ctx context.Context, cs *mcp.ClientSession) error {
			_, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///notes.txt"})

			return err
		}},
		{"completion/complete", func(ctx context.Context, cs *mcp.ClientSession) error {
			_, err := cs.Complete(ctx, &mcp.CompleteParams{
				Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "greeting"},
				Argument: mcp.CompleteParamsArgument{Name: "name"},
			})

			return err
		}},
	}

	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			cs := connect(t, e.url, version)

			for _, r := range requests {
				var werr *jsonrpc.Error

				if err := r.do(t.Context(), cs); !errors.As(err, &werr) || werr.Code != -32601 {
					t.Errorf("%s: err = %v, want JSON-RPC error -32601", r.method, err)
				}
			}

			// The session survives, and the declared capability is served.
			if _, err := cs.ListTools(t.Context(), nil); err != nil {
				t.Errorf("tools/list after them: %v", err)
			}
		})
	}
}

// /mcp reads at most 4 MiB of a request body (design §7.6). A larger body
// is 413 with a message naming the limit, at every protocol version; a body
// of exactly the limit is read.
func TestMCPRequestBodyIsCappedAt4MiB(t *testing.T) {
	e := newEnv(t, map[string][]fakeupstream.Option{"linear": {fakeupstream.WithTools(createIssue())}})
	const limit = 4 << 20
	// initialize padded with spaces to n bytes: valid JSON of any size.
	initialize := func(version string, n int) string {
		msg := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + version +
			`","capabilities":{},"clientInfo":{"name":"t","version":"v0"}}}`

		return msg[:len(msg)-1] + strings.Repeat(" ", n-len(msg)) + "}"
	}
	post := func(t *testing.T, version, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.url, strings.NewReader(body))

		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", version)
		resp, err := http.DefaultClient.Do(req)

		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)

		if err != nil {
			t.Fatal(err)
		}

		return resp.StatusCode, string(b)
	}

	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			code, body := post(t, version, initialize(version, limit+1))

			if code != http.StatusRequestEntityTooLarge || !strings.Contains(body, "request body exceeds 4194304 bytes") {
				t.Errorf("one byte over the limit: %d %q, want 413 naming the 4194304-byte limit", code, body)
			}

			// Read in full and answered by the protocol layer: 200 to
			// initialize before 2026-07-28, 400 for its missing _meta from
			// then on. Either way, not 413.
			if code, body := post(t, version, initialize(version, limit)); code == http.StatusRequestEntityTooLarge {
				t.Errorf("exactly the limit: %d %q, want it read", code, body)
			}
		})
	}
}

// call calls a meta-tool and fails the test on a protocol error.
func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})

	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}

	return res
}

// text is a result's single text block.
func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()

	if len(res.Content) != 1 {
		t.Fatalf("result has %d content blocks, want 1: %+v", len(res.Content), res.Content)
	}

	tc, ok := res.Content[0].(*mcp.TextContent)

	if !ok {
		t.Fatalf("content is %T, want *mcp.TextContent", res.Content[0])
	}

	return tc.Text
}

// search calls search_tools and decodes its successful response.
func search(t *testing.T, cs *mcp.ClientSession, args map[string]any) map[string]any {
	t.Helper()
	res := call(t, cs, "search_tools", args)

	if res.IsError {
		t.Fatalf("search_tools failed: %s", text(t, res))
	}

	var out map[string]any

	if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
		t.Fatalf("search_tools response is not JSON: %v", err)
	}

	return out
}

func TestSearchReturnsQualifiedDefinitionsAndUnavailableConnectors(t *testing.T) {
	e := newEnv(t, map[string][]fakeupstream.Option{"linear": {fakeupstream.WithTools(createIssue())}}, "slack")

	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			cs := connect(t, e.url, version)
			got := search(t, cs, map[string]any{"query": "create issue"})

			unavailable, _ := got["unavailable"].([]any)
			delete(got, "unavailable")
			want := map[string]any{
				"tools": []any{map[string]any{
					"name":        "linear.create_issue",
					"title":       "Create issue",
					"description": "Creates a Linear issue.",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"title": map[string]any{"type": "string", "description": "Issue title"}},
						"required":   []any{"title"},
					},
					// As the SDK upstream sends them: it always writes these
					// two hints.
					"annotations": map[string]any{"destructiveHint": false, "idempotentHint": false, "readOnlyHint": false},
				}},
				"page":     float64(1),
				"size":     float64(5),
				"has_more": false,
				"warnings": []any{},
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("search_tools response (-want +got):\n%s", diff)
			}

			if len(unavailable) != 1 {
				t.Fatalf("unavailable = %v, want one entry for slack", unavailable)
			}

			u, _ := unavailable[0].(map[string]any)

			if u["connector"] != "slack" || u["status"] != "unreachable" {
				t.Errorf("unavailable[0] = %v, want connector slack with status unreachable", u)
			}

			if s, _ := u["summary"].(string); s == "" {
				t.Errorf("unavailable[0] has no summary: %v", u)
			}

			if a, _ := u["action"].(string); a == "" {
				t.Errorf("unavailable[0] has no action: %v", u)
			}
		})
	}
}

func TestCallToolForwardsValidArgumentsAndReturnsTheResultUnchanged(t *testing.T) {
	e := newEnv(t, map[string][]fakeupstream.Option{"linear": {fakeupstream.WithTools(createIssue())}})

	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			cs := connect(t, e.url, version)
			res := call(t, cs, "call_tool", map[string]any{
				"name":      "linear.create_issue",
				"arguments": map[string]any{"title": "Fix login"},
			})

			if res.IsError {
				t.Fatalf("call_tool failed: %s", text(t, res))
			}

			// The fake echoes its arguments as compact JSON.
			if got, want := text(t, res), `{"title":"Fix login"}`; got != want {
				t.Errorf("result text = %s, want %s", got, want)
			}
		})
	}
}

func TestCallToolRendersAnUpstreamFailureAsAnErrorResult(t *testing.T) {
	e := newEnv(t, map[string][]fakeupstream.Option{"linear": {fakeupstream.WithSampling()}})

	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			cs := connect(t, e.url, version)
			res := call(t, cs, "call_tool", map[string]any{"name": "linear." + fakeupstream.ToolSample})

			if got := text(t, res); !res.IsError || !strings.Contains(got, "sample a message") || !strings.Contains(got, "To fix: ") {
				t.Errorf("result (isError %v) %q, want the interactive_input (sampling) advice", res.IsError, got)
			}
		})
	}
}

func TestCallToolRejectsInvalidArgumentsBeforeTheUpstream(t *testing.T) {
	e := newEnv(t, map[string][]fakeupstream.Option{"linear": {fakeupstream.WithTools(createIssue())}})

	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			cs := connect(t, e.url, version)
			res := call(t, cs, "call_tool", map[string]any{
				"name":      "linear.create_issue",
				"arguments": map[string]any{"title": 42},
			})

			// The fake would echo any arguments back as a success, so an
			// isError result naming the schema failure came from the gateway.
			if !res.IsError {
				t.Fatalf("call_tool succeeded with %s, want an isError result", text(t, res))
			}

			got := text(t, res)

			for _, want := range []string{"linear.create_issue's inputSchema", "/title", "To fix:"} {
				if !strings.Contains(got, want) {
					t.Errorf("result text %q does not contain %q", got, want)
				}
			}
		})
	}
}
