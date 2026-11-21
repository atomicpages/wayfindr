// Package toolsonly is the receiving middleware of an mcp.Server that
// declares only the tools capability. Wayfindr runs two such servers, the
// gateway on /mcp and the stdio shim, and both answer the capabilities they
// don't declare the same way through it (design §7.6, §7.9). It imports only
// the MCP SDK, so the shim can share it without importing the gateway.
package toolsonly

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Middleware answers every request of a capability the server doesn't
// declare (prompts, resources, completions) with JSON-RPC -32601, as a
// server that never implemented them would. Without it the SDK serves empty
// prompt and resource lists, which contradicts the declared capabilities.
// logging/setLevel is left to the SDK: before 2026-07-28 it accepts the
// level as a no-op, and from 2026-07-28 it is a removed method.
func Middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if undeclared(method) {
			return nil, &jsonrpc.Error{
				Code:    jsonrpc.CodeMethodNotFound,
				Message: fmt.Sprintf("method not found: %q (wayfindr declares only tools)", method),
			}
		}

		return next(ctx, method, req)
	}
}

// undeclared reports whether method belongs to a capability the server
// doesn't declare.
func undeclared(method string) bool {
	for _, prefix := range []string{"prompts/", "resources/", "completion/"} {
		if strings.HasPrefix(method, prefix) {
			return true
		}
	}

	return false
}
