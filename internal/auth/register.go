package auth

import (
	"cmp"
	"context"
	"crypto"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// Method says how a [Registration] was obtained (ADR 0007 registration
// order).
type Method string

// Registration methods, in the order they are tried.
const (
	// MethodConfig: client_id (and client_secret) from config.
	MethodConfig Method = "config"
	// MethodCIMD: the client_metadata_url is the client_id, because the
	// authorization server supports Client ID Metadata Documents.
	MethodCIMD Method = "cimd"
	// MethodStoredDCR: a client registered earlier through DCR, read back
	// from the credentials row.
	MethodStoredDCR Method = "stored_dcr"
	// MethodDCR: a new Dynamic Client Registration (RFC 7591).
	MethodDCR Method = "dcr"
)

// Registration is the OAuth client a connector authorizes as. It is an
// OAuth client of the upstream's authorization server, not a Wayfindr
// client.
type Registration struct {
	ClientID string
	// ClientSecret is empty for a public client. Never log it.
	ClientSecret string
	Method       Method
	// SecretExpiry is when a DCR client's secret expires
	// (client_secret_expires_at, RFC 7591 §3.2.1); zero when it never does,
	// and for a client not registered through DCR.
	SecretExpiry time.Time
	// AuthMethod is the client authentication method the client must use,
	// when that is fixed: the connector's token_endpoint_auth_method, which
	// wins, else the method a DCR client was registered with
	// (token_endpoint_auth_method in the registration response). Empty
	// leaves the choice to [chooseClientAuth].
	AuthMethod config.ClientAuthMethod
	// SigningKey is the key the client authenticates with
	// (private_key_jwt), from the connector's signing_key table; nil when
	// it has none, or when the key is the assertion's (see AssertionKey).
	SigningKey *SigningKey
	// AssertionKey is the key a jwt_bearer grant signs its own assertion
	// with (assertion.subject), from the same signing_key table; nil for
	// every other grant.
	AssertionKey *SigningKey
}

// SigningKey is a connector's signing key (GLOSSARY: Signing key), parsed,
// with what the JWTs it signs name. Never log it.
type SigningKey struct {
	// Signer is the private key: EC P-256 or P-384, RSA, or Ed25519.
	Signer crypto.Signer
	// KID is the key ID each JWT's header names; empty names none.
	KID string
	// Alg is the configured JWS algorithm, which Signer can sign; empty
	// chooses one from the key and the authorization server's metadata.
	Alg string
	// Audience is what each JWT's aud claim names.
	Audience config.Audience
}

// RegisterRequest names the connector a client is chosen for, and what the
// choice depends on.
type RegisterRequest struct {
	// Connector is the connector name: the credentials row key, and for logs.
	Connector string
	// Auth is the connector's [auth] table, with config defaults applied.
	Auth config.Auth
	// Discovery is the outcome of [Discoverer.Discover] for the connector.
	Discovery Discovery
	// RedirectURI is the effective redirect URI: the connector's
	// redirect_uri, else the default loopback callback. A refresh does not
	// use it.
	RedirectURI string
	// GrantType is the grant the client is for: the auth flow's grant
	// type. Empty means the authorization code.
	GrantType string
	// Client is the connector's HTTP client, which a DCR registration goes
	// through; nil uses the Registrar's own.
	Client *http.Client
}

// grantType is the request's grant type, the authorization code when it
// names none.
func (req RegisterRequest) grantType() string {
	if req.GrantType == "" {
		return grantAuthorizationCode
	}

	return req.GrantType
}

// dcrGrantTypes are the grant types a new DCR client is registered for:
// the authorization code and refresh, plus the device code when the
// authorization server lists it in grant_types_supported or the client is
// for a device code flow, so one client serves both flow kinds.
func dcrGrantTypes(req RegisterRequest) []string {
	types := []string{grantAuthorizationCode, "refresh_token"}

	if m := req.Discovery.Metadata; req.grantType() == grantDeviceCode ||
		(m != nil && slices.Contains(m.GrantTypesSupported, grantDeviceCode)) {
		types = append(types, grantDeviceCode)
	}

	return types
}

// coversGrant reports whether a client registered for types, a DCR
// client's grant_types, may use grant. A client registered without
// grant_types has RFC 7591 §2's default, the authorization code only.
func coversGrant(types []string, grant string) bool {
	if len(types) == 0 {
		return grant == grantAuthorizationCode
	}

	return slices.Contains(types, grant)
}

// Registrar chooses the OAuth client a connector authorizes as, in ADR 0007
// order, which it alone knows: the config client_id, then CIMD, then the
// stored DCR client, then a new DCR registration. The order serves two
// uses: [Registrar.existing], the client a stored token was issued to, for
// a refresh; and [Registrar.obtain], the client an auth flow authorizes
// as, registering one if needed. A [Broker] owns one, sharing its
// credentials module, so a registration's write is serialized with the
// broker's.
type Registrar struct {
	creds    *credentials
	resolver config.Resolver
	client   *http.Client
	log      zerolog.Logger
	// flights single-flights obtain per connector, so concurrent auth
	// flows never each register a DCR client.
	flights singleflight.Group
}

// newRegistrar returns a Registrar that reads and stores clients through
// creds, resolves a config client_secret through resolver, and registers
// through client, unless a request names its own.
func newRegistrar(creds *credentials, resolver config.Resolver, client *http.Client, log zerolog.Logger) *Registrar {
	return &Registrar{creds: creds, resolver: resolver, client: client, log: log}
}

// configured returns the client the connector's config names, the first
// two steps of the order: the config client_id; else CIMD, when
// client_metadata_url is set and the authorization server advertises
// client_id_metadata_document_supported. It reports false when neither
// applies. An unresolvable client_secret or signing_key.key is a
// [CaseUnresolvedValue] error, and a signing key Wayfindr cannot sign with
// is [CaseSigningKeyInvalid].
func (r *Registrar) configured(ctx context.Context, req RegisterRequest) (Registration, bool, error) {
	a, d := req.Auth, req.Discovery

	switch {
	case a.ClientID != "":
		secret, err := a.ClientSecret.Resolve(ctx, r.resolver)

		if err != nil {
			return Registration{}, false, fail(CaseUnresolvedValue, req.Connector, "client_secret", err)
		}

		key, err := r.signingKey(ctx, req.Connector, a.SigningKey)

		if err != nil {
			return Registration{}, false, err
		}

		reg := Registration{
			ClientID: a.ClientID, ClientSecret: secret, Method: MethodConfig,
			AuthMethod: a.TokenEndpointAuthMethod, SigningKey: key,
		}

		// A jwt_bearer connector's signing key signs its assertion. It is
		// a client credential too only when token_endpoint_auth_method
		// says private_key_jwt: otherwise the client authenticates with its
		// secret, or as a public client (design §7.3).
		if a.Grant == config.GrantJWTBearer {
			reg.AssertionKey = key

			if a.TokenEndpointAuthMethod != config.AuthMethodPrivateKeyJWT {
				reg.SigningKey = nil
			}
		}

		return reg, true, nil
	case a.ClientMetadataURL != "" && d.Metadata != nil && d.Metadata.ClientIDMetadataDocumentSupported:
		return Registration{ClientID: a.ClientMetadataURL, Method: MethodCIMD, AuthMethod: a.TokenEndpointAuthMethod}, true, nil
	}

	return Registration{}, false, nil
}

// signingKey resolves and parses the connector's signing_key table; nil
// when there is none. An unresolvable key is [CaseUnresolvedValue]; a key
// that is not a PEM private key Wayfindr can sign with, or cannot sign the
// configured alg, is [CaseSigningKeyInvalid]. No error quotes the key.
func (r *Registrar) signingKey(ctx context.Context, connector string, sk *config.SigningKey) (*SigningKey, error) {
	if sk == nil {
		return nil, nil
	}

	text, err := sk.Key.Resolve(ctx, r.resolver)

	if err != nil {
		return nil, fail(CaseUnresolvedValue, connector, "signing_key.key", err)
	}

	signer, err := parsePrivateKey(text)

	if err != nil {
		return nil, fail(CaseSigningKeyInvalid, connector, "signing_key.key: "+err.Error(), nil)
	}

	if sk.Alg != "" && !canSign(sk.Alg, signer) {
		return nil, fail(CaseSigningKeyInvalid, connector, "signing_key.key cannot sign signing_key.alg "+sk.Alg, nil)
	}

	return &SigningKey{Signer: signer, KID: sk.KID, Alg: sk.Alg, Audience: sk.Audience}, nil
}

// existing returns the client the token in v, the grant being refreshed,
// was issued to: the configured client, else v's stored DCR client. It
// never registers a new client, which could not spend the old refresh
// token. Errors are [*Error]s: a stored client that cannot be opened is
// [CaseCredentialUnreadable], and no client at all [CaseRefreshFailed].
func (r *Registrar) existing(ctx context.Context, req RegisterRequest, v grantView) (Registration, error) {
	if reg, ok, err := r.configured(ctx, req); ok || err != nil {
		return reg, err
	}

	switch {
	case v.ClientErr != nil:
		return Registration{}, fail(CaseCredentialUnreadable, req.Connector, "the stored oauth client cannot be read", v.ClientErr)
	case v.Client != nil:
		return withConfiguredMethod(*v.Client, req.Auth), nil
	}

	return Registration{}, fail(CaseRefreshFailed, req.Connector, "no oauth client to refresh the token with", nil)
}

// obtain returns the client an auth flow authorizes as: the configured
// client; else the stored DCR client, if it suits the request (see
// [Registrar.storedFor]); else a new DCR registration, which is stored in
// place of any older one. It runs once per connector at a time: concurrent
// flows share one registration. It runs detached from the caller, so a
// registration the server accepted is stored even if the caller gave up.
//
// A client that cannot authenticate at the authorization server's token
// endpoint (see [chooseClientAuth]) is refused here, before the user is
// sent to authorize it.
//
// Errors are [*Error]s. [KindConfig] means the connector belongs in
// auth_error: an unresolvable client_secret or signing key, a registration
// the authorization server rejected, no way to register at all, or no
// client authentication method that fits. A transport failure is
// [KindUnreachable], wrapping it.
func (r *Registrar) obtain(ctx context.Context, req RegisterRequest) (Registration, error) {
	ch := r.flights.DoChan(req.Connector, func() (any, error) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		defer cancel()

		return r.choose(rctx, req)
	})

	var res singleflight.Result

	select {
	case res = <-ch:
	case <-ctx.Done():
		return Registration{}, ctx.Err()
	}

	reg, _ := res.Val.(Registration)

	if res.Err != nil {
		return reg, res.Err
	}

	if _, err := clientAuthOf(req.Connector, reg, req.Discovery); err != nil {
		return Registration{}, err
	}

	return reg, nil
}

