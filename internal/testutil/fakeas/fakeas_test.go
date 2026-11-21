package fakeas_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

const redirectURI = "http://127.0.0.1:7777/oauth/callback"

// client is the OAuth client side the self-tests drive the fake with: plain
// net/http for discovery, oauthex for parsing and DCR, and x/oauth2 for the
// authorization code and refresh grants, the way design §7.3 does it.
type client struct {
	t    *testing.T
	http *http.Client
}

func newClient(t *testing.T) *client {
	t.Helper()

	// oauthex refuses loopback dials through a bare *http.Transport; any
	// other RoundTripper opts out of that check.
	return &client{t: t, http: &http.Client{
		Transport:     wrapped{http.DefaultTransport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type wrapped struct{ http.RoundTripper }

func (c *client) ctx() context.Context {
	return context.WithValue(c.t.Context(), oauth2.HTTPClient, c.http)
}

// response is a fully read HTTP response.
type response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// get fetches url with an optional bearer token. It does not follow
// redirects.
func (c *client) get(rawURL, token string) response {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodGet, rawURL, nil)

	if err != nil {
		c.t.Fatal(err)
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)

	if err != nil {
		c.t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)

	if err != nil {
		c.t.Fatalf("GET %s: read body: %v", rawURL, err)
	}

	return response{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}
}

// getJSON fetches a metadata document. It returns the status code, and
// decodes the body into v only on 200.
func (c *client) getJSON(rawURL string, v any) int {
	c.t.Helper()
	resp := c.get(rawURL, "")

	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode
	}

	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "application/json" {
		c.t.Fatalf("GET %s: Content-Type %q, want application/json", rawURL, mt)
	}

	if err := json.Unmarshal(resp.Body, v); err != nil {
		c.t.Fatalf("GET %s: decode: %v", rawURL, err)
	}

	return resp.StatusCode
}

// challenge requests the resource with token and returns the status and the
// parsed Bearer challenge parameters.
func (c *client) challenge(resourceURL, token string) (int, map[string]string) {
	c.t.Helper()
	resp := c.get(resourceURL, token)

	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, nil
	}

	chs, err := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))

	if err != nil || len(chs) != 1 || chs[0].Scheme != "bearer" {
		c.t.Fatalf("WWW-Authenticate %q: challenges %v, err %v; want one Bearer challenge",
			resp.Header.Values("WWW-Authenticate"), chs, err)
	}

	return resp.StatusCode, chs[0].Params
}

func (c *client) prm(rawURL string) *oauthex.ProtectedResourceMetadata {
	c.t.Helper()
	var prm oauthex.ProtectedResourceMetadata

	if code := c.getJSON(rawURL, &prm); code != http.StatusOK {
		c.t.Fatalf("GET PRM %s: status %d", rawURL, code)
	}

	return &prm
}

func (c *client) asMeta(rawURL string) *oauthex.AuthServerMeta {
	c.t.Helper()
	var asm oauthex.AuthServerMeta

	if code := c.getJSON(rawURL, &asm); code != http.StatusOK {
		c.t.Fatalf("GET AS metadata %s: status %d", rawURL, code)
	}

	return &asm
}

func (c *client) register(endpoint string, meta *oauthex.ClientRegistrationMetadata) *oauthex.ClientRegistrationResponse {
	c.t.Helper()

	if meta == nil {
		meta = &oauthex.ClientRegistrationMetadata{
			RedirectURIs:            []string{redirectURI},
			TokenEndpointAuthMethod: "none",
			ClientName:              "wayfindr",
		}
	}

	reg, err := oauthex.RegisterClient(c.t.Context(), endpoint, meta, c.http)

	if err != nil {
		c.t.Fatalf("RegisterClient: %v", err)
	}

	return reg
}

// authorize visits the authorization endpoint and returns the query of the
// redirect back to the client, or fails the test if there is none.
func (c *client) authorize(cfg *oauth2.Config, state string, opts ...oauth2.AuthCodeOption) url.Values {
	c.t.Helper()
	resp := c.get(cfg.AuthCodeURL(state, opts...), "")

	if resp.StatusCode != http.StatusFound {
		c.t.Fatalf("authorize: status %d, want 302", resp.StatusCode)
	}

	loc, err := url.Parse(resp.Header.Get("Location"))

	if err != nil {
		c.t.Fatal(err)
	}

	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != cfg.RedirectURL {
		c.t.Fatalf("authorize redirected to %s, want %s", got, cfg.RedirectURL)
	}

	return loc.Query()
}

