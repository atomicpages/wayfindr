package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// protocolInputRequired is the first protocol version whose clients can be
// asked for input over the stateless /mcp: as an input_required result,
// answered by calling again (SEP-2322). Earlier clients would need a
// server-to-client request, which a stateless handler can't send.
const protocolInputRequired = "2026-07-28"

// requestState is what the requestState of a call_tool input_required
// result holds, sealed with the master key so the client can neither read
// nor forge it (GLOSSARY: Request state). It binds the answer to the call
// that asked: the user (in the sealing's AAD), the client token instance,
// the tool, the arguments, and an expiry.
type requestState struct {
	// Token is the Instance of the client token the call came with, so an
	// answer is taken only from a call made with the same token, and not
	// from a later token given the name of a revoked one.
	Token     string `json:"token"`
	Connector string `json:"connector"`
	Tool      string `json:"tool"`
	// Args is argsDigest of the call's arguments.
	Args string `json:"args"`
	// Expires is when the state stops being accepted, in Unix seconds.
	Expires int64 `json:"expires"`
	// Upstream is the upstream package's state, for Continue.
	Upstream string `json:"upstream"`
}

// stateAAD binds a sealed request state to the gateway's user.
func (g *Gateway) stateAAD() []byte {
	return secrets.AAD("request_state", g.userID)
}

// sealState seals a request state for the call of tool on connector with
// args, made with the client token instance token, carrying the
// upstream's state.
func (g *Gateway) sealState(token, connector, tool string, args json.RawMessage, upstreamState string) string {
	plain, _ := json.Marshal(requestState{ // a struct of strings and an int always marshals
		Token:     token,
		Connector: connector,
		Tool:      tool,
		Args:      argsDigest(args),
		Expires:   g.now().Add(upstream.InteractionTTL).Unix(),
		Upstream:  upstreamState,
	})

	return base64.RawURLEncoding.EncodeToString(g.sealer.Seal(plain, g.stateAAD()))
}

// openState opens a request state the client sent back, and reports
// whether it is one this gateway sealed for this user, for the call of
// tool on connector with args made with the client token instance token,
// and not expired.
func (g *Gateway) openState(state, token, connector, tool string, args json.RawMessage) (requestState, bool) {
	st, ok := g.unseal(state)

	return st, ok && st.sealedFor(token) && st.Connector == connector && st.Tool == tool && st.Args == argsDigest(args) &&
		g.now().Unix() < st.Expires
}

// unseal opens a request state this gateway sealed for this user, whatever
// call it is bound to.
func (g *Gateway) unseal(state string) (requestState, bool) {
	var st requestState
	sealed, err := base64.RawURLEncoding.DecodeString(state)

	if err != nil {
		return st, false
	}

	plain, err := g.sealer.Open(sealed, g.stateAAD())

	return st, err == nil && json.Unmarshal(plain, &st) == nil
}

// discardState gives up on the question a request state answers, when the
// answer, sent with the client token instance token, is refused: the
// upstream is told cancel rather than waiting for it until the state
// expires. A state this gateway didn't seal is ignored, and so is one
// sealed for another token, so a client holding another's state can't
// cancel that client's question; it waits for its answer or its expiry.
func (g *Gateway) discardState(state, token string) {
	if st, ok := g.unseal(state); ok && st.sealedFor(token) {
		g.upstreams.Discard(st.Upstream)
	}
}

// sealedFor reports whether st was sealed for the client token instance
// token. An empty token is no instance, so it matches nothing, not even a
// state sealed before states carried a token.
func (st requestState) sealedFor(token string) bool {
	return token != "" && st.Token == token
}

// argsDigest is a digest of a call's arguments that doesn't depend on how
// the client serialized them: key order and whitespace may differ between
// the call that asked and the one that answers.
func argsDigest(args json.RawMessage) string {
	var v any

	if len(args) > 0 {
		// Numbers stay as written, so integers past 2^53 don't collide. The
		// arguments were validated as an object or null already.
		d := json.NewDecoder(bytes.NewReader(args))
		d.UseNumber()
		_ = d.Decode(&v)
	}

	canonical, _ := json.Marshal(v) // values json decoded always marshal
	sum := sha256.Sum256(canonical)

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ask puts res's input requests to the client: each request's id and
// message say which connector asks, and a requestState sealed for the
// client token instance token replaces the upstream's. A client that can't answer gets an error, and
// the upstream's question is discarded.
func (g *Gateway) ask(a Asker, token, connector, tool string, args json.RawMessage, res *mcp.CallToolResult) (*mcp.CallToolResult, error) {
	name := connector + "." + tool
	reqs := make(mcp.InputRequestMap, len(res.InputRequests))

	for id, r := range res.InputRequests {
		p, ok := r.(*mcp.ElicitParams)

		if !ok {
			// upstream passes on elicitations only.
			g.upstreams.Discard(res.RequestState)

			return nil, &Error{Kind: KindInternal, Case: CaseUpstream, Tool: name, Connector: connector}
		}

		cp := *p
		cp.Message = connector + " asks: " + p.Message
		reqs[connector+"-"+id] = &cp
	}

	if v := a.ProtocolVersion; v < protocolInputRequired {
		g.upstreams.Discard(res.RequestState)

		return nil, &Error{Kind: KindCannotAsk, Case: CaseClientTooOld, Tool: name, Connector: connector, Detail: v}
	}

	if !canElicit(a.Capabilities, reqs) {
		g.upstreams.Discard(res.RequestState)

		return nil, &Error{Kind: KindCannotAsk, Case: CaseNoElicitation, Tool: name, Connector: connector}
	}

	return &mcp.CallToolResult{
		InputRequests: reqs,
		RequestState:  g.sealState(token, connector, tool, args, res.RequestState),
	}, nil
}

// canElicit reports whether a client with caps can answer every one of
// reqs. As in the spec, a client that declares elicitation with neither
// mode supports form.
func canElicit(caps *mcp.ClientCapabilities, reqs mcp.InputRequestMap) bool {
	if caps == nil || caps.Elicitation == nil {
		return false
	}

	e := caps.Elicitation

	for _, r := range reqs {
		p, _ := r.(*mcp.ElicitParams) // ask built reqs from elicitations

		switch {
		case p.Mode == "url" && e.URL == nil:
			return false
		case p.Mode != "url" && e.Form == nil && e.URL != nil:
			return false
		}
	}

	return true
}

// unprefix maps the client's answers back to the upstream's request ids,
// dropping any not addressed to connector.
func unprefix(connector string, in mcp.InputResponseMap) mcp.InputResponseMap {
	out := make(mcp.InputResponseMap, len(in))

	for id, r := range in {
		if rest, ok := strings.CutPrefix(id, connector+"-"); ok {
			out[rest] = r
		}
	}

	return out
}

// now is the gateway's clock.
func (g *Gateway) now() time.Time {
	if g.clock != nil {
		return g.clock()
	}

	return time.Now()
}
