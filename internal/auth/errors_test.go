package auth_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// authCaser is how a package that cannot import auth (upstream, design §2)
// reads the Case of any auth error.
type authCaser interface{ AuthCase() string }

// TestTransportErrorsCarryKindAndCase covers the broker's seam errors as
// upstream sees them: wrapped by net/http in a *url.Error, still matching
// AuthCase(), NeedsAuth() for needs_auth only, and AuthConfig() for config
// only, and narrowing to an *auth.Error with the right Kind.
func TestTransportErrorsCarryKindAndCase(t *testing.T) {
	reject := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	tests := []struct {
		name     string
		opts     []fakeas.Option
		setup    func(t *testing.T, h *brokerHarness)
		wantKind auth.Kind
		wantCase string
		wantLink string
		stepUp   bool
	}{
		{
			name:     "never authorized",
			setup:    func(*testing.T, *brokerHarness) {},
			wantKind: auth.KindNeedsAuth, wantCase: "no_credential", wantLink: wantAuthURL,
		},
		{
			name: "connector url changed",
			setup: func(t *testing.T, h *brokerHarness) {
				h.seed(t, "", time.Hour)
				h.conn.URL = config.Template(h.as.URL + "/other-mcp")
			},
			wantKind: auth.KindNeedsAuth, wantCase: "url_changed", wantLink: wantAuthURL,
		},
		{
			name: "expired with no refresh token",
			setup: func(t *testing.T, h *brokerHarness) {
				if err := auth.SeedToken(t.Context(), h.broker, "linear", h.as.ResourceURL, h.as.TokenEndpoint, "access", "",
					h.clock.Now().Add(30*time.Second), ""); err != nil {
					t.Fatalf("seed token: %v", err)
				}
			},
			wantKind: auth.KindNeedsAuth, wantCase: "refresh_failed", wantLink: wantAuthURL,
		},
		{
			name: "refresh token revoked",
			setup: func(t *testing.T, h *brokerHarness) {
				h.seed(t, "", 30*time.Second)
				h.as.Revoke()
			},
			wantKind: auth.KindNeedsAuth, wantCase: "token_revoked", wantLink: wantAuthURL,
		},
		{
			name:     "upstream rejects a refreshed token",
			opts:     []fakeas.Option{fakeas.WithResourceHandler(reject)},
			setup:    func(t *testing.T, h *brokerHarness) { h.seed(t, "", time.Hour) },
			wantKind: auth.KindNeedsAuth, wantCase: "token_rejected", wantLink: wantAuthURL,
		},
		{
			name:     "step-up",
			opts:     []fakeas.Option{fakeas.WithRequiredScopes("read", "write")},
			setup:    func(t *testing.T, h *brokerHarness) { h.seed(t, "read", time.Hour) },
			wantKind: auth.KindNeedsAuth, wantCase: "step_up", wantLink: wantAuthURL, stepUp: true,
		},
		{
			name: "client rejected",
			setup: func(t *testing.T, h *brokerHarness) {
				h.seed(t, "", 30*time.Second)
				h.conn.Auth.ClientID = "not-registered"
			},
			wantKind: auth.KindConfig, wantCase: "client_rejected",
		},
		{
			name:     "issuer mismatch",
			opts:     []fakeas.Option{fakeas.WithIssuerMismatch()},
			setup:    func(t *testing.T, h *brokerHarness) { h.seed(t, "", 30*time.Second) },
			wantKind: auth.KindConfig, wantCase: "issuer_mismatch",
		},
		{
			name: "client secret reference missing",
			setup: func(t *testing.T, h *brokerHarness) {
				h.seed(t, "", 30*time.Second)
				h.conn.Auth.ClientSecret = "${secret:linear_secret}"
			},
			wantKind: auth.KindConfig, wantCase: "unresolved_value",
		},
		{
			name: "token endpoint answers 502 without an error code",
			setup: func(t *testing.T, h *brokerHarness) {
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
				}))
				t.Cleanup(proxy.Close)
				h.conn.Auth.AuthorizationEndpoint = h.as.AuthorizationEndpoint
				h.conn.Auth.TokenEndpoint = proxy.URL + "/token"
				h.seed(t, "", 30*time.Second)
			},
			wantKind: auth.KindMalformed, wantCase: "unexpected_status",
		},
		{
			name: "token endpoint unreachable",
			setup: func(t *testing.T, h *brokerHarness) {
				down := httptest.NewServer(http.NotFoundHandler())
				down.Close()
				h.conn.Auth.AuthorizationEndpoint = h.as.AuthorizationEndpoint
				h.conn.Auth.TokenEndpoint = down.URL + "/token"
				h.seed(t, "", 30*time.Second)
			},
			wantKind: auth.KindUnreachable, wantCase: "transport",
		},
		{
			name: "token endpoint changed since the grant",
			setup: func(t *testing.T, h *brokerHarness) {
				h.seed(t, "", 30*time.Second)
				h.conn.Auth.AuthorizationEndpoint = h.as.AuthorizationEndpoint
				h.conn.Auth.TokenEndpoint = "https://elsewhere.example/token"
			},
			wantKind: auth.KindNeedsAuth, wantCase: "authorization_server_changed", wantLink: wantAuthURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newBrokerHarness(t, tt.opts...)
			tt.setup(t, h)

			_, err := h.get(t)

			var ue *url.Error

			if !errors.As(err, &ue) {
				t.Fatalf("get error = %v, want it wrapped in *url.Error by net/http", err)
			}

			var ac authCaser

			if !errors.As(err, &ac) {
				t.Fatalf("get error = %v, want AuthCase() through errors.As", err)
			}

			if got := ac.AuthCase(); got != tt.wantCase {
				t.Errorf("AuthCase() = %q, want %q", got, tt.wantCase)
			}

			e, ok := auth.AsError(err)

			if !ok {
				t.Fatalf("AsError(%v) = false", err)
			}

			if diff := cmp.Diff([]any{tt.wantKind, tt.wantCase}, []any{e.Kind, string(e.Case)}); diff != "" {
				t.Errorf("Kind, Case (-want +got):\n%s", diff)
			}

			var na needsAuther

			if got := errors.As(err, &na); got != (tt.wantKind == auth.KindNeedsAuth) {
				t.Errorf("NeedsAuth() found = %v, want %v", got, !got)
			}

			if na != nil {
				link, stepUp := na.NeedsAuth()

				if diff := cmp.Diff([]any{tt.wantLink, tt.stepUp}, []any{link, stepUp}); diff != "" {
					t.Errorf("NeedsAuth() (-want +got):\n%s", diff)
				}
			}

			var ac2 authConfiger
			isConfig := errors.As(err, &ac2) && ac2.AuthConfig()

			if isConfig != (tt.wantKind == auth.KindConfig) {
				t.Errorf("AuthConfig() = %v, want %v", isConfig, !isConfig)
			}
		})
	}
}

