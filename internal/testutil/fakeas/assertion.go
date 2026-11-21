package fakeas

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"time"
)

// jwtBearerAssertionType is the client_assertion_type of a JWT client
// assertion (RFC 7523 §2.2).
const jwtBearerAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" //nolint:gosec // G101: a URN, not a credential

// assertionAuth authenticates a token request by its client assertion
// (RFC 7523 §3): client_secret_jwt when it is signed with HS256 over the
// client's secret, private_key_jwt otherwise, with the key
// [WithClientKey] registered. Its iss and sub must name the client (and
// client_id, when sent, too), its aud the issuer or the token endpoint,
// its exp must be in the future, and its jti must not have been seen. s.mu
// must be held.
func (s *Server) assertionAuth(f url.Values, tr *TokenRequest) (Client, bool) {
	tr.AuthMethod = "private_key_jwt"
	id := f.Get("client_id")

	if f.Get("client_assertion_type") != jwtBearerAssertionType {
		return Client{ID: id}, false
	}

	a, input, sig, ok := decodeAssertion(f.Get("client_assertion"))

	if !ok {
		return Client{ID: id}, false
	}

	tr.Assertion = &a

	if a.Alg == "HS256" {
		tr.AuthMethod = "client_secret_jwt"
	}

	if id == "" {
		id = a.Iss
	}

	c, known := s.lookupClient(id)

	switch {
	case !known, a.Iss != id, a.Sub != id:
		return Client{ID: id}, false
	case !slices.Contains(a.Aud, s.MetadataIssuer) && !slices.Contains(a.Aud, s.Issuer) &&
		!slices.Contains(a.Aud, s.TokenEndpoint):
		return Client{ID: id}, false
	case a.Exp <= time.Now().Unix(), a.JTI == "", s.jtis[a.JTI]:
		return Client{ID: id}, false
	case !s.verifies(c, a.Alg, input, sig), !s.registeredFor(c, tr.AuthMethod):
		return Client{ID: id}, false
	}

	s.jtis[a.JTI] = true

	return c, true
}

// decodeAssertion splits a JWS compact serialization and decodes its
// header and claims, without verifying it. It returns the signing input
// and the signature too.
func decodeAssertion(jwt string) (Assertion, []byte, []byte, bool) {
	parts := strings.Split(jwt, ".")

	if len(parts) != 3 {
		return Assertion{}, nil, nil, false
	}

	var (
		header struct{ Alg, Typ, Kid string }
		claims struct {
			Iss, Sub, Jti string
			Aud           json.RawMessage
			Iat, Exp      int64
		}
	)

	if !decodePart(parts[0], &header) || !decodePart(parts[1], &claims) {
		return Assertion{}, nil, nil, false
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])

	if err != nil {
		return Assertion{}, nil, nil, false
	}

	a := Assertion{
		Alg: header.Alg, Typ: header.Typ, KID: header.Kid, Iss: claims.Iss, Sub: claims.Sub,
		JTI: claims.Jti, IAT: claims.Iat, Exp: claims.Exp,
	}
	var one string

	if json.Unmarshal(claims.Aud, &one) == nil {
		a.Aud = []string{one}
	} else if json.Unmarshal(claims.Aud, &a.Aud) != nil {
		return Assertion{}, nil, nil, false
	}

	return a, []byte(parts[0] + "." + parts[1]), sig, true
}

// decodePart decodes one base64url JSON part of a JWS into v.
func decodePart(part string, v any) bool {
	b, err := base64.RawURLEncoding.DecodeString(part)

	return err == nil && json.Unmarshal(b, v) == nil
}

// verifies reports whether sig is c's signature over input with alg (RFC
// 7518 §3): HS256 over c's secret, or the other algorithms with the
// public key [WithClientKey] registered for c.
func (s *Server) verifies(c Client, alg string, input, sig []byte) bool {
	if alg == "HS256" {
		if c.Secret == "" {
			return false
		}

		mac := hmac.New(sha256.New, []byte(c.Secret))
		mac.Write(input)

		return hmac.Equal(mac.Sum(nil), sig)
	}

	return verifyWith(s.cfg.clientKeys[c.ID], alg, input, sig)
}

// verifyWith reports whether sig is key's signature over input with alg
// (RFC 7518 §3), for an ECDSA, RSA, or Ed25519 public key.
func verifyWith(key crypto.PublicKey, alg string, input, sig []byte) bool {
	sum256 := sha256.Sum256(input)
	sum384 := sha512.Sum384(input)

	switch k := key.(type) {
	case *ecdsa.PublicKey:
		// RFC 7518 §3.4: r and s, fixed width, not ASN.1.
		size := (k.Curve.Params().BitSize + 7) / 8

		if len(sig) != 2*size {
			return false
		}

		r, ss := new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:])

		switch {
		case alg == "ES256" && size == 32:
			return ecdsa.Verify(k, sum256[:], r, ss)
		case alg == "ES384" && size == 48:
			return ecdsa.Verify(k, sum384[:], r, ss)
		}
	case *rsa.PublicKey:
		switch alg {
		case "RS256":
			return rsa.VerifyPKCS1v15(k, crypto.SHA256, sum256[:], sig) == nil
		case "PS256":
			return rsa.VerifyPSS(k, crypto.SHA256, sum256[:], sig, nil) == nil
		}
	case ed25519.PublicKey:
		return alg == "EdDSA" && ed25519.Verify(k, input, sig)
	}

	return false
}
