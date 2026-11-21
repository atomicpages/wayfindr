package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
)

// This file is a sign-only JWS (RFC 7515) compact serializer, and the keys
// it signs with (ADR 0018). Wayfindr signs JWTs but never verifies one, so
// there is no parsing or verification here.

// The JWS algorithms Wayfindr signs with (RFC 7518 §3.1, RFC 8037).
const (
	algES256 = "ES256"
	algES384 = "ES384"
	algRS256 = "RS256"
	algPS256 = "PS256"
	algEdDSA = "EdDSA"
	algHS256 = "HS256"
)

// minRSABits is the smallest RSA key RFC 7518 §3.3 allows.
const minRSABits = 2048

// jwsHeader is a JOSE header: the algorithm, the type of the JWT, the key
// ID when there is one, and the public key itself when the JWT carries it
// (a DPoP proof, RFC 9449 §4.2).
type jwsHeader struct {
	Alg string   `json:"alg"`
	Typ string   `json:"typ,omitempty"`
	KID string   `json:"kid,omitempty"`
	JWK *dpopJWK `json:"jwk,omitempty"`
}

// signJWS returns the compact serialization of payload, encoded as JSON,
// signed with h.Alg by key: a crypto.Signer that alg fits (see [canSign]),
// or the []byte secret of HS256.
func signJWS(h jwsHeader, payload any, key any) (string, error) {
	head, err := json.Marshal(h)

	if err != nil {
		return "", fmt.Errorf("encode jws header: %w", err)
	}

	body, err := json.Marshal(payload)

	if err != nil {
		return "", fmt.Errorf("encode jws payload: %w", err)
	}

	input := b64url(head) + "." + b64url(body)
	sig, err := jwsSignature(h.Alg, key, []byte(input))

	if err != nil {
		return "", err
	}

	return input + "." + b64url(sig), nil
}

// jwsSignature signs input with alg by key (RFC 7518 §3).
func jwsSignature(alg string, key any, input []byte) ([]byte, error) {
	if secret, ok := key.([]byte); ok {
		if alg != algHS256 {
			return nil, fmt.Errorf("jws: a secret cannot sign %s", alg)
		}

		mac := hmac.New(sha256.New, secret)
		mac.Write(input)

		return mac.Sum(nil), nil
	}

	signer, ok := key.(crypto.Signer)

	if !ok || !canSign(alg, signer) {
		return nil, fmt.Errorf("jws: the key cannot sign %s", alg)
	}

	switch k := signer.(type) {
	case *ecdsa.PrivateKey:
		return ecdsaSignature(k, input)
	case *rsa.PrivateKey:
		digest := sha256.Sum256(input)

		if alg == algPS256 {
			// RFC 7518 §3.5: the salt is as long as the hash.
			return rsa.SignPSS(rand.Reader, k, crypto.SHA256, digest[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		}

		return rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:])
	case ed25519.PrivateKey:
		return ed25519.Sign(k, input), nil
	}

	return nil, fmt.Errorf("jws: the key cannot sign %s", alg)
}

// ecdsaSignature is the JWS form of an ECDSA signature (RFC 7518 §3.4): r
// and s as fixed-width big-endian integers, one after the other, rather
// than the ASN.1 sequence crypto/ecdsa produces.
func ecdsaSignature(k *ecdsa.PrivateKey, input []byte) ([]byte, error) {
	var digest []byte

	switch k.Curve {
	case elliptic.P256():
		sum := sha256.Sum256(input)
		digest = sum[:]
	case elliptic.P384():
		sum := sha512.Sum384(input)
		digest = sum[:]
	default:
		return nil, errors.New("jws: unsupported curve")
	}

	r, s, err := ecdsa.Sign(rand.Reader, k, digest)

	if err != nil {
		return nil, fmt.Errorf("jws: sign: %w", err)
	}

	size := (k.Curve.Params().BitSize + 7) / 8
	sig := make([]byte, 2*size)
	r.FillBytes(sig[:size])
	s.FillBytes(sig[size:])

	return sig, nil
}

// canSign reports whether key can sign alg: ES256 needs a P-256 key, ES384
// a P-384 key, RS256 and PS256 an RSA key, and EdDSA an Ed25519 key.
func canSign(alg string, key crypto.Signer) bool {
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		return (alg == algES256 && k.Curve == elliptic.P256()) || (alg == algES384 && k.Curve == elliptic.P384())
	case *rsa.PrivateKey:
		return alg == algRS256 || alg == algPS256
	case ed25519.PrivateKey:
		return alg == algEdDSA
	}

	return false
}

// naturalAlg is the algorithm a key signs with when nothing names one: ES256
// for P-256, ES384 for P-384, RS256 for RSA, and EdDSA for Ed25519.
func naturalAlg(key crypto.Signer) string {
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		if k.Curve == elliptic.P384() {
			return algES384
		}

		return algES256
	case *rsa.PrivateKey:
		return algRS256
	case ed25519.PrivateKey:
		return algEdDSA
	}

	return ""
}

// pickAlg chooses the algorithm key signs with: want, when set; else the
// first of offered (the authorization server's signing algorithms, in its
// order) that key can sign; else the key's natural one.
func pickAlg(key crypto.Signer, want string, offered []string) string {
	if want != "" {
		return want
	}

	if i := slices.IndexFunc(offered, func(alg string) bool { return canSign(alg, key) }); i >= 0 {
		return offered[i]
	}

	return naturalAlg(key)
}

// parsePrivateKey reads a PEM private key Wayfindr can sign with: PKCS#8
// ("PRIVATE KEY") holding an EC P-256 or P-384, RSA, or Ed25519 key, or the
// older SEC 1 ("EC PRIVATE KEY") and PKCS#1 ("RSA PRIVATE KEY") forms. An
// RSA key must have at least 2048 bits. The error never quotes the key.
func parsePrivateKey(text string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(text))

	if block == nil {
		return nil, errors.New("not a PEM block")
	}

	var (
		key any
		err error
	)

	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, errors.New("the PEM block is not a private key")
	}

	if err != nil {
		return nil, errors.New("the PEM block is not a valid private key")
	}

	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return nil, errors.New("an EC key must be on P-256 or P-384")
		}

		return k, nil
	case *rsa.PrivateKey:
		if k.N.BitLen() < minRSABits {
			return nil, fmt.Errorf("an RSA key must have at least %d bits", minRSABits)
		}

		return k, nil
	case ed25519.PrivateKey:
		return k, nil
	}

	return nil, fmt.Errorf("unsupported key type %T", key)
}

// b64url is unpadded base64url (RFC 7515 §2).
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
