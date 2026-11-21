package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/atomicpages/wayfindr/internal/config"
)

// chooseClientAuth is a decision table (design §7.3, ADR 0018): the
// method a client authenticates with, from what is fixed for it, the
// credential it holds, and the authorization server's list. The table
// below is its clearest spec, so it is tested directly; the tests in
// clientauth_test.go cover its effect at the token request.

// methodOf names the token_endpoint_auth_method a clientAuth is.
func methodOf(a clientAuth) config.ClientAuthMethod {
	switch a.(type) {
	case noClientAuth:
		return config.AuthMethodNone
	case secretBasic:
		return config.AuthMethodClientSecretBasic
	case secretPost:
		return config.AuthMethodClientSecretPost
	case secretJWT:
		return config.AuthMethodClientSecretJWT
	case privateKeyJWT:
		return config.AuthMethodPrivateKeyJWT
	case tlsClient:
		return config.AuthMethodTLSClientAuth
	}

	return "?"
}

func TestChooseClientAuth(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	key := &SigningKey{Signer: ec, Audience: config.AudienceIssuer}
	const (
		none   = config.AuthMethodNone
		basic  = config.AuthMethodClientSecretBasic
		post   = config.AuthMethodClientSecretPost
		secJWT = config.AuthMethodClientSecretJWT
		pkJWT  = config.AuthMethodPrivateKeyJWT
		tls    = config.AuthMethodTLSClientAuth
	)
	// absent is metadata without token_endpoint_auth_methods_supported;
	// noMetadata is no metadata document at all.
	var absent, noMetadata []string
	both := []string{"client_secret_basic", "client_secret_post"}

	type cred int
	const (
		public cred = iota
		secret
		signing
		secretAndKey
	)

	tests := []struct {
		name   string
		listed []string
		nometa bool
		held   cred
		fixed  config.ClientAuthMethod
		want   config.ClientAuthMethod // empty: auth_method_unsupported
	}{
		// Today's behaviour: both secret methods, or nothing listed.
		{name: "secret, both listed", listed: both, held: secret, want: basic},
		{name: "secret, field absent", listed: absent, held: secret, want: basic},
		{name: "secret, no metadata", nometa: true, listed: noMetadata, held: secret, want: basic},
		{name: "public, both listed", listed: both, held: public, want: none},
		{name: "public, field absent", listed: absent, held: public, want: none},
		{name: "public, no metadata", nometa: true, held: public, want: none},
		// A secret, filtered by the list.
		{name: "secret, post only", listed: []string{"client_secret_post"}, held: secret, want: post},
		{name: "secret, jwt only", listed: []string{"client_secret_jwt"}, held: secret, want: secJWT},
		{name: "secret, post and jwt", listed: []string{"client_secret_jwt", "client_secret_post"}, held: secret, want: post},
		{name: "secret, only private_key_jwt listed", listed: []string{"private_key_jwt"}, held: secret},
		{name: "secret, only none listed", listed: []string{"none"}, held: secret},
		// A signing key.
		{name: "key, listed", listed: []string{"client_secret_basic", "private_key_jwt"}, held: signing, want: pkJWT},
		{name: "key, field absent", listed: absent, held: signing},
		{name: "key, no metadata", nometa: true, held: signing, want: pkJWT},
		{name: "key, not listed", listed: both, held: signing},
		{name: "key and secret, key listed", listed: []string{"client_secret_basic", "private_key_jwt"}, held: secretAndKey, want: pkJWT},
		{name: "key and secret, key not listed", listed: both, held: secretAndKey, want: basic},
		// A public client sends its client_id whatever the list says.
		{name: "public, none not listed", listed: []string{"private_key_jwt"}, held: public, want: none},
		// A fixed method (explicit, or registered) wins over the list.
		{name: "fixed post, basic only listed", listed: []string{"client_secret_basic"}, held: secret, fixed: post, want: post},
		{name: "fixed basic, post only listed", listed: []string{"client_secret_post"}, held: secret, fixed: basic, want: basic},
		{name: "fixed jwt", listed: both, held: secret, fixed: secJWT, want: secJWT},
		{name: "fixed private_key_jwt", listed: both, held: signing, fixed: pkJWT, want: pkJWT},
		{name: "fixed none with a secret", listed: both, held: secret, fixed: none, want: none},
		{name: "fixed basic without a secret", listed: both, held: public, fixed: basic},
		{name: "fixed private_key_jwt without a key", listed: both, held: secret, fixed: pkJWT},
		// The tls methods are used when fixed, and never chosen otherwise.
		{name: "fixed tls", listed: both, held: public, fixed: tls, want: tls},
		{name: "tls only listed, public", listed: []string{"tls_client_auth"}, held: public, want: none},
		{name: "tls only listed, secret", listed: []string{"tls_client_auth"}, held: secret},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := Registration{ClientID: "c", AuthMethod: tt.fixed}

			if tt.held == secret || tt.held == secretAndKey {
				reg.ClientSecret = "s"
			}

			if tt.held == signing || tt.held == secretAndKey {
				reg.SigningKey = key
			}

			disc := Discovery{Issuer: "https://as.example", Endpoints: Endpoints{Token: "https://as.example/token"}} //nolint:gosec // G101: a URL, not a credential

			if !tt.nometa {
				disc.Metadata = &oauthex.AuthServerMeta{Issuer: "https://as.example", TokenEndpointAuthMethodsSupported: tt.listed}
			}

			got, err := chooseClientAuth(reg, disc)

			switch {
			case tt.want == "" && err == nil:
				t.Fatalf("chooseClientAuth = %s, want no fit", methodOf(got))
			case tt.want != "" && err != nil:
				t.Fatalf("chooseClientAuth: %v, want %s", err, tt.want)
			case tt.want != "" && methodOf(got) != tt.want:
				t.Errorf("chooseClientAuth = %s, want %s", methodOf(got), tt.want)
			}
		})
	}
}
