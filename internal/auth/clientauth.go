package auth

import (
	"crypto"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/atomicpages/wayfindr/internal/config"
)

// clientAuth is how Wayfindr proves to the token endpoint that it is the
// OAuth client (GLOSSARY: Client authentication). Each method is one small
// type here, and [chooseClientAuth] is the one selector: nothing else
// knows how a method works.
type clientAuth interface {
	// apply authenticates r, a token request, as the client at now.
	apply(r *endpointRequest, now time.Time) error
}

// noClientAuth is a public client: it only names itself (RFC 6749 §2.3).
type noClientAuth struct{ clientID string }

func (a noClientAuth) apply(r *endpointRequest, _ time.Time) error {
	r.form.Set("client_id", a.clientID)

	return nil
}

// secretBasic is client_secret_basic: the client ID and secret in HTTP
// Basic authentication.
type secretBasic struct{ clientID, secret string }

func (a secretBasic) apply(r *endpointRequest, _ time.Time) error {
	// RFC 6749 §2.3.1: both are form-urlencoded inside Basic.
	userinfo := url.QueryEscape(a.clientID) + ":" + url.QueryEscape(a.secret)
	r.header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(userinfo)))

	return nil
}

// secretPost is client_secret_post: the client ID and secret in the form
// body.
type secretPost struct{ clientID, secret string }

func (a secretPost) apply(r *endpointRequest, _ time.Time) error {
	r.form.Set("client_id", a.clientID)
	r.form.Set("client_secret", a.secret)

	return nil
}

// secretJWT is client_secret_jwt: a client assertion signed with HS256
// over the client secret (RFC 7523 §2.2, OpenID Connect Core §9).
type secretJWT struct{ clientID, secret, audience string }

func (a secretJWT) apply(r *endpointRequest, now time.Time) error {
	return applyAssertion(r, a.clientID, a.audience, jwsHeader{Alg: algHS256, Typ: assertionTyp}, []byte(a.secret), now)
}

// privateKeyJWT is private_key_jwt: a client assertion signed with the
// client's signing key, whose public key the authorization server holds.
type privateKeyJWT struct {
	clientID, audience string
	key                crypto.Signer
	alg, kid           string
}

func (a privateKeyJWT) apply(r *endpointRequest, now time.Time) error {
	return applyAssertion(r, a.clientID, a.audience, jwsHeader{Alg: a.alg, Typ: assertionTyp, KID: a.kid}, a.key, now)
}

// tlsClient is tls_client_auth and self_signed_tls_client_auth (RFC 8705
// §2): the client certificate the connection presents is the credential,
// so the form only names the client. The connector's HTTP client presents
// the certificate on every request to the authorization server, whichever
// method authenticates; the two methods differ only in how the server
// checks it, a CA or a registered key, which Wayfindr has no part in.
type tlsClient struct{ clientID string }

func (a tlsClient) apply(r *endpointRequest, _ time.Time) error {
	r.form.Set("client_id", a.clientID)

	return nil
}

// unusableClientAuth is a client that cannot authenticate at this
// authorization server: every token request fails with err, before it is
// sent.
type unusableClientAuth struct{ err error }

func (a unusableClientAuth) apply(*endpointRequest, time.Time) error { return a.err }

// What a client assertion carries (RFC 7523 §2.2 and §3, as updated by
// RFC 10070).
const (
	clientAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	assertionTyp        = "client-authentication+jwt"
	assertionLifetime   = 60 * time.Second
)

// assertionClaims are a client assertion's claims: the client is both the
// issuer and the subject, and the audience is the authorization server.
type assertionClaims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	Aud string `json:"aud"`
	JTI string `json:"jti"`
	IAT int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// applyAssertion adds a client assertion for clientID, for audience,
// signed with h.Alg by key, to r's form, with the client_id beside it.
// Each assertion has a fresh jti and lives a minute from now.
func applyAssertion(r *endpointRequest, clientID, audience string, h jwsHeader, key any, now time.Time) error {
	var nonce [16]byte

	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("client assertion jti: %w", err)
	}

	jwt, err := signJWS(h, assertionClaims{
		Iss: clientID, Sub: clientID, Aud: audience, JTI: b64url(nonce[:]),
		IAT: now.Unix(), Exp: now.Add(assertionLifetime).Unix(),
	}, key)

	if err != nil {
		return fmt.Errorf("sign client assertion: %w", err)
	}

	r.form.Set("client_id", clientID)
	r.form.Set("client_assertion_type", clientAssertionType)
	r.form.Set("client_assertion", jwt)

	return nil
}

// clientAuthOf is [chooseClientAuth] for connector, with no fit as a
// [CaseAuthMethodUnsupported] error.
func clientAuthOf(connector string, reg Registration, disc Discovery) (clientAuth, error) {
	a, err := chooseClientAuth(reg, disc)

	if err != nil {
		return nil, fail(CaseAuthMethodUnsupported, connector, err.Error(), nil)
	}

	return a, nil
}

