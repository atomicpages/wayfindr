package auth

import (
	"net/http"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/atomicpages/wayfindr/internal/config"
)

// role is which side of OAuth a request goes to: the authorization server
// or the resource (the upstream MCP server).
type role string

// Roles.
const (
	roleAuthorizationServer role = "as"
	roleResource            role = "rs"
)

// sender is the sender constraint of a connector's tokens (GLOSSARY: Sender
// constraint): what a token request and an upstream request carry to bind
// the token to Wayfindr. There are two: [bearer], and [dpop].
type sender interface {
	// tokenRequest adds what a token request carries. It is called afresh
	// for every attempt, a nonce retry's included.
	tokenRequest(r *endpointRequest) error
	// resourceRequest adds tok to r, an upstream request. It is called
	// afresh for every attempt, each replay's included.
	resourceRequest(r *http.Request, tok token) error
	// observe reads resp, any answer from a server of role r, for what the
	// sender keeps: a DPoP nonce.
	observe(resp *http.Response, r role)
	// retry reports whether resp, the answer from a server of role r, asks
	// for the request to be sent once more with a fresh proof.
	retry(resp *http.Response, r role) bool
}

// bearer is no sender constraint (RFC 6750): whoever holds the token can
// use it.
type bearer struct{}

func (bearer) tokenRequest(*endpointRequest) error { return nil }

// resourceRequest names the scheme Bearer whatever case the token_type
// came in, as RFC 6750 does.
func (bearer) resourceRequest(r *http.Request, tok token) error {
	r.Header.Set("Authorization", "Bearer "+tok.Access)

	return nil
}

func (bearer) observe(*http.Response, role) {}

func (bearer) retry(*http.Response, role) bool { return false }

// unusableSender is the sender of a connector whose sender constraint
// cannot be met, such as dpop = "always" with an authorization server that
// cannot verify ES256 proofs: every token request fails with err, before it
// is sent.
type unusableSender struct{ err error }

func (u unusableSender) tokenRequest(*endpointRequest) error { return u.err }

func (u unusableSender) resourceRequest(*http.Request, token) error { return u.err }

func (unusableSender) observe(*http.Response, role) {}

func (unusableSender) retry(*http.Response, role) bool { return false }

// chooseSender is the one choice of a grant's sender constraint, from the
// connector's [auth] table a, its discovery disc, and the resource's
// WWW-Authenticate challenge:
//
//   - dpop = "never": bearer;
//   - dpop = "always": DPoP, unless the authorization server lists
//     dpop_signing_alg_values_supported without ES256, the one algorithm
//     Wayfindr signs proofs with ([CaseDPoPAlgUnsupported]);
//   - dpop = "auto" (or unset): DPoP when the resource asks for it, by
//     dpop_bound_access_tokens_required in its protected resource metadata
//     or a DPoP challenge, or when the grant is already bound to a DPoP key,
//     and the authorization server lists ES256 in
//     dpop_signing_alg_values_supported; otherwise bearer.
//
// bound keeps a grant on DPoP once the resource has asked: a refresh token
// bound to the key is refused without a proof (RFC 9449 §5), and a new
// unattended grant, which has no fresh challenge to read, would otherwise
// fall back to bearer and earn a 401 each time.
//
// The DPoP sender it returns has no key yet: [Broker.oauthClient] binds the
// grant's. A token the authorization server did not bind is still sent as a
// bearer token (see [dpop]), so a server that declines DPoP keeps working.
func chooseSender(a config.Auth, disc Discovery, challenge []string, bound bool) (sender, error) {
	algs := disc.Extensions.DPoPSigningAlgValuesSupported
	listed := algs != nil
	es256 := slices.Contains(algs, algES256)

	switch a.DPoP {
	case config.DPoPNever:
		return bearer{}, nil
	case config.DPoPAlways:
		if listed && !es256 {
			return nil, newError(CaseDPoPAlgUnsupported, "",
				"the authorization server's dpop_signing_alg_values_supported does not list ES256", nil)
		}

		return dpop{}, nil
	case config.DPoPAuto:
	}

	asked := bound || (disc.PRM != nil && disc.PRM.DPOPBoundAccessTokensRequired) || offersDPoP(challenge)

	if asked && es256 {
		return dpop{}, nil
	}

	return bearer{}, nil
}

// offersDPoP reports whether challenge holds a DPoP challenge (RFC 9449
// §7.1).
func offersDPoP(challenge []string) bool {
	cs, _ := oauthex.ParseWWWAuthenticate(challenge)

	return slices.ContainsFunc(cs, func(c oauthex.Challenge) bool { return c.Scheme == "dpop" })
}

// tokenScheme reports whether scheme, lowercased as oauthex parses it, is
// one an access token is sent with, Bearer (RFC 6750) or DPoP (RFC 9449),
// so its challenge's resource_metadata, scope, and error apply.
func tokenScheme(scheme string) bool { return scheme == "bearer" || scheme == "dpop" }
