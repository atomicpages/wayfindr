package auth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// This file is the DPoP sender constraint (RFC 9449, GLOSSARY: DPoP): the
// key a grant's tokens are bound to, the proofs signed with it, and the
// nonces servers demand in them.

// tokenTypeDPoP is the token_type of a DPoP-bound token (RFC 9449 §5),
// and the scheme it is sent with (§7.1).
const tokenTypeDPoP = "DPoP"

// errUseDPoPNonce is the error code of a server that wants a nonce in the
// proof (RFC 9449 §8, §9).
const errUseDPoPNonce = "use_dpop_nonce"

// dpopKey is a grant's DPoP key: an ECDSA P-256 key, signing ES256. One is
// made per grant. An auth flow's is sealed with its token (the dpop_key of
// the token blob, PKCS#8 in base64); an unattended grant's lives with its
// in-memory token. Never log it.
type dpopKey struct {
	priv *ecdsa.PrivateKey
}

// newDPoPKey makes a new DPoP key.
func newDPoPKey() (*dpopKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	if err != nil {
		return nil, fmt.Errorf("generate dpop key: %w", err)
	}

	return &dpopKey{priv: priv}, nil
}

// MarshalJSON writes the key as its PKCS#8 encoding in standard base64,
// as the token blob holds it.
func (k *dpopKey) MarshalJSON() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k.priv)

	if err != nil {
		return nil, fmt.Errorf("encode dpop key: %w", err)
	}

	return json.Marshal(base64.StdEncoding.EncodeToString(der))
}

// UnmarshalJSON reads a key MarshalJSON wrote. Anything but a P-256 ECDSA
// key is an error, which makes the stored token unreadable.
func (k *dpopKey) UnmarshalJSON(b []byte) error {
	var text string

	if err := json.Unmarshal(b, &text); err != nil {
		return fmt.Errorf("decode dpop key: %w", err)
	}

	der, err := base64.StdEncoding.DecodeString(text)

	if err != nil {
		return fmt.Errorf("decode dpop key: %w", err)
	}

	parsed, err := x509.ParsePKCS8PrivateKey(der)

	if err != nil {
		return fmt.Errorf("decode dpop key: %w", err)
	}

	priv, ok := parsed.(*ecdsa.PrivateKey)

	if !ok || priv.Curve != elliptic.P256() {
		return errors.New("decode dpop key: not an EC P-256 key")
	}

	k.priv = priv

	return nil
}

