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
	"strings"
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// dpopProof returns a DPoP proof of a method request to target signed by
// key, with the hash of access when it is set.
func dpopProof(t *testing.T, key *ecdsa.PrivateKey, method, target, access, jti string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)

		if err != nil {
			t.Fatalf("encode: %v", err)
		}

		return base64.RawURLEncoding.EncodeToString(b)
	}
	point, err := key.PublicKey.Bytes()

	if err != nil {
		t.Fatal(err)
	}

	jwk := map[string]string{
		"kty": "EC", "crv": "P-256",
		"x": base64.RawURLEncoding.EncodeToString(point[1:33]), "y": base64.RawURLEncoding.EncodeToString(point[33:]),
	}
	claims := map[string]any{"jti": jti, "htm": method, "htu": target, "iat": time.Now().Unix()}

	if access != "" {
		sum := sha256.Sum256([]byte(access))
		claims["ath"] = base64.RawURLEncoding.EncodeToString(sum[:])
	}

	input := enc(map[string]any{"typ": "dpop+jwt", "alg": "ES256", "jwk": jwk}) + "." + enc(claims)
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

// do sends a request with headers and returns the status, the headers,
// and the JSON body, if any.
func do(t *testing.T, method, target string, form url.Values, headers map[string]string) (int, http.Header, map[string]any) {
	t.Helper()
	var body *strings.Reader

	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}

	req, err := http.NewRequestWithContext(t.Context(), method, target, body)

	if err != nil {
		t.Fatal(err)
	}

	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)

	return resp.StatusCode, resp.Header, out
}

func TestDPoP(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithClient("svc", "s3cret"), fakeas.WithDPoP(false))
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cc := url.Values{"grant_type": {"client_credentials"}, "client_id": {"svc"}, "client_secret": {"s3cret"}}

	var meta, prm map[string]any

	_, _, meta = do(t, http.MethodGet, s.MetadataURL, nil, nil)
	_, _, prm = do(t, http.MethodGet, s.PRMURL, nil, nil)

	if algs, _ := meta["dpop_signing_alg_values_supported"].([]any); len(algs) != 1 || algs[0] != "ES256" ||
		prm["dpop_bound_access_tokens_required"] != true {
		t.Errorf("metadata algs = %v, PRM required = %v; want [ES256] and true", algs, prm["dpop_bound_access_tokens_required"])
	}

	status, _, body := do(t, http.MethodPost, s.TokenEndpoint, cc,
		map[string]string{"DPoP": dpopProof(t, key, http.MethodPost, s.TokenEndpoint, "", "j1")})
	at, _ := body["access_token"].(string)

	if status != http.StatusOK || body["token_type"] != "DPoP" {
		t.Fatalf("token request with a proof = %d %v, want 200 and token_type DPoP", status, body)
	}

	resource := func(scheme string, signer *ecdsa.PrivateKey, jti string) (int, string) {
		h := map[string]string{"Authorization": scheme + " " + at}

		if signer != nil {
			h["DPoP"] = dpopProof(t, signer, http.MethodGet, s.ResourceURL, at, jti)
		}

		status, header, _ := do(t, http.MethodGet, s.ResourceURL, nil, h)

		return status, header.Get("WWW-Authenticate")
	}

	if status, _ := resource("DPoP", key, "r1"); status != http.StatusOK {
		t.Errorf("bound token with a good proof = %d, want 200", status)
	}

	tests := []struct {
		name   string
		scheme string
		signer *ecdsa.PrivateKey
		jti    string
	}{
		{"as Bearer", "Bearer", nil, ""},
		{"with another key's proof", "DPoP", other, "r2"},
		{"without a proof", "DPoP", nil, ""},
		{"with a replayed proof", "DPoP", key, "r1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, challenge := resource(tt.scheme, tt.signer, tt.jti)

			if status != http.StatusUnauthorized || !strings.HasPrefix(challenge, "DPoP ") {
				t.Errorf("bound token %s = %d %q, want 401 with a DPoP challenge", tt.name, status, challenge)
			}
		})
	}

	// Without a proof the token is a bearer token, which this resource
	// does not take.
	status, _, body = do(t, http.MethodPost, s.TokenEndpoint, cc, nil)
	at, _ = body["access_token"].(string)

	if status != http.StatusOK || body["token_type"] != "Bearer" {
		t.Fatalf("token request without a proof = %d %v, want a bearer token", status, body)
	}

	if status, _ := resource("Bearer", nil, ""); status != http.StatusUnauthorized {
		t.Errorf("unbound token = %d, want 401: the resource requires bound tokens", status)
	}

	// A proof for another URL is refused.
	status, _, body = do(t, http.MethodPost, s.TokenEndpoint, cc,
		map[string]string{"DPoP": dpopProof(t, key, http.MethodPost, s.ResourceURL, "", "j2")})

	if status != http.StatusBadRequest || body["error"] != "invalid_dpop_proof" {
		t.Errorf("token request with a proof for another htu = %d %v, want 400 invalid_dpop_proof", status, body)
	}
}

func TestDPoPNonces(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithClient("svc", "s3cret"), fakeas.WithDPoP(true))
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cc := url.Values{"grant_type": {"client_credentials"}, "client_id": {"svc"}, "client_secret": {"s3cret"}}

	status, header, body := do(t, http.MethodPost, s.TokenEndpoint, cc,
		map[string]string{"DPoP": dpopProof(t, key, http.MethodPost, s.TokenEndpoint, "", "j1")})

	if status != http.StatusBadRequest || body["error"] != "use_dpop_nonce" || header.Get("DPoP-Nonce") != fakeas.ASNonce {
		t.Errorf("token request without a nonce = %d %v nonce %q, want 400 use_dpop_nonce with %q",
			status, body, header.Get("DPoP-Nonce"), fakeas.ASNonce)
	}

	reqs := s.TokenRequests()

	if p := reqs[len(reqs)-1].DPoP; p == nil || p.Error != "nonce missing or stale" {
		t.Errorf("recorded proof = %+v, want the nonce refusal", p)
	}
}

func TestDPoPAlgsAdvertised(t *testing.T) {
	tests := []struct {
		name string
		opts []fakeas.Option
		want string
	}{
		{"off", nil, "null"},
		{"WithDPoP", []fakeas.Option{fakeas.WithDPoP(false)}, `["ES256"]`},
		{"WithDPoPAlgs", []fakeas.Option{fakeas.WithDPoPAlgs("RS256")}, `["RS256"]`},
		{"WithDPoPAlgs none", []fakeas.Option{fakeas.WithDPoPAlgs()}, `[]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeas.Start(t, tt.opts...)
			_, _, meta := do(t, http.MethodGet, s.MetadataURL, nil, nil)
			got, _ := json.Marshal(meta["dpop_signing_alg_values_supported"])

			if string(got) != tt.want {
				t.Errorf("dpop_signing_alg_values_supported = %s, want %s", got, tt.want)
			}
		})
	}
}
