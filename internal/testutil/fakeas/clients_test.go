package fakeas_test

import (
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// TestConfidentialDCRClient registers with client_secret_basic and
// authenticates to the token endpoint in the Basic header.
func TestConfidentialDCRClient(t *testing.T) {
	s := fakeas.Start(t)
	c := newClient(t)
	reg := c.register(s.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
		RedirectURIs: []string{redirectURI}, ClientName: "wayfindr",
	})

	if reg.ClientSecret == "" {
		t.Fatal("DCR with the default auth method returned no client_secret")
	}

	want := []fakeas.Client{{
		ID: reg.ClientID, Secret: reg.ClientSecret, RedirectURIs: []string{redirectURI}, Name: "wayfindr",
		AuthMethod: "client_secret_basic",
	}}

	if diff := cmp.Diff(want, s.Clients()); diff != "" {
		t.Errorf("Clients (-want +got):\n%s", diff)
	}

	cfg := &oauth2.Config{
		ClientID: reg.ClientID, ClientSecret: reg.ClientSecret, RedirectURL: redirectURI,
		Endpoint: oauth2.Endpoint{AuthURL: s.AuthorizationEndpoint, TokenURL: s.TokenEndpoint, AuthStyle: oauth2.AuthStyleInHeader},
	}
	v := oauth2.GenerateVerifier()
	q := c.authorize(cfg, "st", oauth2.S256ChallengeOption(v))

	if _, err := cfg.Exchange(c.ctx(), q.Get("code"), oauth2.VerifierOption(v)); err != nil {
		t.Fatalf("Exchange with client_secret_basic: %v", err)
	}

	bad := *cfg
	bad.ClientSecret = "wrong"
	q = c.authorize(&bad, "st", oauth2.S256ChallengeOption(v))

	if _, err := bad.Exchange(c.ctx(), q.Get("code"), oauth2.VerifierOption(v)); err == nil || oauthErrorCode(t, err) != "invalid_client" {
		t.Errorf("Exchange with a wrong secret: %v, want invalid_client", err)
	}
}

func TestDCRRejectsMissingRedirectURIs(t *testing.T) {
	s := fakeas.Start(t)
	c := newClient(t)
	_, err := oauthex.RegisterClient(t.Context(), s.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{}, c.http)
	var regErr *oauthex.ClientRegistrationError

	if !errors.As(err, &regErr) || regErr.ErrorCode != "invalid_redirect_uri" {
		t.Errorf("RegisterClient without redirect_uris: %v, want invalid_redirect_uri", err)
	}
}

// TestWithoutDCR: no registration endpoint, so only a pre-registered
// (bring-your-own-app) client can authorize.
func TestWithoutDCR(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithoutDCR(), fakeas.WithClient("byo-app", "byo-secret", redirectURI))
	c := newClient(t)

	if got := c.asMeta(s.MetadataURL).RegistrationEndpoint; got != "" || s.RegistrationEndpoint != "" {
		t.Errorf("registration endpoint advertised %q, field %q; want none", got, s.RegistrationEndpoint)
	}

	if code := c.getJSON(s.URL+"/register", new(any)); code == http.StatusOK {
		t.Error("GET /register answered 200")
	}

	if _, err := oauthex.RegisterClient(t.Context(), s.URL+"/register",
		&oauthex.ClientRegistrationMetadata{RedirectURIs: []string{redirectURI}}, c.http); err == nil {
		t.Error("RegisterClient succeeded without DCR")
	}

	cfg := &oauth2.Config{
		ClientID: "byo-app", ClientSecret: "byo-secret", RedirectURL: redirectURI,
		Endpoint: oauth2.Endpoint{AuthURL: s.AuthorizationEndpoint, TokenURL: s.TokenEndpoint, AuthStyle: oauth2.AuthStyleInParams},
	}
	v := oauth2.GenerateVerifier()
	q := c.authorize(cfg, "st", oauth2.S256ChallengeOption(v))

	if _, err := cfg.Exchange(c.ctx(), q.Get("code"), oauth2.VerifierOption(v)); err != nil {
		t.Fatalf("Exchange with client_secret_post: %v", err)
	}

	want := []fakeas.Client{{ID: "byo-app", Secret: "byo-secret", RedirectURIs: []string{redirectURI}}}

	if diff := cmp.Diff(want, s.Clients()); diff != "" {
		t.Errorf("Clients (-want +got):\n%s", diff)
	}
}

// TestAuthorizeRejectsUnknownClients: an unknown client or redirect URI gets
// an error page, never a redirect (RFC 6749 §4.1.2.1).
func TestAuthorizeRejectsUnknownClients(t *testing.T) {
	tests := []struct {
		name, clientID, redirect string
	}{
		{"unknown client", "nobody", redirectURI},
		{"unregistered redirect", "byo-app", "http://127.0.0.1:9999/evil"},
		{"https client_id without CIMD", "https://client.example/meta.json", redirectURI},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeas.Start(t, fakeas.WithClient("byo-app", "", redirectURI))
			c := newClient(t)
			cfg := &oauth2.Config{ClientID: tt.clientID, RedirectURL: tt.redirect,
				Endpoint: oauth2.Endpoint{AuthURL: s.AuthorizationEndpoint}}
			resp := c.get(cfg.AuthCodeURL("st", oauth2.S256ChallengeOption(oauth2.GenerateVerifier())), "")

			if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
				t.Errorf("status %d Location %q, want 400 and no redirect", resp.StatusCode, resp.Header.Get("Location"))
			}
		})
	}
}