// withConfiguredMethod returns reg, a DCR client, with the connector's
// token_endpoint_auth_method in place of the method it was registered
// with, when one is configured: an explicit method wins.
func withConfiguredMethod(reg Registration, a config.Auth) Registration {
	if a.TokenEndpointAuthMethod != "" {
		reg.AuthMethod = a.TokenEndpointAuthMethod
	}

	return reg
}

// choose is the body of an obtain flight.
func (r *Registrar) choose(ctx context.Context, req RegisterRequest) (Registration, error) {
	if reg, ok, err := r.configured(ctx, req); ok || err != nil {
		return reg, err
	}

	v, err := r.creds.stored(ctx, req.Connector)

	if err != nil {
		return Registration{}, err
	}

	if reg, ok := r.storedFor(req, v); ok {
		return withConfiguredMethod(reg, req.Auth), nil
	}

	if req.Discovery.Endpoints.Registration != "" {
		reg, err := r.registerDCR(ctx, req)

		return withConfiguredMethod(reg, req.Auth), err
	}

	return Registration{}, noRegistration(req.Connector)
}

// noRegistration is the auth_error when there is no way to obtain a client
// (ADR 0007 registration order, last step).
func noRegistration(connector string) error {
	return fail(CaseNoRegistration, connector, "no client_id is configured and the authorization server "+
		"supports neither CIMD nor dynamic client registration", nil)
}

