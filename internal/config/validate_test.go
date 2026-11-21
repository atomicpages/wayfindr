package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
)

// TestParseValid covers the option snippets prd.md and design §4 mention
// outside the main example; each must parse.
func TestParseValid(t *testing.T) {
	tests := []struct {
		name string
		toml string
	}{
		{"prd: transport sse forces legacy SSE", `
[connectors.old]
url = "https://old.example.com/sse"
transport = "sse"`},
		{"prd: commented-out slack options enabled", `
[connectors.slack]
url = "https://mcp.slack.com/mcp"
[connectors.slack.auth]
type          = "oauth"
client_id     = "1234.5678"
client_secret = "${secret:slack_client_secret}"
scopes        = ["channels:read", "chat:write"]
redirect_uri = "https://abc.ngrok.app/oauth/callback"
validate_issuer = false`},
		{"prd: pkce = false with client_secret", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", client_id = "id", client_secret = "${secret:x}", pkce = false }`},
		{"design §4: resource_indicator = false on manual connector", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", client_id = "id", resource_indicator = false }`},
		{"authorize_params with provider parameters", `
[connectors.gmail]
url = "https://gmailmcp.googleapis.com/mcp/v1"
[connectors.gmail.auth]
type = "oauth"
client_id = "id"
[connectors.gmail.auth.authorize_params]
access_type = "offline"
prompt = "consent"`},
		{"design §4: explicit endpoints set together", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.auth]
type = "oauth"
client_id = "id"
authorization_endpoint = "https://as.example.com/authorize"
token_endpoint = "https://as.example.com/token"`},
		{"design §4: full embedding and storage tables", `
[server]
listen = "localhost:7000"
[storage]
driver = "postgres"
dsn = "postgres://localhost/wayfindr"
[embedding]
base_url = "https://api.example.com/v1"
model = "m"
dimensions = 8
api_key = "${env:EMBED_KEY}"
timeout = "500ms"`},
		{"design §4: connector timeout and IPv6 loopback", `
[server]
listen = "[::1]:7777"
[connectors.a-b_1]
command = "srv"
timeout = "30s"`},
		{"template inside a url", `
[connectors.x]
url = "https://${env:HOST}/mcp"`},
		{"tools list with classes, globs, and exclusions", `
[connectors.x]
url = "https://x.example.com/mcp"
tools = ["@read", "@write", "create_draft", "list_*", "!delete_*"]`},
		{"tls: client certificate with a key file and a private CA", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.tls]
cert_file = "~/corp/client.pem"
key_file = "~/corp/client.key"
ca_file = "~/corp/ca.pem"`},
		{"tls: client certificate with its key in a secret", `
[connectors.x]
url = "https://x.example.com/mcp"
tls = { cert_file = "/etc/corp/client.pem", key = "${secret:corp_key}" }`},
		{"tls: a private CA alone", `
[connectors.x]
url = "https://x.example.com/mcp"
tls = { ca_file = "/etc/corp/ca.pem" }`},
		{"grant: authorization_code written out, with every concept", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.auth]
type = "oauth"
grant = "authorization_code"
client_id = "svc"
token_endpoint_auth_method = "private_key_jwt"
dpop = "always"
device_authorization_endpoint = "https://as.example.com/device"
[connectors.x.auth.signing_key]
key = "${secret:svc_key}"
kid = "k1"
alg = "ES256"
audience = "token_endpoint"`},
		{"grant: device_code with a token_endpoint alone", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.auth]
type = "oauth"
grant = "device_code"
token_endpoint = "https://as.example.com/token"
device_authorization_endpoint = "http://127.0.0.1:9000/device"
authorize_params = { audience = "api" }`},
		{"grant: client_credentials with a client secret and a token_endpoint alone", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.auth]
type = "oauth"
grant = "client_credentials"
authorization_server = "https://as.example"
client_id = "svc"
client_secret = "${secret:svc}"
token_endpoint_auth_method = "client_secret_post"
token_endpoint = "https://as.example.com/token"
scopes = ["read"]
dpop = "never"`},
		{"grant: client_credentials with a signing key", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.auth]
type = "oauth"
grant = "client_credentials"
authorization_server = "https://as.example"
client_id = "svc"
[connectors.x.auth.signing_key]
key = "${secret:svc_key}"`},
		{"grant: jwt_bearer with a workload token file", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.auth]
type = "oauth"
grant = "jwt_bearer"
authorization_server = "https://as.example"
client_id = "svc"
assertion = { file = "/var/run/secrets/tokens/mcp" }`},
		{"grant: jwt_bearer with a value from the environment", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.auth]
type = "oauth"
grant = "jwt_bearer"
authorization_server = "https://as.example"
client_id = "svc"
assertion = { value = "${env:ACTIONS_ID_TOKEN}" }`},
		{"grant: jwt_bearer with a self-signed assertion", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.auth]
