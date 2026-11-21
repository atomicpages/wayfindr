package fakeas_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// publicConfig registers a public client with s and returns its config.
func (c *client) publicConfig(s *fakeas.Server, scopes ...string) *oauth2.Config {
	c.t.Helper()
	reg := c.register(s.RegistrationEndpoint, nil)

	return &oauth2.Config{
		ClientID:    reg.ClientID,
		RedirectURL: redirectURI,
		Scopes:      scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL: s.AuthorizationEndpoint, TokenURL: s.TokenEndpoint, AuthStyle: oauth2.AuthStyleInParams,
		},
	}
}

func s256Opts(v string) []oauth2.AuthCodeOption {
	return []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(v)}
}

// TestPKCE covers the default PKCE S256 requirement and the
// WithoutPKCEAdvertised quirk.
func TestPKCE(t *testing.T) {
	tests := []struct {
		name          string
		opts          []fakeas.Option
		challenge     func(verifier string) []oauth2.AuthCodeOption
		exchange      func(verifier string) []oauth2.AuthCodeOption
		wantAdvertise []string
		wantAuthzErr  string
		wantTokenErr  string
	}{
		{
			name:          "S256 round trip",
			challenge:     s256Opts,
			exchange:      func(v string) []oauth2.AuthCodeOption { return []oauth2.AuthCodeOption{oauth2.VerifierOption(v)} },
			wantAdvertise: []string{"S256"},
		},
		{
			name:          "missing challenge rejected",
			wantAdvertise: []string{"S256"},
			wantAuthzErr:  "invalid_request",
		},
		{
			name: "plain method rejected",
			challenge: func(v string) []oauth2.AuthCodeOption {
				return []oauth2.AuthCodeOption{
					oauth2.SetAuthURLParam("code_challenge", v),
					oauth2.SetAuthURLParam("code_challenge_method", "plain"),
				}
			},
			wantAdvertise: []string{"S256"},
			wantAuthzErr:  "invalid_request",
		},
		{
			name:      "wrong verifier rejected",
			challenge: s256Opts,
			exchange: func(string) []oauth2.AuthCodeOption {
				return []oauth2.AuthCodeOption{oauth2.VerifierOption(oauth2.GenerateVerifier())}
			},
			wantAdvertise: []string{"S256"},
			wantTokenErr:  "invalid_grant",
		},
		{
			name:          "missing verifier rejected",
			challenge:     s256Opts,
			wantAdvertise: []string{"S256"},
			wantTokenErr:  "invalid_grant",
		},
		{
			name: "not advertised: no PKCE accepted",
			opts: []fakeas.Option{fakeas.WithoutPKCEAdvertised()},
		},
		{
			name:         "not advertised: a sent challenge is still checked",
			opts:         []fakeas.Option{fakeas.WithoutPKCEAdvertised()},
			challenge:    s256Opts,
			wantTokenErr: "invalid_grant",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeas.Start(t, tt.opts...)
			c := newClient(t)

			if got := c.asMeta(s.MetadataURL).CodeChallengeMethodsSupported; !cmp.Equal(got, tt.wantAdvertise) {
				t.Errorf("code_challenge_methods_supported = %v, want %v", got, tt.wantAdvertise)
			}

			cfg := c.publicConfig(s)
			verifier := oauth2.GenerateVerifier()
			var authOpts, exOpts []oauth2.AuthCodeOption

			if tt.challenge != nil {
				authOpts = tt.challenge(verifier)
			}

			if tt.exchange != nil {
				exOpts = tt.exchange(verifier)
			}

			q := c.authorize(cfg, "st", authOpts...)

			if q.Get("error") != tt.wantAuthzErr || q.Get("state") != "st" {
				t.Fatalf("authorize redirect %v, want error %q and state st", q, tt.wantAuthzErr)
			}

			if tt.wantAuthzErr != "" {
				return
			}

			_, err := cfg.Exchange(c.ctx(), q.Get("code"), exOpts...)

			if tt.wantTokenErr == "" {
				if err != nil {
					t.Fatalf("Exchange: %v", err)
				}

				return
			}

			if err == nil || oauthErrorCode(t, err) != tt.wantTokenErr {
				t.Fatalf("Exchange error %v, want %s", err, tt.wantTokenErr)
			}
		})
	}
}