// TestAdvise is the user-facing reading of every auth Kind and Case: what
// went wrong, the action that fixes it, and the link for needs_auth.
func TestAdvise(t *testing.T) {
	const link = "http://127.0.0.1:7777/auth/linear"
	const reauth = "Open the link to authorize the connector again."
	const register = "Register an app with the provider and set client_id (and client_secret) in the connector's [auth] table."
	tests := []struct {
		kind auth.Kind
		c    auth.Case
		code string
		want auth.Advice
	}{
		{auth.KindNeedsAuth, auth.CaseNoCredential, "", auth.Advice{
			Summary: "The connector has not been authorized yet.",
			Action:  "Open the link to authorize it.", Link: link}},
		{auth.KindNeedsAuth, auth.CaseURLChanged, "", auth.Advice{
			Summary: "The connector's URL changed since it was authorized, so its token is not sent to the new URL.",
			Action:  "Open the link to authorize it for the new URL.", Link: link}},
		{auth.KindNeedsAuth, auth.CaseClientChanged, "", auth.Advice{
			Summary: "The connector's client_id changed since it was authorized. Authorize it again.",
			Action:  reauth, Link: link}},
		{auth.KindNeedsAuth, auth.CaseCredentialUnreadable, "", auth.Advice{
			Summary: "The stored credential cannot be read, for example because the master key changed.",
			Action:  reauth, Link: link}},
		{auth.KindNeedsAuth, auth.CaseRefreshFailed, "", auth.Advice{
			Summary: "The connector's token expired and cannot be refreshed.",
			Action:  reauth, Link: link}},
		{auth.KindNeedsAuth, auth.CaseTokenRevoked, "invalid_grant", auth.Advice{
			Summary: "The authorization server revoked or expired the connector's grant (provider code invalid_grant).",
			Action:  reauth, Link: link}},
		{auth.KindNeedsAuth, auth.CaseProviderError, "team_added_to_org", auth.Advice{
			Summary: "The authorization server refused the token with an error Wayfindr does not recognise (provider code team_added_to_org).",
			Action:  "Open the link to authorize the connector again. If that fails too, look the provider code up in the provider's documentation.",
			Link:    link}},
		{auth.KindNeedsAuth, auth.CaseTokenRejected, "", auth.Advice{
			Summary: "The upstream rejected a freshly refreshed token.",
			Action:  reauth, Link: link}},
		{auth.KindNeedsAuth, auth.CaseStepUp, "", auth.Advice{
			Summary: "The upstream needs scopes the connector was not granted.",
			Action:  "Open the link to authorize the connector with the larger set of scopes.", Link: link}},
		{auth.KindNeedsAuth, auth.CaseRegistrationExpired, "invalid_client", auth.Advice{
			Summary: "The authorization server no longer accepts the OAuth client Wayfindr registered for this connector " +
				"(provider code invalid_client).",
			Action: "Open the link to authorize the connector again. Wayfindr registers a new client.", Link: link}},
		{auth.KindConfig, auth.CaseClientRejected, "invalid_client", auth.Advice{
			Summary: "The authorization server rejected the configured OAuth client (provider code invalid_client).",
			Action:  "Check client_id and client_secret in the connector's [auth] table."}},
		{auth.KindConfig, auth.CaseIssuerMismatch, "", auth.Advice{
			Summary: "The authorization server's metadata names a different issuer than the one it was found for.",
			Action:  "Check authorization_server in the connector's [auth] table. Set validate_issuer = false only if you trust this server."}},
		{auth.KindConfig, auth.CaseNoRegistration, "", auth.Advice{
			Summary: "There is no OAuth client to authorize with: no client_id is configured, and the authorization server " +
				"supports neither client ID metadata documents nor dynamic client registration.",
			Action: register}},
		{auth.KindConfig, auth.CaseRegistrationRejected, "invalid_redirect_uri", auth.Advice{
			Summary: "The authorization server rejected dynamic client registration (provider code invalid_redirect_uri).",
			Action:  register}},
		{auth.KindConfig, auth.CaseUnresolvedValue, "", auth.Advice{
			Summary: "A value in the connector's config cannot be resolved: a ${…} reference " +
				"(url, client_secret, signing_key.key, assertion.value, or tls.key) or the assertion file.",
			Action: "Set the missing secret or environment variable, create the file, or fix the reference."}},
		{auth.KindConfig, auth.CaseInvalidURL, "", auth.Advice{
			Summary: "A URL in the connector's auth setup is not an absolute URL.",
			Action:  "Fix url, authorization_server, or token_endpoint in the connector's config."}},
		{auth.KindConfig, auth.CaseNoAuthorizationServer, "", auth.Advice{
			Summary: "The connector's protected resource metadata names no authorization server.",
			Action:  "Set authorization_server, or authorization_endpoint and token_endpoint, in the connector's [auth] table."}},
		{auth.KindConfig, auth.CaseNoMetadata, "", auth.Advice{
			Summary: "The authorization server publishes no metadata.",
			Action:  "Set authorization_endpoint and token_endpoint in the connector's [auth] table."}},
		{auth.KindConfig, auth.CasePKCEUnsupported, "", auth.Advice{
			Summary: "The authorization server does not advertise PKCE (S256), which Wayfindr requires for a connector without a client_id.",
			Action:  register}},
		{auth.KindConfig, auth.CaseDeviceUnsupported, "", auth.Advice{
			Summary: "The authorization server does not support signing in with a code on another device (device code).",
			Action: "Set device_authorization_endpoint in the connector's [auth] table if the provider has one, " +
				`or sign in in a browser: drop --device, or set grant = "authorization_code".`}},
		{auth.KindNeedsAuth, auth.CaseAuthorizationPending, "authorization_pending", auth.Advice{
			Summary: "The authorization is waiting for you to enter the code on the provider's page.",
			Action:  "Enter the code at the address Wayfindr showed, or open the link to start again.", Link: link}},
		{auth.KindUnreachable, auth.CaseSlowDown, "slow_down", auth.Advice{
			Summary: "The authorization server asked Wayfindr to poll less often (provider code slow_down).",
			Action:  "Wayfindr tries again on the next call."}},
		{auth.KindConfig, auth.CaseAuthMethodUnsupported, "", auth.Advice{
			Summary: "Wayfindr cannot authenticate as the connector's OAuth client: the authorization server " +
				"accepts no client authentication method that fits the client's credential.",
			Action: "Set token_endpoint_auth_method in the connector's [auth] table to a method the provider " +
				"accepts, and configure its credential: client_secret, or signing_key for private_key_jwt."}},
		{auth.KindConfig, auth.CaseSigningKeyInvalid, "", auth.Advice{
			Summary: "The connector's signing_key is not a private key Wayfindr can sign with, or cannot sign the configured alg.",
			Action: "Store a PEM private key (EC P-256 or P-384, RSA of 2048 bits or more, or Ed25519) with " +
				"`wayfindr connector edit linear --signing-key < key.pem`, or fix signing_key.alg."}},
		{auth.KindConfig, auth.CaseGrantRejected, "invalid_scope", auth.Advice{
			Summary: "The authorization server refused the connector's unattended grant (provider code invalid_scope).",
			Action: "Fix the OAuth client's access or the assertion at the provider, or client_id, scopes, or " +
				"[auth.assertion] in the connector's config. A config change retries at once; after a fix at the " +
				"provider, run `wayfindr auth logout linear` to have Wayfindr run the grant again."}},
		{auth.KindConfig, auth.CaseScopeDenied, "", auth.Advice{
			Summary: "The upstream needs scopes the connector's unattended grant asked for but was not given.",
			Action: "Allow the OAuth client those scopes at the provider, or fix scopes in the connector's [auth] table. " +
				"A config change retries at once; after a fix at the provider, run `wayfindr auth logout linear` " +
				"to have Wayfindr run the grant again."}},
		{auth.KindConfig, auth.CaseDPoPAlgUnsupported, "", auth.Advice{
			Summary: `The connector's dpop is "always", but the authorization server does not accept ES256 DPoP ` +
				"proofs, the only kind Wayfindr signs.",
			Action: `Set dpop = "auto" or "never" in the connector's [auth] table to send bearer tokens.`}},
		{auth.KindConfig, auth.CaseDPoPRejected, "invalid_dpop_proof", auth.Advice{
			Summary: "The authorization server rejected Wayfindr's DPoP proof (provider code invalid_dpop_proof). " +
				"The clock of the host the daemon runs on may be wrong.",
			Action: `Check the host's clock. If the server still rejects the proof, set dpop = "never" in the ` +
				"connector's [auth] table to send bearer tokens; a config change retries at once."}},
		{auth.KindNeedsAuth, auth.CaseAuthorizationDenied, "access_denied", auth.Advice{
			Summary: "The authorization was not granted (provider code access_denied).",
			Action:  "Open the link to try again, and allow access on the provider's page.", Link: link}},
		{auth.KindNeedsAuth, auth.CaseFlowExpired, "", auth.Advice{
			Summary: "The authorization expired: it was not completed within 10 minutes.",
			Action:  "Open the link to start it again.", Link: link}},
		{auth.KindNeedsAuth, auth.CaseFlowCancelled, "", auth.Advice{
			Summary: "The authorization was cancelled: the connector's credential was revoked, " +
				"or its url or [auth] table changed or it was removed from the config, while it was in progress.",
			Action: "Open the link to start it again.", Link: link}},
		{auth.KindNeedsAuth, auth.CaseCallbackIssuerMismatch, "", auth.Advice{
			Summary: "The authorization server's answer did not name the issuer this authorization was started with, " +
				"so Wayfindr did not use it. This may be a mix-up attack.",
			Action: "Open the link to start it again. If it happens again, report it with the daemon log.", Link: link}},
		{auth.KindUnreachable, auth.CaseTransport, "", auth.Advice{
			Summary: "The authorization server cannot be reached.",
			Action:  "Check the network. Wayfindr tries again on the next call."}},
		{auth.KindUnreachable, auth.CaseProviderUnavailable, "ratelimited", auth.Advice{
			Summary: "The authorization server is temporarily unavailable or rate limiting Wayfindr (provider code ratelimited).",
			Action:  "Try again later. Wayfindr tries again on the next call."}},
		{auth.KindMalformed, auth.CaseMalformedResponse, "", auth.Advice{
			Summary: "The authorization server sent a response Wayfindr cannot read.",
			Action:  "Wayfindr tries again on the next call. If it persists, report it with the daemon log."}},
		{auth.KindMalformed, auth.CaseUnexpectedStatus, "", auth.Advice{
			Summary: "The authorization server answered with an unexpected HTTP status.",
			Action:  "Wayfindr tries again on the next call. If it persists, check the provider's status."}},
		{auth.KindInternal, auth.CaseStore, "", auth.Advice{
			Summary: "Wayfindr could not read or write its credential store.",
			Action:  "Check the daemon log and the disk the store is on."}},
		{auth.KindInternal, auth.CaseNotReplayable, "", auth.Advice{
			Summary: "The upstream rejected the token and the request could not be sent again. The token has been refreshed.",
			Action:  "Retry the call."}},
		{auth.KindRequest, auth.CaseUnknownConnector, "", auth.Advice{
			Summary: `No connector named "linear" is configured.`,
			Action:  "Check the connector's name with `wayfindr status`."}},
		{auth.KindRequest, auth.CaseNotOAuth, "", auth.Advice{
			Summary: `linear does not use OAuth: it has no [auth] table of type "oauth".`,
			Action:  "Add one with `wayfindr connector edit linear --oauth` to authorize it through Wayfindr, or put its API key in headers."}},
		{auth.KindRequest, auth.CaseNotInteractive, "", auth.Advice{
			Summary: "linear uses an unattended grant, so there is nothing to sign in to: Wayfindr gets its token itself.",
			Action:  "Check it with `wayfindr status`. To have Wayfindr get a new token, run `wayfindr auth logout linear`."}},
	}

	for _, tt := range tests {
		t.Run(string(tt.c), func(t *testing.T) {
			e := &auth.Error{Kind: tt.kind, Case: tt.c, Connector: "linear", ProviderCode: tt.code}

			if tt.kind == auth.KindNeedsAuth {
				e.Link = link
			}

			if diff := cmp.Diff(tt.want, auth.Advise(e)); diff != "" {
				t.Errorf("Advise (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAsError(t *testing.T) {
	if e, ok := auth.AsError(errors.New("plain")); ok || e != nil {
		t.Errorf("AsError(plain) = %v, %v; want nil, false", e, ok)
	}

	want := &auth.Error{Kind: auth.KindConfig, Case: auth.CaseNoMetadata}
	wrapped := &url.Error{Op: "Get", URL: "https://mcp.example.com/mcp", Err: fmt.Errorf("discover: %w", error(want))}

	if e, ok := auth.AsError(wrapped); !ok || e != want {
		t.Errorf("AsError(wrapped) = %v, %v; want the *auth.Error, true", e, ok)
	}
}

// HTTPStatus is how both doors answer a flow that cannot start: whose
// problem it is, by Kind.
func TestHTTPStatus(t *testing.T) {
	tests := []struct {
		kind auth.Kind
		want int
	}{
		{auth.KindRequest, 404},
		{auth.KindNeedsAuth, 409},
		{auth.KindConfig, 409},
		{auth.KindUnreachable, 502},
		{auth.KindMalformed, 502},
		{auth.KindInternal, 500},
	}

	for _, tt := range tests {
		err := fmt.Errorf("start: %w", &auth.Error{Kind: tt.kind})

		if got := auth.HTTPStatus(err); got != tt.want {
			t.Errorf("HTTPStatus(%s) = %d, want %d", tt.kind, got, tt.want)
		}
	}

	if got := auth.HTTPStatus(errors.New("plain")); got != 500 {
		t.Errorf("HTTPStatus(plain) = %d, want 500", got)
	}
}
