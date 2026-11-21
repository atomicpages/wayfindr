package config_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
)

// TestGrantUnattended: client_credentials and jwt_bearer have no user;
// the auth flows, and the zero value that means authorization_code, do.
func TestGrantUnattended(t *testing.T) {
	for _, tt := range []struct {
		grant config.Grant
		want  bool
	}{
		{config.GrantAuthorizationCode, false},
		{config.GrantDeviceCode, false},
		{config.GrantClientCredentials, true},
		{config.GrantJWTBearer, true},
		{"", false},
	} {
		if got := tt.grant.Unattended(); got != tt.want {
			t.Errorf("Grant(%q).Unattended() = %v, want %v", tt.grant, got, tt.want)
		}
	}
}

// TestParseAuthDefaults: an absent grant is authorization_code, an absent
// dpop is auto, and an absent signing_key audience is issuer.
func TestParseAuthDefaults(t *testing.T) {
	c := mustParse(t, `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", client_id = "id", signing_key = { key = "${secret:k}" } }

[connectors.y]
url = "https://y.example.com/mcp"
[connectors.y.auth]
type = "oauth"
grant = "client_credentials"
token_endpoint = "https://as.example/token"
client_id = "id"
dpop = "never"
signing_key = { key = "${secret:k}", audience = "token_endpoint" }`)

	for _, tt := range []struct {
		name string
		got  *config.Auth
		want *config.Auth
	}{
		{"defaults", c.Connectors["x"].Auth, &config.Auth{
			Type: "oauth", ClientID: "id", PKCE: true, ValidateIssuer: true, ResourceIndicator: true,
			Grant: config.GrantAuthorizationCode, DPoP: config.DPoPAuto,
			SigningKey: &config.SigningKey{Key: "${secret:k}", Audience: config.AudienceIssuer},
		}},
		{"written out", c.Connectors["y"].Auth, &config.Auth{ //nolint:gosec // G101: TokenEndpoint is a URL, not a credential
			Type: "oauth", ClientID: "id", PKCE: true, ValidateIssuer: true, ResourceIndicator: true,
			Grant: config.GrantClientCredentials, DPoP: config.DPoPNever,
			TokenEndpoint: "https://as.example/token",
			SigningKey:    &config.SigningKey{Key: "${secret:k}", Audience: config.AudienceTokenEndpoint},
		}},
	} {
		if diff := cmp.Diff(tt.want, tt.got); diff != "" {
			t.Errorf("%s: auth (-want +got):\n%s", tt.name, diff)
		}
	}
}