// storedFor returns v's stored DCR client if it was registered with the
// request's issuer, for the request's grant type, and, for the
// authorization code, which sends it, with the request's redirect URI. A
// stored client that cannot be opened (for example after a key change) is
// logged and treated as absent, so a new one is registered in its place.
func (r *Registrar) storedFor(req RegisterRequest, v grantView) (Registration, bool) {
	grant := req.grantType()

	if (v.Client == nil && v.ClientErr == nil) || v.Issuer != req.Discovery.Issuer ||
		(grant == grantAuthorizationCode && v.RedirectURI != req.RedirectURI) {
		return Registration{}, false
	}

	if v.ClientErr != nil {
		r.log.Warn().Str("connector", req.Connector).Err(v.ClientErr).Msg("stored oauth client unusable, registering a new one")

		return Registration{}, false
	}

	if !coversGrant(v.ClientGrantTypes, grant) {
		r.log.Info().Str("connector", req.Connector).Str("grant_type", grant).
			Msg("stored oauth client not registered for the grant, registering a new one")

		return Registration{}, false
	}

	return *v.Client, true
}

// registerDCR registers a new client with the effective redirect URI, then
// stores it in the connector's credentials row (see [credentials.register]).
// It asks for the connector's token_endpoint_auth_method, the one its
// token requests will use (see [withConfiguredMethod]), else the one
// [dcrAuthMethod] picks, and names the redirect URI's [applicationType]. The server may register
// another method; the response says which, and the client uses that one. A
// response that leaves the method out is taken to have registered the one
// asked for, which is stored with the client.
func (r *Registrar) registerDCR(ctx context.Context, req RegisterRequest) (Registration, error) {
	d := req.Discovery
	asked := cmp.Or(req.Auth.TokenEndpointAuthMethod, dcrAuthMethod(d))
	resp, err := oauthex.RegisterClient(ctx, d.Endpoints.Registration, &oauthex.ClientRegistrationMetadata{
		RedirectURIs:            []string{req.RedirectURI},
		GrantTypes:              dcrGrantTypes(req),
		ResponseTypes:           []string{"code"},
		ClientName:              "Wayfindr",
		TokenEndpointAuthMethod: string(asked),
		ApplicationType:         applicationType(req.RedirectURI),
	}, noRedirects(cmp.Or(req.Client, r.client)))

	if err != nil {
		return Registration{}, registrationError(req.Connector, d, err)
	}

	if resp.TokenEndpointAuthMethod == "" {
		resp.TokenEndpointAuthMethod = string(asked)
	}

	if err := r.creds.register(ctx, req.Connector, resp, d.Issuer, req.RedirectURI, d.Resource); err != nil {
		return Registration{}, err
	}

	r.log.Info().Str("connector", req.Connector).Str("issuer", redact.URL(d.Issuer)).
		Str("method", string(MethodDCR)).Msg("oauth client registered")

	reg := registered(resp)
	reg.Method = MethodDCR

	return reg, nil
}

