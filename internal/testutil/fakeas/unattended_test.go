package fakeas_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// es256 returns a compact JWS of claims signed with ES256 by key.
func es256(t *testing.T, key *ecdsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)

		if err != nil {
			t.Fatalf("encode: %v", err)
		}

		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(map[string]string{"alg": "ES256", "typ": "JWT"}) + "." + enc(claims)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])

	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestClientCredentialsGrant(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithClient("svc", "s3cret"), fakeas.WithClient("pub", ""),
		fakeas.WithWithheldScopes("admin"))
	c := newClient(t)

	status, body := c.postToken(s, url.Values{
		"grant_type": {"client_credentials"}, "client_id": {"svc"}, "client_secret": {"s3cret"},
		"scope": {"read admin"},
	})

	if status != http.StatusOK || body["scope"] != "read" || body["refresh_token"] != nil {
		t.Errorf("confidential client = %d %v, want 200 with scope read (admin withheld) and no refresh token", status, body)
	}

	at, _ := body["access_token"].(string)

	if got := c.get(s.ResourceURL, at).StatusCode; got != http.StatusOK {
		t.Errorf("resource with the token = %d, want 200", got)
	}

	s.RefuseAllTokens(true)

	if got := c.get(s.ResourceURL, at).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("resource with every token refused = %d, want 401", got)
	}

	_, body = c.postToken(s, url.Values{"grant_type": {"client_credentials"}, "client_id": {"pub"}})

	if body["error"] != "unauthorized_client" {
		t.Errorf("public client = %v, want unauthorized_client", body)
	}
}

func TestJWTBearerGrant(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	s := fakeas.Start(t, fakeas.WithClient("wl", ""), fakeas.WithJWTBearerKey(key.Public()))
	c := newClient(t)
	now := time.Now().Unix()
	claims := func(aud string, exp int64) map[string]any {
		return map[string]any{"iss": "https://idp", "sub": "workload", "aud": aud, "exp": exp}
	}

	for _, tt := range []struct {
		name      string
		assertion string
		want      string
	}{
		{"a good assertion", es256(t, key, claims(s.Issuer, now+60)), ""},
		{"aud the token endpoint", es256(t, key, claims(s.TokenEndpoint, now+60)), ""},
		{"another key", es256(t, other, claims(s.Issuer, now+60)), "invalid_grant"},
		{"another aud", es256(t, key, claims("https://elsewhere", now+60)), "invalid_grant"},
		{"expired", es256(t, key, claims(s.Issuer, now-1)), "invalid_grant"},
		{"not a JWT", "opaque", "invalid_grant"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, body := c.postToken(s, url.Values{
				"grant_type": {fakeas.GrantJWTBearer}, "client_id": {"wl"}, "assertion": {tt.assertion},
			})

			if got, _ := body["error"].(string); got != tt.want {
				t.Errorf("token request error = %q, want %q", got, tt.want)
			}
		})
	}

	if got := s.TokenRequests()[0]; got.GrantAssertion == nil || got.GrantAssertion.Sub != "workload" {
		t.Errorf("first token request = %+v, want the assertion's claims recorded", got)
	}

	plain := fakeas.Start(t, fakeas.WithClient("wl", ""))
	_, body := c.postToken(plain, url.Values{
		"grant_type": {fakeas.GrantJWTBearer}, "client_id": {"wl"}, "assertion": {es256(t, key, claims(plain.Issuer, now+60))},
	})

	if body["error"] != "unsupported_grant_type" {
		t.Errorf("without WithJWTBearerKey = %v, want unsupported_grant_type", body)
	}
}
