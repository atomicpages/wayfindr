// Package fakeas provides a fake OAuth authorization server, with a protected
// fake MCP resource, for testing the auth broker (design §7.3, ADR 0007).
//
//	s := fakeas.Start(t)                           // spec-compliant defaults
//	s := fakeas.Start(t, fakeas.WithoutMetadata()) // a 2025-03-26 legacy server
//
// One httptest server hosts everything:
//
//   - the protected resource at [Server.ResourceURL] (path /mcp), which answers
//     a missing or bad bearer token with 401 and WWW-Authenticate, a token
//     lacking [WithRequiredScopes] with 403 insufficient_scope, and anything
//     else with the [WithResourceHandler] handler;
//   - Protected Resource Metadata (RFC 9728) at [Server.PRMURL];
//   - authorization server metadata (RFC 8414 or OIDC discovery) at
//     [Server.MetadataURL];
//   - DCR (RFC 7591), authorize, token, and device authorization (RFC 8628)
//     endpoints under [Server.Issuer].
//
// The authorize endpoint has no consent page: it validates the request and
// redirects straight back with code, state, and iss (RFC 9207). PKCE S256 is
// required and checked (RFC 7636), and the resource indicator, when sent,
// must name the resource (RFC 8707). The token endpoint also serves the
// unattended grants: client_credentials for an authenticated client, and
// the JWT bearer grant with [WithJWTBearerKey].
//
// Options are quirk switches that reproduce real-world servers; methods such
// as [Server.Revoke] change behavior mid-test; accessors such as
// [Server.TokenRequests] report what clients did.
package fakeas