// TestIssuerMismatch: PRM names one issuer, while the metadata found from it
// and the RFC 9207 iss parameter name another.
func TestIssuerMismatch(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithIssuerMismatch(), fakeas.WithIssuerPath("/tenant1"))
	c := newClient(t)

	if s.MetadataIssuer == s.Issuer {
		t.Fatalf("MetadataIssuer = Issuer = %q, want them to differ", s.Issuer)
	}

	if got := c.prm(s.PRMURL).AuthorizationServers; !cmp.Equal(got, []string{s.Issuer}) {
		t.Errorf("PRM authorization_servers = %v, want [%s]", got, s.Issuer)
	}

	if s.MetadataURL != s.URL+"/.well-known/oauth-authorization-server/tenant1" {
		t.Errorf("MetadataURL = %q, want it derived from Issuer %q", s.MetadataURL, s.Issuer)
	}

	if got := c.asMeta(s.MetadataURL).Issuer; got != s.MetadataIssuer {
		t.Errorf("metadata issuer = %q, want %q", got, s.MetadataIssuer)
	}

	// The RFC 8414 §3.3 check a strict client makes must fail.
	if _, err := oauthex.GetAuthServerMeta(t.Context(), s.MetadataURL, s.Issuer, c.http); err == nil {
		t.Error("GetAuthServerMeta accepted the mismatched issuer")
	}

	cfg := c.publicConfig(s)

	if q := c.authorize(cfg, "st", s256Opts(oauth2.GenerateVerifier())...); q.Get("iss") != s.MetadataIssuer {
		t.Errorf("iss = %q, want %q", q.Get("iss"), s.MetadataIssuer)
	}
}

func TestRefreshTokens(t *testing.T) {
	t.Run("default keeps the refresh token reusable", func(t *testing.T) {
		s := fakeas.Start(t)
		c := newClient(t)
		cfg, tok := c.loggedIn(s)

		for i := range 2 {
			got, err := c.refresh(cfg, tok)

			if err != nil || got.RefreshToken != tok.RefreshToken {
				t.Fatalf("refresh %d = %+v, %v; want the same refresh token back", i, got, err)
			}
		}
	})
	t.Run("rotating invalidates the old refresh token", func(t *testing.T) {
		s := fakeas.Start(t, fakeas.WithRotatingRefreshTokens())
		c := newClient(t)
		cfg, tok := c.loggedIn(s)
		next, err := c.refresh(cfg, tok)

		if err != nil {
			t.Fatalf("first refresh: %v", err)
		}

		if next.RefreshToken == "" || next.RefreshToken == tok.RefreshToken {
			t.Fatalf("rotated refresh token = %q, want a new one", next.RefreshToken)
		}

		if _, err := c.refresh(cfg, tok); err == nil || oauthErrorCode(t, err) != "invalid_grant" {
			t.Errorf("reusing the old refresh token: %v, want invalid_grant", err)
		}

		if _, err := c.refresh(cfg, next); err != nil {
			t.Errorf("refresh with the rotated token: %v", err)
		}

		var outcomes []string

		for _, r := range s.TokenRequests() {
			outcomes = append(outcomes, r.GrantType+":"+r.Error)
		}

		want := []string{"authorization_code:", "refresh_token:", "refresh_token:invalid_grant", "refresh_token:"}

		if diff := cmp.Diff(want, outcomes); diff != "" {
			t.Errorf("token request outcomes (-want +got):\n%s", diff)
		}
	})
}

// TestRevoke is early revocation: the AS drops tokens before they expire.
func TestRevoke(t *testing.T) {
	s := fakeas.Start(t)
	c := newClient(t)
	cfg, tok := c.loggedIn(s)
	s.Revoke()

	code, params := c.challenge(s.ResourceURL, tok.AccessToken)

	if code != http.StatusUnauthorized || params["error"] != "invalid_token" {
		t.Errorf("revoked token at resource: %d %v, want 401 error=invalid_token", code, params)
	}

	if _, err := c.refresh(cfg, tok); err == nil || oauthErrorCode(t, err) != "invalid_grant" {
		t.Errorf("refresh after Revoke: %v, want invalid_grant", err)
	}

	_, tok2 := c.loggedIn(s)

	if code, _ := c.challenge(s.ResourceURL, tok2.AccessToken); code != http.StatusOK {
		t.Errorf("token issued after Revoke: status %d, want 200", code)
	}
}

func TestExpireAccessTokens(t *testing.T) {
	s := fakeas.Start(t)
	c := newClient(t)
	cfg, tok := c.loggedIn(s)
	s.ExpireAccessTokens()

	if code, _ := c.challenge(s.ResourceURL, tok.AccessToken); code != http.StatusUnauthorized {
		t.Errorf("expired token at resource: status %d, want 401", code)
	}

	fresh, err := c.refresh(cfg, tok)

	if err != nil {
		t.Fatalf("refresh after expiry: %v", err)
	}

	if code, _ := c.challenge(s.ResourceURL, fresh.AccessToken); code != http.StatusOK {
		t.Errorf("refreshed token: status %d, want 200", code)
	}
}

