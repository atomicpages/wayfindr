package gateway

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
)

// NewestProtocolVersion is the newest MCP version the gateway speaks. A
// door that answers input_required itself, such as the admin API's call
// route, claims it in its Asker, since its callers take the round trip.
const NewestProtocolVersion = protocolInputRequired

// CallResponse is what [Gateway.Call] returns: the result, and what a door
// that isn't /mcp may know beside it.
type CallResponse struct {
	// Result is the call's result, as /mcp sends it.
	Result *mcp.CallToolResult
	// Approval is the id of the pending approval the call was refused for
	// (ADR 0024, Kind approval, Case pending), so the owner's CLI and
	// console can offer to approve it. It is empty for every other outcome,
	// a denied or expired approval included. It travels Go-side only: /mcp
	// carries it in the refusal's text, which nothing parses.
	Approval string
}

// Call runs one call_tool call through the call path every door shares
// (design §7.6): the same narrowing, validation, input_required round trip,
// and call log as /mcp, with the options the door built. It is the entry
// point of the admin API's call route, which the daemon adapts; /mcp's
// handler calls the same path.
func (g *Gateway) Call(ctx context.Context, r CallRequest, opts CallOptions) CallResponse {
	return g.callCore(ctx, r, opts)
}

// Class is the class of connector's tool as the catalog holds it, by the
// rule call_tool narrows by: a definition whose annotations don't decode is
// destructive. False when the catalog has no such tool or can't be read.
// The admin API's call route reports it beside the result; only the owner
// reaches that route, and the owner sees every tool's class anyway.
func (g *Gateway) Class(ctx context.Context, connector, tool string) (config.ToolClass, bool) {
	t, err := g.catalog.GetTool(ctx, g.userID, connector, tool)

	if err != nil {
		return "", false
	}

	return toolClass(t.Definition), true
}
