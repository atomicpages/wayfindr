package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
)

// Call calls tool on the connector called name with args, a JSON object (nil means {}), and
// returns the upstream's result unchanged, isError results included.
//
// The connector's timeout (default 120s) bounds the whole call, connecting
// included, but not the time the user takes to answer. Calls run
// concurrently over one session. Every failure is an *Error.
//
// When the upstream asks the user something, the result carries
// InputRequests (elicitations only) and a RequestState for [Continue]: the
// upstream's own input_required at 2026-07-28, or an elicitation/create it
// sent during the call at 2025-x, which is parked as an interaction until
// Continue answers it, or for [InteractionTTL]. A request for sampling
// becomes a KindInteractiveInput error, as does an elicitation that can't
// be tied to one call.
func (m *Manager) Call(ctx context.Context, name, tool string, args json.RawMessage) (*mcp.CallToolResult, error) {
	return m.call(ctx, name, &mcp.CallToolParams{Name: tool, Arguments: arguments(args)})
}

// arguments is the arguments of a tools/call: args, or {} when it is nil.
func arguments(args json.RawMessage) any {
	if len(args) == 0 {
		return map[string]any{}
	}

	return args
}

// call starts a run of params on the connector called name and waits for
// its result or its first question.
func (m *Manager) call(ctx context.Context, name string, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	c := m.lookup(name)

	if c == nil {
		return nil, &Error{Connector: name, Kind: KindUnknownConnector}
	}

	c.mu.Lock()
	timeout := callTimeout(c.cfg)
	c.mu.Unlock()

	// The run may outlive ctx, parked; while ctx is waited on, its end
	// cancels the run.
	runCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	r := &run{
		c:         c,
		tool:      params.Name,
		asks:      make(chan *ask),
		returned:  make(chan struct{}),
		done:      make(chan struct{}),
		abandoned: make(chan struct{}),
		cancel:    cancel,
	}
	// The cause tells the connector's timeout from the caller's deadline.
	r.clock = newClock(timeout, func() { cancel(errUseTimeout) })

	stop := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	l, err := m.acquire(runCtx, c)
	stop()

	if err != nil {
		// A failed connect has already applied its verdict.
		r.clock.stop()
		cancel(nil)
		m.notify(c)

		return nil, err
	}

	go m.runCall(runCtx, r, l, params)

	return r.wait(ctx, m)
}

// runCall makes r's tools/call over l's session, judges the outcome, and
// applies the verdict, then releases the lease and ends r.
func (m *Manager) runCall(ctx context.Context, r *run, l lease, params *mcp.CallToolParams) {
	c := r.c
	c.addRun(r)
	res, err := l.session.CallTool(ctx, params)
	close(r.returned)
	c.removeRun(r)
	r.clock.stop()
	// A question the upstream sends now has no call to go to; this releases
	// a handler that got r from soleRun just before the call returned.
	r.abandon()

	needsInput := err == nil && res.NeedsInput()
	declined := needsInput && !onlyElicitations(res) || asksForInput(err) || declinedInResult(res)

	if needsInput && !declined {
		res.RequestState = stateUpstream + res.RequestState
	}

	v := judge(contextOutcome(ctx, outcome{
		connector: c.name, phase: phaseUse, err: err, declined: declined, ambiguous: r.ambiguous.Load(), mask: l.mask,
		overflow: l.overflow.seen(),
	}))
	// The close a verdict calls for still tries its DELETE when the run's
	// ctx has ended, often why the call failed.
	err = m.apply(context.WithoutCancel(ctx), c, l, v)
	r.cancel(nil)

	if err != nil {
		res = nil
	}

	r.res, r.err = res, err
	// The lease is released and the callbacks delivered before the result
	// is, as for every use.
	l.release()
	m.notify(c)
	close(r.done)
}

// onlyElicitations reports whether every input request res makes is an
// elicitation, the only kind Wayfindr passes on.
func onlyElicitations(res *mcp.CallToolResult) bool {
	for _, req := range res.InputRequests {
		if _, ok := req.(*mcp.ElicitParams); !ok {
			return false
		}
	}

	return true
}