// registered is the client a DCR registration response describes, as a
// stored DCR client: its credentials, and the client authentication method
// it was registered with, which an empty response field leaves to
// [chooseClientAuth].
func registered(resp *oauthex.ClientRegistrationResponse) Registration {
	return Registration{
		ClientID: resp.ClientID, ClientSecret: resp.ClientSecret, Method: MethodStoredDCR,
		SecretExpiry: resp.ClientSecretExpiresAt,
		AuthMethod:   config.ClientAuthMethod(resp.TokenEndpointAuthMethod),
	}
}

// dcrAuthMethod is the client authentication method a DCR request asks
// for: none, a public client, when the authorization server lists it;
// else client_secret_basic when it lists that or lists nothing (RFC 8414's
// default); else client_secret_post.
func dcrAuthMethod(d Discovery) config.ClientAuthMethod {
	var listed []string

	if d.Metadata != nil {
		listed = d.Metadata.TokenEndpointAuthMethodsSupported
	}

	switch {
	case slices.Contains(listed, string(config.AuthMethodNone)):
		return config.AuthMethodNone
	case len(listed) == 0 || slices.Contains(listed, string(config.AuthMethodClientSecretBasic)):
		return config.AuthMethodClientSecretBasic
	}

	return config.AuthMethodClientSecretPost
}

// applicationType is the DCR application_type (OpenID Connect Dynamic
// Client Registration §2) of a client that redirects to redirectURI:
// native for a loopback redirect, which only an app on the user's own
// machine receives, and web otherwise.
func applicationType(redirectURI string) string {
	if u, err := url.Parse(redirectURI); err == nil && isLoopback(u.Hostname()) {
		return "native"
	}

	return "web"
}

// registrationError classifies a failed DCR. A rejection (RFC 7591 §3.2.2)
// needs the user to act, and so does a missing endpoint that was only
// guessed (the 2025-03-26 legacy /register). A transport failure is
// unreachable. Anything else is a malformed answer; its text is dropped,
// because the SDK's errors quote the response body, which can hold a
// client secret.
func registrationError(connector string, d Discovery, err error) error {
	endpoint := d.Endpoints.Registration
	var rejected *oauthex.ClientRegistrationError

	switch {
	case errors.As(err, &rejected):
		e := newError(CaseRegistrationRejected, connector, "dynamic client registration at "+endpoint+" was rejected", nil)
		e.ProviderCode = sanitizeProviderCode(rejected.ErrorCode)

		return e.seam()
	case transportErr(err):
		return fail(transportCase(err), connector, "register client at "+endpoint, err)
	case d.Source == SourceLegacyDefaults:
		return noRegistration(connector)
	}

	return fail(CaseMalformedResponse, connector,
		"register client at "+endpoint+": the answer is neither a registration nor an RFC 7591 error", nil)
}
