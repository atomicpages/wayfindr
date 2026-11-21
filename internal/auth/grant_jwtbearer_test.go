package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// workloadKey is the key a test identity provider signs workload tokens
// with.
func workloadKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return key
}

// workloadToken is a JWT for sub at aud, signed with ES256 by key, as a
// workload identity provider issues one.
func workloadToken(t *testing.T, key *ecdsa.PrivateKey, aud, sub string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)

		if err != nil {
			t.Fatalf("encode: %v", err)
		}

		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(map[string]string{"alg": "ES256", "typ": "JWT"}) + "." +
		enc(map[string]any{"iss": "https://idp.test", "sub": sub, "aud": aud, "exp": time.Now().Add(time.Hour).Unix()})
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

// newJWTBearerHarness is a broker harness whose connector "linear" uses the
// jwt_bearer grant as the public client wayfindr-test, with assertion,
// against a fake that trusts key.
func newJWTBearerHarness(t *testing.T, key *ecdsa.PrivateKey, assertion config.Assertion) *brokerHarness {
	t.Helper()
	h := newBrokerHarness(t, fakeas.WithJWTBearerKey(key.Public()))
	h.conn.Auth = &config.Auth{
		Type: config.AuthTypeOAuth, Grant: config.GrantJWTBearer, ClientID: testClientID,
		ValidateIssuer: true, ResourceIndicator: true, Assertion: &assertion,
	}

	return h
}

// assertionSubjects returns the sub of each jwt-bearer grant's assertion.
func assertionSubjects(as *fakeas.Server) []string {
	var out []string

	for _, tr := range as.TokenRequests() {
		if tr.GrantType == fakeas.GrantJWTBearer && tr.GrantAssertion != nil {
			out = append(out, tr.GrantAssertion.Sub)
		}
	}

	return out
}

// Scenario 6: the workload token is read from its file for every grant, so
// one Kubernetes rotated is used by the next grant.
func TestJWTBearerFileIsReadEveryGrant(t *testing.T) {
	key := workloadKey(t)
	file := filepath.Join(t.TempDir(), "token")
	h := newJWTBearerHarness(t, key, config.Assertion{File: file})
	write := func(sub string) {
		t.Helper()

		if err := os.WriteFile(file, []byte(workloadToken(t, key, h.as.Issuer, sub)+"\n"), 0o600); err != nil {
			t.Fatalf("write token: %v", err)
		}
	}

	write("pod-1")
	h.mustGet(t)
	write("pod-2")
	h.clock.Advance(2 * time.Hour)
	h.mustGet(t)

	if diff := cmp.Diff([]string{"pod-1", "pod-2"}, assertionSubjects(h.as)); diff != "" {
		t.Errorf("assertion subjects (-want +got):\n%s", diff)
	}

	if got := h.as.TokenRequests()[0]; got.AuthMethod != "none" || got.Assertion != nil {
		t.Errorf("client authentication = %s with %+v, want none: the client is public", got.AuthMethod, got.Assertion)
	}
}

func TestJWTBearerMissingAssertion(t *testing.T) {
	key := workloadKey(t)

	for _, tt := range []struct {
		name    string
		content *string
	}{
		{"missing", nil},
		{"empty", new("  \n")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "token")

			if tt.content != nil {
				if err := os.WriteFile(file, []byte(*tt.content), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}

			h := newJWTBearerHarness(t, key, config.Assertion{File: file})
			_, err := h.get(t)
			wantConfigError(t, err, auth.CaseUnresolvedValue)

			if got := len(h.as.TokenRequests()); got != 0 {
				t.Errorf("token requests = %d, want none without an assertion", got)
			}
		})
	}
}

// assertion.value is resolved for every grant, so a CI token rotated in
// the daemon's environment is used by the next grant.
func TestJWTBearerValueIsResolvedEveryGrant(t *testing.T) {
	key := workloadKey(t)
	h := newJWTBearerHarness(t, key, config.Assertion{Value: "${env:WORKLOAD_TOKEN}"})
	var mu sync.Mutex
	sub := "run-1"
	h.opts.Resolver = config.ResolverFunc(func(_ context.Context, ref config.Ref) (string, error) {
		mu.Lock()
		defer mu.Unlock()

		if ref.Kind != config.RefEnv || ref.Name != "WORKLOAD_TOKEN" {
			return "", config.ErrNotFound
		}

		return workloadToken(t, key, h.as.Issuer, sub), nil
	})
	h.broker = auth.NewBroker(h.opts)

	h.mustGet(t)
	mu.Lock()
	sub = "run-2"
	mu.Unlock()
	h.clock.Advance(2 * time.Hour)
	h.mustGet(t)

	if diff := cmp.Diff([]string{"run-1", "run-2"}, assertionSubjects(h.as)); diff != "" {
		t.Errorf("assertion subjects (-want +got):\n%s", diff)
	}
}

// assertion.subject: Wayfindr signs the assertion with the signing key,
// which is the assertion's: with no token_endpoint_auth_method the client
// sends no client assertion of its own.
func TestJWTBearerSelfSigned(t *testing.T) {
	key := workloadKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)

	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	h := newJWTBearerHarness(t, key, config.Assertion{Subject: "svc@example.com"})
	h.conn.Auth.SigningKey = &config.SigningKey{Key: config.Template(pemKey), KID: "k1", Audience: config.AudienceIssuer}
	h.clock.mu.Lock()
	h.clock.t = time.Now() // the fake checks exp against its own clock
	h.clock.mu.Unlock()

	h.mustGet(t)

	reqs := h.as.TokenRequests()

	if len(reqs) != 1 || reqs[0].GrantAssertion == nil {
		t.Fatalf("token requests = %+v, want one jwt-bearer grant", reqs)
	}

	a := *reqs[0].GrantAssertion
	want := fakeas.Assertion{
		Alg: "ES256", Typ: "JWT", KID: "k1", Iss: testClientID, Sub: "svc@example.com",
		Aud: []string{h.as.Issuer}, JTI: a.JTI, IAT: a.IAT, Exp: a.IAT + 60,
	}

	if diff := cmp.Diff(want, a); diff != "" || a.JTI == "" {
		t.Errorf("assertion (-want +got):\n%s", diff)
	}

	if reqs[0].AuthMethod != "none" || reqs[0].Assertion != nil {
		t.Errorf("client authentication = %s with %+v, want none: the signing key is the assertion's", reqs[0].AuthMethod, reqs[0].Assertion)
	}
}
