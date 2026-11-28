package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

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
// the tool, the arguments, and an expiry. A state the approval gate covers
// is also bound to the process that sealed it (Boot), since only that
// process remembers it was spent.
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
	// Boot is the boot id of the Gateway that sealed it. A state the
	// approval gate covers is taken only with the same boot id: the set of
	// spent states is in memory, so after a restart a state spent before
	// it would be taken again. Empty in a state sealed before states
	// carried it, which no gated call takes.
	Boot string `json:"boot,omitempty"`
	// Gated is set when the call that minted it was gated, so its state is
	// taken once even if the call is no longer gated when it resumes (its
	// tool's class changed, say).
	Gated bool `json:"gated,omitempty"`
}

// stateAAD binds a sealed request state to the gateway's user.
func (g *Gateway) stateAAD() []byte {
	return secrets.AAD("request_state", g.userID)
}

// sealState seals a request state for the call of tool on connector with
// args, made with the client token instance token, carrying the
// upstream's state. gated is whether the approval gate covered the call.
func (g *Gateway) sealState(token, connector, tool string, args json.RawMessage, upstreamState string, gated bool) string {
	plain, _ := json.Marshal(requestState{ // a struct of strings, an int, and a bool always marshals
		Token:     token,
		Connector: connector,
		Tool:      tool,
		Args:      argsDigest(args),
		Expires:   g.now().Add(upstream.InteractionTTL).Unix(),
		Upstream:  upstreamState,
		Boot:      g.boot,
		Gated:     gated,
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

// The bounds on a gated call's arguments (ADR 0024). The owner reads them
// in full before approving, and the queue holds them in memory: at most
// maxApprovals of these per client token instance.
const (
	// maxGatedArgsBytes bounds their size as sent.
	maxGatedArgsBytes = 64 << 10
	// maxGatedArgsDepth bounds their nesting: the arguments object is one
	// level, and each object or array inside it one more.
	maxGatedArgsDepth = 32
)

// gatedArguments is why args, a gated call's arguments (an object, or nil
// for none), are refused, or "" when they are not (ADR 0024).
//
// CaseArgumentsTooLarge: they are larger than maxGatedArgsBytes, or nest
// deeper than maxGatedArgsDepth: more than the owner can be asked to read,
// or the queue to hold.
//
// CaseAmbiguousArguments: they could read one way to argsDigest and another
// to the upstream, so the owner's approval could bind other arguments than
// the upstream runs. They must be valid UTF-8, with no lone surrogate
// escape, which a decoder may turn into U+FFFD; no object may hold two
// keys that are equal, which decoders resolve differently (Go's keeps the
// last), or that look alike (foldKey), which an upstream that normalizes or
// ignores case would merge; and nothing may follow the one JSON value,
// which a lenient reader could take as more arguments.
func gatedArguments(args json.RawMessage) Case {
	if len(args) == 0 {
		return ""
	}

	if len(args) > maxGatedArgsBytes {
		return CaseArgumentsTooLarge
	}

	if !utf8.Valid(args) || loneSurrogate(args) {
		return CaseAmbiguousArguments
	}

	d := json.NewDecoder(bytes.NewReader(args))
	d.UseNumber()

	if c := checkValue(d, 0); c != "" {
		return c
	}

	// One value and only whitespace after it. The call path's other
	// readers refuse trailing data too, but the approval's digest reads
	// only the first value, so this check doesn't lean on them.
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return CaseAmbiguousArguments
	}

	return ""
}

// checkValue reads one value from d, nested in depth objects and arrays,
// and returns gatedArguments' Case for it: CaseArgumentsTooLarge for an
// object or array past maxGatedArgsDepth, CaseAmbiguousArguments for an
// object with two keys whose foldKeys are equal, or for JSON that isn't
// valid. Keys are compared as decoded, so "a" and its a escape are
// the same key. The depth bound also bounds its recursion.
func checkValue(d *json.Decoder, depth int) Case {
	tok, err := d.Token()

	if err != nil {
		return CaseAmbiguousArguments
	}

	delim, ok := tok.(json.Delim)

	if !ok {
		return ""
	}

	if depth+1 > maxGatedArgsDepth {
		return CaseArgumentsTooLarge
	}

	// The keys seen so far, by foldKey, when the value is an object.
	seen := map[string]bool{}

	for d.More() {
		if delim == '{' {
			k, err := d.Token()
			key, ok := k.(string)

			if err != nil || !ok {
				return CaseAmbiguousArguments
			}

			if key = foldKey(key); seen[key] {
				return CaseAmbiguousArguments
			}

			seen[key] = true
		}

		if c := checkValue(d, depth+1); c != "" {
			return c
		}
	}

	if _, err := d.Token(); err != nil {
		return CaseAmbiguousArguments
	}

	return ""
}

// foldKey is what s is compared by among its sibling keys: two keys with
// the same foldKey look alike, or an upstream that normalizes keys or
// ignores their case could read them as one. It is s
//
//   - in NFKC, so composed and decomposed accents, ligatures (ﬁ), full-width
//     letters (ｆ), and compatibility forms (the Kelvin sign, ſ) read as the
//     plain text they stand for;
//   - with each rune upper-cased then lower-cased (unicode.ToUpper,
//     unicode.ToLower), which full case folding alone doesn't do for the
//     Turkish dotless ı and dotted İ: Unicode folds ı to itself and İ to i
//     plus a combining dot, so neither would match i;
//   - fully case-folded (cases.Fold), which turns ß into ss and ς into σ;
//   - in NFKC again, since folding may leave text unnormalized.
//
// ASCII keys take a shortcut with the same result: lower case.
func foldKey(s string) string {
	ascii := true

	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false

			break
		}
	}

	if ascii {
		return strings.ToLower(s)
	}

	s = strings.Map(func(r rune) rune { return unicode.ToLower(unicode.ToUpper(r)) }, norm.NFKC.String(s))

	return norm.NFKC.String(cases.Fold().String(s))
}

