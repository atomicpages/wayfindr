package gateway

import (
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// sourceCase is the hidden_by_* Case for what hides a tool, or "" for
// config.SourceNone.
func sourceCase(src config.Source) Case {
	switch src {
	case config.SourceProfile:
		return CaseHiddenByProfile
	case config.SourceFile:
		return CaseHiddenByScope
	case config.SourceNone:
	}

	return ""
}

// hiddenError is the call_tool refusal of tool, on connector, for reason c,
// a hidden_by_* Case. Only a profile's refusal carries a name, the
// profile's, so its advice can say where to allow the tool.
func hiddenError(n config.Narrowing, c Case, tool, connector string) error {
	e := &Error{Kind: KindUnknownTool, Case: c, Tool: tool, Connector: connector}

	if c == CaseHiddenByProfile {
		e.Detail = n.ProfileName
	}

	return e
}

// narrowingOf is what narrows req: the connectors its client token allows,
// from client, the Client clientOf read from req; the project scope in
// config.ScopeHeader, as scopeOf reads it; and the profile
// config.ProfileHeader names, looked up in the running config. The profile
// is resolved per request, so an edit to it applies at the next reload
// without a reconnect.
//
// It fails closed. A request with no Client (authed false) is a
// KindInternal *Error with CaseNoClient: httpd puts one on every request
// it lets through, and
// serving one without it would ignore the token's limit. A profile named
// more than once or with an invalid name is a KindInvalidScope *Error with
// CaseBadProfile, which doesn't echo the name, and a name the config
// doesn't define is KindUnknownProfile. Serving either unscoped would widen
// what the client gets.
func (g *Gateway) narrowingOf(req *mcp.CallToolRequest, client clienttoken.Client, authed bool) (config.Narrowing, error) {
	if !authed {
		return config.Narrowing{}, errNoClient()
	}

	file, err := scopeOf(req)

	if err != nil {
		return config.Narrowing{}, err
	}

	n := config.Narrowing{Token: client.Connectors, File: file}
	vals := req.Extra.Header.Values(config.ProfileHeader)

	switch len(vals) {
	case 0:
		return n, nil
	case 1:
	default:
		return config.Narrowing{}, &Error{Kind: KindInvalidScope, Case: CaseBadProfile, Err: errors.New("profile named more than once")}
	}

	name := vals[0]

	if !config.IsProfileName(name) {
		return config.Narrowing{}, &Error{Kind: KindInvalidScope, Case: CaseBadProfile, Err: errors.New("profile name is not valid")}
	}

	var (
		p  *config.Scope
		ok bool
	)

	if g.profile != nil {
		p, ok = g.profile(name)
	}

	if !ok {
		return config.Narrowing{}, &Error{Kind: KindUnknownProfile, Detail: name}
	}

	n.ProfileName, n.Profile = name, p

	return n, nil
}

// errNoClient is narrowingOf's refusal of a request that carries no Client,
// and callCore's of a call whose options have no Narrow.
func errNoClient() error {
	return &Error{Kind: KindInternal, Case: CaseNoClient, Err: errors.New("request carries no client token info")}
}

// scopeOf is the project scope the request carries in config.ScopeHeader
// (ADR 0010), or nil for none. A header that doesn't parse, or is sent
// twice, is a KindInvalidScope *Error: ignoring it would widen what the
// project gets. Trusting the client is safe because a scope only narrows
// what its client token already allows.
func scopeOf(req *mcp.CallToolRequest) (*config.Scope, error) {
	if req.Extra == nil {
		return nil, nil
	}

	vals := req.Extra.Header.Values(config.ScopeHeader)

	switch len(vals) {
	case 0:
		return nil, nil
	case 1:
	default:
		return nil, &Error{Kind: KindInvalidScope, Err: errors.New("scope header sent more than once")}
	}

	s, err := config.ParseScopeHeader(vals[0])

	if err != nil {
		return nil, &Error{Kind: KindInvalidScope, Err: err}
	}

	return s, nil
}

// inScope keeps the statuses of the connectors n allows (its token, its
// profile, and its project scope), so to a narrowed request every other
// connector is not configured: search_tools refuses it in connectors, never
// lists it as configured or unavailable, and never warns about it.
func inScope(statuses []upstream.ConnectorStatus, n config.Narrowing) []upstream.ConnectorStatus {
	if n.Allowlist() == nil {
		return statuses
	}

	out := make([]upstream.ConnectorStatus, 0, len(statuses))

	for _, st := range statuses {
		if n.Allows(st.Connector) {
			out = append(out, st)
		}
	}

	return out
}
