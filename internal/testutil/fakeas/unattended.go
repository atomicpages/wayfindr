package fakeas

import (
	"crypto"
	"net/url"
	"slices"
	"strings"
	"time"
)

// The unattended grant types the token endpoint serves.
const (
	// GrantClientCredentials is the client credentials grant (RFC 6749
	// §4.4).
	GrantClientCredentials = "client_credentials"
	// GrantJWTBearer is the JWT bearer grant (RFC 7523 §2.1).
	GrantJWTBearer = "urn:ietf:params:oauth:grant-type:jwt-bearer" //nolint:gosec // G101: a URN, not a credential
)

// unattendedConfig is the unattended grants' configuration.
type unattendedConfig struct {
	// jwtBearerKey verifies jwt-bearer assertions; nil refuses the grant.
	jwtBearerKey crypto.PublicKey
	// withheld are scopes an unattended grant never gets.
	withheld []string
	// refreshTokens issues refresh tokens with unattended grants.
	refreshTokens bool
}

// WithJWTBearerKey accepts the JWT bearer grant (RFC 7523 §2.1) with an
// assertion signed by the private half of key (ECDSA, RSA, or Ed25519),
// whose aud names the issuer or the token endpoint and whose exp is in the
// future. A refused assertion is invalid_grant. Without it the grant is
// unsupported_grant_type. The assertion's claims are recorded in
// [TokenRequest.GrantAssertion]; its sub is not checked.
func WithJWTBearerKey(key crypto.PublicKey) Option {
	return func(c *config) { c.unattended.jwtBearerKey = key }
}

// WithWithheldScopes makes the client_credentials and jwt-bearer grants
// leave scopes out of what they grant, whatever the request asked for, as
// a provider does when the client may not have them. Combined with
// [WithRequiredScopes], the resource keeps answering 403 insufficient_scope.
func WithWithheldScopes(scopes ...string) Option {
	return func(c *config) { c.unattended.withheld = scopes }
}

// WithUnattendedRefreshTokens issues a refresh token with the
// client_credentials and jwt-bearer grants too, which RFC 6749 §4.4.3 says
// a server should not, so a client can be checked to ignore it.
func WithUnattendedRefreshTokens() Option {
	return func(c *config) { c.unattended.refreshTokens = true }
}

// RefuseAllTokens makes the resource answer every token, even one issued
// later, with 401 invalid_token while on is set, as an upstream that does
// not trust the authorization server would.
func (s *Server) RefuseAllTokens(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuseTokens = on
}

// unattendedGrant redeems a client_credentials or jwt-bearer request from
// client c, recorded in tr, and returns what it stands for, or an OAuth
// error code and description. s.mu must be held.
func (s *Server) unattendedGrant(c Client, f url.Values, tr *TokenRequest) (*grant, string, string) {
	switch tr.GrantType {
	case GrantClientCredentials:
		// Only a client that proved who it is has access of its own.
		if tr.AuthMethod == "none" {
			return nil, "unauthorized_client", "a public client cannot use client_credentials"
		}
	case GrantJWTBearer:
		if code, desc := s.checkGrantAssertion(f.Get("assertion"), tr); code != "" {
			return nil, code, desc
		}
	}

	var granted []string

	for _, sc := range strings.Fields(tr.Scope) {
		if !slices.Contains(s.cfg.unattended.withheld, sc) {
			granted = append(granted, sc)
		}
	}

	return &grant{clientID: c.ID, scope: strings.Join(granted, " "), resource: tr.Resource}, "", ""
}

// checkGrantAssertion verifies a jwt-bearer grant's assertion with the key
// [WithJWTBearerKey] registered and records its claims in tr. It returns
// an OAuth error code and description, or "" for a good one.
func (s *Server) checkGrantAssertion(jwt string, tr *TokenRequest) (string, string) {
	key := s.cfg.unattended.jwtBearerKey

	if key == nil {
		return "unsupported_grant_type", "the jwt-bearer grant is not enabled"
	}

	a, input, sig, ok := decodeAssertion(jwt)

	if !ok {
		return "invalid_grant", "the assertion is not a JWT"
	}

	tr.GrantAssertion = &a

	switch {
	case !verifyWith(key, a.Alg, input, sig):
		return "invalid_grant", "the assertion's signature does not verify"
	case !slices.Contains(a.Aud, s.MetadataIssuer) && !slices.Contains(a.Aud, s.Issuer) &&
		!slices.Contains(a.Aud, s.TokenEndpoint):
		return "invalid_grant", "the assertion's audience is not this server"
	case a.Exp <= time.Now().Unix():
		return "invalid_grant", "the assertion has expired"
	}

	return "", ""
}
