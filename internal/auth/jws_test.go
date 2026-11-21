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
	"math/big"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// The JWS signer is unexported: Wayfindr's seam is the token request, and
// these table tests are the clearest spec of RFC 7515/7518's encoding (the
// fixed-width r||s of ECDSA above all). They verify each signature with
// the standard library, never with the code under test. The token request
// tests in clientauth_test.go cover the same signer at the seam.

// testKeys returns a fresh key of every type Wayfindr signs with.
func testKeys(t *testing.T) map[string]crypto.Signer {
	t.Helper()
	p256, err1 := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p384, err2 := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsaKey, err3 := rsa.GenerateKey(rand.Reader, 2048)
	_, ed, err4 := ed25519.GenerateKey(rand.Reader)

	for _, err := range []error{err1, err2, err3, err4} {
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
	}

	return map[string]crypto.Signer{"p256": p256, "p384": p384, "rsa": rsaKey, "ed25519": ed}
}

// splitJWS decodes a compact JWS into its header, payload, signing input,
// and signature.
func splitJWS(t *testing.T, jws string) (header, payload map[string]any, input, sig []byte) {
	t.Helper()
	parts := strings.Split(jws, ".")

	if len(parts) != 3 {
		t.Fatalf("JWS %q has %d parts, want 3", jws, len(parts))
	}

	decode := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)

		if err != nil {
			t.Fatalf("part %q is not unpadded base64url: %v", s, err)
		}

		return b
	}

	if err := json.Unmarshal(decode(parts[0]), &header); err != nil {
		t.Fatalf("header: %v", err)
	}

	if err := json.Unmarshal(decode(parts[1]), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}

	return header, payload, []byte(parts[0] + "." + parts[1]), decode(parts[2])
}