type = "oauth"
grant = "jwt_bearer"
authorization_server = "https://as.example"
client_id = "svc"
signing_key = { key = "${secret:svc_key}", alg = "EdDSA" }
assertion = { subject = "svc@example.com" }`},
		{"token_endpoint_auth_method none on a discovered client", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", token_endpoint_auth_method = "none" }`},
		{"token_endpoint_auth_method client_secret_jwt", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", client_id = "id", client_secret = "${secret:x}", token_endpoint_auth_method = "client_secret_jwt" }`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := config.Parse([]byte(tt.toml), options()); err != nil {
				t.Fatalf("Parse: %v", err)
			}
		})
	}
}

// TestParseInvalid has one failing case per validation rule (design §4,
// ADR 0006, ADR 0007). Each lists the key paths that must be reported.
func TestParseInvalid(t *testing.T) {
	tests := []struct {
		name  string
		toml  string
		paths []string
	}{
		{"unknown top-level key", `wat = 1`, []string{"wat"}},
		{"unknown connector key", `
[connectors.x]
url = "https://x.example.com/mcp"
urll = "typo"`, []string{"connectors.x.urll"}},
		{"unknown auth key", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", pkce_mode = "s256" }`, []string{"connectors.x.auth.pkce_mode"}},
		{"neither url nor command", `
[connectors.x]
timeout = "1s"`, []string{"connectors.x"}},
		{"both url and command", `
[connectors.x]
url = "https://x.example.com/mcp"
command = "srv"`, []string{"connectors.x"}},
		{"connector name with a dot", `
[connectors."a.b"]
command = "srv"`, []string{`connectors."a.b"`}},
		{"connector name uppercase", `
[connectors.Linear]
command = "srv"`, []string{"connectors.Linear"}},
		{"connector name too long", `
[connectors.a1234567890123456789012345678901234567890123456789012345678901234]
command = "srv"`, []string{"connectors.a1234567890123456789012345678901234567890123456789012345678901234"}},
		{"args on url connector", `
[connectors.x]
url = "https://x.example.com/mcp"
args = ["-v"]`, []string{"connectors.x.args"}},
		{"env on url connector", `
[connectors.x]
url = "https://x.example.com/mcp"
env = { A = "b" }`, []string{"connectors.x.env"}},
		{"headers on stdio connector", `
[connectors.x]
command = "srv"
headers = { A = "b" }`, []string{"connectors.x.headers"}},
		{"transport on stdio connector", `
[connectors.x]
command = "srv"
transport = "sse"`, []string{"connectors.x.transport"}},
		{"auth on stdio connector", `
[connectors.x]
command = "srv"
auth = { type = "oauth" }`, []string{"connectors.x.auth"}},
		{"Authorization header with an auth table", `
[connectors.x]
url = "https://x.example.com/mcp"
headers = { authorization = "Bearer ${secret:x}" }
auth = { type = "oauth" }`, []string{"connectors.x.headers.authorization"}},
		{"unknown transport", `
[connectors.x]
url = "https://x.example.com/mcp"
transport = "websocket"`, []string{"connectors.x.transport"}},
		{"url not http(s)", `
[connectors.x]
url = "ftp://x.example.com/mcp"`, []string{"connectors.x.url"}},
		{"auth type missing", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { client_id = "id" }`, []string{"connectors.x.auth.type"}},
		{"auth type unknown", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "basic" }`, []string{"connectors.x.auth.type"}},
		{"ADR 0007: pkce = false without client_secret", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", client_id = "id", pkce = false }`, []string{"connectors.x.auth.pkce"}},
		{"ADR 0007: resource_indicator = false without client_id", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", resource_indicator = false }`, []string{"connectors.x.auth.resource_indicator"}},
		{"ADR 0007: authorization_endpoint without token_endpoint", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", client_id = "id", authorization_endpoint = "https://as.example.com/authorize" }`,
			[]string{"connectors.x.auth"}},
		{"ADR 0007: token_endpoint without authorization_endpoint", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", client_id = "id", token_endpoint = "https://as.example.com/token" }`,
			[]string{"connectors.x.auth"}},
		{"client_secret without client_id", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", client_secret = "s" }`, []string{"connectors.x.auth.client_secret"}},
		{"authorize_params overriding protocol parameters", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", authorize_params = { access_type = "offline", scope = "a", state = "s", code_challenge_method = "plain", resource = "r", redirect_uri = "https://evil.example/cb", client_id = "c", response_type = "token", code_challenge = "x", "" = "v" } }`,
			[]string{
				"connectors.x.auth.authorize_params",
				"connectors.x.auth.authorize_params.client_id",
				"connectors.x.auth.authorize_params.code_challenge",
				"connectors.x.auth.authorize_params.code_challenge_method",
				"connectors.x.auth.authorize_params.redirect_uri",
				"connectors.x.auth.authorize_params.resource",
				"connectors.x.auth.authorize_params.response_type",
				"connectors.x.auth.authorize_params.scope",
				"connectors.x.auth.authorize_params.state",
			}},
		{"redirect_uri not a URL", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", redirect_uri = "/oauth/callback" }`, []string{"connectors.x.auth.redirect_uri"}},
		{"listen on all interfaces", `
[server]
listen = ":7777"`, []string{"server.listen"}},
		{"listen on a non-loopback address", `
[server]
listen = "0.0.0.0:7777"`, []string{"server.listen"}},
		{"listen on a hostname", `
[server]
listen = "example.com:7777"`, []string{"server.listen"}},
		{"listen without port", `
[server]
listen = "127.0.0.1"`, []string{"server.listen"}},
		{"unknown storage driver", `
[storage]
driver = "mysql"`, []string{"storage.driver"}},
		{"postgres without dsn", `
[storage]
driver = "postgres"`, []string{"storage.dsn"}},
		{"embedding table missing required keys", `
[embedding]
timeout = "1s"`, []string{"embedding.base_url", "embedding.model", "embedding.dimensions"}},
		{"embedding dimensions not positive", `
[embedding]
base_url = "http://localhost:11434/v1"
model = "m"
dimensions = -1`, []string{"embedding.dimensions"}},
		{"zero refresh_interval", `
[index]
refresh_interval = "0s"`, []string{"index.refresh_interval"}},
		{"negative call retention", `
[calls]
retention = "-1h"`, []string{"calls.retention"}},
		{"negative connector timeout", `
[connectors.x]
command = "srv"
timeout = "-1s"`, []string{"connectors.x.timeout"}},
		{"unknown template kind", `
[connectors.x]
url = "https://x.example.com/mcp"
headers = { A = "${vault:k}" }`, []string{"connectors.x.headers.A"}},
		{"unterminated template", `
[connectors.x]
command = "srv"
args = ["${env:HOME"]`, []string{"connectors.x.args[0]"}},
		{"empty template name", `
[connectors.x]
command = "srv"
env = { A = "${secret:}" }`, []string{"connectors.x.env.A"}},
		{"bad template in client_secret", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", client_id = "id", client_secret = "${secret}" }`,
			[]string{"connectors.x.auth.client_secret"}},
		{"bad template in api_key", `
[embedding]
base_url = "http://localhost:11434/v1"
model = "m"
dimensions = 1
api_key = "${env:BAD-NAME}"`, []string{"embedding.api_key"}},
		{"tls on a stdio connector", `
[connectors.x]
command = "srv"
tls = { ca_file = "/etc/corp/ca.pem" }`, []string{"connectors.x.tls"}},
		{"empty tls table", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.tls]`, []string{"connectors.x.tls"}},
		{"tls cert_file without a key", `
[connectors.x]
url = "https://x.example.com/mcp"
tls = { cert_file = "/c.pem" }`, []string{"connectors.x.tls.cert_file"}},
		{"tls key_file and key both set", `
[connectors.x]
url = "https://x.example.com/mcp"
tls = { cert_file = "/c.pem", key_file = "/c.key", key = "${secret:k}" }`, []string{"connectors.x.tls"}},
		{"tls key_file without cert_file", `
[connectors.x]
url = "https://x.example.com/mcp"
tls = { key_file = "/c.key", ca_file = "/ca.pem" }`, []string{"connectors.x.tls.key_file"}},
		{"tls key without cert_file", `
[connectors.x]
url = "https://x.example.com/mcp"
tls = { key = "${secret:k}" }`, []string{"connectors.x.tls.key"}},
		{"bad template in tls key", `
[connectors.x]
url = "https://x.example.com/mcp"
tls = { cert_file = "/c.pem", key = "${secret}" }`, []string{"connectors.x.tls.key"}},
		{"tools entries: unknown class, negated class, bad glob, empty", `
[connectors.x]
command = "srv"
tools = ["@read", "@writes", "!@read", "list_[", "!", " "]`, []string{
			"connectors.x.tools[1]", "connectors.x.tools[2]", "connectors.x.tools[3]",
			"connectors.x.tools[4]", "connectors.x.tools[5]",
		}},
		{"authorize_params setting dpop_jkt", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", authorize_params = { dpop_jkt = "x" } }`, []string{"connectors.x.auth.authorize_params.dpop_jkt"}},
		{"unknown grant, dpop, and token_endpoint_auth_method", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", grant = "password", dpop = "sometimes", token_endpoint_auth_method = "magic" }`, []string{
			"connectors.x.auth.dpop", "connectors.x.auth.token_endpoint_auth_method", "connectors.x.auth.grant",
		}},
		{"empty grant and dpop written out", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", grant = "", dpop = "" }`, []string{"connectors.x.auth.dpop", "connectors.x.auth.grant"}},
		{"device_authorization_endpoint not a URL", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", grant = "device_code", device_authorization_endpoint = "/device" }`,
			[]string{"connectors.x.auth.device_authorization_endpoint"}},
		{"device_code: redirect_uri and authorization_endpoint", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.auth]
type = "oauth"
grant = "device_code"
redirect_uri = "https://abc.ngrok.app/oauth/callback"
authorization_endpoint = "https://as.example.com/authorize"
token_endpoint = "https://as.example.com/token"`, []string{
			"connectors.x.auth.authorization_endpoint", "connectors.x.auth.redirect_uri",
		}},
		{"client_credentials: no client_id, no credential", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", grant = "client_credentials", authorization_server = "https://as.example" }`, []string{
			"connectors.x.auth.client_id", "connectors.x.auth.grant",
		}},
		{"client_credentials: the keys of a flow with a user", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.auth]
type = "oauth"
grant = "client_credentials"
authorization_server = "https://as.example"
client_id = "svc"
client_secret = "${secret:svc}"
authorization_endpoint = "https://as.example.com/authorize"
token_endpoint = "https://as.example.com/token"
device_authorization_endpoint = "https://as.example.com/device"
client_metadata_url = "https://me.example.com/client.json"
redirect_uri = "https://abc.ngrok.app/oauth/callback"
pkce = true
authorize_params = { prompt = "consent" }`, []string{
			"connectors.x.auth.authorization_endpoint", "connectors.x.auth.device_authorization_endpoint",
			"connectors.x.auth.client_metadata_url", "connectors.x.auth.redirect_uri", "connectors.x.auth.pkce",
			"connectors.x.auth.authorize_params",
		}},
		{"client_credentials: an empty authorize_params table and pkce = false", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", grant = "client_credentials", authorization_server = "https://as.example", client_id = "svc", client_secret = "${secret:svc}", pkce = false, authorize_params = {} }`,
			[]string{"connectors.x.auth.pkce", "connectors.x.auth.authorize_params"}},
		{"client_credentials: a tls method without a client certificate", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", grant = "client_credentials", authorization_server = "https://as.example", client_id = "svc", token_endpoint_auth_method = "tls_client_auth" }`,
			[]string{"connectors.x.auth.token_endpoint_auth_method"}},
		{"jwt_bearer: no client_id and no assertion", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", grant = "jwt_bearer", authorization_server = "https://as.example", redirect_uri = "https://abc.ngrok.app/oauth/callback" }`, []string{
			"connectors.x.auth.client_id", "connectors.x.auth.redirect_uri", "connectors.x.auth.assertion",
		}},
		{"jwt_bearer: an empty assertion table", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.auth]
type = "oauth"
grant = "jwt_bearer"
authorization_server = "https://as.example"
client_id = "svc"
[connectors.x.auth.assertion]`, []string{"connectors.x.auth.assertion"}},
		{"jwt_bearer: two assertion sources", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", grant = "jwt_bearer", authorization_server = "https://as.example", client_id = "svc", assertion = { file = "/t", value = "${env:T}" } }`,
			[]string{"connectors.x.auth.assertion"}},
		{"jwt_bearer: a subject without a signing key", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", grant = "jwt_bearer", authorization_server = "https://as.example", client_id = "svc", assertion = { subject = "svc" } }`,
			[]string{"connectors.x.auth.assertion.subject"}},
		{"jwt_bearer: a bad template in the assertion value", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", grant = "jwt_bearer", authorization_server = "https://as.example", client_id = "svc", assertion = { value = "${env:}" } }`,
			[]string{"connectors.x.auth.assertion.value"}},
		{"an assertion without the jwt_bearer grant", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", grant = "client_credentials", authorization_server = "https://as.example", client_id = "svc", client_secret = "${secret:svc}", assertion = { file = "/t" } }`,
			[]string{"connectors.x.auth.assertion"}},
		{"an assertion with the default grant", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", assertion = { file = "/t" } }`, []string{"connectors.x.auth.assertion"}},
		{"private_key_jwt without a signing key or client_id", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", token_endpoint_auth_method = "private_key_jwt" }`, []string{
			"connectors.x.auth.token_endpoint_auth_method", "connectors.x.auth.token_endpoint_auth_method",
		}},
		{"client_secret methods without a client secret", `
[connectors.a]
url = "https://a.example.com/mcp"
auth = { type = "oauth", client_id = "id", token_endpoint_auth_method = "client_secret_basic" }
[connectors.b]
url = "https://b.example.com/mcp"
auth = { type = "oauth", client_id = "id", token_endpoint_auth_method = "client_secret_post" }
[connectors.c]
url = "https://c.example.com/mcp"
auth = { type = "oauth", token_endpoint_auth_method = "client_secret_jwt" }`, []string{
			"connectors.a.auth.token_endpoint_auth_method", "connectors.b.auth.token_endpoint_auth_method",
			"connectors.c.auth.token_endpoint_auth_method", "connectors.c.auth.token_endpoint_auth_method",
		}},
		{"none with a client secret and a signing key", `
[connectors.x]
url = "https://x.example.com/mcp"
[connectors.x.auth]
type = "oauth"
client_id = "id"
client_secret = "${secret:x}"
token_endpoint_auth_method = "none"
signing_key = { key = "${secret:k}" }`, []string{"connectors.x.auth.client_secret", "connectors.x.auth.signing_key"}},
		{"tls methods without a client certificate or client_id", `
[connectors.a]
url = "https://a.example.com/mcp"
auth = { type = "oauth", client_id = "id", token_endpoint_auth_method = "tls_client_auth" }
[connectors.b]
url = "https://b.example.com/mcp"
auth = { type = "oauth", token_endpoint_auth_method = "self_signed_tls_client_auth" }
tls = { cert_file = "/c.pem", key_file = "/c.key" }
[connectors.c]
url = "https://c.example.com/mcp"
auth = { type = "oauth", client_id = "id", token_endpoint_auth_method = "tls_client_auth" }
tls = { ca_file = "/ca.pem" }`, []string{
			"connectors.a.auth.token_endpoint_auth_method", "connectors.b.auth.token_endpoint_auth_method",
			"connectors.c.auth.token_endpoint_auth_method",
		}},
		{"signing_key: no client_id, no key, unknown alg and audience", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", signing_key = { alg = "HS256", audience = "resource" } }`, []string{
			"connectors.x.auth.signing_key", "connectors.x.auth.signing_key.key",
			"connectors.x.auth.signing_key.alg", "connectors.x.auth.signing_key.audience",
		}},
		{"signing_key: a bad template in the key", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", client_id = "id", signing_key = { key = "${secret}" } }`,
			[]string{"connectors.x.auth.signing_key.key"}},
		{"signing_key: an unknown key", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", client_id = "id", signing_key = { key = "${secret:k}", kty = "EC" } }`,
			[]string{"connectors.x.auth.signing_key.kty"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tt.toml), options())
			var verr *config.ValidationError

			if !errors.As(err, &verr) {
				t.Fatalf("Parse error = %v, want *config.ValidationError", err)
			}

			var got []string

			for _, is := range verr.Issues {
				got = append(got, is.Path)
			}

			if diff := cmp.Diff(tt.paths, got); diff != "" {
				t.Errorf("issue paths (-want +got):\n%s\nerror: %v", diff, err)
			}
		})
	}
}

