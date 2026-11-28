package gateway

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CallCore is callCore, the call path every door shares, so a test can
// drive it with options no MCP request carries, as a second door will.
// A deliberate exception to testing at the package's seams, which the
// call-core plan asked for, until `wayfindr call` is the second door.
func (g *Gateway) CallCore(ctx context.Context, r CallRequest, opts CallOptions) *mcp.CallToolResult {
	return g.callCore(ctx, r, opts)
}