// loneSurrogate reports whether a string in data, JSON text, holds a \u
// escape of a UTF-16 surrogate that isn't half of a pair.
func loneSurrogate(data []byte) bool {
	in := false

	for i := 0; i < len(data); i++ {
		switch {
		case !in:
			in = data[i] == '"'
		case data[i] == '"':
			in = false
		case data[i] == '\\' && i+1 < len(data) && data[i+1] == 'u':
			r := escapedUnit(data, i)

			switch {
			case r >= 0xD800 && r < 0xDC00:
				if lo := escapedUnit(data, i+6); lo < 0xDC00 || lo > 0xDFFF {
					return true
				}

				i += 11
			case r >= 0xDC00 && r <= 0xDFFF:
				return true
			default:
				i += 5
			}
		case data[i] == '\\':
			i++ // the escaped character, which may be a quote
		}
	}

	return false
}

// escapedUnit is the UTF-16 code unit of the \uXXXX escape at data[i:], or
// -1 when none starts there.
func escapedUnit(data []byte, i int) int {
	if i+6 > len(data) || data[i] != '\\' || data[i+1] != 'u' {
		return -1
	}

	v, err := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)

	if err != nil {
		return -1
	}

	return int(v)
}

// ask puts res's input requests to the client: each request's id and
// message say which connector asks, and a requestState sealed for the
// client token instance token replaces the upstream's; gated is whether the
// approval gate covered the call, or the state it resumed. A client that
// can't answer gets an error, and the upstream's question is discarded.
func (g *Gateway) ask(a Asker, token, connector, tool string, args json.RawMessage, gated bool, res *mcp.CallToolResult) (*mcp.CallToolResult, error) {
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
		RequestState:  g.sealState(token, connector, tool, args, res.RequestState, gated),
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
