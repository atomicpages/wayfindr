package fakeas

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// The DPoP nonces the fake demands with [WithDPoP] (RFC 9449 §8, §9). They
// differ, so a client that keeps one nonce per origin, not per role, sends
// the wrong one to one of them: the fake is its own resource and
// authorization server, on one origin.
const (
	// ASNonce is the nonce the token endpoint demands.
	ASNonce = "as-nonce-1"
	// RSNonce is the nonce the resource demands.
	RSNonce = "rs-nonce-1"
)

// dpopIATWindow is how far a proof's iat may be from the fake's clock.
const dpopIATWindow = 5 * time.Minute

// dpopConfig is the DPoP configuration.
type dpopConfig struct {
	// on verifies proofs, binds tokens, and requires bound tokens.
	on bool
	// requireNonce demands ASNonce and RSNonce in proofs.
	requireNonce bool
	// algs is dpop_signing_alg_values_supported; advertised only when
	// algsSet, or with on (as ES256).
	algs    []string
	algsSet bool
}

// WithDPoP makes the fake a DPoP authorization server and resource (RFC
// 9449):
//
//   - its metadata advertises dpop_signing_alg_values_supported ES256
//     (unless [WithDPoPAlgs] says otherwise), and its protected resource
//     metadata dpop_bound_access_tokens_required;
//   - the token endpoint verifies a request's DPoP proof (typ dpop+jwt,
//     alg ES256, the jwk's signature, htm, htu, an iat within 5 minutes,
//     an unseen jti) and answers a bad one invalid_dpop_proof; a token
//     request with a good proof gets token_type DPoP, bound to the key's
//     thumbprint, and so does its refresh token; one without a proof gets
//     a bearer token;
//   - a code from an authorize request with dpop_jkt, and a bound refresh
//     token, are redeemed only with a proof by that key (invalid_grant);
//   - the resource answers a bound token sent as Bearer, or with a proof by
//     another key, a bad proof, or a wrong ath, with 401 invalid_token, and
//     a token not bound to a key too; its challenges use the DPoP scheme;
//   - with requireNonce, a proof without the right nonce gets 400
//     use_dpop_nonce at the token endpoint, with DPoP-Nonce: [ASNonce], and
//     401 with a DPoP use_dpop_nonce challenge at the resource, with
//     DPoP-Nonce: [RSNonce].
//
// Without it, a proof is decoded and recorded but not checked, and every
// token is a bearer token.
func WithDPoP(requireNonce bool) Option {
	return func(c *config) { c.dpop.on, c.dpop.requireNonce = true, requireNonce }
}

// WithDPoPAlgs advertises algs as dpop_signing_alg_values_supported, even
// none (an empty list), with or without [WithDPoP]; the fake still
// verifies ES256 proofs only.
func WithDPoPAlgs(algs ...string) Option {
	return func(c *config) { c.dpop.algs, c.dpop.algsSet = algs, true }
}

// dpopAlgs is dpop_signing_alg_values_supported, or nil to leave it out.
func (s *Server) dpopAlgs() []string {
	switch {
	case s.cfg.dpop.algsSet && s.cfg.dpop.algs == nil:
		return []string{}
	case s.cfg.dpop.algsSet:
		return s.cfg.dpop.algs
	case s.cfg.dpop.on:
		return []string{"ES256"}
	}

	return nil
}

// DPoPProof is a DPoP proof as a request carried it: its header, its
// claims, and the thumbprint of its jwk (RFC 9449 §4.2, RFC 7638).
type DPoPProof struct {
	Typ, Alg string
	// JKT is the jwk's SHA-256 thumbprint; empty when the jwk is not a
	// P-256 public key.
	JKT                string
	JTI, HTM, HTU, ATH string
	Nonce              string
	IAT                int64
	// PrivateKeyInJWK is whether the jwk held a private key.
	PrivateKeyInJWK bool
	// Error is why the fake refused the proof, or empty.
	Error string
}

// proofJWK is a proof's jwk header member.
type proofJWK struct {
	Kty, Crv, X, Y string
	D              string `json:"d"`
}

// decodeProof decodes a compact DPoP proof without checking it, and
// returns its signing input, its signature, and its key, if the jwk is a
// P-256 key.
func decodeProof(jwt string) (DPoPProof, []byte, []byte, *ecdsa.PublicKey, bool) {
	parts := strings.Split(jwt, ".")

	if len(parts) != 3 {
		return DPoPProof{}, nil, nil, nil, false
	}

	var (
		header struct {
			Typ, Alg string
			JWK      proofJWK
		}
		claims struct {
			JTI, HTM, HTU, ATH, Nonce string
			IAT                       int64
		}
	)

	if !decodePart(parts[0], &header) || !decodePart(parts[1], &claims) {
		return DPoPProof{}, nil, nil, nil, false
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])

	if err != nil {
		return DPoPProof{}, nil, nil, nil, false
	}

	p := DPoPProof{
		Typ: header.Typ, Alg: header.Alg, JTI: claims.JTI, HTM: claims.HTM, HTU: claims.HTU,
		ATH: claims.ATH, Nonce: claims.Nonce, IAT: claims.IAT, PrivateKeyInJWK: header.JWK.D != "",
	}
	key := p256Key(header.JWK)

	if key != nil {
		p.JKT = thumbprint(header.JWK)
	}

	return p, []byte(parts[0] + "." + parts[1]), sig, key, true
}