func TestSignJWSVerifiesWithTheStandardLibrary(t *testing.T) {
	keys := testKeys(t)
	secret := []byte("a-client-secret-of-some-length")
	verifyEC := func(size int) func(t *testing.T, key any, input, sig []byte) {
		return func(t *testing.T, key any, input, sig []byte) {
			t.Helper()

			if len(sig) != 2*size {
				t.Fatalf("signature is %d bytes, want %d: r||s at fixed width, not ASN.1", len(sig), 2*size)
			}

			var digest []byte

			if size == 32 {
				s := sha256.Sum256(input)
				digest = s[:]
			} else {
				s := sha512.Sum384(input)
				digest = s[:]
			}

			r, s := new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:])

			if !ecdsa.Verify(&key.(*ecdsa.PrivateKey).PublicKey, digest, r, s) {
				t.Error("ecdsa.Verify failed")
			}
		}
	}
	tests := []struct {
		alg    string
		key    any
		verify func(t *testing.T, key any, input, sig []byte)
	}{
		{"ES256", keys["p256"], verifyEC(32)},
		{"ES384", keys["p384"], verifyEC(48)},
		{"RS256", keys["rsa"], func(t *testing.T, key any, input, sig []byte) {
			t.Helper()
			d := sha256.Sum256(input)

			if err := rsa.VerifyPKCS1v15(&key.(*rsa.PrivateKey).PublicKey, crypto.SHA256, d[:], sig); err != nil {
				t.Errorf("rsa.VerifyPKCS1v15: %v", err)
			}
		}},
		{"PS256", keys["rsa"], func(t *testing.T, key any, input, sig []byte) {
			t.Helper()
			d := sha256.Sum256(input)
			opts := &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}

			if err := rsa.VerifyPSS(&key.(*rsa.PrivateKey).PublicKey, crypto.SHA256, d[:], sig, opts); err != nil {
				t.Errorf("rsa.VerifyPSS: %v", err)
			}
		}},
		{"EdDSA", keys["ed25519"], func(t *testing.T, key any, input, sig []byte) {
			t.Helper()

			if !ed25519.Verify(key.(ed25519.PrivateKey).Public().(ed25519.PublicKey), input, sig) {
				t.Error("ed25519.Verify failed")
			}
		}},
		{"HS256", secret, func(t *testing.T, key any, input, sig []byte) {
			t.Helper()
			mac := hmac.New(sha256.New, key.([]byte))
			mac.Write(input)

			if !hmac.Equal(mac.Sum(nil), sig) {
				t.Error("HMAC does not match")
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.alg, func(t *testing.T) {
			jws, err := signJWS(jwsHeader{Alg: tt.alg, Typ: "test+jwt", KID: "k1"}, map[string]any{"sub": "me"}, tt.key)

			if err != nil {
				t.Fatalf("signJWS: %v", err)
			}

			header, payload, input, sig := splitJWS(t, jws)

			if diff := cmp.Diff(map[string]any{"alg": tt.alg, "typ": "test+jwt", "kid": "k1"}, header); diff != "" {
				t.Errorf("header (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff(map[string]any{"sub": "me"}, payload); diff != "" {
				t.Errorf("payload (-want +got):\n%s", diff)
			}

			tt.verify(t, tt.key, input, sig)
		})
	}
}

func TestSignJWSLeavesOutAnEmptyKID(t *testing.T) {
	jws, err := signJWS(jwsHeader{Alg: "HS256"}, map[string]any{}, []byte("secret"))

	if err != nil {
		t.Fatalf("signJWS: %v", err)
	}

	header, _, _, _ := splitJWS(t, jws)

	if diff := cmp.Diff(map[string]any{"alg": "HS256"}, header); diff != "" {
		t.Errorf("header (-want +got):\n%s", diff)
	}
}

func TestSignJWSRefusesAKeyThatCannotSignTheAlg(t *testing.T) {
	keys := testKeys(t)
	tests := []struct {
		alg string
		key any
	}{
		{"ES256", keys["p384"]},
		{"ES384", keys["p256"]},
		{"RS256", keys["p256"]},
		{"EdDSA", keys["rsa"]},
		{"HS256", keys["p256"]},
		{"ES256", []byte("secret")},
		{"none", keys["p256"]},
	}

	for _, tt := range tests {
		t.Run(tt.alg, func(t *testing.T) {
			if _, err := signJWS(jwsHeader{Alg: tt.alg}, map[string]any{}, tt.key); err == nil {
				t.Error("signJWS succeeded, want an error")
			}
		})
	}
}

// pemOf encodes key as PKCS#8 PEM.
func pemOf(t *testing.T, key crypto.Signer) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)

	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestParsePrivateKey(t *testing.T) {
	keys := testKeys(t)
	p224, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)

	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	small, err := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // G403: a key too small to accept, on purpose

	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	sec1, err := x509.MarshalECPrivateKey(keys["p256"].(*ecdsa.PrivateKey))

	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	tests := []struct {
		name    string
		pem     string
		natural string // empty when the key must be refused
	}{
		{"PKCS#8 P-256", pemOf(t, keys["p256"]), "ES256"},
		{"PKCS#8 P-384", pemOf(t, keys["p384"]), "ES384"},
		{"PKCS#8 RSA", pemOf(t, keys["rsa"]), "RS256"},
		{"PKCS#8 Ed25519", pemOf(t, keys["ed25519"]), "EdDSA"},
		{"SEC 1 P-256", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})), "ES256"},
		{"PKCS#1 RSA", string(pem.EncodeToMemory(&pem.Block{
			Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(keys["rsa"].(*rsa.PrivateKey)),
		})), "RS256"},
		{"P-224", pemOf(t, p224), ""},
		{"RSA 1024", pemOf(t, small), ""},
		{"not PEM", "not a key", ""},
		{"a public key", string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{1}})), ""},
		{"garbage in a PEM block", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("garbage")})), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := parsePrivateKey(tt.pem)

			switch {
			case tt.natural == "" && err == nil:
				t.Fatal("parsePrivateKey succeeded, want an error")
			case tt.natural != "" && err != nil:
				t.Fatalf("parsePrivateKey: %v", err)
			case tt.natural != "" && naturalAlg(key) != tt.natural:
				t.Errorf("natural alg = %s, want %s", naturalAlg(key), tt.natural)
			}
		})
	}
}

func TestPickAlg(t *testing.T) {
	keys := testKeys(t)
	tests := []struct {
		name    string
		key     crypto.Signer
		want    string
		offered []string
		got     string
	}{
		{"the configured alg wins", keys["rsa"], "PS256", []string{"RS256"}, "PS256"},
		{"the first offered the key can sign", keys["rsa"], "", []string{"ES256", "PS256", "RS256"}, "PS256"},
		{"the natural alg when none offered fits", keys["p384"], "", []string{"ES256", "RS256"}, "ES384"},
		{"the natural alg when none is offered", keys["ed25519"], "", nil, "EdDSA"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickAlg(tt.key, tt.want, tt.offered); got != tt.got {
				t.Errorf("pickAlg = %s, want %s", got, tt.got)
			}
		})
	}
}
