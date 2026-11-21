package auth_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// The tests in this file are about client authentication (GLOSSARY: Client
// authentication) at the token request's seam: the method each request
// uses, as the fake authorization server saw it, and the client assertion
// it carried.

// withSecrets rebuilds the harness's broker with a resolver that knows
// values as ${secret:…} references, and sets its clock to the wall clock,
// which the fake checks an assertion's exp against.
func (h *brokerHarness) withSecrets(values map[string]string) {
	h.opts.Resolver = config.ResolverFunc(func(_ context.Context, ref config.Ref) (string, error) {
		if v, ok := values[ref.Name]; ok && ref.Kind == config.RefSecret {
			return v, nil
		}

		return "", fmt.Errorf("%s: %w", ref, config.ErrNotFound)
	})
	h.clock.Advance(time.Since(h.clock.Now()))
	h.broker = auth.NewBroker(h.opts)
}

// tokenRequests are the fake's token requests by grant type and method.
func (h *brokerHarness) tokenRequests() [][3]string {
	var out [][3]string

	for _, tr := range h.as.TokenRequests() {
		out = append(out, [3]string{tr.GrantType, tr.AuthMethod, tr.Error})
	}

	return out
}

// newKey returns a fresh key of the given kind and its PKCS#8 PEM.
func newKey(t *testing.T, kind string) (crypto.Signer, string) {
	t.Helper()
	var (
		key crypto.Signer
		err error
	)

	switch kind {
	case "rsa":
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	default:
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}

	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)

	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// TestDCRClientUsesTheMethodItWasRegisteredWith is the regression for a
// DCR client registered for client_secret_post at a server that lists
// both secret methods: Wayfindr sent basic, got invalid_client, forgot the
// client, and registered again on every flow.
func TestDCRClientUsesTheMethodItWasRegisteredWith(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithTokenAuthMethods("client_secret_basic", "client_secret_post"),
		fakeas.WithDCRAuthMethod("client_secret_post"), fakeas.WithStrictClientAuth())
	h.conn.Auth.ClientID = ""
	h.authorizedThroughFlow(t)
	h.as.ExpireAccessTokens()

	// The refresh reads the client back from the credentials row.
	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Fatalf("resource through the broker = %d, %v; want 200 after a refresh", status, err)
	}

	want := [][3]string{
		{"authorization_code", "client_secret_post", ""},
		{"refresh_token", "client_secret_post", ""},
	}

	if diff := cmp.Diff(want, h.tokenRequests()); diff != "" {
		t.Errorf("token requests (-want +got):\n%s", diff)
	}

	if n := len(h.as.Clients()); n != 2 {
		t.Errorf("clients = %d, want the pre-registered one and one DCR client", n)
	}
}

func TestPrivateKeyJWT(t *testing.T) {
	tests := []struct {
		name     string
		audience config.Audience
		opts     []fakeas.Option
		// endpoints sets the endpoints in config, with no issuer anywhere.
		endpoints bool
		wantAud   func(as *fakeas.Server) string
	}{
		{name: "aud is the issuer", audience: config.AudienceIssuer,
			opts:    []fakeas.Option{fakeas.WithTokenAuthMethods("private_key_jwt")},
			wantAud: func(as *fakeas.Server) string { return as.Issuer }},
		{name: "aud is the token endpoint when configured", audience: config.AudienceTokenEndpoint,
			opts:    []fakeas.Option{fakeas.WithTokenAuthMethods("private_key_jwt")},
			wantAud: func(as *fakeas.Server) string { return as.TokenEndpoint }},
		{name: "aud is the token endpoint with no issuer", audience: config.AudienceIssuer, endpoints: true,
			opts:    []fakeas.Option{fakeas.WithoutMetadata()},
			wantAud: func(as *fakeas.Server) string { return as.TokenEndpoint }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, keyPEM := newKey(t, "ec")
			h := newBrokerHarness(t, append(tt.opts, fakeas.WithClientKey(testClientID, key.Public()))...)
			h.withSecrets(map[string]string{"linear_signing_key": keyPEM})
			h.conn.Auth.SigningKey = &config.SigningKey{Key: "${secret:linear_signing_key}", KID: "k1", Audience: tt.audience}

			if tt.endpoints {
				h.conn.Auth.AuthorizationEndpoint = h.as.AuthorizationEndpoint
				h.conn.Auth.TokenEndpoint = h.as.TokenEndpoint
			}

			h.authorizedThroughFlow(t)
			h.as.ExpireAccessTokens()

			if status, err := h.get(t); err != nil || status != http.StatusOK {
				t.Fatalf("resource through the broker = %d, %v; want 200 after a refresh", status, err)
			}

			reqs := h.as.TokenRequests()
			want := [][3]string{{"authorization_code", "private_key_jwt", ""}, {"refresh_token", "private_key_jwt", ""}}

			if diff := cmp.Diff(want, h.tokenRequests()); diff != "" {
				t.Fatalf("token requests (-want +got):\n%s", diff)
			}

			now := h.clock.Now().Unix()
			wantAssertion := &fakeas.Assertion{
				Alg: "ES256", Typ: "client-authentication+jwt", KID: "k1",
				Iss: testClientID, Sub: testClientID, Aud: []string{tt.wantAud(h.as)}, IAT: now, Exp: now + 60,
			}

			if diff := cmp.Diff(wantAssertion, reqs[0].Assertion, cmpopts.IgnoreFields(fakeas.Assertion{}, "JTI")); diff != "" {
				t.Errorf("client assertion (-want +got):\n%s", diff)
			}

			first, second := reqs[0].Assertion.JTI, reqs[1].Assertion.JTI

			if len(first) < 22 || first == second {
				t.Errorf("jti = %q then %q, want 16 random bytes each time", first, second)
			}
		})
	}
}

