package config

import (
	"slices"
	"strconv"
	"strings"
)

// authKeys locates an auth table's keys: path renders the path of a key,
// or of the table itself with no keys, and set reports whether the file
// sets a key of the table itself, rather than a default filling it.
type authKeys struct {
	path func(keys ...string) string
	set  func(key string) bool
}

// auth applies the rules every auth table follows (ADR 0007), then those
// of each concept the table configures, then its grant's. Each grant's and
// each concept's rules are in a function of their own, so each path reads
// on its own. t is the connector's tls table, nil when it has none: the tls
// client authentication methods need its client certificate.
func (v *validator) auth(k authKeys, a Auth, t *TLS) {
	p := k.path

	switch a.Type {
	case AuthTypeOAuth:
	case "":
		v.add(p("type"), "is required (%q)", AuthTypeOAuth)
	default:
		v.add(p("type"), "must be %q, not %q", AuthTypeOAuth, a.Type)
	}

	if v.template(p("client_secret"), a.ClientSecret) && a.ClientSecret != "" && a.ClientID == "" {
		v.add(p("client_secret"), "requires client_id")
	}

	if !a.ResourceIndicator && a.ClientID == "" {
		v.add(p("resource_indicator"), "resource_indicator = false requires client_id")
	}

	for _, f := range []struct{ key, val string }{
		{"authorization_server", a.AuthorizationServer},
		{"authorization_endpoint", a.AuthorizationEndpoint},
		{"token_endpoint", a.TokenEndpoint},
		{"device_authorization_endpoint", a.DeviceAuthorizationEndpoint},
		{"client_metadata_url", a.ClientMetadataURL},
		{"redirect_uri", a.RedirectURI},
	} {
		if f.val != "" {
			v.httpURL(p(f.key), f.val)
		}
	}

	v.dpopRules(p, a)
	v.clientAuthRules(p, a, t)

	if a.SigningKey != nil {
		v.signingKeyRules(p, a)
	}

	if a.Assertion != nil {
		v.assertionRules(p, a)
	}

	switch a.Grant {
	case GrantAuthorizationCode:
		v.authCodeRules(p, a)
	case GrantDeviceCode:
		v.deviceRules(p, a)
	case GrantClientCredentials:
		v.clientCredentialsRules(k, a)
	case GrantJWTBearer:
		v.jwtBearerRules(k, a)
	default:
		v.add(p("grant"), "must be %s, not %q", oneOf(Grants()), a.Grant)
	}
}

// authCodeRules are the authorization code grant's: a browser flow with
// PKCE and an authorization request of its own.
func (v *validator) authCodeRules(p func(keys ...string) string, a Auth) {
	if !a.PKCE && a.ClientSecret == "" {
		v.add(p("pkce"), "pkce = false requires client_secret")
	}

	if (a.AuthorizationEndpoint == "") != (a.TokenEndpoint == "") {
		v.add(p(), "authorization_endpoint and token_endpoint must be set together")
	}

	v.authorizeParams(p, a)
}

// deviceRules are the device code grant's: the user enters a user code on
// any device, so there is no authorization request and no callback to
// redirect to.
func (v *validator) deviceRules(p func(keys ...string) string, a Auth) {
	for _, f := range []struct {
		key string
		set bool
	}{
		{"authorization_endpoint", a.AuthorizationEndpoint != ""},
		{"redirect_uri", a.RedirectURI != ""},
	} {
		if f.set {
			v.add(p(f.key), "is not allowed with grant = %q, which has no authorization request or callback", a.Grant)
		}
	}

	v.authorizeParams(p, a)
}

// clientCredentialsRules are the client_credentials grant's: an unattended
// grant for the OAuth client's own access, so the client proves who it is
// with a credential.
func (v *validator) clientCredentialsRules(k authKeys, a Auth) {
	v.unattendedRules(k, a)

	if a.ClientSecret == "" && a.SigningKey == nil && !a.TokenEndpointAuthMethod.usesTLS() {
		v.add(k.path("grant"), "grant = %q requires a client credential: client_secret or signing_key", a.Grant)
	}
}

// jwtBearerRules are the jwt_bearer grant's: an unattended grant that
// presents an assertion, which the assertion table says where to get.
func (v *validator) jwtBearerRules(k authKeys, a Auth) {
	v.unattendedRules(k, a)

	if a.Assertion == nil {
		v.add(k.path("assertion"), "is required with grant = %q", a.Grant)
	}
}

// unattendedRules are what the unattended grants share: a configured OAuth
// client, and none of the keys of a flow with a user in it. A key is
// refused when the file sets it at all, pkce included, whose default these
// grants never read.
func (v *validator) unattendedRules(k authKeys, a Auth) {
	if a.ClientID == "" {
		v.add(k.path("client_id"), "is required with grant = %q", a.Grant)
	}

	// The grant sends its client credential or assertion on every grant,
	// with no one to see where: it goes only to a server the file names,
	// never to one the upstream's metadata names (wayfindr-afb1).
	if a.AuthorizationServer == "" && a.TokenEndpoint == "" {
		v.add(k.path("grant"), "grant = %q requires authorization_server or token_endpoint, "+
			"so its credential goes only to the server you name", a.Grant)
	}

	for _, f := range []struct {
		key string
		set bool
	}{
		{"authorization_endpoint", a.AuthorizationEndpoint != ""},
		{"device_authorization_endpoint", a.DeviceAuthorizationEndpoint != ""},
		{"client_metadata_url", a.ClientMetadataURL != ""},
		{"redirect_uri", a.RedirectURI != ""},
		{"pkce", false},
		{"authorize_params", len(a.AuthorizeParams) > 0},
	} {
		if f.set || k.set(f.key) {
			v.add(k.path(f.key), "is not allowed with grant = %q, which has no user", a.Grant)
		}
	}
}