// dpopJWK is the public half of a DPoP key as a JWK (RFC 7517), with its
// members in the lexicographic order RFC 7638 §3.2 hashes them in, so its
// JSON is also the input of the thumbprint.
type dpopJWK struct {
	Crv string `json:"crv"`
	Kty string `json:"kty"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// jwk is the key's public JWK: the curve and the point, never the private
// scalar.
func (k *dpopKey) jwk() dpopJWK {
	// The uncompressed point: 0x04, then x and y, 32 bytes each.
	point, _ := k.priv.PublicKey.Bytes() // never fails for a P-256 key
	const size = 32

	return dpopJWK{
		Crv: "P-256", Kty: "EC",
		X: b64url(point[1 : 1+size]), Y: b64url(point[1+size:]),
	}
}

// thumbprint is the key's JWK SHA-256 thumbprint (RFC 7638), which
// dpop_jkt names on the authorization request (RFC 9449 §10).
func (k *dpopKey) thumbprint() string {
	doc, _ := json.Marshal(k.jwk()) // strings only: never fails
	sum := sha256.Sum256(doc)

	return b64url(sum[:])
}

// dpopClaims are a DPoP proof's claims (RFC 9449 §4.2).
type dpopClaims struct {
	JTI   string `json:"jti"`
	HTM   string `json:"htm"`
	HTU   string `json:"htu"`
	IAT   int64  `json:"iat"`
	Nonce string `json:"nonce,omitempty"`
	// ATH is the access token's hash, on a resource request only.
	ATH string `json:"ath,omitempty"`
}

// proof is a fresh DPoP proof for a method request to target, at now,
// carrying nonce when it is set and, for a resource request, the hash of
// access.
func (k *dpopKey) proof(method, target string, now time.Time, nonce, access string) (string, error) {
	htu, err := proofTarget(target)

	if err != nil {
		return "", err
	}

	claims := dpopClaims{JTI: random(16), HTM: method, HTU: htu, IAT: now.Unix(), Nonce: nonce}

	if access != "" {
		sum := sha256.Sum256([]byte(access))
		claims.ATH = b64url(sum[:])
	}

	jwk := k.jwk()

	return signJWS(jwsHeader{Alg: algES256, Typ: "dpop+jwt", JWK: &jwk}, claims, k.priv)
}

// proofTarget is a proof's htu: target without its query and fragment
// (RFC 9449 §4.2).
func proofTarget(target string) (string, error) {
	u, err := url.Parse(target)

	if err != nil {
		return "", fmt.Errorf("dpop proof target: %w", err)
	}

	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment, u.User = "", false, "", "", nil

	return u.String(), nil
}

// nonceKey is whose nonce it is: the server's origin and its role. One
// origin may be both the authorization server and the resource, each with
// a nonce of its own.
type nonceKey struct {
	origin string
	role   role
}

// nonceCache holds the last DPoP nonce each server sent (RFC 9449 §8,
// §9), in memory. The broker has one, shared by all its connectors.
type nonceCache struct {
	mu     sync.Mutex
	nonces map[nonceKey]string
}

func newNonceCache() *nonceCache { return &nonceCache{nonces: map[nonceKey]string{}} }

// get returns the nonce of the server of role r at target's origin, or "".
func (c *nonceCache) get(target string, r role) string {
	u, err := url.Parse(target)

	if err != nil {
		return ""
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.nonces[nonceKey{origin(u), r}]
}

// observe keeps the DPoP-Nonce resp carries, if any, as the nonce of the
// server of role r that sent it.
func (c *nonceCache) observe(resp *http.Response, r role) {
	nonce := resp.Header.Get("DPoP-Nonce")

	if nonce == "" || resp.Request == nil || resp.Request.URL == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.nonces[nonceKey{origin(resp.Request.URL), r}] = nonce
}

// origin is u's scheme and host, lowercased.
func origin(u *url.URL) string {
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// dpop is the DPoP sender. A token request carries a proof signed with
// key, the grant's key. A resource request carries one signed with the key
// its token is bound to, and names the DPoP scheme; a token the
// authorization server did not bind (its token_type is not DPoP) is sent
// as a bearer token instead. Each proof is fresh: a new jti every time,
// replays included.
type dpop struct {
	// key signs token requests; nil on the Transport's sender, which signs
	// with each token's own key.
	key    *dpopKey
	nonces *nonceCache
	now    func() time.Time
}

// tokenRequest adds a proof of the POST to the token endpoint.
func (d dpop) tokenRequest(r *endpointRequest) error {
	if d.key == nil {
		return errors.New("dpop: no key to sign the token request with")
	}

	proof, err := d.key.proof(http.MethodPost, r.url, d.now(), d.nonces.get(r.url, roleAuthorizationServer), "")

	if err != nil {
		return err
	}

	r.header.Set("DPoP", proof)

	return nil
}

// resourceRequest sends tok with a proof of r, signed with the key tok is
// bound to, or as a bearer token if it is not bound.
func (d dpop) resourceRequest(r *http.Request, tok token) error {
	if tok.DPoPKey == nil || !strings.EqualFold(tok.Type, tokenTypeDPoP) {
		return bearer{}.resourceRequest(r, tok)
	}

	target := r.URL.String()
	proof, err := tok.DPoPKey.proof(r.Method, target, d.now(), d.nonces.get(target, roleResource), tok.Access)

	if err != nil {
		return err
	}

	r.Header.Set("Authorization", tokenTypeDPoP+" "+tok.Access)
	r.Header.Set("DPoP", proof)

	return nil
}

// observe keeps any nonce the server sent.
func (d dpop) observe(resp *http.Response, r role) { d.nonces.observe(resp, r) }

// retry reports whether resp is a demand for a nonce the proof lacked or
// had wrong, with the nonce to use: a 400 use_dpop_nonce from the
// authorization server (RFC 9449 §8), or a 401 with a DPoP use_dpop_nonce
// challenge from the resource (§9), to a request that carried a proof. The
// nonce is already kept (see observe), so the next proof carries it.
func (d dpop) retry(resp *http.Response, r role) bool {
	if resp.Header.Get("DPoP-Nonce") == "" || resp.Request == nil || resp.Request.Header.Get("DPoP") == "" {
		return false
	}

	switch r {
	case roleAuthorizationServer:
		return resp.StatusCode == http.StatusBadRequest && peekErrorCode(resp) == errUseDPoPNonce
	case roleResource:
		if resp.StatusCode != http.StatusUnauthorized {
			return false
		}

		cs, _ := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))

		for _, c := range cs {
			if c.Scheme == "dpop" && c.Params["error"] == errUseDPoPNonce {
				return true
			}
		}
	}

	return false
}

// peekErrorCode returns the error of resp's RFC 6749 §5.2 error body, or
// "", leaving the body to be read again from the start.
func peekErrorCode(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse+1))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}

	if err != nil {
		return ""
	}

	var e struct {
		Error string `json:"error"`
	}

	if json.Unmarshal(body, &e) != nil {
		return ""
	}

	return e.Error
}