// oauthErrorCode returns the OAuth error code of a token endpoint failure.
func oauthErrorCode(t *testing.T, err error) string {
	t.Helper()
	var re *oauth2.RetrieveError

	if !errors.As(err, &re) {
		t.Fatalf("error %v is not an *oauth2.RetrieveError", err)
	}

	return re.ErrorCode
}

func resourceOpt(s *fakeas.Server) oauth2.AuthCodeOption {
	return oauth2.SetAuthURLParam("resource", s.ResourceURL)
}

// loggedIn runs DCR, authorize with PKCE, and the code exchange against s,
// and returns the config and token.
func (c *client) loggedIn(s *fakeas.Server, scopes ...string) (*oauth2.Config, *oauth2.Token) {
	c.t.Helper()
	reg := c.register(s.RegistrationEndpoint, nil)
	cfg := &oauth2.Config{
		ClientID:    reg.ClientID,
		RedirectURL: redirectURI,
		Scopes:      scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:   s.AuthorizationEndpoint,
			TokenURL:  s.TokenEndpoint,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
	verifier := oauth2.GenerateVerifier()
	q := c.authorize(cfg, "st", oauth2.S256ChallengeOption(verifier), resourceOpt(s))
	tok, err := cfg.Exchange(c.ctx(), q.Get("code"), oauth2.VerifierOption(verifier), resourceOpt(s))

	if err != nil {
		c.t.Fatalf("Exchange: %v", err)
	}

	return cfg, tok
}

// refresh forces a refresh_token grant.
func (c *client) refresh(cfg *oauth2.Config, tok *oauth2.Token) (*oauth2.Token, error) {
	expired := *tok
	expired.Expiry = time.Now().Add(-time.Hour)

	return cfg.TokenSource(c.ctx(), &expired).Token()
}

// TestDefaultFlow walks the full ADR 0007 chain against a default fake: a
// 401 from the resource, PRM at the path-inserted well-known URL, RFC 8414
// metadata at the root issuer, DCR, authorize with PKCE S256 and a resource
// indicator, the code exchange, a call to the resource, and a refresh.
func TestDefaultFlow(t *testing.T) {
	s := fakeas.Start(t)
	c := newClient(t)

	code, params := c.challenge(s.ResourceURL, "")

	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated resource request: status %d, want 401", code)
	}

	if len(params) != 0 {
		t.Errorf("default challenge params = %v, want a bare Bearer challenge", params)
	}

	prm := c.prm(s.URL + "/.well-known/oauth-protected-resource/mcp")

	if prm.Resource != s.ResourceURL || !cmp.Equal(prm.AuthorizationServers, []string{s.Issuer}) {
		t.Errorf("PRM resource %q servers %v, want %q [%s]", prm.Resource, prm.AuthorizationServers, s.ResourceURL, s.Issuer)
	}

	if s.Issuer != s.URL {
		t.Errorf("default Issuer = %q, want the root %q", s.Issuer, s.URL)
	}

	asm := c.asMeta(s.URL + "/.well-known/oauth-authorization-server")
	want := oauthex.AuthServerMeta{
		Issuer:                                     s.Issuer,
		AuthorizationEndpoint:                      s.URL + "/authorize",
		TokenEndpoint:                              s.URL + "/token",
		RegistrationEndpoint:                       s.URL + "/register",
		ResponseTypesSupported:                     []string{"code"},
		GrantTypesSupported:                        []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported:          []string{"client_secret_basic", "client_secret_post"},
		CodeChallengeMethodsSupported:              []string{"S256"},
		AuthorizationResponseIssParameterSupported: true,
	}

	if diff := cmp.Diff(want, *asm); diff != "" {
		t.Errorf("AS metadata (-want +got):\n%s", diff)
	}

	reg := c.register(asm.RegistrationEndpoint, nil)
	cfg := &oauth2.Config{
		ClientID:    reg.ClientID,
		RedirectURL: redirectURI,
		Scopes:      []string{"read"},
		Endpoint: oauth2.Endpoint{
			AuthURL:   asm.AuthorizationEndpoint,
			TokenURL:  asm.TokenEndpoint,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
	verifier := oauth2.GenerateVerifier()
	q := c.authorize(cfg, "state-1", oauth2.S256ChallengeOption(verifier), resourceOpt(s))

	if q.Get("state") != "state-1" || q.Get("iss") != s.Issuer || q.Get("code") == "" {
		t.Fatalf("authorize redirect query %v, want state=state-1, iss=%s and a code", q, s.Issuer)
	}

	tok, err := cfg.Exchange(c.ctx(), q.Get("code"), oauth2.VerifierOption(verifier), resourceOpt(s))

	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	if tok.RefreshToken == "" || tok.Type() != "Bearer" || tok.Expiry.IsZero() {
		t.Errorf("token = %+v, want a Bearer token with a refresh token and an expiry", tok)
	}

	if code, _ := c.challenge(s.ResourceURL, tok.AccessToken); code != http.StatusOK {
		t.Errorf("resource with token: status %d, want 200", code)
	}

	refreshed, err := c.refresh(cfg, tok)

	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if refreshed.AccessToken == tok.AccessToken {
		t.Error("refresh returned the same access token")
	}

	if refreshed.RefreshToken != tok.RefreshToken {
		t.Error("non-rotating fake changed the refresh token")
	}

	if code, _ := c.challenge(s.ResourceURL, refreshed.AccessToken); code != http.StatusOK {
		t.Errorf("resource with refreshed token: status %d, want 200", code)
	}

	gotReqs := s.TokenRequests()
	wantReqs := []fakeas.TokenRequest{
		{GrantType: "authorization_code", ClientID: reg.ClientID, Resource: s.ResourceURL, AuthMethod: "none"},
		{GrantType: "refresh_token", ClientID: reg.ClientID, AuthMethod: "none"},
	}

	if diff := cmp.Diff(wantReqs, gotReqs); diff != "" {
		t.Errorf("TokenRequests (-want +got):\n%s", diff)
	}

	wantAuthz := []fakeas.Authorization{{
		ClientID: reg.ClientID, RedirectURI: redirectURI, Scope: "read", Resource: s.ResourceURL,
		State: "state-1", CodeChallengeMethod: "S256",
	}}

	if diff := cmp.Diff(wantAuthz, s.Authorizations()); diff != "" {
		t.Errorf("Authorizations (-want +got):\n%s", diff)
	}

	if got := s.Clients(); len(got) != 1 || got[0].ID != reg.ClientID {
		t.Errorf("Clients() = %+v, want the one DCR client", got)
	}
}

// wellKnown lists every URL ADR 0007 probes for issuer path p on origin.
// Without a path, the two OIDC forms are one URL.
func wellKnown(origin, p string) []string {
	return slices.Compact([]string{
		origin + "/.well-known/oauth-protected-resource/mcp",
		origin + "/.well-known/oauth-protected-resource",
		origin + "/.well-known/oauth-authorization-server" + p,
		origin + "/.well-known/openid-configuration" + p,
		origin + p + "/.well-known/openid-configuration",
	})
}

// served returns the subset of urls that answer 200.
func (c *client) served(urls []string) []string {
	c.t.Helper()
	var ok []string

	for _, u := range urls {
		if c.getJSON(u, new(map[string]any)) == http.StatusOK {
			ok = append(ok, u)
		}
	}

	return ok
}

func TestMetadataLocations(t *testing.T) {
	tests := []struct {
		name     string
		opts     []fakeas.Option
		issPath  string
		wantPath string
	}{
		{"root RFC 8414", nil, "", "/.well-known/oauth-authorization-server"},
		{"root OIDC", []fakeas.Option{fakeas.WithMetadataLocation(fakeas.OIDCPathInserted)}, "",
			"/.well-known/openid-configuration"},
		{"path-inserted RFC 8414", []fakeas.Option{fakeas.WithIssuerPath("tenant1")}, "/tenant1",
			"/.well-known/oauth-authorization-server/tenant1"},
		{"path-inserted OIDC", []fakeas.Option{
			fakeas.WithIssuerPath("/tenant1"), fakeas.WithMetadataLocation(fakeas.OIDCPathInserted),
		}, "/tenant1", "/.well-known/openid-configuration/tenant1"},
		{"path-appended OIDC", []fakeas.Option{
			fakeas.WithIssuerPath("/tenant1/"), fakeas.WithMetadataLocation(fakeas.OIDCPathAppended),
		}, "/tenant1", "/tenant1/.well-known/openid-configuration"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeas.Start(t, tt.opts...)
			c := newClient(t)

			if s.Issuer != s.URL+tt.issPath || s.MetadataURL != s.URL+tt.wantPath {
				t.Fatalf("Issuer %q MetadataURL %q, want %q %q", s.Issuer, s.MetadataURL, s.URL+tt.issPath, s.URL+tt.wantPath)
			}

			wantServed := []string{s.URL + "/.well-known/oauth-protected-resource/mcp", s.URL + tt.wantPath}

			if diff := cmp.Diff(wantServed, c.served(wellKnown(s.URL, tt.issPath))); diff != "" {
				t.Errorf("well-known URLs served (-want +got):\n%s", diff)
			}

			prm := c.prm(s.PRMURL)
			asm := c.asMeta(s.URL + tt.wantPath)

			if !cmp.Equal(prm.AuthorizationServers, []string{s.Issuer}) || asm.Issuer != s.Issuer {
				t.Errorf("PRM servers %v, metadata issuer %q, want %s", prm.AuthorizationServers, asm.Issuer, s.Issuer)
			}

			if want := s.Issuer + "/token"; asm.TokenEndpoint != want {
				t.Errorf("token_endpoint %q, want %q", asm.TokenEndpoint, want)
			}

			_, tok := c.loggedIn(s)

			if code, _ := c.challenge(s.ResourceURL, tok.AccessToken); code != http.StatusOK {
				t.Errorf("resource with token: status %d, want 200", code)
			}
		})
	}
}

func TestPRMLocations(t *testing.T) {
	tests := []struct {
		name string
		loc  fakeas.PRMLocation
		want string
	}{
		{"path-inserted", fakeas.PRMPathInserted, "/.well-known/oauth-protected-resource/mcp"},
		{"root", fakeas.PRMRoot, "/.well-known/oauth-protected-resource"},
		{"unlisted", fakeas.PRMUnlisted, "/resource-metadata/mcp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeas.Start(t, fakeas.WithPRMLocation(tt.loc))
			c := newClient(t)

			if s.PRMURL != s.URL+tt.want {
				t.Errorf("PRMURL = %q, want %q", s.PRMURL, s.URL+tt.want)
			}

			want := []string{s.URL + tt.want, s.URL + "/.well-known/oauth-authorization-server"}

			if tt.loc == fakeas.PRMUnlisted {
				// Found only through the challenge's resource_metadata.
				want = want[1:]
			}

			if diff := cmp.Diff(want, c.served(wellKnown(s.URL, ""))); diff != "" {
				t.Errorf("well-known URLs served (-want +got):\n%s", diff)
			}

			// A real client checks resource against the URL it protects.
			prm, err := oauthex.GetProtectedResourceMetadata(t.Context(), s.PRMURL, s.ResourceURL, c.http)

			if err != nil || !cmp.Equal(prm.AuthorizationServers, []string{s.Issuer}) {
				t.Errorf("GetProtectedResourceMetadata = %+v, %v; want servers [%s]", prm, err, s.Issuer)
			}
		})
	}
}

