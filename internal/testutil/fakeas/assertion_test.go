package fakeas_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// hs256 returns a compact JWS of claims signed with HS256 over secret.
func hs256(t *testing.T, secret string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)

		if err != nil {
			t.Fatalf("encode: %v", err)
		}

		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(map[string]string{"alg": "HS256", "typ": "client-authentication+jwt"}) + "." + enc(claims)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(input))

	return input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// TestClientAssertions covers the checks on a client_secret_jwt or
// private_key_jwt assertion. A bogus code tells the two failures apart: a
// request whose client authenticated fails invalid_grant, and one whose
// client did not fails invalid_client.
func TestClientAssertions(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithClient("conf", "s3cret", redirectURI))
	c := newClient(t)
	now := time.Now().Unix()
	claims := func(jti string, edit func(map[string]any)) map[string]any {
		m := map[string]any{"iss": "conf", "sub": "conf", "aud": s.Issuer, "jti": jti, "iat": now, "exp": now + 60}

		if edit != nil {
			edit(m)
		}

		return m
	}
	post := func(assertion string) string {
		t.Helper()
		_, body := c.postToken(s, url.Values{
			"grant_type": {"authorization_code"}, "code": {"bogus"}, "redirect_uri": {redirectURI},
			"client_id":             {"conf"},
			"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
			"client_assertion":      {assertion},
		})
		code, _ := body["error"].(string)

		return code
	}

	tests := []struct {
		name      string
		assertion string
		want      string
	}{
		{"a good assertion", hs256(t, "s3cret", claims("j1", nil)), "invalid_grant"},
		{"a replayed jti", hs256(t, "s3cret", claims("j1", nil)), "invalid_client"},
		{"aud the token endpoint", hs256(t, "s3cret", claims("j2", func(m map[string]any) { m["aud"] = s.TokenEndpoint })), "invalid_grant"},
		{"aud an array", hs256(t, "s3cret", claims("j3", func(m map[string]any) { m["aud"] = []string{"x", s.Issuer} })), "invalid_grant"},
		{"another aud", hs256(t, "s3cret", claims("j4", func(m map[string]any) { m["aud"] = "https://elsewhere" })), "invalid_client"},
		{"expired", hs256(t, "s3cret", claims("j5", func(m map[string]any) { m["exp"] = now - 1 })), "invalid_client"},
		{"another sub", hs256(t, "s3cret", claims("j6", func(m map[string]any) { m["sub"] = "other" })), "invalid_client"},
		{"the wrong secret", hs256(t, "wrong", claims("j7", nil)), "invalid_client"},
		{"no jti", hs256(t, "s3cret", claims("", nil)), "invalid_client"},
		{"not a JWS", "not.a.jws", "invalid_client"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := post(tt.assertion); got != tt.want {
				t.Errorf("token request error = %q, want %q", got, tt.want)
			}
		})
	}

	reqs := s.TokenRequests()

	if got := reqs[0]; got.AuthMethod != "client_secret_jwt" || got.Assertion == nil || got.Assertion.JTI != "j1" {
		t.Errorf("first token request = %+v, want client_secret_jwt with its assertion", got)
	}
}

// TestPrivateKeyJWTNeedsTheRegisteredKey: a private_key_jwt assertion is
// checked against the key WithClientKey registered.
func TestPrivateKeyJWTNeedsTheRegisteredKey(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	s := fakeas.Start(t, fakeas.WithClient("svc", "", redirectURI), fakeas.WithClientKey("svc", key.Public()))
	c := newClient(t)
	now := time.Now().Unix()
	enc := func(v any) string {
		b, _ := json.Marshal(v)

		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(map[string]string{"alg": "ES256"}) + "." +
		enc(map[string]any{"iss": "svc", "sub": "svc", "aud": s.Issuer, "jti": "k1", "exp": now + 60})
	digest := sha256.Sum256([]byte(input))
	r, ss, err := ecdsa.Sign(rand.Reader, key, digest[:])

	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	good := input + "." + base64.RawURLEncoding.EncodeToString(sig)
	sig[0] ^= 0xff
	bad := input + "." + base64.RawURLEncoding.EncodeToString(sig)

	for _, tt := range []struct{ name, assertion, want string }{
		{"a forged signature", bad, "invalid_client"},
		{"the key's signature", good, "invalid_grant"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, body := c.postToken(s, url.Values{
				"grant_type": {"authorization_code"}, "code": {"bogus"}, "redirect_uri": {redirectURI},
				"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
				"client_assertion":      {tt.assertion},
			})

			if got, _ := body["error"].(string); got != tt.want {
				t.Errorf("token request error = %q, want %q", got, tt.want)
			}
		})
	}
}