// TestParseDurationMustBeString rejects a bare integer duration: design §4
// says durations are Go duration strings.
func TestParseDurationMustBeString(t *testing.T) {
	_, err := config.Parse([]byte("[index]\nrefresh_interval = 3600\n"), options())

	if err == nil {
		t.Fatal("Parse accepted an integer duration")
	}
}

func TestParseSyntaxError(t *testing.T) {
	_, err := config.Parse([]byte("[server\n"), options())

	if err == nil {
		t.Fatal("Parse accepted malformed TOML")
	}

	var verr *config.ValidationError

	if errors.As(err, &verr) {
		t.Errorf("syntax error reported as a validation error: %v", err)
	}
}

// TestParseBadURLOmitsItsQuery keeps a secret in a malformed url's query out
// of the validation error, which is printed and logged (design §9).
func TestParseBadURLOmitsItsQuery(t *testing.T) {
	tests := []struct {
		name, toml, path string
	}{
		{"connector url with a bad port", `
[connectors.x]
url = "https://x.example.com:bad/mcp?key=hunter2"`, "connectors.x.url"},
		{"connector url without a scheme", `
[connectors.x]
url = "://x.example.com/mcp?key=hunter2"`, "connectors.x.url"},
		{"embedding base_url with a control character", `
[embedding]
base_url = "http://localhost:11434/v1?key=hunter2\u007f"
model = "m"
dimensions = 1`, "embedding.base_url"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tt.toml), options())
			var verr *config.ValidationError

			if !errors.As(err, &verr) {
				t.Fatalf("Parse error = %v, want *config.ValidationError", err)
			}

			if len(verr.Issues) != 1 || verr.Issues[0].Path != tt.path {
				t.Fatalf("issues = %+v, want one at %s", verr.Issues, tt.path)
			}

			if msg := err.Error(); strings.Contains(msg, "hunter2") {
				t.Errorf("error %q contains the url's query", msg)
			}
		})
	}
}