// TestParseAuthMessages pins the wording of the new auth rules: each
// refusal says what to do, and an unknown value lists the known ones.
func TestParseAuthMessages(t *testing.T) {
	for _, tt := range []struct {
		name, auth string
		want       []config.Issue
	}{
		{"unknown grant", `{ type = "oauth", grant = "password" }`, []config.Issue{{
			Path:    "connectors.x.auth.grant",
			Message: `must be "authorization_code", "device_code", "client_credentials", or "jwt_bearer", not "password"`,
		}}},
		{"unknown dpop", `{ type = "oauth", dpop = "on" }`, []config.Issue{{
			Path: "connectors.x.auth.dpop", Message: `must be "auto", "always", or "never", not "on"`,
		}}},
		{"unknown token_endpoint_auth_method", `{ type = "oauth", token_endpoint_auth_method = "jwt" }`, []config.Issue{{
			Path: "connectors.x.auth.token_endpoint_auth_method",
			Message: `must be "none", "client_secret_basic", "client_secret_post", "client_secret_jwt", ` +
				`"private_key_jwt", "tls_client_auth", or "self_signed_tls_client_auth", not "jwt"`,
		}}},
		{"tls method", `{ type = "oauth", client_id = "id", token_endpoint_auth_method = "tls_client_auth" }`, []config.Issue{{
			Path: "connectors.x.auth.token_endpoint_auth_method",
			Message: `token_endpoint_auth_method = "tls_client_auth" requires a client certificate: ` +
				`cert_file in the connector's tls table`,
		}}},
		{"private_key_jwt", `{ type = "oauth", client_id = "id", token_endpoint_auth_method = "private_key_jwt" }`, []config.Issue{{
			Path:    "connectors.x.auth.token_endpoint_auth_method",
			Message: `token_endpoint_auth_method = "private_key_jwt" requires signing_key`,
		}}},
		{"none", `{ type = "oauth", client_id = "id", client_secret = "${secret:s}", token_endpoint_auth_method = "none" }`, []config.Issue{{
			Path: "connectors.x.auth.client_secret", Message: `is not allowed with token_endpoint_auth_method = "none"`,
		}}},
		{"unattended", `{ type = "oauth", grant = "client_credentials", authorization_server = "https://as.example", pkce = true }`, []config.Issue{
			{Path: "connectors.x.auth.client_id", Message: `is required with grant = "client_credentials"`},
			{Path: "connectors.x.auth.pkce", Message: `is not allowed with grant = "client_credentials", which has no user`},
			{Path: "connectors.x.auth.grant", Message: `grant = "client_credentials" requires a client credential: client_secret or signing_key`},
		}},
		{"unattended without an authorization server", `{ type = "oauth", grant = "client_credentials", client_id = "id", client_secret = "${secret:s}" }`, []config.Issue{{
			Path: "connectors.x.auth.grant",
			Message: `grant = "client_credentials" requires authorization_server or token_endpoint, ` +
				`so its credential goes only to the server you name`,
		}}},
		{"device code", `{ type = "oauth", grant = "device_code", redirect_uri = "https://cb.example.com/cb" }`, []config.Issue{{
			Path:    "connectors.x.auth.redirect_uri",
			Message: `is not allowed with grant = "device_code", which has no authorization request or callback`,
		}}},
		{"jwt_bearer", `{ type = "oauth", grant = "jwt_bearer", authorization_server = "https://as.example", client_id = "id" }`, []config.Issue{{
			Path: "connectors.x.auth.assertion", Message: `is required with grant = "jwt_bearer"`,
		}}},
		{"assertion", `{ type = "oauth", grant = "jwt_bearer", authorization_server = "https://as.example", client_id = "id", assertion = { subject = "s", file = "/t" } }`, []config.Issue{
			{Path: "connectors.x.auth.assertion", Message: "exactly one of file, value, or subject is allowed"},
			{Path: "connectors.x.auth.assertion.subject", Message: "requires signing_key, which Wayfindr signs the assertion with"},
		}},
		{"assertion without jwt_bearer", `{ type = "oauth", assertion = { file = "/t" } }`, []config.Issue{{
			Path: "connectors.x.auth.assertion", Message: `is allowed only with grant = "jwt_bearer"`,
		}}},
		{"signing key", `{ type = "oauth", signing_key = { key = "${secret:k}", alg = "HS256", audience = "aud" } }`, []config.Issue{
			{Path: "connectors.x.auth.signing_key", Message: "requires client_id: a signing key belongs to a client you registered"},
			{Path: "connectors.x.auth.signing_key.alg", Message: `must be "ES256", "ES384", "RS256", "PS256", or "EdDSA", or absent, not "HS256"`},
			{Path: "connectors.x.auth.signing_key.audience", Message: `must be "issuer" or "token_endpoint", not "aud"`},
		}},
		{"dpop_jkt", `{ type = "oauth", authorize_params = { dpop_jkt = "x" } }`, []config.Issue{{
			Path: "connectors.x.auth.authorize_params.dpop_jkt", Message: "is set by Wayfindr; use dpop",
		}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte("[connectors.x]\nurl = \"https://x.example.com/mcp\"\nauth = "+tt.auth+"\n"), options())
			var verr *config.ValidationError

			if !errors.As(err, &verr) {
				t.Fatalf("Parse error = %v, want *config.ValidationError", err)
			}

			if diff := cmp.Diff(tt.want, verr.Issues); diff != "" {
				t.Errorf("issues (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAuthEqual: every key of an auth table counts, nested tables
// included, so a pending auth flow is cancelled by any change to it.
func TestAuthEqual(t *testing.T) {
	base := func() *config.Auth {
		return &config.Auth{
			Type: "oauth", ClientID: "id", Grant: config.GrantAuthorizationCode, DPoP: config.DPoPAuto,
			TokenEndpointAuthMethod:     config.AuthMethodPrivateKeyJWT,
			DeviceAuthorizationEndpoint: "https://as.example.com/device",
			SigningKey:                  &config.SigningKey{Key: "${secret:k}", KID: "k1", Alg: "ES256", Audience: config.AudienceIssuer},
			Assertion:                   &config.Assertion{File: "/t"},
		}
	}

	if !config.AuthEqual(base(), base()) {
		t.Error("AuthEqual of two equal tables = false")
	}

	for _, tt := range []struct {
		name   string
		change func(*config.Auth)
	}{
		{"grant", func(a *config.Auth) { a.Grant = config.GrantDeviceCode }},
		{"token_endpoint_auth_method", func(a *config.Auth) { a.TokenEndpointAuthMethod = config.AuthMethodNone }},
		{"dpop", func(a *config.Auth) { a.DPoP = config.DPoPAlways }},
		{"device_authorization_endpoint", func(a *config.Auth) { a.DeviceAuthorizationEndpoint = "" }},
		{"signing_key removed", func(a *config.Auth) { a.SigningKey = nil }},
		{"signing_key.key", func(a *config.Auth) { a.SigningKey.Key = "${secret:k2}" }},
		{"signing_key.kid", func(a *config.Auth) { a.SigningKey.KID = "k2" }},
		{"signing_key.alg", func(a *config.Auth) { a.SigningKey.Alg = "RS256" }},
		{"signing_key.audience", func(a *config.Auth) { a.SigningKey.Audience = config.AudienceTokenEndpoint }},
		{"assertion removed", func(a *config.Auth) { a.Assertion = nil }},
		{"assertion.file", func(a *config.Auth) { a.Assertion.File = "/u" }},
		{"assertion.value", func(a *config.Auth) { a.Assertion = &config.Assertion{Value: "${env:T}"} }},
		{"assertion.subject", func(a *config.Auth) { a.Assertion = &config.Assertion{Subject: "s"} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := base()
			tt.change(b)

			if config.AuthEqual(base(), b) {
				t.Errorf("AuthEqual = true after changing %s", tt.name)
			}
		})
	}
}
