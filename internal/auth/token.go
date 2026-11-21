package auth

import "time"

// token is an OAuth token as Wayfindr holds it: what a token response
// carries, with its expiries on the broker's clock. It is also the sealed
// content of credentials.token.
type token struct {
	Access  string    `json:"access"`
	Refresh string    `json:"refresh,omitempty"`
	Type    string    `json:"type,omitempty"`
	Expiry  time.Time `json:"expiry,omitzero"` // zero when the server sent no expires_in
	// RefreshExpiry is when Refresh stops working: zero when the server
	// sent no refresh_token_expires_in, and in tokens sealed before it was
	// read.
	RefreshExpiry time.Time `json:"refresh_expiry,omitzero"`
	// DPoPKey is the key of the grant, when its token requests were proved
	// with DPoP: the token is bound to it if Type is DPoP. It is sealed
	// with the token (dpop_key, PKCS#8 in base64), so it is replaced and
	// deleted with it; a token sealed before DPoP has none. Never log it.
	DPoPKey *dpopKey `json:"dpop_key,omitempty"`
	// TokenEndpoint is the token endpoint the grant was obtained at, as
	// discovered or configured (before an mTLS alias). A refresh spends
	// Refresh only there: an upstream that later names another
	// authorization server gets no refresh token or client authentication
	// (wayfindr-afb1).
	TokenEndpoint string `json:"token_endpoint,omitempty"`
	// PRMURL is where the grant's flow found the resource's Protected
	// Resource Metadata. A refresh made before the token expires has no
	// challenge to name it, and a PRM at a path only the challenge names is
	// otherwise not found (wayfindr-q8y9). Empty when the flow found none
	// (authorization_server set, or a legacy server).
	PRMURL string `json:"prm_url,omitempty"`
	// PRMKnown is true once PRMURL holds what discovery found: set by every
	// flow, and by the first refresh of a token sealed before PRMURL was
	// kept, which probes the resource for its challenge instead.
	PRMKnown bool `json:"prm_known,omitempty"`
}