import (
	"crypto"
	"crypto/x509"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// ResourcePath is the path of the protected fake MCP resource.
const ResourcePath = "/mcp"

// PRMLocation is a well-known location for Protected Resource Metadata
// (RFC 9728 §3.1).
type PRMLocation int

const (
	// PRMPathInserted serves PRM at /.well-known/oauth-protected-resource/mcp.
	PRMPathInserted PRMLocation = iota
	// PRMRoot serves PRM at /.well-known/oauth-protected-resource.
	PRMRoot
	// PRMUnlisted serves PRM at /resource-metadata/mcp, which is no
	// well-known location: a client finds it only through the
	// resource_metadata of the challenge ([WithChallengeResourceMetadata]).
	PRMUnlisted
)

// MetadataLocation is a well-known location for authorization server
// metadata, relative to an issuer with path {path} (ADR 0007 step 2).
type MetadataLocation int

const (
	// OAuthPathInserted serves RFC 8414 metadata at
	// /.well-known/oauth-authorization-server{path}.
	OAuthPathInserted MetadataLocation = iota
	// OIDCPathInserted serves metadata at /.well-known/openid-configuration{path}.
	OIDCPathInserted
	// OIDCPathAppended serves metadata at {path}/.well-known/openid-configuration.
	OIDCPathAppended
)

// Option is a quirk switch.
type Option func(*config)

type config struct {
	issuerPath        string
	prmLocation       PRMLocation
	metadataLocation  MetadataLocation
	noMetadata        bool
	noPKCE            bool
	issuerMismatch    bool
	prmIssuerSlash    bool
	rotateRefresh     bool
	noDCR             bool
	cimd              bool
	requiredScopes    []string
	scopesSupported   []string
	challengeMetadata bool
	challengeScope    []string
	noGET             bool
	anonymousSession  bool
	lifetime          time.Duration
	refreshLifetime   time.Duration
	secretExpiry      time.Time
	tokenErrorStatus  int
	slackErrors       bool
	clients           []Client
	resource          http.Handler
	device            deviceConfig
	unattended        unattendedConfig
	tokenAuthMethods  []string
	signingAlgs       []string
	clientKeys        map[string]crypto.PublicKey
	dcrAuthMethod     string
	terseDCR          bool
	strictClientAuth  bool
	tls               bool
	clientCAs         *x509.CertPool
	mtlsAliases       bool
	certBound         bool
	dpop              dpopConfig
}

// DefaultTokenAuthMethods are the token_endpoint_auth_methods_supported the
// metadata lists unless [WithTokenAuthMethods] says otherwise: the two
// client secret methods, as most confidential-client servers list.
func DefaultTokenAuthMethods() []string {
	return []string{"client_secret_basic", "client_secret_post"}
}

// WithTokenAuthMethods lists methods as token_endpoint_auth_methods_supported
// in the metadata; with none, the metadata leaves the field out (which RFC
// 8414 reads as client_secret_basic). The token endpoint accepts every
// method it can check whatever the list says; see [WithStrictClientAuth].
func WithTokenAuthMethods(methods ...string) Option {
	return func(c *config) { c.tokenAuthMethods = methods }
}

// WithSigningAlgs lists algs as token_endpoint_auth_signing_alg_values_supported
// in the metadata.
func WithSigningAlgs(algs ...string) Option {
	return func(c *config) { c.signingAlgs = algs }
}

// WithClientKey registers key, an ECDSA, RSA, or Ed25519 public key, as the
// key clientID signs its private_key_jwt client assertions with. Register
// the client itself with [WithClient].
func WithClientKey(clientID string, key crypto.PublicKey) Option {
	return func(c *config) {
		if c.clientKeys == nil {
			c.clientKeys = map[string]crypto.PublicKey{}
		}

		c.clientKeys[clientID] = key
	}
}

// WithDCRAuthMethod makes DCR register every client with method, whatever
// token_endpoint_auth_method the request asked for, as RFC 7591 §3.2.1
// lets a server do. A "none" client gets no secret.
func WithDCRAuthMethod(method string) Option {
	return func(c *config) { c.dcrAuthMethod = method }
}

// WithTerseDCR makes DCR register every client as confidential, whatever
// it asked for, and answer with only client_id, client_secret, and
// redirect_uris, leaving token_endpoint_auth_method out, as some servers
// (the MCP conformance suite's among them) do.
func WithTerseDCR() Option {
	return func(c *config) { c.terseDCR = true }
}

// WithStrictClientAuth makes the token endpoint refuse, with
// invalid_client, a request from a client registered through DCR that
// authenticates with another method than the one it was registered with.
func WithStrictClientAuth() Option {
	return func(c *config) { c.strictClientAuth = true }
}

// WithIssuerPath gives the issuer a path, such as "/tenant1", so metadata is
// served at path-inserted or path-appended well-known URLs and the
// endpoints live under the path. The default issuer is the server root.
func WithIssuerPath(path string) Option {
	return func(c *config) { c.issuerPath = "/" + strings.Trim(path, "/") }
}

// WithPRMLocation chooses where PRM is served. The default is
// [PRMPathInserted].
func WithPRMLocation(loc PRMLocation) Option { return func(c *config) { c.prmLocation = loc } }

// WithMetadataLocation chooses where authorization server metadata is
// served. The default is [OAuthPathInserted].
func WithMetadataLocation(loc MetadataLocation) Option {
	return func(c *config) { c.metadataLocation = loc }
}

// WithoutMetadata serves no PRM and no authorization server metadata at all,
// like a 2025-03-26 server. The issuer is then the server root (any
// [WithIssuerPath] is ignored), so the endpoints are the legacy defaults
// /authorize, /token, and /register on the MCP origin.
func WithoutMetadata() Option { return func(c *config) { c.noMetadata = true } }

// WithoutPKCEAdvertised omits code_challenge_methods_supported from the
// metadata. PKCE then becomes optional: authorize accepts a request without
// a code_challenge, but a challenge that is sent is still checked.
func WithoutPKCEAdvertised() Option { return func(c *config) { c.noPKCE = true } }

// WithIssuerMismatch makes the metadata's issuer, and the iss parameter on
// the authorize redirect, differ from the issuer URL that PRM lists and the
// metadata was discovered at (like Google). See [Server.MetadataIssuer].
func WithIssuerMismatch() Option { return func(c *config) { c.issuerMismatch = true } }

// WithPRMIssuerTrailingSlash makes PRM list the issuer with a terminating
// "/" while the metadata reports it without one, as Google's MCP servers
// list https://accounts.google.com/.
func WithPRMIssuerTrailingSlash() Option { return func(c *config) { c.prmIssuerSlash = true } }

// WithRotatingRefreshTokens makes each refresh issue a new refresh token and
// invalidate the old one, so reusing it fails with invalid_grant.
func WithRotatingRefreshTokens() Option { return func(c *config) { c.rotateRefresh = true } }

// WithoutDCR omits registration_endpoint from the metadata and serves no
// registration endpoint.
func WithoutDCR() Option { return func(c *config) { c.noDCR = true } }

// WithClientIDMetadataDocuments advertises
// client_id_metadata_document_supported. Authorize and token then accept
// any https client_id URL as a public client. The document is not fetched.
func WithClientIDMetadataDocuments() Option { return func(c *config) { c.cimd = true } }

// WithClient pre-registers a client, as a user would with a bring-your-own
// app. An empty secret makes it a public client.
func WithClient(id, secret string, redirectURIs ...string) Option {
	return func(c *config) {
		c.clients = append(c.clients, Client{ID: id, Secret: secret, RedirectURIs: redirectURIs})
	}
}

// WithRequiredScopes makes the resource answer a valid token that lacks any
// of scopes with 403 and
// WWW-Authenticate: Bearer error="insufficient_scope", scope="<scopes>".
func WithRequiredScopes(scopes ...string) Option {
	return func(c *config) { c.requiredScopes = scopes }
}

// WithScopesSupported lists scopes as scopes_supported in PRM and in the
// authorization server metadata.
func WithScopesSupported(scopes ...string) Option {
	return func(c *config) { c.scopesSupported = scopes }
}

// WithChallengeResourceMetadata adds resource_metadata="<PRM URL>" to the
// resource's WWW-Authenticate challenges. Without it, a 401 carries a bare
// Bearer challenge and clients must probe the well-known PRM URLs.
func WithChallengeResourceMetadata() Option { return func(c *config) { c.challengeMetadata = true } }

// WithChallengeScope adds scope="<scopes>" to the resource's 401 challenge.
func WithChallengeScope(scopes ...string) Option {
	return func(c *config) { c.challengeScope = scopes }
}

// WithoutGET makes the resource answer every GET with 405 Method Not
// Allowed, before any token check, like a Streamable HTTP server that offers
// no SSE stream. A POST still gets the 401 challenge.
func WithoutGET() Option { return func(c *config) { c.noGET = true } }

// WithAnonymousSession makes the resource serve a request without a token
// unless it is a tools/call, as Google's MCP servers do: initialize and
// tools/list need no token, and only a tool call gets the 401 challenge. A
// request with a bad token is still challenged.
func WithAnonymousSession() Option { return func(c *config) { c.anonymousSession = true } }

// WithTokenLifetime sets the access token lifetime (expires_in). The
// default is one hour. The resource rejects expired tokens.
func WithTokenLifetime(d time.Duration) Option { return func(c *config) { c.lifetime = d } }

// WithRefreshTokenLifetime sends refresh_token_expires_in, d in seconds,
// with every token, as GitHub Apps do. The fake does not enforce it.
func WithRefreshTokenLifetime(d time.Duration) Option {
	return func(c *config) { c.refreshLifetime = d }
}

// WithClientSecretExpiry registers DCR clients whose secret expires at t
// (client_secret_expires_at). The fake does not enforce it.
func WithClientSecretExpiry(t time.Time) Option { return func(c *config) { c.secretExpiry = t } }

// WithTokenErrorStatus answers every token endpoint error with HTTP status
// instead of RFC 6749's 400 or 401, keeping the RFC 6749 body. Status 200
// reproduces servers such as GitHub that report errors only in the body.
func WithTokenErrorStatus(status int) Option { return func(c *config) { c.tokenErrorStatus = status } }

// WithSlackStyleErrors makes the token endpoint answer like Slack's
// oauth.v2.access: every error is HTTP 200 with {"ok": false, "error":
// "<code>"}, and every success also carries "ok": true. Use
// [Server.FailTokenRequests] for Slack's own codes.
func WithSlackStyleErrors() Option { return func(c *config) { c.slackErrors = true } }

// WithResourceHandler serves h at the resource once a request is
// authorized, for example a reverse proxy to a fakeupstream server. The
// default answers 200 with a small JSON body. h sees the request with its
// Authorization header.
func WithResourceHandler(h http.Handler) Option { return func(c *config) { c.resource = h } }

// Client is a registered client.
type Client struct {
	ID           string
	Secret       string // empty for a public client
	RedirectURIs []string
	Name         string
	// GrantTypes are the grant_types a DCR client registered with; nil for
	// a pre-registered client, which may use any grant. A DCR client
	// registered with grant_types may use only those.
	GrantTypes []string
	// AuthMethod is the token_endpoint_auth_method a DCR client was
	// registered with; empty for a pre-registered client, which may use any.
	AuthMethod string
	// ApplicationType is the application_type a DCR request named, if any.
	ApplicationType string
}

// Authorization records a request that reached the authorize endpoint with
// a known client and redirect URI.
type Authorization struct {
	ClientID            string
	RedirectURI         string
	Scope               string
	Resource            string
	State               string
	CodeChallengeMethod string
	// Extra holds the parameters the fake doesn't read itself, such as
	// access_type, or is nil when there are none.
	Extra map[string]string
	// DPoPJKT is the dpop_jkt the request named (RFC 9449 §10), which the
	// code is bound to; empty when it named none.
	DPoPJKT string
	// Error is the OAuth error the request was redirected back with, or
	// empty if it got a code.
	Error string
}

// TokenRequest records a request to the token endpoint.
type TokenRequest struct {
	GrantType string
	ClientID  string
	Resource  string
	Scope     string
	// AuthMethod is the client authentication method the request used, as
	// a token_endpoint_auth_method: client_secret_basic,
	// client_secret_post, client_secret_jwt, private_key_jwt, or none.
	AuthMethod string
	// Assertion is the client assertion of a client_secret_jwt or
	// private_key_jwt request, decoded (verified or not); nil otherwise.
	Assertion *Assertion
	// GrantAssertion is the assertion a jwt-bearer grant presented,
	// decoded (verified or not); nil for other grants and for one that is
	// not a JWT.
	GrantAssertion *Assertion
	// Error is the OAuth error the request failed with, or empty if it
	// got a token.
	Error string
	// ClientCert is the common name of the client certificate the request
	// presented ([WithTLS]), or empty.
	ClientCert string
	// MTLS is whether the request came to the mTLS alias of the token
	// endpoint ([WithMTLSAliases]).
	MTLS bool
	// DPoP is the DPoP proof the request carried, decoded, with why the
	// fake refused it, if it did; nil when it carried none.
	DPoP *DPoPProof
}

// Assertion is a client assertion as a token request carried it: its
// header and its claims (RFC 7523 §3).
type Assertion struct {
	Alg, Typ, KID string
	Iss, Sub      string
	// Aud is the aud claim, a single string or each of an array's.
	Aud      []string
	JTI      string
	IAT, Exp int64
}

// ResourceRequest records a request to the protected resource.
type ResourceRequest struct {
	Method string
	// Bearer is whether it carried a bearer token, valid or not.
	Bearer bool
	// DPoP is the DPoP proof it carried, decoded, with why the fake
	// refused it, if it did; nil when it carried none. A token sent with
	// the DPoP scheme is not Bearer.
	DPoP *DPoPProof
	// ClientCert is the common name of the client certificate the request
	// presented ([WithTLS]), or empty.
	ClientCert string
}

// Server is a running fake. Its fields are fixed once [Start] returns.
type Server struct {
	// URL is the origin of the fake, such as http://127.0.0.1:41234.
	URL string
	// ResourceURL is the protected resource: URL + [ResourcePath].
	ResourceURL string
	// Issuer is the authorization server's issuer URL, as listed in PRM.
	Issuer string
	// MetadataIssuer is the issuer the metadata and the iss parameter
	// report. It equals Issuer unless [WithIssuerMismatch] is set.
	MetadataIssuer string
	// PRMURL is where PRM is served, or empty with [WithoutMetadata].
	PRMURL string
	// MetadataURL is where authorization server metadata is served, or
	// empty with [WithoutMetadata].
	MetadataURL string
	// AuthorizationEndpoint, TokenEndpoint, and RegistrationEndpoint are
	// the endpoints. RegistrationEndpoint is empty with [WithoutDCR].
	AuthorizationEndpoint string
	TokenEndpoint         string
	RegistrationEndpoint  string
	// DeviceAuthorizationEndpoint is the device authorization endpoint. It
	// is always served, and advertised in the metadata only with
	// [WithDeviceCode].
	DeviceAuthorizationEndpoint string
	// MTLSTokenEndpoint, MTLSDeviceAuthorizationEndpoint, and
	// MTLSRegistrationEndpoint are the mTLS aliases ([WithMTLSAliases]);
	// empty without it, and the registration alias with [WithoutDCR].
	MTLSTokenEndpoint               string
	MTLSDeviceAuthorizationEndpoint string
	MTLSRegistrationEndpoint        string
	// CertificatePEM is the server's certificate as PEM, for a client's
	// ca_file ([WithTLS]); nil without TLS.
	CertificatePEM []byte

	cfg config

	mu             sync.Mutex
	clients        map[string]*Client
	clientOrder    []string
	codes          map[string]*grant
	access         map[string]*accessToken
	refresh        map[string]*grant
	authorizations []Authorization
	tokenRequests  []TokenRequest
	resourceReqs   []ResourceRequest
	failCode       string
	devices        map[string]*device
	deviceReqs     []DeviceAuthorization
	// jtis are the client assertion IDs seen, so none is replayed.
	jtis map[string]bool
	// refuseTokens is set by [Server.RefuseAllTokens].
	refuseTokens bool
	// dpopJTIs are the DPoP proof IDs seen, so none is replayed.
	dpopJTIs map[string]bool
}

// grant is what a code or refresh token stands for.
type grant struct {
	clientID    string
	redirectURI string
	scope       string
	resource    string
	challenge   string
	// jkt is the thumbprint of the DPoP key the code (dpop_jkt) or the
	// refresh token is bound to; empty when it is not bound.
	jkt string
}

type accessToken struct {
	scope  string
	expiry time.Time
	// cert is the thumbprint of the client certificate the token is bound
	// to ([WithCertBoundTokens]); empty for a bearer token.
	cert string
	// jkt is the thumbprint of the DPoP key the token is bound to
	// ([WithDPoP]); empty for a token not bound to one.
	jkt string
}

// Start starts a fake configured by opts and closes it when the test ends.
func Start(t testing.TB, opts ...Option) *Server {
	t.Helper()
	cfg := config{lifetime: time.Hour, tokenAuthMethods: DefaultTokenAuthMethods()}

	for _, o := range opts {
		o(&cfg)
	}

	if cfg.noMetadata {
		cfg.issuerPath = ""
	}

	if cfg.resource == nil {
		cfg.resource = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}` + "\n"))
		})
	}

	s := &Server{
		cfg:      cfg,
		clients:  map[string]*Client{},
		codes:    map[string]*grant{},
		access:   map[string]*accessToken{},
		refresh:  map[string]*grant{},
		devices:  map[string]*device{},
		jtis:     map[string]bool{},
		dpopJTIs: map[string]bool{},
	}

	for _, c := range cfg.clients {
		s.addClient(c)
	}

	mux := http.NewServeMux()
	srv := s.listen(t, mux)

	s.URL = srv.URL
	s.ResourceURL = s.URL + ResourcePath
	s.Issuer = s.URL + cfg.issuerPath
	s.MetadataIssuer = s.Issuer

	if cfg.issuerMismatch {
		s.MetadataIssuer = s.Issuer + "/mismatched-issuer"
	}

	s.AuthorizationEndpoint = s.Issuer + "/authorize"
	s.TokenEndpoint = s.Issuer + "/token"

	mux.HandleFunc(ResourcePath, s.serveResource)
	mux.HandleFunc("GET "+cfg.issuerPath+"/authorize", s.serveAuthorize)
	mux.HandleFunc("POST "+cfg.issuerPath+"/token", s.serveToken)
	s.DeviceAuthorizationEndpoint = s.Issuer + "/device_authorization"
	mux.HandleFunc("POST "+cfg.issuerPath+"/device_authorization", s.serveDeviceAuthorization)

	if !cfg.noDCR {
		s.RegistrationEndpoint = s.Issuer + "/register"
		mux.HandleFunc("POST "+cfg.issuerPath+"/register", s.serveRegister)
	}

	if cfg.mtlsAliases {
		s.serveMTLSAliases(mux)
	}

	if !cfg.noMetadata {
		var prmPath string

		switch cfg.prmLocation {
		case PRMPathInserted:
			prmPath = "/.well-known/oauth-protected-resource" + ResourcePath
		case PRMRoot:
			prmPath = "/.well-known/oauth-protected-resource"
		case PRMUnlisted:
			prmPath = "/resource-metadata" + ResourcePath
		}

		s.PRMURL = s.URL + prmPath
		mux.HandleFunc("GET "+prmPath, s.servePRM)

		var metaPath string

		switch cfg.metadataLocation {
		case OAuthPathInserted:
			metaPath = "/.well-known/oauth-authorization-server" + cfg.issuerPath
		case OIDCPathInserted:
			metaPath = "/.well-known/openid-configuration" + cfg.issuerPath
		case OIDCPathAppended:
			metaPath = cfg.issuerPath + "/.well-known/openid-configuration"
		}

		s.MetadataURL = s.URL + metaPath
		mux.HandleFunc("GET "+metaPath, s.serveMetadata)
	}

	return s
}

func (s *Server) addClient(c Client) {
	c.RedirectURIs = slices.Clone(c.RedirectURIs)
	c.GrantTypes = slices.Clone(c.GrantTypes)
	s.clients[c.ID] = &c
	s.clientOrder = append(s.clientOrder, c.ID)
}

// Revoke revokes every access and refresh token issued so far, as an
// authorization server can at any time: the resource then answers 401 and a
// refresh fails with invalid_grant. Tokens issued later are unaffected.
func (s *Server) Revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.access)
	clear(s.refresh)
}

// RevokeClient forgets the registered client id, as a server that expires
// or deletes DCR clients does: its token requests then fail with
// invalid_client, and it no longer appears in [Server.Clients].
func (s *Server) RevokeClient(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, id)
	s.clientOrder = slices.DeleteFunc(s.clientOrder, func(c string) bool { return c == id })
}

// FailTokenRequests makes every later token request fail with the OAuth
// error code, in the configured error shape, until it is called again with
// "". The status is 400 unless [WithTokenErrorStatus] or
// [WithSlackStyleErrors] says otherwise.
func (s *Server) FailTokenRequests(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCode = code
}

// ExpireAccessTokens expires every access token issued so far, while
// refresh tokens keep working, so the resource answers 401 until the
// client refreshes.
func (s *Server) ExpireAccessTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, at := range s.access {
		at.expiry = time.Time{}
	}
}

// Clients returns the registered clients, pre-registered ones first, in
// registration order.
func (s *Server) Clients() []Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Client, 0, len(s.clientOrder))

	for _, id := range s.clientOrder {
		c := *s.clients[id]
		c.RedirectURIs = slices.Clone(c.RedirectURIs)
		c.GrantTypes = slices.Clone(c.GrantTypes)
		out = append(out, c)
	}

	return out
}

// Authorizations returns the authorize requests seen so far, in order.
func (s *Server) Authorizations() []Authorization {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.authorizations)
}

// ResourceRequests returns the requests to the resource seen so far, in
// order. Those without Bearer are a client's probes for the challenge.
func (s *Server) ResourceRequests() []ResourceRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.resourceReqs)
}

// TokenRequests returns the token requests seen so far, in order.
func (s *Server) TokenRequests() []TokenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.tokenRequests)
}