func TestClientSecretJWT(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithClient("conf", "s3cret", defaultRedirect))
	h.withSecrets(map[string]string{"conf_secret": "s3cret"})
	h.conn.Auth.ClientID = "conf"
	h.conn.Auth.ClientSecret = "${secret:conf_secret}"
	h.conn.Auth.TokenEndpointAuthMethod = config.AuthMethodClientSecretJWT
	h.authorizedThroughFlow(t)

	reqs := h.as.TokenRequests()

	if diff := cmp.Diff([][3]string{{"authorization_code", "client_secret_jwt", ""}}, h.tokenRequests()); diff != "" {
		t.Fatalf("token requests (-want +got):\n%s", diff)
	}

	now := h.clock.Now().Unix()
	want := &fakeas.Assertion{
		Alg: "HS256", Typ: "client-authentication+jwt",
		Iss: "conf", Sub: "conf", Aud: []string{h.as.Issuer}, IAT: now, Exp: now + 60,
	}

	if diff := cmp.Diff(want, reqs[0].Assertion, cmpopts.IgnoreFields(fakeas.Assertion{}, "JTI")); diff != "" {
		t.Errorf("client assertion (-want +got):\n%s", diff)
	}
}

func TestPrivateKeyJWTAlg(t *testing.T) {
	key, keyPEM := newKey(t, "rsa")
	tests := []struct {
		name string
		algs []string
		alg  string
		want string
	}{
		{name: "the first of the server's algs the key can sign", algs: []string{"ES256", "PS256", "RS256"}, want: "PS256"},
		{name: "the configured alg", algs: []string{"ES256", "PS256"}, alg: "RS256", want: "RS256"},
		{name: "the key's own alg when the server lists none", want: "RS256"},
		{name: "the key's own alg when it can sign none listed", algs: []string{"ES256", "EdDSA"}, want: "RS256"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newBrokerHarness(t, fakeas.WithTokenAuthMethods("private_key_jwt"), fakeas.WithSigningAlgs(tt.algs...),
				fakeas.WithClientKey(testClientID, key.Public()))
			h.withSecrets(map[string]string{"k": keyPEM})
			h.conn.Auth.SigningKey = &config.SigningKey{Key: "${secret:k}", Alg: tt.alg, Audience: config.AudienceIssuer}
			h.authorizedThroughFlow(t)

			reqs := h.as.TokenRequests()

			if len(reqs) != 1 || reqs[0].Error != "" || reqs[0].Assertion == nil {
				t.Fatalf("token requests = %+v, want one that succeeded with an assertion", reqs)
			}

			if got := reqs[0].Assertion.Alg; got != tt.want {
				t.Errorf("alg = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNoFittingClientAuthMethodFailsBeforeSending(t *testing.T) {
	newHarness := func(t *testing.T) *brokerHarness {
		t.Helper()
		h := newBrokerHarness(t, fakeas.WithClient("conf", "s3cret", defaultRedirect),
			fakeas.WithTokenAuthMethods("private_key_jwt", "tls_client_auth"))
		h.withSecrets(map[string]string{"conf_secret": "s3cret"})
		h.conn.Auth.ClientID = "conf"
		h.conn.Auth.ClientSecret = "${secret:conf_secret}"

		return h
	}

	t.Run("an auth flow does not start", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.flows().Start(t.Context(), h.conn.Name, auth.StartOptions{})

		if e, ok := auth.AsError(err); !ok || e.Kind != auth.KindConfig || e.Case != auth.CaseAuthMethodUnsupported {
			t.Fatalf("StartFlow = %v, want config auth_method_unsupported", err)
		}

		if n, m := len(h.as.Authorizations()), len(h.as.TokenRequests()); n+m != 0 {
			t.Errorf("the server saw %d authorize and %d token requests, want none", n, m)
		}
	})
	t.Run("a refresh is not sent", func(t *testing.T) {
		h := newHarness(t)
		access, refresh := h.authorizeAs(t, auth.Registration{ClientID: "conf", ClientSecret: "s3cret"}, "")

		if err := auth.SeedGrant(t.Context(), h.broker, "linear", h.as.ResourceURL, h.as.TokenEndpoint, "conf", access, refresh,
			h.clock.Now().Add(-time.Minute), ""); err != nil {
			t.Fatalf("seed: %v", err)
		}

		before := len(h.as.TokenRequests())
		_, err := h.get(t)

		if got := caseOf(err); got != auth.CaseAuthMethodUnsupported {
			t.Fatalf("resource through the broker: %v, want auth_method_unsupported", err)
		}

		if n := len(h.as.TokenRequests()) - before; n != 0 {
			t.Errorf("the server saw %d token requests, want none", n)
		}
	})
}

func TestSigningKeyInvalid(t *testing.T) {
	_, keyPEM := newKey(t, "ec")
	tests := []struct {
		name string
		pem  string
		alg  string
	}{
		{name: "not a PEM private key", pem: "-----BEGIN NONSENSE-----\nAAAA\n-----END NONSENSE-----\n"},
		{name: "a key that cannot sign the configured alg", pem: keyPEM, alg: "RS256"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newBrokerHarness(t, fakeas.WithTokenAuthMethods("private_key_jwt"))
			h.withSecrets(map[string]string{"k": tt.pem})
			h.conn.Auth.SigningKey = &config.SigningKey{Key: "${secret:k}", Alg: tt.alg, Audience: config.AudienceIssuer}
			_, err := h.flows().Start(t.Context(), h.conn.Name, auth.StartOptions{})

			if e, ok := auth.AsError(err); !ok || e.Kind != auth.KindConfig || e.Case != auth.CaseSigningKeyInvalid {
				t.Fatalf("StartFlow = %v, want config signing_key_invalid", err)
			}

			if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), "NONSENSE") {
				t.Errorf("error %q quotes the key", err)
			}

			if n := len(h.as.Authorizations()); n != 0 {
				t.Errorf("the server saw %d authorize requests, want none", n)
			}
		})
	}
}