// TestCIMD: client_id_metadata_document_supported is advertised, and an
// https client_id works as a public client without registration.
func TestCIMD(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithClientIDMetadataDocuments())
	c := newClient(t)

	if !c.asMeta(s.MetadataURL).ClientIDMetadataDocumentSupported {
		t.Error("client_id_metadata_document_supported not advertised")
	}

	if c.asMeta(fakeas.Start(t).MetadataURL).ClientIDMetadataDocumentSupported {
		t.Error("default fake advertises client_id_metadata_document_supported")
	}

	cfg := &oauth2.Config{
		ClientID: "https://client.example/wayfindr.json", RedirectURL: redirectURI,
		Endpoint: oauth2.Endpoint{AuthURL: s.AuthorizationEndpoint, TokenURL: s.TokenEndpoint, AuthStyle: oauth2.AuthStyleInParams},
	}
	v := oauth2.GenerateVerifier()
	q := c.authorize(cfg, "st", oauth2.S256ChallengeOption(v))

	if _, err := cfg.Exchange(c.ctx(), q.Get("code"), oauth2.VerifierOption(v)); err != nil {
		t.Fatalf("Exchange with a CIMD client_id: %v", err)
	}
}

// TestResourceIndicator covers RFC 8707: the resource may be omitted
// (resource_indicator = false), but if sent it must name the resource.
func TestResourceIndicator(t *testing.T) {
	s := fakeas.Start(t)
	c := newClient(t)
	cfg := c.publicConfig(s)
	v := oauth2.GenerateVerifier()

	wrong := oauth2.SetAuthURLParam("resource", s.URL+"/other")

	if q := c.authorize(cfg, "st", oauth2.S256ChallengeOption(v), wrong); q.Get("error") != "invalid_target" {
		t.Errorf("authorize with a foreign resource: %v, want error=invalid_target", q)
	}

	q := c.authorize(cfg, "st", oauth2.S256ChallengeOption(v))

	if _, err := cfg.Exchange(c.ctx(), q.Get("code"), oauth2.VerifierOption(v), wrong); err == nil || oauthErrorCode(t, err) != "invalid_target" {
		t.Errorf("token with a foreign resource: %v, want invalid_target", err)
	}

	q = c.authorize(cfg, "st", oauth2.S256ChallengeOption(v))

	if _, err := cfg.Exchange(c.ctx(), q.Get("code"), oauth2.VerifierOption(v)); err != nil {
		t.Errorf("token without a resource: %v", err)
	}
}

// TestCodeChecks: a code is single-use and bound to its redirect URI.
func TestCodeChecks(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithClient("app", "", redirectURI, "http://127.0.0.1:7777/other"))
	c := newClient(t)
	cfg := &oauth2.Config{ClientID: "app", RedirectURL: redirectURI,
		Endpoint: oauth2.Endpoint{AuthURL: s.AuthorizationEndpoint, TokenURL: s.TokenEndpoint, AuthStyle: oauth2.AuthStyleInParams}}
	v := oauth2.GenerateVerifier()

	q := c.authorize(cfg, "st", oauth2.S256ChallengeOption(v))

	if _, err := cfg.Exchange(c.ctx(), q.Get("code"), oauth2.VerifierOption(v)); err != nil {
		t.Fatalf("first Exchange: %v", err)
	}

	if _, err := cfg.Exchange(c.ctx(), q.Get("code"), oauth2.VerifierOption(v)); err == nil || oauthErrorCode(t, err) != "invalid_grant" {
		t.Errorf("reused code: %v, want invalid_grant", err)
	}

	q = c.authorize(cfg, "st", oauth2.S256ChallengeOption(v))
	other := *cfg
	other.RedirectURL = "http://127.0.0.1:7777/other"

	if _, err := other.Exchange(c.ctx(), q.Get("code"), oauth2.VerifierOption(v)); err == nil || oauthErrorCode(t, err) != "invalid_grant" {
		t.Errorf("code redeemed with another redirect_uri: %v, want invalid_grant", err)
	}
}

func TestResourceHandler(t *testing.T) {
	auths := make(chan string, 1)
	s := fakeas.Start(t, fakeas.WithResourceHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths <- r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "upstream")
	})))
	c := newClient(t)
	_, tok := c.loggedIn(s)
	resp := c.get(s.ResourceURL, tok.AccessToken)

	if resp.StatusCode != http.StatusOK || string(resp.Body) != "upstream" {
		t.Errorf("resource answered %d %q; want the handler's 200 \"upstream\"", resp.StatusCode, resp.Body)
	}

	if got := <-auths; got != "Bearer "+tok.AccessToken {
		t.Errorf("handler saw Authorization %q, want the bearer token", got)
	}
}

func TestTokenLifetime(t *testing.T) {
	s := fakeas.Start(t, fakeas.WithTokenLifetime(90*time.Second))
	c := newClient(t)
	before := time.Now()
	_, tok := c.loggedIn(s)

	if d := tok.Expiry.Sub(before); d < 85*time.Second || d > 95*time.Second {
		t.Errorf("token expires in %v, want about 90s", d)
	}
}
