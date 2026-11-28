package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// callArgs are call_tool's arguments.
type callArgs struct {
	Name      any             `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// CallRequest is one call_tool call, whichever door it came through.
type CallRequest struct {
	// Name is the qualified name, as the caller wrote it.
	Name string
	// Args are the tool's arguments: an object, or nil (or JSON null) for
	// none.
	Args json.RawMessage
	// Resume is set when the caller answers an earlier input_required.
	Resume *Resume
}

// Resume is a caller's answer to an input_required result: the sealed
// request state it carried, and the caller's responses to its requests.
type Resume struct {
	State     string
	Responses mcp.InputResponseMap
}

// CallOptions is who is calling and how they can be asked. It is built
// per call, by value, and never mutated.
type CallOptions struct {
	// Client is the client token the call came with: it narrows the call
	// (through Narrow), the call log names it, and a request state is
	// sealed for its Instance and opened only by it.
	Client clienttoken.Client
	// ClientName is the caller's clientInfo name, for the call log, which
	// clips it.
	ClientName string
	// Narrow builds the call's narrowing, and is required: callCore refuses
	// a call without it, as it would one with no Client (CaseNoClient).
	// callCore calls it after the name is parsed, so a call with a bad name
	// and a bad profile header is refused for its name.
	Narrow func() (config.Narrowing, error)
	// Asker is what ask needs to know about the caller.
	Asker Asker
}

// Asker is what ask needs to know about the caller to put an upstream's
// question to it.
type Asker struct {
	// ProtocolVersion is the MCP version the caller speaks.
	ProtocolVersion string
	// Capabilities are the caller's; nil when it declared none.
	Capabilities *mcp.ClientCapabilities
}

// callTool is the call_tool handler: it maps the request onto callCore and
// returns callCore's result. The Client is read once, for both the
// narrowing and the call log. A request with no Client is logged with no
// token name; Narrow refuses it.
func (g *Gateway) callTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a callArgs

	if len(req.Params.Arguments) > 0 {
		// A malformed object leaves name unset, which callCore reports.
		_ = json.Unmarshal(req.Params.Arguments, &a)
	}

	name, _ := a.Name.(string)
	r := CallRequest{Name: name, Args: a.Arguments}

	if p := req.Params; p.RequestState != "" || p.InputResponses != nil {
		r.Resume = &Resume{State: p.RequestState, Responses: p.InputResponses}
	}

	client, authed := clientOf(req)

	return g.callCore(ctx, r, CallOptions{
		Client:     client,
		ClientName: clientName(req),
		Narrow:     func() (config.Narrowing, error) { return g.narrowingOf(req, client, authed) },
		Asker:      Asker{ProtocolVersion: req.ProtocolVersion(), Capabilities: req.ClientCapabilities()},
	}), nil
}

// callCore is the call path every door shares (design §7.6): it runs the
// call and records it in the call log, once, whichever way it ended.
func (g *Gateway) callCore(ctx context.Context, r CallRequest, opts CallOptions) *mcp.CallToolResult {
	started := time.Now()
	res, o := g.call(ctx, r, opts)

	if g.calls != nil {
		g.calls.Record(store.Call{
			UserID:    g.userID,
			Client:    clip(opts.ClientName),
			TokenName: opts.Client.Name,
			Connector: clip(o.connector),
			Tool:      clip(o.tool),
			StartedAt: started,
			Duration:  o.duration,
			Result:    o.result,
			ErrorKind: o.kind,
			ErrorCase: o.errCase,
			ErrorText: o.text,

			ResultBytes: o.resultBytes,
		})
	}

	return res
}

// outcome is how a call ended, for the call log.
type outcome struct {
	// connector and tool are the qualified name's parts as the agent wrote
	// them. A name without a dot is all tool.
	connector, tool string
	result          store.CallResult
	kind, errCase   string
	// text is the error text the agent got back, errorText-cleaned; empty
	// for ok and input_required.
	text string
	// duration is how long upstreams.Call took, connecting included; nil
	// when the call never got that far.
	duration *time.Duration
	// resultBytes is the length of the upstream's result's JSON encoding
	// (see resultBytes); nil when no result came back, and for an
	// input_required result, which is not the tool's own.
	resultBytes *int64
}

// call validates the call and forwards it (design §7.6): split the
// qualified name, check that the client token allows the connector and that
// it is configured, look the tool up in the catalog, check the connector's
// tool policy and the request's profile and project scope, validate the
// arguments against the tool's inputSchema, then call the upstream and
// return its result unchanged, isError results included.
func (g *Gateway) call(ctx context.Context, r CallRequest, opts CallOptions) (*mcp.CallToolResult, outcome) {
	var (
		o outcome
		// continued is set once an answer is handed to the upstream. An
		// answer refused before that discards the question it answers,
		// which would otherwise hold its upstream call until it expired,
		// when it comes with the client token the question was put to.
		continued bool
	)

	if p := r.Resume; p != nil {
		defer func() {
			if !continued {
				g.discardState(p.State, opts.Client.Instance())
			}
		}()
	}

	name := r.Name

	if strings.TrimSpace(name) == "" {
		return g.fail(o, &Error{Kind: KindInvalidName, Case: CaseMissingName})
	}

	// Connector names may not contain dots, so the first one splits the
	// qualified name unambiguously.
	connector, tool, ok := strings.Cut(name, ".")

	if !ok {
		o.tool = name
	} else {
		o.connector, o.tool = connector, tool
	}

	if !ok || connector == "" || tool == "" {
		return g.fail(o, &Error{Kind: KindInvalidName, Case: CaseUnqualified, Tool: name})
	}

	if opts.Narrow == nil {
		return g.fail(o, errNoClient())
	}

	scope, err := opts.Narrow()

	if err != nil {
		return g.fail(o, err)
	}

	// The client token first: to a client whose token leaves the connector
	// out, it is not configured, so it is refused exactly as an
	// unconfigured one is, before any hidden_by_* Case could tell it exists.
	if !g.configuredFor(scope, connector) {
		return g.fail(o, noConnector(name, connector))
	}

	// Refused by name before the catalog is read, so an out-of-scope call
	// learns nothing about the tool. The same order as hiddenBy: the
	// policy, then the profile, then the scope file.
	if c := g.hiddenByName(scope, connector, tool); c != "" {
		return g.fail(o, hiddenError(scope, c, name, connector))
	}

	t, err := g.catalog.GetTool(ctx, g.userID, connector, tool)

	if errors.Is(err, store.ErrNotFound) {
		return g.fail(o, g.unknownTool(ctx, scope, name, connector, tool))
	}

	if err != nil {
		return g.fail(o, catalogError(ctx, err, name, connector))
	}

	// The catalog may still hold a tool a reload just hid, until the
	// connector's re-sync drops it, and the profile or scope may hide the
	// tool by its class. Checked before Call and Continue alike.
	if c := g.hiddenBy(scope, t); c != "" {
		return g.fail(o, hiddenError(scope, c, name, connector))
	}

	args := r.Args

	if isNull(args) {
		args = nil
	}

	if err := g.validate(t, name, args); err != nil {
		return g.fail(o, err)
	}

	var res *mcp.CallToolResult

	upstreamStarted := time.Now()

	if p := r.Resume; p != nil {
		// The client answers what an earlier call asked.
		st, ok := g.openState(p.State, opts.Client.Instance(), connector, tool, args)

		if !ok {
			return g.fail(o, &Error{Kind: KindInvalidRequestState, Tool: name, Connector: connector})
		}

		continued = true
		res, err = g.upstreams.Continue(ctx, connector, tool, args,
			upstream.Input{State: st.Upstream, Responses: unprefix(connector, p.Responses)})
	} else {
		res, err = g.upstreams.Call(ctx, connector, tool, args)
	}

	took := time.Since(upstreamStarted)
	o.duration = &took

	if err != nil {
		return g.fail(o, err)
	}

	if res.InputRequests != nil {
		asked, err := g.ask(opts.Asker, opts.Client.Instance(), connector, tool, args, res)

		if err != nil {
			return g.fail(o, err)
		}

		o.result = store.CallInputRequired

		return asked, o
	}

	o.resultBytes = resultBytes(res)
	o.result = store.CallOK

	if res.IsError {
		o.result = store.CallToolError
		o.text = errorText(res)
	}

	return res, o
}

// resultBytes is the size of res as the call log keeps it: the length of
// its JSON encoding, which covers its content and structuredContent. It is
// not the bytes on the wire: the SDK adds resultType, normalizes a nil
// content, and wraps the result in the JSON-RPC envelope when it sends it.
// Encoding the result again here costs one marshal per call: acceptable, and
// cheaper than hooking the transport to count the bytes it writes. Nil when
// res does not encode, which the SDK would then fail to send as well.
func resultBytes(res *mcp.CallToolResult) *int64 {
	b, err := json.Marshal(res)

	if err != nil {
		return nil
	}

	n := int64(len(b))

	return &n
}

// fail is the error result of a call that ended with err, and its outcome.
func (g *Gateway) fail(o outcome, err error) (*mcp.CallToolResult, outcome) {
	o.result, o.kind, o.errCase = classify(err)
	res := g.errorResult(err)
	o.text = errorText(res)

	return res, o
}

// classify maps the error a call ended with to its call-log result, Kind,
// and Case: the gateway's refusals are rejected, everything else failed.
func classify(err error) (result store.CallResult, kind, errCase string) {
	if e, ok := AsError(err); ok {
		switch e.Kind {
		case KindInvalidName, KindUnknownTool, KindInvalidArguments, KindInvalidSearch, KindInvalidRequestState, KindInvalidScope, KindUnknownProfile:
			return store.CallRejected, string(e.Kind), string(e.Case)
		case KindInternal, KindCannotAsk, KindCanceled:
			return store.CallFailed, string(e.Kind), string(e.Case)
		}
	}

	if e, ok := upstream.AsError(err); ok {
		return store.CallFailed, string(e.Kind), string(e.Case)
	}

	return store.CallFailed, string(KindInternal), ""
}

// maxNameBytes bounds a client, connector, or tool name in the call log.
// The names come from the agent and its client, so they are clipped
// rather than stored at any length.
const maxNameBytes = 256

// maxErrorTextBytes bounds a call's error text in the call log (ADR 0014).
const maxErrorTextBytes = 500

// clip makes s a storable call-log name: printable (redact.Printable), at
// most maxNameBytes long, cut on a rune boundary. Postgres text refuses
// NUL, and `wayfindr calls` prints the names to a terminal, where an escape
// sequence would be obeyed, a bidi control would reorder the row, and a tab
// would break the table.
func clip(s string) string {
	return cut(redact.Printable(s), maxNameBytes)
}

// errorText is the text of an isError result as the call log keeps it: its
// text contents on one line (whitespace runs become one space), printable,
// URL query strings masked (they may carry tokens), and at most
// maxErrorTextBytes long. Other content, such as images, is left out.
func errorText(res *mcp.CallToolResult) string {
	var parts []string

	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}

	s := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")

	return cut(redact.URLQueries(redact.Printable(s)), maxErrorTextBytes)
}

// cut is s cut to at most n bytes on a rune boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}

// clientOf is the Client httpd resolved for req, from req.Extra.TokenInfo:
// the client token it was authenticated with. False when req carries none,
// which narrowingOf refuses.
func clientOf(req *mcp.CallToolRequest) (clienttoken.Client, bool) {
	if req.Extra == nil {
		return clienttoken.Client{}, false
	}

	return clienttoken.FromTokenInfo(req.Extra.TokenInfo)
}

// clientName is the calling client's clientInfo name, unclipped; empty
// when the request carries none, as a 2025 request to a stateless server
// doesn't.
func clientName(req *mcp.CallToolRequest) string {
	if info := req.ClientInfo(); info != nil {
		return info.Name
	}

	return ""
}

// unknownTool is the error for a name the catalog doesn't have, telling an
// unconfigured connector and a tool the connector's policy hides apart from
// a missing tool. A connector n's token leaves out counts as unconfigured.
// A tool is hidden if the policy hides its name whatever its class, or the
// last sync hid it and the live policy, applied to the class that sync
// recorded, still does. A failure to read the sync record only costs the
// finer Case.
func (g *Gateway) unknownTool(ctx context.Context, n config.Narrowing, name, connector, tool string) error {
	// Checked again: a reload may have removed the connector since call
	// checked it.
	if !g.configuredFor(n, connector) {
		return noConnector(name, connector)
	}

	policy := g.toolPolicy(connector)
	hidden := policy.HidesName(tool)

	if !hidden {
		rec, err := g.catalog.LastSync(ctx, g.userID, connector)

		if err != nil && !errors.Is(err, store.ErrNotFound) {
			g.log.Debug().Err(err).Str("connector", connector).Msg("read last sync for hidden tools")
		}

		hidden = err == nil && slices.ContainsFunc(rec.Hidden, func(h store.HiddenTool) bool {
			return h.Name == tool && !policy.Exposes(h.Name, config.ToolClass(h.Class))
		})
	}

	if hidden {
		return &Error{Kind: KindUnknownTool, Case: CaseHiddenByPolicy, Tool: name, Connector: connector}
	}

	return &Error{Kind: KindUnknownTool, Case: CaseNotInCatalog, Tool: name, Connector: connector}
}

// configuredFor reports whether connector is configured as n's client sees
// it: configured, and allowed by its client token. The profile and the
// scope file don't count here, so they refuse with their own hidden_by_*
// Case, which a client may see, since it chose them.
func (g *Gateway) configuredFor(n config.Narrowing, connector string) bool {
	return n.TokenAllows(connector) &&
		slices.ContainsFunc(g.upstreams.Status(), func(s upstream.ConnectorStatus) bool { return s.Connector == connector })
}

// noConnector is call_tool's refusal of name, whose connector is not
// configured, or not for this client.
func noConnector(name, connector string) error {
	return &Error{Kind: KindUnknownTool, Case: CaseNoConnector, Tool: name, Connector: connector}
}

// hiddenBy is why catalog entry t is hidden from a request narrowed by s.
// The first to hide it decides: CaseHiddenByPolicy for the live policy,
// then CaseHiddenByProfile for the request's profile (ADR 0011), then
// CaseHiddenByScope for its project scope (ADR 0010); "" when all expose
// it.
func (g *Gateway) hiddenBy(n config.Narrowing, t store.Tool) Case {
	class := toolClass(t.Definition)

	if !g.toolPolicy(t.Connector).Exposes(t.Name, class) {
		return CaseHiddenByPolicy
	}

	return sourceCase(n.Hides(t.Connector, t.Name, class))
}

// hiddenByName is why connector's tool called name is hidden whatever its
// class, in hiddenBy's order, or "" when nothing hides it by name alone.
func (g *Gateway) hiddenByName(n config.Narrowing, connector, name string) Case {
	if g.toolPolicy(connector).HidesName(name) {
		return CaseHiddenByPolicy
	}

	return sourceCase(n.HidesName(connector, name))
}

// toolPolicy returns connector's tool policy; nil without Options.Policy.
func (g *Gateway) toolPolicy(connector string) config.ToolPolicy {
	if g.policy == nil {
		return nil
	}

	return g.policy(connector)
}

// toolClass is the class of a catalog entry's definition. A definition
// whose annotations don't decode counts as destructive, as one without
// annotations does.
func toolClass(definition json.RawMessage) config.ToolClass {
	var d struct {
		Annotations *mcp.ToolAnnotations `json:"annotations"`
	}

	if json.Unmarshal(definition, &d) != nil {
		return config.ClassDestructive
	}

	return config.ClassOf(d.Annotations)
}

// validate checks args, a JSON object or nil (meaning {}), against t's
// inputSchema. A schema that does not compile is logged once and skipped:
// the upstream then judges the arguments itself.
func (g *Gateway) validate(t store.Tool, name string, args json.RawMessage) error {
	if args != nil && !isObject(args) {
		return &Error{Kind: KindInvalidArguments, Case: CaseNotObject, Tool: name, Connector: t.Connector}
	}

	e, fresh := g.schemas.get(t.Definition)

	if e.err != nil {
		if fresh {
			g.log.Warn().Err(e.err).Str("connector", t.Connector).Str("tool", t.Name).
				Msg("input schema does not compile; forwarding calls without validation")
		}

		return nil
	}

	if e.schema == nil {
		return nil
	}

	if args == nil {
		args = json.RawMessage("{}")
	}

	detail, err := e.schema.check(args)

	if err != nil {
		return &Error{Kind: KindInvalidArguments, Case: CaseNotObject, Tool: name, Connector: t.Connector, Err: err}
	}

	if detail != "" {
		return &Error{Kind: KindInvalidArguments, Case: CaseSchemaMismatch, Tool: name, Connector: t.Connector, Detail: detail}
	}

	return nil
}

func isNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)

	return len(t) == 0 || slices.Equal(t, []byte("null"))
}

func isObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)

	return len(t) > 0 && t[0] == '{'
}
