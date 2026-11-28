package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/gateway"
)

// caller is the part of the gateway the admin API's call route uses.
// *gateway.Gateway satisfies it.
type caller interface {
	Call(ctx context.Context, r gateway.CallRequest, opts gateway.CallOptions) gateway.CallResponse
	Class(ctx context.Context, connector, tool string) (config.ToolClass, bool)
}

// tokenLookup resolves a scoped client token's digest.
// *clienttoken.Registry satisfies it.
type tokenLookup interface {
	Lookup(d [sha256.Size]byte) (clienttoken.Client, bool)
}

// adminCall adapts the gateway's Call to admin.Options.Call: one call_tool
// call through the call path /mcp shares (design §7.6), as the request's
// client and token. defaultSum is the digest of the default client token.
//
// The token is resolved as httpd's bearer guard resolves one: the default
// token's digest, compared in constant time, gives the default Client;
// otherwise the registry's. No token is the owner, the default Client. A
// token that resolves to neither is an *admin.CallError. The token is
// neither stored nor logged.
//
// The call is narrowed by the Client's connectors alone: the owner's call
// has no profile and no scope file. Its Asker claims the newest protocol
// version and both kinds of elicitation, since the CLI and the console
// answer either.
func adminCall(gw caller, defaultSum [sha256.Size]byte, tokens tokenLookup) func(context.Context, admin.CallRequest) (admin.CallOutcome, error) {
	return func(ctx context.Context, req admin.CallRequest) (admin.CallOutcome, error) {
		client, ok := resolveToken(req.Token, defaultSum, tokens)

		if !ok {
			return admin.CallOutcome{}, &admin.CallError{Case: admin.CallUnknownToken}
		}

		r := gateway.CallRequest{Name: req.Name, Args: req.Arguments}

		if req.RequestState != "" || req.InputResponses != nil {
			r.Resume = &gateway.Resume{State: req.RequestState, Responses: make(mcp.InputResponseMap, len(req.InputResponses))}

			for id, a := range req.InputResponses {
				r.Resume.Responses[id] = &mcp.ElicitResult{Action: a.Action, Content: a.Content}
			}
		}

		reply := gw.Call(ctx, r, gateway.CallOptions{
			Client:     client,
			ClientName: req.Client,
			Narrow:     func() (config.Narrowing, error) { return config.Narrowing{Token: client.Connectors}, nil },
			Asker: gateway.Asker{
				ProtocolVersion: gateway.NewestProtocolVersion,
				Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{
					Form: &mcp.FormElicitationCapabilities{},
					URL:  &mcp.URLElicitationCapabilities{},
				}},
			},
		})

		wire, err := wireResult(reply.Result)

		if err != nil {
			return admin.CallOutcome{}, err
		}

		out := admin.CallOutcome{Result: wire, Approval: reply.Approval}

		if connector, tool, ok := strings.Cut(req.Name, "."); ok && connector != "" && tool != "" {
			if class, ok := gw.Class(ctx, connector, tool); ok {
				out.Class = string(class)
			}
		}

		return out, nil
	}
}

// resolveToken is the Client token names: the default one for "" or the
// default token's value, else the registry's.
func resolveToken(token string, defaultSum [sha256.Size]byte, tokens tokenLookup) (clienttoken.Client, bool) {
	if token == "" {
		return clienttoken.Default(), true
	}

	sum := clienttoken.Digest(token)

	if subtle.ConstantTimeCompare(sum[:], defaultSum[:]) == 1 {
		return clienttoken.Default(), true
	}

	// The registry holds only scoped tokens, so a hit never passes for
	// the default token.
	return tokens.Lookup(sum)
}

// wireResult is res in MCP's wire form, as an MCP client receives it. The
// SDK writes resultType only for a result its server sends, so an
// input_required result gets it here.
func wireResult(res *mcp.CallToolResult) (json.RawMessage, error) {
	b, err := json.Marshal(res)

	if err != nil {
		return nil, fmt.Errorf("marshal call result: %w", err)
	}

	if len(res.InputRequests) == 0 {
		return b, nil
	}

	var fields map[string]json.RawMessage

	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, fmt.Errorf("marshal call result: %w", err)
	}

	fields["resultType"], _ = json.Marshal(admin.ResultInputRequired) // a string always marshals

	b, err = json.Marshal(fields)

	if err != nil {
		return nil, fmt.Errorf("marshal call result: %w", err)
	}

	return b, nil
}
