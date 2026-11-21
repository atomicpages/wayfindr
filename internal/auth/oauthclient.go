package auth

import (
	"net/http"
	"time"

	"github.com/atomicpages/wayfindr/internal/config"
)

// Grant types, as a token request's grant_type and a DCR client's
// grant_types name them.
const (
	grantAuthorizationCode = "authorization_code"
	grantDeviceCode        = "urn:ietf:params:oauth:grant-type:device_code" // RFC 8628 §3.4
)

// oauthClient is everything needed to talk to one connector's authorization
// server as its OAuth client (GLOSSARY: OAuth client): who the client is,
// how it authenticates, how its tokens are bound, and where the endpoints
// are. Every grant reaches the authorization server through one. It is
// built in one place, [Broker.oauthClient].
type oauthClient struct {
	connector string
	// issuer is the authorization server's issuer; empty when config
	// supplies the endpoints and nothing names one.
	issuer    string
	endpoints Endpoints
	// disc is the discovery the client was built from, for the audience of
	// the JWTs it signs (see [assertionAudience]).
	disc Discovery
	// registration is the client the grant is made as.
	registration Registration
	auth         clientAuth
	sender       sender
	// dpopKey is the grant's DPoP key when sender is DPoP; nil otherwise.
	dpopKey *dpopKey
	http    *http.Client
	// inspectors read every token response before anything else does.
	inspectors Inspectors
	// now is the broker's clock, which token expiries are set by.
	now func() time.Time
	// link is the connector's auth link, for needs_auth errors.
	link string
}

// oauthClient returns the OAuth client of connector c, whose discovery is
// disc, as registration reg, talking through hc, the connector's HTTP
// client ([Broker.httpFor]): the one place a grant's client
// authentication, sender, and endpoints are chosen. The connector's config
// reaches the client authentication through reg, which the Registrar built
// from it (its secret, signing key, and token_endpoint_auth_method). A
// client that cannot authenticate here fails every token request, before
// sending it, with [CaseAuthMethodUnsupported].
//
// key is the grant's DPoP key, if it has one already: a refresh's from the
// stored token, an auth flow's from its start, an unattended grant's from
// its in-memory entry. When [chooseSender] picks DPoP and key is nil, a
// new key is made for the grant; the caller keeps oc.dpopKey. A sender
// constraint that cannot be met fails every token request, before sending
// it, with chooseSender's error (see [oauthClient.usable]).
func (b *Broker) oauthClient(c config.Connector, hc *http.Client, disc Discovery, reg Registration, key *dpopKey) oauthClient {
	link := b.Link(c.Name)
	disc.Endpoints = endpointsFor(c, reg.AuthMethod, disc)
	auth, err := clientAuthOf(c.Name, reg, disc)

	if err != nil {
		auth = unusableClientAuth{err: err}
	}

	snd, key := b.senderFor(c, disc, key)

	return oauthClient{
		connector:    c.Name,
		issuer:       disc.Issuer,
		endpoints:    disc.Endpoints,
		disc:         disc,
		registration: reg,
		auth:         auth,
		sender:       snd,
		dpopKey:      key,
		http:         hc,
		inspectors:   b.inspectors,
		now:          b.now,
		link:         link,
	}
}

// senderFor is the sender of a grant of connector c, whose discovery is
// disc, as [chooseSender] picks it, with key, or a new key if key is nil,
// bound to a DPoP sender. It returns the key the sender proves with: nil
// for any other sender.
func (b *Broker) senderFor(c config.Connector, disc Discovery, key *dpopKey) (sender, *dpopKey) {
	var a config.Auth

	if c.Auth != nil {
		a = *c.Auth
	}

	snd, err := chooseSender(a, disc, disc.Challenge, key != nil)
	d, isDPoP := snd.(dpop)

	switch {
	case err == nil && isDPoP:
		if key == nil {
			key, err = newDPoPKey()
		}

		d.key, d.nonces, d.now = key, b.nonces, b.now
		snd = d
	case err == nil:
		key = nil
	}

	if err != nil {
		if e, ok := AsError(err); ok {
			err = b.fail(e.Case, c.Name, e.Detail, "", e.Err)
		}

		return unusableSender{err: err}, nil
	}

	return snd, key
}

// usable returns the error every token request of oc fails with before it
// is sent because its sender constraint cannot be met, or nil. An auth flow
// checks it as it starts, so the user is not sent to consent for a grant
// that cannot complete.
func (oc oauthClient) usable() error {
	if u, ok := oc.sender.(unusableSender); ok {
		return u.err
	}

	return nil
}

// endpointsFor is the one rule for which endpoints connector c's OAuth
// client calls, given the client authentication method it uses (empty for
// one chosen automatically, which is never a tls method) and its
// discovery: the discovered ones, with the authorization server's mTLS
// aliases (RFC 8705 §5) in place of the token, device authorization, and
// registration endpoints when
//
//   - the method authenticates with the client certificate (tls_client_auth
//     or self_signed_tls_client_auth); or
//   - the connector has a client certificate and the server binds tokens
//     to it (tls_client_certificate_bound_access_tokens), so the token
//     endpoint must see the certificate to bind the token.
//
// An endpoint with no alias, and a device authorization endpoint the
// config names, stay as they are.
func endpointsFor(c config.Connector, method config.ClientAuthMethod, disc Discovery) Endpoints {
	e, ext := disc.Endpoints, disc.Extensions
	hasCert := c.TLS != nil && c.TLS.HasClientCert()
	usesCert := method == config.AuthMethodTLSClientAuth || method == config.AuthMethodSelfSignedTLSClientAuth

	if !usesCert && (!hasCert || !ext.TLSClientCertificateBoundAccessTokens) {
		return e
	}

	alias := ext.MTLSEndpointAliases

	if alias.Token != "" {
		e.Token = alias.Token
	}

	if alias.DeviceAuthorization != "" && (c.Auth == nil || c.Auth.DeviceAuthorizationEndpoint == "") {
		e.DeviceAuthorization = alias.DeviceAuthorization
	}

	if alias.Registration != "" {
		e.Registration = alias.Registration
	}

	return e
}

// fail returns an [*Error] about the client's connector as the error the
// seam returns, as [Broker.fail] does.
func (oc oauthClient) fail(c Case, detail, providerCode string, cause error) error {
	return linkedFail(oc.link, c, oc.connector, detail, providerCode, cause)
}