// TestWithoutMetadata is the 2025-03-26 legacy fallback: nothing at any
// well-known URL, and the default endpoints on the MCP origin.
func TestWithoutMetadata(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithoutMetadata(), fakeas.WithIssuerPath("/ignored"),
		fakeas.WithChallengeResourceMetadata())
	c := newClient(t)

	if got := c.served(append(wellKnown(s.URL, ""), wellKnown(s.URL, "/ignored")...)); len(got) != 0 {
		t.Errorf("well-known URLs served = %v, want none", got)
	}

	if s.PRMURL != "" || s.MetadataURL != "" {
		t.Errorf("PRMURL %q MetadataURL %q, want both empty", s.PRMURL, s.MetadataURL)
	}

	if _, params := c.challenge(s.ResourceURL, ""); len(params) != 0 {
		t.Errorf("challenge params = %v, want none (there is no PRM to point at)", params)
	}

	got := []string{s.Issuer, s.AuthorizationEndpoint, s.TokenEndpoint, s.RegistrationEndpoint}
	want := []string{s.URL, s.URL + "/authorize", s.URL + "/token", s.URL + "/register"}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("issuer and endpoints (-want +got):\n%s", diff)
	}

	_, tok := c.loggedIn(s)

	if code, _ := c.challenge(s.ResourceURL, tok.AccessToken); code != http.StatusOK {
		t.Errorf("resource with token: status %d, want 200", code)
	}
}