// codeInputDeclined is the JSON-RPC error code Wayfindr answers an
// upstream's sampling request with, and an elicitation it can't tie to
// one call (see Manager.elicit). It sits in the range
// MCP leaves to implementations (-32000 to -32019), clear of the codes the
// SDK uses itself. An upstream that fails a call because the request was
// declined usually passes the code on (the Go SDK keeps a wrapped error's
// code), so the decline is told apart per call, from that call's own error.
const codeInputDeclined = -32010

// declineMessage is the message of Wayfindr's decline of sampling, which
// a tool that turns the decline into its result quotes.
const declineMessage = "this tool asks the client to sample a message, which Wayfindr does not support"

// asksForInput reports whether err is an upstream's refusal to run a tool
// because the client can't answer: either it carries codeInputDeclined, or
// it is the upstream's own refusal. Go SDK servers check the client's
// capabilities before sampling or eliciting and fail the call with
// "client does not support sampling", a plain error with no code, so the
// request never reaches declineInput.
func asksForInput(err error) bool {
	var werr *jsonrpc.Error

	if !errors.As(err, &werr) {
		return false
	}

	if werr.Code == codeInputDeclined {
		return true
	}

	msg := strings.ToLower(werr.Message)

	return strings.Contains(msg, "does not support elicitation") ||
		strings.Contains(msg, "does not support sampling")
}

// declinedInResult reports whether res is a tool's isError result that
// quotes one of Wayfindr's declines. A tool that asks for input itself, and
// turns the error into its result (as the Go SDK's typed handlers do),
// drops the code but keeps the message; the message is Wayfindr's own, so
// it can't be another call's failure.
func declinedInResult(res *mcp.CallToolResult) bool {
	if res == nil || !res.IsError {
		return false
	}

	return slices.ContainsFunc(res.Content, func(c mcp.Content) bool {
		t, ok := c.(*mcp.TextContent)

		return ok && (strings.Contains(t.Text, declineMessage) || strings.Contains(t.Text, ambiguousMessage))
	})
}

// ListTools returns every tool the connector called name serves, following pagination
// cursors to the last page. The connector's timeout bounds the whole list.
// Every failure is an *Error. A successful list records its cache hint in
// the connector's status (ConnectorStatus.ToolsTTL), so it is the lister
// for catalog syncs, whose schedule the hint sets.
func (m *Manager) ListTools(ctx context.Context, name string) ([]*mcp.Tool, error) {
	tools, ttl, err := m.listTools(ctx, name)

	if err != nil {
		return nil, err
	}

	if c := m.lookup(name); c != nil {
		c.stateMu.Lock()
		c.toolsTTL = ttl
		c.stateMu.Unlock()
	}

	return tools, nil
}

// PeekTools is ListTools without recording the cache hint: a look at the
// connector's tools that is not a sync, such as `wayfindr connector tools`.
// ToolsTTL stays tied to the last sync's list, which the catalog holds.
func (m *Manager) PeekTools(ctx context.Context, name string) ([]*mcp.Tool, error) {
	tools, _, err := m.listTools(ctx, name)

	return tools, err
}