// authorizeParams refuses an empty key and the parameters Wayfindr sets
// itself.
func (v *validator) authorizeParams(p func(keys ...string) string, a Auth) {
	for _, k := range sortedKeys(a.AuthorizeParams) {
		switch setting, reserved := ReservedAuthorizeParam(k); {
		case k == "":
			v.add(p("authorize_params"), "must not have an empty key")
		case reserved:
			v.add(p("authorize_params", k), "is set by Wayfindr; %s", setting)
		}
	}
}

// clientAuthRules check token_endpoint_auth_method against the credential
// each method needs. The tls methods (RFC 8705) need the client
// certificate of the connector's tls table, t, which every request to the
// authorization server presents, and a client_id, which the certificate
// stands for.
func (v *validator) clientAuthRules(p func(keys ...string) string, a Auth, t *TLS) {
	m := a.TokenEndpointAuthMethod
	at := p("token_endpoint_auth_method")
	requires := func(what string) { v.add(at, "token_endpoint_auth_method = %q requires %s", m, what) }

	switch m {
	case "":
		return
	case AuthMethodNone:
		if a.ClientSecret != "" {
			v.add(p("client_secret"), "is not allowed with token_endpoint_auth_method = %q", m)
		}

		if a.SigningKey != nil {
			v.add(p("signing_key"), "is not allowed with token_endpoint_auth_method = %q", m)
		}
	case AuthMethodClientSecretBasic, AuthMethodClientSecretPost:
		if a.ClientSecret == "" {
			requires("client_secret")
		}
	case AuthMethodClientSecretJWT:
		if a.ClientSecret == "" {
			requires("client_secret")
		}

		if a.ClientID == "" {
			requires("client_id")
		}
	case AuthMethodPrivateKeyJWT:
		if a.SigningKey == nil {
			requires("signing_key")
		}

		if a.ClientID == "" {
			requires("client_id")
		}
	case AuthMethodTLSClientAuth, AuthMethodSelfSignedTLSClientAuth:
		if t == nil || !t.HasClientCert() {
			requires("a client certificate: cert_file in the connector's tls table")
		}

		if a.ClientID == "" {
			requires("client_id")
		}
	default:
		v.add(at, "must be %s, not %q", oneOf(ClientAuthMethods()), m)
	}
}

// usesTLS reports whether m authenticates the client with its TLS client
// certificate.
func (m ClientAuthMethod) usesTLS() bool {
	return m == AuthMethodTLSClientAuth || m == AuthMethodSelfSignedTLSClientAuth
}

// dpopRules check the dpop mode.
func (v *validator) dpopRules(p func(keys ...string) string, a Auth) {
	switch a.DPoP {
	case DPoPAuto, DPoPAlways, DPoPNever:
	default:
		v.add(p("dpop"), "must be %s, not %q", oneOf(DPoPModes()), a.DPoP)
	}
}

// signingKeyRules check a signing_key table. Only a client you registered
// has a signing key: a dynamically registered one gets a secret or none.
func (v *validator) signingKeyRules(p func(keys ...string) string, a Auth) {
	k := *a.SigningKey

	if a.ClientID == "" {
		v.add(p("signing_key"), "requires client_id: a signing key belongs to a client you registered")
	}

	if k.Key == "" {
		v.add(p("signing_key", "key"), "is required")
	} else {
		v.template(p("signing_key", "key"), k.Key)
	}

	if k.Alg != "" && !slices.Contains(SigningAlgs(), k.Alg) {
		v.add(p("signing_key", "alg"), "must be %s, or absent, not %q", oneOf(SigningAlgs()), k.Alg)
	}

	switch k.Audience {
	case AudienceIssuer, AudienceTokenEndpoint:
	default:
		v.add(p("signing_key", "audience"), "must be %s, not %q", oneOf(Audiences()), k.Audience)
	}
}

// assertionRules check an assertion table: only the jwt_bearer grant
// presents one, and it comes from exactly one source.
func (v *validator) assertionRules(p func(keys ...string) string, a Auth) {
	s := *a.Assertion

	if a.Grant != GrantJWTBearer {
		v.add(p("assertion"), "is allowed only with grant = %q", GrantJWTBearer)

		return
	}

	v.template(p("assertion", "value"), s.Value)

	switch n := countSet(s.File, string(s.Value), s.Subject); {
	case n == 0:
		v.add(p("assertion"), "must set one of file, value, or subject")
	case n > 1:
		v.add(p("assertion"), "exactly one of file, value, or subject is allowed")
	}

	if s.Subject != "" && a.SigningKey == nil {
		v.add(p("assertion", "subject"), "requires signing_key, which Wayfindr signs the assertion with")
	}
}

// countSet returns how many of vals are not empty.
func countSet(vals ...string) int {
	n := 0

	for _, s := range vals {
		if s != "" {
			n++
		}
	}

	return n
}

// oneOf lists vals quoted, for a message: "a", "b", or "c".
func oneOf[T ~string](vals []T) string {
	q := make([]string, len(vals))

	for i, s := range vals {
		q[i] = strconv.Quote(string(s))
	}

	switch len(q) {
	case 0:
		return ""
	case 1:
		return q[0]
	case 2:
		return q[0] + " or " + q[1]
	}

	return strings.Join(q[:len(q)-1], ", ") + ", or " + q[len(q)-1]
}
