// Package gateway is the client-facing MCP server: one stateless Streamable
// HTTP handler exposing exactly two meta-tools, search_tools and call_tool
// (ADR 0001, docs/internal/design.md §7.6). Searches run against the catalog; calls
// are validated against the cached inputSchema and forwarded to the
// upstream. Every gateway-side failure reaches the client as an isError
// result that names the fix.
//
// gateway sits below daemon, which wires its handler into httpd's
// Handlers.MCP.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/toolsonly"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// Upstreams calls connectors' tools and reports their states.
// *upstream.Manager satisfies it.
type Upstreams interface {
	// Call fails only with an *upstream.Error. A result with
	// InputRequests asks the user something (elicitations only).
	Call(ctx context.Context, connector, tool string, args json.RawMessage) (*mcp.CallToolResult, error)
	// Continue answers what a Call or Continue result asked; it returns
	// and fails as Call does.
	Continue(ctx context.Context, connector, tool string, args json.RawMessage, in upstream.Input) (*mcp.CallToolResult, error)
	// Discard gives up on what a result asked, when no one will answer.
	Discard(state string)
	Status() []upstream.ConnectorStatus
}

// Catalog reads the catalog. store.Store satisfies it.
type Catalog interface {
	GetTool(ctx context.Context, userID, connector, name string) (store.Tool, error)
	Search(ctx context.Context, q store.SearchQuery) (store.SearchPage, error)
	// LastSync names the tools the connector's policy hid at its last sync.
	LastSync(ctx context.Context, userID, connector string) (store.SyncRecord, error)
}

// Calls is the call log (GLOSSARY: Call log). The gateway records every
// call_tool call in it before returning the call's result, so Record must
// not block or fail the call.
type Calls interface {
	Record(c store.Call)
}

// Options configures a Gateway.
type Options struct {
	Upstreams Upstreams
	Catalog   Catalog
	// UserID owns the catalog rows searched. Local mode has one user.
	UserID string
	Logger zerolog.Logger
	// Version is the Wayfindr version sent to clients as serverInfo. Empty
	// means "dev".
	Version string
	// Embedder embeds search queries for hybrid search. Nil means no
	// embedding provider is configured: search is lexical only.
	Embedder Embedder
	// EmbedTimeout bounds one query embedding (embedding.timeout). Zero
	// means config.DefaultEmbeddingTimeout.
	EmbedTimeout time.Duration
	// Warnings returns the failures of connectors, all ready, due within a
	// day, that the agent should tell the user of (GLOSSARY: Warning); the
	// daemon reads them from the credentials. Nil means none.
	Warnings func(ctx context.Context, connectors []string) []Warning
	// Calls records call_tool calls. Nil means they are not recorded.
	Calls Calls
	// OnSearch is told of every search_tools call, as it starts; it must
	// not block. Nil means no one is.
	OnSearch func()
	// Sealer seals the requestState of call_tool's input_required
	// results. Required.
	Sealer *secrets.Sealer
	// Now is the clock request states and approvals expire by. Nil means
	// time.Now.
	Now func() time.Time
	// ApprovalIDs is where approval ids' random bytes come from. Nil means
	// crypto/rand; tests set it to draw known ids.
	ApprovalIDs io.Reader
	// Policy returns a connector's tool policy as the config now has it.
	// call_tool refuses a tool it hides, even one a sync under an older
	// policy left in the catalog. Nil exposes every tool.
	Policy func(connector string) config.ToolPolicy
	// Profile returns the profile called name as the config now has it
	// (ADR 0011), or false when the config defines none by that name. Nil
	// defines no profiles, so a request naming one is refused.
	Profile func(name string) (*config.Scope, bool)
}

// Warning is a connector failure due soon that the user can head off:
// search_tools lists it under warnings, so the agent can say so before a
// call fails.
type Warning struct {
	Connector string `json:"connector"`
	Summary   string `json:"summary"`
	Action    string `json:"action,omitempty"`
	Link      string `json:"link,omitempty"`
}

// Gateway is the client-facing MCP endpoint. It is an http.Handler for
// POST, GET, and DELETE /mcp.
type Gateway struct {
	upstreams Upstreams
	catalog   Catalog
	userID    string
	log       zerolog.Logger
	schemas   *schemaCache
	queries   *queryEmbedder
	warnings  func(ctx context.Context, connectors []string) []Warning
	calls     Calls
	onSearch  func()
	sealer    *secrets.Sealer
	clock     func() time.Time
	policy    func(connector string) config.ToolPolicy
	profile   func(name string) (*config.Scope, bool)
	approvals *approvalQueue
	// boot is a random id this Gateway seals into each request state, so a
	// state the approval gate covers is taken only by the process that
	// sealed it, whose approvals.spent remembers whether it was taken.
	boot    string
	handler http.Handler
}

// SetEmbedder replaces the query embedder and its timeout, as on an
// EmbeddingChanged config reload. A nil e (an untyped nil interface)
// removes it, so search becomes lexical only. Cached query vectors are
// keyed by model identity, so none is reused under another model.
func (g *Gateway) SetEmbedder(e Embedder, timeout time.Duration) {
	g.queries.set(e, timeout)
}