// listTools lists the connector's tools, page by page, and returns them
// with the list's cache hint: the smallest page's, or 0 if any page has
// none.
//
// It follows at most maxSyncPages pages and keeps at most maxSyncTools
// tools, whose definitions add up to at most maxSyncBytes (or
// Options.MaxSyncBytes), and fails
// response_too_large past any of them, or on a cursor the list already
// named: an upstream must not keep a sync going, or growing, forever. The
// caller keeps its last good list.
//
// It pages with ClientSession.ListTools rather than the Tools iterator,
// which yields only the tools and drops each page's ttlMs.
func (m *Manager) listTools(ctx context.Context, name string) ([]*mcp.Tool, time.Duration, error) {
	var (
		tools []*mcp.Tool
		ttl   time.Duration
	)
	err := m.use(ctx, name, func(ctx context.Context, s *mcp.ClientSession) (bool, error) {
		params := &mcp.ListToolsParams{}
		cursors := map[string]bool{}
		size := 0

		for n := 1; ; n++ {
			res, err := s.ListTools(ctx, params)

			if err != nil {
				return false, err
			}

			tools = append(tools, res.Tools...)

			if len(tools) > maxSyncTools {
				return false, &syncOverflowError{kase: CaseSyncTools, detail: fmt.Sprintf("more than %d tools", maxSyncTools)}
			}

			// Re-encoding is the honest measure of what the list keeps:
			// the bytes on the wire are not all seen here (an SSE answer
			// isn't counted), and include the page's envelope.
			for _, tool := range res.Tools {
				b, err := json.Marshal(tool)

				if err != nil {
					return false, fmt.Errorf("measure tool list: %w", err)
				}

				size += len(b)
			}

			if size > m.maxSyncBytes {
				return false, &syncOverflowError{kase: CaseSyncBytes, detail: fmt.Sprintf("tool definitions over %d bytes", m.maxSyncBytes)}
			}

			// The list is fresh only as long as its stalest page; a page
			// without a hint (0) leaves the whole list without one.
			page := time.Duration(max(res.TTLMs, 0)) * time.Millisecond

			if n == 1 || page < ttl {
				ttl = page
			}

			switch {
			case res.NextCursor == "":
				return false, nil
			case n == maxSyncPages:
				return false, &syncOverflowError{kase: CaseSyncPages, detail: fmt.Sprintf("more than %d pages", maxSyncPages)}
			case cursors[res.NextCursor]:
				return false, &syncOverflowError{kase: CaseSyncPages, detail: fmt.Sprintf("page %d repeats a cursor", n)}
			}

			cursors[res.NextCursor] = true
			params.Cursor = res.NextCursor
		}
	})

	if err != nil {
		return nil, 0, err
	}

	return tools, ttl, nil
}

// use runs f over the session of the connector called name under the
// connector's timeout, judges the outcome, and applies the verdict. f
// reports whether its call asked for input that Wayfindr declined.
func (m *Manager) use(ctx context.Context, name string, f func(context.Context, *mcp.ClientSession) (declined bool, err error)) error {
	c := m.lookup(name)

	if c == nil {
		return &Error{Connector: name, Kind: KindUnknownConnector}
	}

	// Deferred first, so it runs last: once the use holds no lock.
	defer m.notify(c)
	c.mu.Lock()
	timeout := callTimeout(c.cfg)
	c.mu.Unlock()
	// The cause tells the connector's timeout from the caller's deadline.
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, errUseTimeout)
	defer cancel()

	l, err := m.acquire(ctx, c)

	if err != nil {
		// A failed connect has already applied its verdict.
		return err
	}

	defer l.release()

	declined, err := f(ctx, l.session)
	v := judge(contextOutcome(ctx, outcome{
		connector: name, phase: phaseUse, err: err, declined: declined, mask: l.mask, overflow: l.overflow.seen(),
	}))

	// The close a verdict calls for still tries its DELETE when the use's
	// ctx has ended, often why the use failed.
	return m.apply(context.WithoutCancel(ctx), c, l, v)
}

// callTimeout is the connector's timeout, or the default for a connector
// built without config defaults.
func callTimeout(cfg config.Connector) time.Duration {
	if cfg.Timeout.Duration > 0 {
		return cfg.Timeout.Duration
	}

	return config.DefaultConnectorTimeout
}

// apply applies a use's verdict to c, over whose leased session the use
// ran, and returns the error the use returns. A connector removed during
// the use is left alone, and the use reads as unknown_connector. Whether a
// success clears needs_auth or auth_error depends on when the session was
// opened, which judge doesn't see; record decides it from the lease. ctx's
// end cuts a close the verdict calls for short (see closeSession).
func (m *Manager) apply(ctx context.Context, c *connector, l lease, v verdict) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.removed {
		return &Error{Connector: c.name, Kind: KindUnknownConnector}
	}

	if v.closeSession && c.session == l.session {
		if cerr := c.closeSession(ctx); cerr != nil {
			m.log.Debug().Str("connector", c.name).Err(redactErr(cerr)).Msg("close failed upstream session")
		}
	}

	if v.event != 0 {
		m.record(c, v.event, v.reason, v.err, l.epoch)
	}

	return v.err
}