// p256Key is jwk's public key, or nil if it is not an EC P-256 key.
func p256Key(jwk proofJWK) *ecdsa.PublicKey {
	if jwk.Kty != "EC" || jwk.Crv != "P-256" {
		return nil
	}

	x, errX := base64.RawURLEncoding.DecodeString(jwk.X)
	y, errY := base64.RawURLEncoding.DecodeString(jwk.Y)

	if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
		return nil
	}

	key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))

	if err != nil {
		return nil
	}

	return key
}

// thumbprint is jwk's RFC 7638 SHA-256 thumbprint: the hash of its
// required members, in lexicographic order, with no whitespace.
func thumbprint(jwk proofJWK) string {
	doc, _ := json.Marshal(struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{jwk.Crv, jwk.Kty, jwk.X, jwk.Y})
	sum := sha256.Sum256(doc)

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// requestURL is r's URL as a proof's htu names it: scheme, host, and path.
func requestURL(r *http.Request) string {
	scheme := "http"

	if r.TLS != nil {
		scheme = "https"
	}

	return scheme + "://" + r.Host + r.URL.Path
}

// checkProof decodes the DPoP proof of r, if it has one, and with
// [WithDPoP] checks it for a request to the server of role (the token
// endpoint, "as", or the resource, "rs"), with access the token a
// resource request presents. It returns the proof (nil without one) and
// the OAuth error that refuses the request: invalid_dpop_proof,
// use_dpop_nonce, or "" for a good proof. A proof's jti is spent once it
// is checked, whatever the outcome. s.mu must be held.
func (s *Server) checkProof(r *http.Request, role, access string) (*DPoPProof, string) {
	values := r.Header.Values("DPoP")

	if len(values) == 0 {
		return nil, ""
	}

	p, input, sig, key, ok := decodeProof(values[0])

	if !ok {
		p = DPoPProof{}
	}

	if !s.cfg.dpop.on {
		return &p, ""
	}

	refuse := func(why string) (*DPoPProof, string) {
		p.Error = why

		return &p, "invalid_dpop_proof"
	}
	now := time.Now()

	switch {
	case !ok || len(values) > 1:
		return refuse("not one compact JWS")
	case p.Typ != "dpop+jwt":
		return refuse("typ is not dpop+jwt")
	case p.Alg != "ES256":
		return refuse("alg is not ES256")
	case key == nil || p.PrivateKeyInJWK:
		return refuse("jwk is not a P-256 public key")
	case !verifyWith(key, p.Alg, input, sig):
		return refuse("bad signature")
	case p.JTI == "" || s.dpopJTIs[p.JTI]:
		return refuse("jti missing or replayed")
	}

	s.dpopJTIs[p.JTI] = true

	switch iat := time.Unix(p.IAT, 0); {
	case p.HTM != r.Method:
		return refuse("htm is not the request method")
	case p.HTU != requestURL(r):
		return refuse("htu is not the request URL")
	case iat.Before(now.Add(-dpopIATWindow)) || iat.After(now.Add(dpopIATWindow)):
		return refuse("iat is outside the window")
	case access != "" && p.ATH != s256(access):
		return refuse("ath is not the hash of the access token")
	case access == "" && p.ATH != "":
		return refuse("ath on a token request")
	}

	want := ASNonce

	if role == "rs" {
		want = RSNonce
	}

	if s.cfg.dpop.requireNonce && p.Nonce != want {
		p.Error = "nonce missing or stale"

		return &p, "use_dpop_nonce"
	}

	return &p, ""
}

// dpopBound reports whether a resource request may use at, sent with the
// DPoP scheme or not and with proof, which checkProof answered proofErr
// to. Without [WithDPoP] every token is a bearer token. With it, the
// resource requires bound tokens (dpop_bound_access_tokens_required), each
// sent with the DPoP scheme and a good proof by the key it is bound to.
// s.mu must be held.
func (s *Server) dpopBound(at *accessToken, dpopScheme bool, proof *DPoPProof, proofErr string) bool {
	if !s.cfg.dpop.on {
		return true
	}

	return at.jkt != "" && dpopScheme && proof != nil && proofErr == "" && proof.JKT == at.jkt
}