// TestInsufficientScope drives a step-up: a token without the required
// scopes gets 403 insufficient_scope naming them, and re-authorizing with
// them succeeds.
func TestInsufficientScope(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithRequiredScopes("read", "write"), fakeas.WithChallengeResourceMetadata())
	c := newClient(t)

	_, tok := c.loggedIn(s, "read")
	code, params := c.challenge(s.ResourceURL, tok.AccessToken)

	if code != http.StatusForbidden {
		t.Errorf("status %d, want 403", code)
	}

	want := map[string]string{"error": "insufficient_scope", "scope": "read write", "resource_metadata": s.PRMURL}

	if diff := cmp.Diff(want, params); diff != "" {
		t.Errorf("challenge params (-want +got):\n%s", diff)
	}

	_, stepped := c.loggedIn(s, strings.Fields(params["scope"])...)

	if code, _ := c.challenge(s.ResourceURL, stepped.AccessToken); code != http.StatusOK {
		t.Errorf("after step-up: status %d, want 200", code)
	}
}

// TestChallengeParams: a 401 that carries resource_metadata and scope, which
// discovery follows before any well-known probe.
func TestChallengeParams(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithChallengeResourceMetadata(), fakeas.WithChallengeScope("read", "admin"),
		fakeas.WithScopesSupported("read", "write", "admin"), fakeas.WithPRMLocation(fakeas.PRMRoot))
	c := newClient(t)

	code, params := c.challenge(s.ResourceURL, "")

	if code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", code)
	}

	want := map[string]string{"resource_metadata": s.PRMURL, "scope": "read admin"}

	if diff := cmp.Diff(want, params); diff != "" {
		t.Fatalf("challenge params (-want +got):\n%s", diff)
	}

	prm, err := oauthex.GetProtectedResourceMetadata(t.Context(), params["resource_metadata"], s.ResourceURL, c.http)

	if err != nil {
		t.Fatalf("GetProtectedResourceMetadata: %v", err)
	}

	wantScopes := []string{"read", "write", "admin"}

	if !cmp.Equal(prm.ScopesSupported, wantScopes) {
		t.Errorf("PRM scopes_supported = %v, want %v", prm.ScopesSupported, wantScopes)
	}

	if got := c.asMeta(s.MetadataURL).ScopesSupported; !cmp.Equal(got, wantScopes) {
		t.Errorf("AS scopes_supported = %v, want %v", got, wantScopes)
	}
}

// TestWithoutGET: GET is 405 whatever the token, while an unauthenticated
// POST still gets the challenge.
func TestWithoutGET(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithoutGET(), fakeas.WithChallengeScope("read"))
	c := newClient(t)
	_, tok := c.loggedIn(s)

	for _, token := range []string{"", tok.AccessToken} {
		resp := c.get(s.ResourceURL, token)

		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET with token %t: status %d, want 405", token != "", resp.StatusCode)
		}
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.ResourceURL, strings.NewReader("{}"))

	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := c.http.Do(req)

	if err != nil {
		t.Fatalf("POST resource: %v", err)
	}

	_ = resp.Body.Close()

	if got := resp.Header.Get("WWW-Authenticate"); resp.StatusCode != http.StatusUnauthorized || got != `Bearer scope="read"` {
		t.Errorf("POST = %d %q, want 401 with Bearer scope=\"read\"", resp.StatusCode, got)
	}

	want := []fakeas.ResourceRequest{
		{Method: http.MethodGet}, {Method: http.MethodGet, Bearer: true}, {Method: http.MethodPost},
	}

	if diff := cmp.Diff(want, s.ResourceRequests()); diff != "" {
		t.Errorf("ResourceRequests (-want +got):\n%s", diff)
	}
}

// postToken POSTs a form to the token endpoint and returns the status and
// the decoded JSON body.
func (c *client) postToken(s *fakeas.Server, form url.Values) (int, map[string]any) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodPost, s.TokenEndpoint, strings.NewReader(form.Encode()))

	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)

	if err != nil {
		c.t.Fatalf("POST token: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()
	var body map[string]any

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		c.t.Fatalf("decode token response: %v", err)
	}

	return resp.StatusCode, body
}