// chooseClientAuth is the one selector of how reg, the OAuth client,
// authenticates at the token endpoint of the authorization server disc
// describes:
//
//  1. the method fixed for the client (reg.AuthMethod): the connector's
//     token_endpoint_auth_method, which wins, else the method a DCR client
//     was registered with;
//  2. otherwise, by the credential the client holds, the first the
//     authorization server lists in token_endpoint_auth_methods_supported
//     (a metadata document without it lists client_secret_basic, RFC 8414
//     §2): a signing key gives private_key_jwt; a secret gives
//     client_secret_basic, then client_secret_post, then client_secret_jwt.
//     Without metadata (endpoints from config, a legacy server) the first
//     is taken. A client with no credential is public and sends its
//     client_id, whatever the list says, as servers that take public
//     clients with PKCE often leave none out of it.
//
// The tls methods are never chosen: only an explicit method (step 1) is
// one. The error says why no method fits:
// the authorization server lists none the client's credential can do, or
// the fixed method needs a credential the client doesn't hold. It is
// returned before any request is sent.
func chooseClientAuth(reg Registration, disc Discovery) (clientAuth, error) {
	method := reg.AuthMethod

	if method == "" {
		var err error

		if method, err = autoClientAuthMethod(reg, disc); err != nil {
			return nil, err
		}
	}

	return clientAuthFor(method, reg, disc)
}

// autoClientAuthMethod is step 2 of [chooseClientAuth].
func autoClientAuthMethod(reg Registration, disc Discovery) (config.ClientAuthMethod, error) {
	var held []config.ClientAuthMethod

	if reg.SigningKey != nil {
		held = append(held, config.AuthMethodPrivateKeyJWT)
	}

	if reg.ClientSecret != "" {
		held = append(held, config.AuthMethodClientSecretBasic, config.AuthMethodClientSecretPost, config.AuthMethodClientSecretJWT)
	}

	switch {
	case len(held) == 0:
		return config.AuthMethodNone, nil
	case disc.Metadata == nil:
		return held[0], nil
	}

	listed := disc.Metadata.TokenEndpointAuthMethodsSupported

	if len(listed) == 0 {
		listed = []string{string(config.AuthMethodClientSecretBasic)}
	}

	for _, m := range held {
		if slices.Contains(listed, string(m)) {
			return m, nil
		}
	}

	return "", fmt.Errorf("the authorization server accepts token_endpoint_auth_methods %s, "+
		"and none of them fits the client's credential", strings.Join(listed, ", "))
}

// clientAuthFor is method for reg, with the credential it needs.
func clientAuthFor(method config.ClientAuthMethod, reg Registration, disc Discovery) (clientAuth, error) {
	id, secret := reg.ClientID, reg.ClientSecret
	needs := func(what string) error {
		return fmt.Errorf("token_endpoint_auth_method %s needs %s, which the client does not have", method, what)
	}

	switch method {
	case config.AuthMethodNone:
		return noClientAuth{clientID: id}, nil
	case config.AuthMethodClientSecretBasic:
		if secret == "" {
			return nil, needs("a client secret")
		}

		return secretBasic{clientID: id, secret: secret}, nil
	case config.AuthMethodClientSecretPost:
		if secret == "" {
			return nil, needs("a client secret")
		}

		return secretPost{clientID: id, secret: secret}, nil
	case config.AuthMethodClientSecretJWT:
		if secret == "" {
			return nil, needs("a client secret")
		}

		return secretJWT{clientID: id, secret: secret, audience: assertionAudience(disc, config.AudienceIssuer)}, nil
	case config.AuthMethodPrivateKeyJWT:
		k := reg.SigningKey

		if k == nil {
			return nil, needs("a signing key")
		}

		var offered []string

		if disc.Metadata != nil {
			offered = disc.Metadata.TokenEndpointAuthSigningAlgValuesSupported
		}

		return privateKeyJWT{
			clientID: id, audience: assertionAudience(disc, k.Audience),
			key: k.Signer, alg: pickAlg(k.Signer, k.Alg, offered), kid: k.KID,
		}, nil
	case config.AuthMethodTLSClientAuth, config.AuthMethodSelfSignedTLSClientAuth:
		// Config validation requires the connector's client certificate
		// with these methods.
		return tlsClient{clientID: id}, nil
	}

	return nil, fmt.Errorf("token_endpoint_auth_method %q is unknown", method)
}

// assertionAudience is the aud of a client assertion: the authorization
// server's issuer as its metadata names it (RFC 10070), or the token
// endpoint when aud says so (Okta, Entra) or when nothing names an issuer
// (endpoints from config).
func assertionAudience(disc Discovery, aud config.Audience) string {
	issuer := disc.Issuer

	if disc.Metadata != nil && disc.Metadata.Issuer != "" {
		issuer = disc.Metadata.Issuer
	}

	if aud == config.AudienceTokenEndpoint || issuer == "" {
		return disc.Endpoints.Token
	}

	return issuer
}