func TestDCRRequestNamesTheMethodAndApplicationType(t *testing.T) {
	tests := []struct {
		name       string
		listed     []string
		configured config.ClientAuthMethod
		redirect   string
		method     config.ClientAuthMethod
		appType    string
	}{
		{name: "none when listed", listed: []string{"client_secret_basic", "none"}, redirect: defaultRedirect,
			method: config.AuthMethodNone, appType: "native"},
		{name: "basic when listed", listed: []string{"client_secret_post", "client_secret_basic"}, redirect: defaultRedirect,
			method: config.AuthMethodClientSecretBasic, appType: "native"},
		{name: "basic when nothing is listed", redirect: defaultRedirect,
			method: config.AuthMethodClientSecretBasic, appType: "native"},
		{name: "post otherwise", listed: []string{"client_secret_post", "private_key_jwt"}, redirect: defaultRedirect,
			method: config.AuthMethodClientSecretPost, appType: "native"},
		{name: "web for a redirect that is not loopback", listed: []string{"client_secret_basic"}, redirect: ngrokRedirect,
			method: config.AuthMethodClientSecretBasic, appType: "web"},
		// The method the client will authenticate with is the one to
		// register: asking for basic here would earn invalid_client on
		// every token request, and a new registration after each.
		{name: "the configured method wins", configured: config.AuthMethodNone, redirect: defaultRedirect,
			method: config.AuthMethodNone, appType: "native"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRegHarness(t, nil, fakeas.WithTokenAuthMethods(tt.listed...))
			a := defaultAuth()
			a.TokenEndpointAuthMethod = tt.configured
			got, err := h.register(t, a, tt.redirect)

			if err != nil {
				t.Fatalf("Register: %v", err)
			}

			clients := h.as.Clients()

			if len(clients) != 1 {
				t.Fatalf("fakeas has %d registered clients, want 1", len(clients))
			}

			c := clients[0]

			if c.AuthMethod != string(tt.method) || c.ApplicationType != tt.appType {
				t.Errorf("DCR request token_endpoint_auth_method %q, application_type %q; want %q, %q",
					c.AuthMethod, c.ApplicationType, tt.method, tt.appType)
			}

			want := auth.Registration{ClientID: c.ID, ClientSecret: c.Secret, Method: auth.MethodDCR, AuthMethod: tt.method}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("Register (-want +got):\n%s", diff)
			}

			if (got.ClientSecret == "") != (tt.method == config.AuthMethodNone) {
				t.Errorf("client secret %q for method %s, want one only for a confidential client", got.ClientSecret, tt.method)
			}
		})
	}
}

// TestDCRResponseWithoutAMethodRegisteredTheOneAskedFor: a server that
// leaves token_endpoint_auth_method out of its answer is taken to have
// registered the method Wayfindr asked for, which the stored client keeps,
// even when it issued a secret anyway (the MCP conformance suite's does).
func TestDCRResponseWithoutAMethodRegisteredTheOneAskedFor(t *testing.T) {
	h := newRegHarness(t, nil, fakeas.WithTokenAuthMethods("none"), fakeas.WithTerseDCR())

	for _, method := range []auth.Method{auth.MethodDCR, auth.MethodStoredDCR} {
		got, err := h.register(t, defaultAuth(), defaultRedirect)

		if err != nil {
			t.Fatalf("Register: %v", err)
		}

		c := h.as.Clients()[0]
		want := auth.Registration{ClientID: c.ID, ClientSecret: c.Secret, Method: method, AuthMethod: config.AuthMethodNone}

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Register (-want +got):\n%s", diff)
		}
	}
}