// TestTokenErrorShapes covers the quirks that change how the token
// endpoint reports errors and successes.
func TestTokenErrorShapes(t *testing.T) {
	tests := []struct {
		name        string
		opts        []fakeas.Option
		wantStatus  int
		wantError   map[string]any
		wantSuccess map[string]any // the fields besides the tokens
	}{
		{
			name:       "RFC 6749 by default",
			wantStatus: http.StatusBadRequest,
			wantError: map[string]any{
				"error": "invalid_grant", "error_description": "unknown, rotated, or revoked refresh token",
			},
			wantSuccess: map[string]any{"token_type": "Bearer", "expires_in": 3600.0},
		},
		{
			name:       "error status 200, like GitHub",
			opts:       []fakeas.Option{fakeas.WithTokenErrorStatus(http.StatusOK)},
			wantStatus: http.StatusOK,
			wantError: map[string]any{
				"error": "invalid_grant", "error_description": "unknown, rotated, or revoked refresh token",
			},
			wantSuccess: map[string]any{"token_type": "Bearer", "expires_in": 3600.0},
		},
		{
			name:        "Slack style",
			opts:        []fakeas.Option{fakeas.WithSlackStyleErrors()},
			wantStatus:  http.StatusOK,
			wantError:   map[string]any{"ok": false, "error": "invalid_grant"},
			wantSuccess: map[string]any{"ok": true, "token_type": "Bearer", "expires_in": 3600.0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeas.Start(t, tt.opts...)
			c := newClient(t)
			cfg, tok := c.loggedIn(s)

			status, body := c.postToken(s, url.Values{
				"grant_type": {"refresh_token"}, "refresh_token": {"rt-unknown"}, "client_id": {cfg.ClientID},
			})

			if status != tt.wantStatus {
				t.Errorf("error status = %d, want %d", status, tt.wantStatus)
			}

			if diff := cmp.Diff(tt.wantError, body); diff != "" {
				t.Errorf("error body (-want +got):\n%s", diff)
			}

			status, body = c.postToken(s, url.Values{
				"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}, "client_id": {cfg.ClientID},
			})

			if status != http.StatusOK || body["access_token"] == nil || body["refresh_token"] == nil {
				t.Fatalf("refresh = %d %v, want 200 with tokens", status, body)
			}

			delete(body, "access_token")
			delete(body, "refresh_token")

			if diff := cmp.Diff(tt.wantSuccess, body); diff != "" {
				t.Errorf("success body (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFailTokenRequests makes the token endpoint fail with a chosen code
// until it is cleared.
func TestFailTokenRequests(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithSlackStyleErrors())
	c := newClient(t)
	cfg, tok := c.loggedIn(s)
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}, "client_id": {cfg.ClientID}}

	s.FailTokenRequests("ratelimited")
	status, body := c.postToken(s, refresh)

	if diff := cmp.Diff(map[string]any{"ok": false, "error": "ratelimited"}, body); status != http.StatusOK || diff != "" {
		t.Errorf("failing refresh = %d, body (-want +got):\n%s", status, diff)
	}

	s.FailTokenRequests("")

	if status, body := c.postToken(s, refresh); status != http.StatusOK || body["ok"] != true {
		t.Errorf("refresh after clearing = %d %v, want 200 ok", status, body)
	}

	var outcomes []string

	for _, r := range s.TokenRequests() {
		outcomes = append(outcomes, r.GrantType+":"+r.Error)
	}

	want := []string{"authorization_code:", "refresh_token:ratelimited", "refresh_token:"}

	if diff := cmp.Diff(want, outcomes); diff != "" {
		t.Errorf("token request outcomes (-want +got):\n%s", diff)
	}
}

// TestRevokeClient forgets a registered client, as a server that expires
// or deletes DCR clients does: its token requests then fail with
// invalid_client, and a new registration still works.
func TestRevokeClient(t *testing.T) {
	s := fakeas.Start(t)
	c := newClient(t)
	cfg, tok := c.loggedIn(s)
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}, "client_id": {cfg.ClientID}}

	s.RevokeClient(cfg.ClientID)

	status, body := c.postToken(s, refresh)

	if status != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Errorf("refresh with a revoked client = %d %v, want 401 invalid_client", status, body)
	}

	if n := len(s.Clients()); n != 0 {
		t.Errorf("Clients() lists %d clients after revocation, want 0", n)
	}

	fresh, _ := c.loggedIn(s)

	if fresh.ClientID == cfg.ClientID {
		t.Error("a new registration reused the revoked client_id")
	}

	if diff := cmp.Diff([]string{fresh.ClientID}, clientIDs(s.Clients())); diff != "" {
		t.Errorf("Clients() after re-registering (-want +got):\n%s", diff)
	}
}

func clientIDs(cs []fakeas.Client) []string {
	ids := make([]string, 0, len(cs))

	for _, c := range cs {
		ids = append(ids, c.ID)
	}

	return ids
}