// New returns a Gateway serving one mcp.Server with search_tools and
// call_tool over a stateless Streamable HTTP handler, which serves every
// supported client protocol version (design §7.6, spike R1).
func New(opts Options) *Gateway {
	version := opts.Version

	if version == "" {
		version = "dev"
	}

	g := &Gateway{
		upstreams: opts.Upstreams,
		catalog:   opts.Catalog,
		userID:    opts.UserID,
		log:       opts.Logger,
		schemas:   newSchemaCache(),
		queries:   newQueryEmbedder(opts.Embedder, opts.EmbedTimeout, opts.Logger),
		warnings:  opts.Warnings,
		policy:    opts.Policy,
		profile:   opts.Profile,
		calls:     opts.Calls,
		onSearch:  opts.OnSearch,
		sealer:    opts.Sealer,
		clock:     opts.Now,
		approvals: newApprovalQueue(opts.ApprovalIDs),
		boot:      rand.Text(),
	}
	server := mcp.NewServer(
		&mcp.Implementation{Name: "wayfindr", Version: version},
		// A stateless handler can push neither list_changed nor log
		// messages, so declare tools without listChanged and no logging;
		// the SDK would advertise both by default (design §7.7).
		&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}},
	)
	// prompts/*, resources/*, and completion/* are -32601, as from a server
	// that never implemented them, not the SDK's empty lists (design §7.6).
	server.AddReceivingMiddleware(toolsonly.Middleware, endWithRequest)
	server.AddTool(searchToolsTool(), g.searchTools)
	server.AddTool(callToolTool(), g.callTool)
	g.handler = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:           true,
			MaxRequestBodyBytes: maxRequestBody,
			// httpd's host guard owns the Host and DNS-rebinding decision,
			// and is stricter than the SDK's: it checks the port and the
			// forwarding headers too, and lets a remote request reach /mcp
			// only with a remote client token (ADR 0021, design §7.7). The
			// SDK's own check would answer 403 to every such request on a
			// loopback listener, so a tunnel that keeps the public Host
			// could never reach the gateway.
			DisableLocalhostProtection: true,
		},
	)

	return g
}

// maxRequestBody bounds the body of a request to /mcp; a larger one is 413
// "request body exceeds 4194304 bytes". The largest legitimate body is a
// call_tool whose arguments carry a document or a file for the upstream,
// so the cap is generous, but it bounds what one request can make the
// daemon buffer (design §7.6). It equals the SDK's default today and is set
// explicitly so an SDK upgrade can't change it unnoticed.
const maxRequestBody = 4 << 20

// ServeHTTP serves the MCP endpoint. A client that connects over HTTP
// directly picks its profile with /mcp?profile=<name> (ADR 0011), which is
// copied into config.ProfileHeader, as the stdio shim sends it, because
// tool handlers see only the request's headers. Each value is added, so a
// name given twice, by query and header or by a repeated query, is refused
// rather than one of them chosen. r itself is left as it is.
//
// It fails closed. A query that doesn't parse might hold a profile that
// would be dropped, and a key such as Profile is a profile misspelled, so
// either adds an empty name, which the tool handlers refuse as bad_profile
// rather than serve the client unscoped.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if names, ok := queryProfiles(r.URL.RawQuery); len(names) > 0 || !ok {
		r = r.Clone(r.Context())

		for _, v := range names {
			r.Header.Add(config.ProfileHeader, v)
		}

		if !ok {
			r.Header.Add(config.ProfileHeader, "")
		}
	}

	// For endWithRequest: the SDK hides r's cancellation from the request
	// handlers, but not its values.
	r = r.WithContext(context.WithValue(r.Context(), requestContextKey{}, r.Context()))
	g.handler.ServeHTTP(w, r)
}

// requestContextKey is the context key ServeHTTP keeps the HTTP request's
// context under, for endWithRequest.
type requestContextKey struct{}

// endWithRequest is receiving middleware that ends each request handler's
// context with the HTTP request that carried it, at every protocol version:
// when the client gives up, and at the daemon's hard stop, which cancels
// every request's context (design §7.6, §7.7). The stateless handler runs
// each request on a session connected with the request's context, but
// hides that context's cancellation from the handlers, so they would run
// on, and hold the hard stop, after no one can receive their result. The
// SDK's StreamableHTTPOptions.PropagateRequestCancellation stops hiding it
// only from 2026-07-28, so the gateway ends them itself, at every version
// alike. A handler whose context ServeHTTP didn't mark is left as it is.
func endWithRequest(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		parent, ok := ctx.Value(requestContextKey{}).(context.Context)

		if !ok {
			return next(ctx, method, req)
		}

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		stop := context.AfterFunc(parent, cancel)
		defer stop()

		return next(ctx, method, req)
	}
}

// queryProfiles is the profile values of raw, a URL query. ok is false
// when raw doesn't parse, or names a key that is profile in another case.
func queryProfiles(raw string) (names []string, ok bool) {
	if raw == "" {
		return nil, true
	}

	q, err := url.ParseQuery(raw)

	if err != nil {
		return nil, false
	}

	for k := range q {
		if k != "profile" && strings.EqualFold(k, "profile") {
			return nil, false
		}
	}

	return q["profile"], true
}
