package fakeas

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// oauthError is an RFC 6749 §5.2 error response.
type oauthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

// slackError is a Slack Web API error response, sent with HTTP 200.
type slackError struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func newSecret(prefix string) string { return prefix + "-" + rand.Text() }

func (s *Server) servePRM(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Resource               string   `json:"resource"`
		AuthorizationServers   []string `json:"authorization_servers"`
		ScopesSupported        []string `json:"scopes_supported,omitempty"`
		BearerMethodsSupported []string `json:"bearer_methods_supported"`
		CertBound              bool     `json:"tls_client_certificate_bound_access_tokens,omitempty"`
		DPoPBound              bool     `json:"dpop_bound_access_tokens_required,omitempty"`
	}{s.ResourceURL, []string{s.prmIssuer()}, s.cfg.scopesSupported, []string{"header"}, s.cfg.certBound, s.cfg.dpop.on})
}

// prmIssuer is the issuer PRM lists: Issuer, with a terminating "/" under
// [WithPRMIssuerTrailingSlash].
func (s *Server) prmIssuer() string {
	if s.cfg.prmIssuerSlash {
		return s.Issuer + "/"
	}

	return s.Issuer
}

func (s *Server) serveMetadata(w http.ResponseWriter, _ *http.Request) {
	meta := struct {
		Issuer                 string            `json:"issuer"`
		AuthorizationEndpoint  string            `json:"authorization_endpoint"`
		TokenEndpoint          string            `json:"token_endpoint"`
		RegistrationEndpoint   string            `json:"registration_endpoint,omitempty"`
		ScopesSupported        []string          `json:"scopes_supported,omitempty"`
		ResponseTypesSupported []string          `json:"response_types_supported"`
		GrantTypesSupported    []string          `json:"grant_types_supported"`
		TokenAuthMethods       []string          `json:"token_endpoint_auth_methods_supported,omitempty"`
		SigningAlgs            []string          `json:"token_endpoint_auth_signing_alg_values_supported,omitempty"`
		CodeChallengeMethods   []string          `json:"code_challenge_methods_supported,omitempty"`
		CIMDSupported          bool              `json:"client_id_metadata_document_supported,omitempty"`
		IssParameterSupported  bool              `json:"authorization_response_iss_parameter_supported"`
		DeviceEndpoint         string            `json:"device_authorization_endpoint,omitempty"`
		MTLSAliases            map[string]string `json:"mtls_endpoint_aliases,omitempty"`
		CertBound              bool              `json:"tls_client_certificate_bound_access_tokens,omitempty"`
		DPoPAlgs               []string          `json:"dpop_signing_alg_values_supported,omitzero"`
	}{
		Issuer:                 s.MetadataIssuer,
		AuthorizationEndpoint:  s.AuthorizationEndpoint,
		TokenEndpoint:          s.TokenEndpoint,
		RegistrationEndpoint:   s.RegistrationEndpoint,
		ScopesSupported:        s.cfg.scopesSupported,
		ResponseTypesSupported: []string{"code"},
		GrantTypesSupported:    []string{"authorization_code", "refresh_token"},
		TokenAuthMethods:       s.cfg.tokenAuthMethods,
		SigningAlgs:            s.cfg.signingAlgs,
		CIMDSupported:          s.cfg.cimd,
		IssParameterSupported:  true,
		MTLSAliases:            s.mtlsAliases(),
		CertBound:              s.cfg.certBound,
		DPoPAlgs:               s.dpopAlgs(),
	}

	if !s.cfg.noPKCE {
		meta.CodeChallengeMethods = []string{"S256"}
	}

	if s.cfg.device.advertised {
		meta.DeviceEndpoint = s.DeviceAuthorizationEndpoint
		meta.GrantTypesSupported = append(meta.GrantTypesSupported, GrantDeviceCode)
	}

	if s.cfg.unattended.jwtBearerKey != nil {
		meta.GrantTypesSupported = append(meta.GrantTypesSupported, GrantJWTBearer)
	}

	writeJSON(w, http.StatusOK, meta)
}

// serveRegister is RFC 7591 DCR.
func (s *Server) serveRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs            []string `json:"redirect_uris"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
		GrantTypes              []string `json:"grant_types,omitempty"`
		ResponseTypes           []string `json:"response_types,omitempty"`
		ClientName              string   `json:"client_name,omitempty"`
		Scope                   string   `json:"scope,omitempty"`
		ApplicationType         string   `json:"application_type,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, oauthError{"invalid_client_metadata", "body is not JSON"})

		return
	}

	if len(req.RedirectURIs) == 0 {
		writeJSON(w, http.StatusBadRequest, oauthError{"invalid_redirect_uri", "redirect_uris is required"})

		return
	}

	switch {
	case s.cfg.dcrAuthMethod != "":
		req.TokenEndpointAuthMethod = s.cfg.dcrAuthMethod
	case req.TokenEndpointAuthMethod == "":
		req.TokenEndpointAuthMethod = "client_secret_basic"
	}

	c := Client{
		ID: newSecret("client"), RedirectURIs: req.RedirectURIs, Name: req.ClientName,
		AuthMethod: req.TokenEndpointAuthMethod, ApplicationType: req.ApplicationType,
		GrantTypes: req.GrantTypes,
	}

	if req.TokenEndpointAuthMethod != "none" || s.cfg.terseDCR {
		c.Secret = newSecret("secret")
	}

	if s.cfg.terseDCR {
		c.AuthMethod = ""
	}

	s.mu.Lock()
	s.addClient(c)
	s.mu.Unlock()

	if s.cfg.terseDCR {
		writeJSON(w, http.StatusCreated, map[string]any{
			"client_id": c.ID, "client_secret": c.Secret, "redirect_uris": req.RedirectURIs,
		})

		return
	}

	resp := map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        time.Now().Unix(),
		"redirect_uris":              req.RedirectURIs,
		"token_endpoint_auth_method": req.TokenEndpointAuthMethod,
	}

	if c.Secret != "" {
		resp["client_secret"] = c.Secret
		resp["client_secret_expires_at"] = 0

		if !s.cfg.secretExpiry.IsZero() {
			resp["client_secret_expires_at"] = s.cfg.secretExpiry.Unix()
		}
	}

	for k, v := range map[string]string{"client_name": req.ClientName, "scope": req.Scope, "application_type": req.ApplicationType} {
		if v != "" {
			resp[k] = v
		}
	}

	if len(req.GrantTypes) > 0 {
		resp["grant_types"] = req.GrantTypes
	}

	if len(req.ResponseTypes) > 0 {
		resp["response_types"] = req.ResponseTypes
	}

	writeJSON(w, http.StatusCreated, resp)
}

// lookupClient finds a registered client, or with CIMD on, treats an https
// URL as a public client. s.mu must be held.
func (s *Server) lookupClient(id string) (Client, bool) {
	if c, ok := s.clients[id]; ok {
		return *c, true
	}

	if s.cfg.cimd && strings.HasPrefix(id, "https://") {
		return Client{ID: id}, true
	}

	return Client{}, false
}

// isProtocolParam reports whether k is an authorize parameter the fake reads
// itself; the others are recorded in Authorization.Extra.
func isProtocolParam(k string) bool {
	switch k {
	case "response_type", "client_id", "redirect_uri", "scope", "state",
		"code_challenge", "code_challenge_method", "resource", "dpop_jkt":
		return true
	}

	return false
}

// serveAuthorize validates an authorization request and redirects straight
// back, as if the user consented.
func (s *Server) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.lookupClient(q.Get("client_id"))

	if !ok {
		http.Error(w, "invalid_client: unknown client_id", http.StatusBadRequest)

		return
	}

	redirect := q.Get("redirect_uri")

	if redirect == "" && len(c.RedirectURIs) == 1 {
		redirect = c.RedirectURIs[0]
	}

	// A CIMD client's redirect URIs live in a document the fake doesn't fetch.
	if redirect == "" || (len(c.RedirectURIs) > 0 && !slices.Contains(c.RedirectURIs, redirect)) {
		http.Error(w, "invalid_request: redirect_uri is not registered", http.StatusBadRequest)

		return
	}

	target, err := url.Parse(redirect)

	if err != nil {
		http.Error(w, "invalid_request: bad redirect_uri", http.StatusBadRequest)

		return
	}

	a := Authorization{
		ClientID:            c.ID,
		RedirectURI:         redirect,
		Scope:               q.Get("scope"),
		Resource:            q.Get("resource"),
		State:               q.Get("state"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
		DPoPJKT:             q.Get("dpop_jkt"),
	}
	challenge := q.Get("code_challenge")

	for k := range q {
		if !isProtocolParam(k) {
			if a.Extra == nil {
				a.Extra = map[string]string{}
			}

			a.Extra[k] = q.Get(k)
		}
	}

	switch {
	case q.Get("response_type") != "code":
		a.Error = "unsupported_response_type"
	case challenge == "" && !s.cfg.noPKCE:
		a.Error = "invalid_request" // PKCE required
	case challenge != "" && a.CodeChallengeMethod != "S256":
		a.Error = "invalid_request" // only S256 is supported
	case a.Resource != "" && a.Resource != s.ResourceURL:
		a.Error = "invalid_target"
	}

	s.authorizations = append(s.authorizations, a)

	params := target.Query()

	if a.Error != "" {
		params.Set("error", a.Error)
	} else {
		code := newSecret("code")
		s.codes[code] = &grant{
			clientID: c.ID, redirectURI: redirect, scope: a.Scope,
			resource: a.Resource, challenge: challenge, jkt: a.DPoPJKT,
		}
		params.Set("code", code)
	}

	if a.State != "" {
		params.Set("state", a.State)
	}

	params.Set("iss", s.MetadataIssuer)
	target.RawQuery = params.Encode()
	// The target is a redirect URI registered for the client, checked above.
	http.Redirect(w, r, target.String(), http.StatusFound) //nolint:gosec // G710: see above

}

// clientAuth authenticates the token request with client_secret_basic,
// client_secret_post, client_secret_jwt, private_key_jwt, or none, and
// records the method, and any client assertion, in tr. s.mu must be held.
func (s *Server) clientAuth(r *http.Request, tr *TokenRequest) (Client, bool) {
	f := r.PostForm
	id, secret, basic := r.BasicAuth()

	switch {
	case basic:
		tr.AuthMethod = "client_secret_basic"
		// RFC 6749 §2.3.1: both are form-urlencoded inside Basic.
		var err1, err2 error
		id, err1 = url.QueryUnescape(id)
		secret, err2 = url.QueryUnescape(secret)

		if err1 != nil || err2 != nil {
			return Client{}, false
		}
	case f.Has("client_assertion_type") || f.Has("client_assertion"):
		return s.assertionAuth(f, tr)
	case f.Has("client_secret"):
		tr.AuthMethod = "client_secret_post"
		id, secret = f.Get("client_id"), f.Get("client_secret")
	case peerCN(r) != "":
		// Only client_id, over a connection with a verified client
		// certificate: RFC 8705 §2.
		tr.AuthMethod = "tls_client_auth"
		id = f.Get("client_id")
	default:
		tr.AuthMethod = "none"
		id = f.Get("client_id")
	}

	c, ok := s.lookupClient(id)

	if !ok || c.Secret != secret || !s.registeredFor(c, tr.AuthMethod) {
		return Client{ID: id}, false
	}

	return c, true
}

// registeredFor reports whether c may authenticate with method: always,
// unless [WithStrictClientAuth] holds c to the method it was registered
// with.
func (s *Server) registeredFor(c Client, method string) bool {
	return !s.cfg.strictClientAuth || c.AuthMethod == "" || c.AuthMethod == method
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Server) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, oauthError{"invalid_request", "bad form body"})

		return
	}

	f := r.PostForm
	s.mu.Lock()
	defer s.mu.Unlock()

	tr := TokenRequest{
		GrantType: f.Get("grant_type"), Resource: f.Get("resource"), Scope: f.Get("scope"),
		ClientCert: peerCN(r), MTLS: viaAlias(r),
	}
	fail := func(status int, code, desc string) {
		tr.Error = code
		s.tokenRequests = append(s.tokenRequests, tr)

		switch {
		case s.cfg.slackErrors:
			writeJSON(w, http.StatusOK, slackError{OK: false, Error: code})
		case s.cfg.tokenErrorStatus != 0:
			writeJSON(w, s.cfg.tokenErrorStatus, oauthError{code, desc})
		default:
			writeJSON(w, status, oauthError{code, desc})
		}
	}
	c, ok := s.clientAuth(r, &tr)
	tr.ClientID = c.ID

	if s.failCode != "" {
		fail(http.StatusBadRequest, s.failCode, "")

		return
	}

	if !ok {
		fail(http.StatusUnauthorized, "invalid_client", "client authentication failed")

		return
	}

	if tr.Resource != "" && tr.Resource != s.ResourceURL {
		fail(http.StatusBadRequest, "invalid_target", "unknown resource")

		return
	}

	proof, code := s.checkProof(r, "as", "")
	tr.DPoP = proof

	if code != "" {
		if code == "use_dpop_nonce" {
			w.Header().Set("DPoP-Nonce", ASNonce)
		}

		fail(http.StatusBadRequest, code, "")

		return
	}

	// jkt is the key the token is bound to: the proof's, with WithDPoP.
	jkt := ""

	if proof != nil && s.cfg.dpop.on {
		jkt = proof.JKT
	}

	var g *grant
	refreshToken := ""

	switch tr.GrantType {
	case "authorization_code":
		code := f.Get("code")
		g = s.codes[code]
		delete(s.codes, code) // single use, even on failure

		switch {
		case g == nil || g.clientID != c.ID:
			fail(http.StatusBadRequest, "invalid_grant", "unknown or used code")

			return
		case f.Get("redirect_uri") != g.redirectURI:
			fail(http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")

			return
		case g.challenge != "" && s256(f.Get("code_verifier")) != g.challenge:
			fail(http.StatusBadRequest, "invalid_grant", "PKCE verification failed")

			return
		case s.cfg.dpop.on && g.jkt != "" && g.jkt != jkt:
			fail(http.StatusBadRequest, "invalid_grant", "the code is bound to another DPoP key")

			return
		}

		refreshToken = newSecret("rt")
		s.refresh[refreshToken] = g
	case "refresh_token":
		refreshToken = f.Get("refresh_token")
		g = s.refresh[refreshToken]

		if g == nil || g.clientID != c.ID {
			fail(http.StatusBadRequest, "invalid_grant", "unknown, rotated, or revoked refresh token")

			return
		}

		if s.cfg.dpop.on && g.jkt != "" && g.jkt != jkt {
			fail(http.StatusBadRequest, "invalid_grant", "the refresh token is bound to another DPoP key")

			return
		}

		if s.cfg.rotateRefresh {
			delete(s.refresh, refreshToken)
			refreshToken = newSecret("rt")
			s.refresh[refreshToken] = g
		}
	case GrantDeviceCode:
		var code, desc string

		if g, code, desc = s.redeemDevice(c, f.Get("device_code")); code != "" {
			fail(http.StatusBadRequest, code, desc)

			return
		}

		refreshToken = newSecret("rt")
		s.refresh[refreshToken] = g
	case GrantClientCredentials, GrantJWTBearer:
		var code, desc string

		if g, code, desc = s.unattendedGrant(c, f, &tr); code != "" {
			fail(http.StatusBadRequest, code, desc)

			return
		}

		if s.cfg.unattended.refreshTokens {
			refreshToken = newSecret("rt")
			s.refresh[refreshToken] = g
		}
	default:
		fail(http.StatusBadRequest, "unsupported_grant_type", "")

		return
	}

	s.tokenRequests = append(s.tokenRequests, tr)
	at := newSecret("at")
	s.access[at] = &accessToken{scope: g.scope, expiry: time.Now().Add(s.cfg.lifetime), cert: s.binding(r), jkt: jkt}
	tokenType := "Bearer"

	if jkt != "" {
		// RFC 9449 §5: the refresh token is bound to the key too.
		g.jkt, tokenType = jkt, "DPoP"
	}

	resp := map[string]any{
		"access_token": at,
		"token_type":   tokenType,
		"expires_in":   int(s.cfg.lifetime.Seconds()),
	}

	if refreshToken != "" {
		resp["refresh_token"] = refreshToken
	}

	if g.scope != "" {
		resp["scope"] = g.scope
	}

	if s.cfg.refreshLifetime > 0 {
		resp["refresh_token_expires_in"] = int(s.cfg.refreshLifetime.Seconds())
	}

	if s.cfg.slackErrors {
		resp["ok"] = true
	}

	writeJSON(w, http.StatusOK, resp)
}

// serveResource is the protected fake MCP resource.
func (s *Server) serveResource(w http.ResponseWriter, r *http.Request) {
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	dpopToken, dpopScheme := strings.CutPrefix(r.Header.Get("Authorization"), "DPoP ")
	hasToken := bearer || (dpopScheme && s.cfg.dpop.on)

	if dpopScheme {
		token = dpopToken
	}

	s.mu.Lock()
	proof, proofErr := s.checkProof(r, "rs", token)
	s.resourceReqs = append(s.resourceReqs, ResourceRequest{
		Method: r.Method, Bearer: bearer, DPoP: proof, ClientCert: peerCN(r),
	})
	s.mu.Unlock()

	if s.cfg.noGET && r.Method == http.MethodGet {
		w.Header().Set("Allow", "POST")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)

		return
	}

	if s.cfg.anonymousSession && !hasToken && !isToolCall(r) {
		s.cfg.resource.ServeHTTP(w, r)

		return
	}

	if hasToken && proofErr == "use_dpop_nonce" {
		w.Header().Set("DPoP-Nonce", RSNonce)
		s.challenge(w, http.StatusUnauthorized, [][2]string{{"error", "use_dpop_nonce"}})

		return
	}

	s.mu.Lock()
	at, ok := s.access[token]
	valid := hasToken && ok && time.Now().Before(at.expiry) && !s.refuseTokens &&
		(at.cert == "" || at.cert == certThumbprint(r)) && s.dpopBound(at, dpopScheme, proof, proofErr)
	var granted []string

	if valid {
		granted = strings.Fields(at.scope)
	}

	s.mu.Unlock()

	if !valid {
		var params [][2]string

		if hasToken {
			params = append(params, [2]string{"error", "invalid_token"})
		}

		if len(s.cfg.challengeScope) > 0 {
			params = append(params, [2]string{"scope", strings.Join(s.cfg.challengeScope, " ")})
		}

		s.challenge(w, http.StatusUnauthorized, params)

		return
	}

	for _, need := range s.cfg.requiredScopes {
		if !slices.Contains(granted, need) {
			s.challenge(w, http.StatusForbidden, [][2]string{
				{"error", "insufficient_scope"},
				{"scope", strings.Join(s.cfg.requiredScopes, " ")},
			})

			return
		}
	}

	s.cfg.resource.ServeHTTP(w, r)
}

// isToolCall reports whether r is a JSON-RPC tools/call POST. It leaves the
// body readable for the handler.
func isToolCall(r *http.Request) bool {
	if r.Method != http.MethodPost || r.Body == nil {
		return false
	}

	body, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))

	if err != nil {
		return false
	}

	var msg struct {
		Method string `json:"method"`
	}

	return json.Unmarshal(body, &msg) == nil && msg.Method == "tools/call"
}

// challenge writes status with a Bearer WWW-Authenticate challenge (RFC 6750
// §3, RFC 9728 §5.1), or with [WithDPoP] a DPoP one (RFC 9449 §7.1).
func (s *Server) challenge(w http.ResponseWriter, status int, params [][2]string) {
	if s.cfg.challengeMetadata && s.PRMURL != "" {
		params = append(params, [2]string{"resource_metadata", s.PRMURL})
	}

	h := "Bearer"

	if s.cfg.dpop.on {
		h = "DPoP"
		params = append(params, [2]string{"algs", "ES256"})
	}

	for i, p := range params {
		sep := ", "

		if i == 0 {
			sep = " "
		}

		h += sep + p[0] + `="` + p[1] + `"`
	}

	w.Header().Set("WWW-Authenticate", h)
	http.Error(w, http.StatusText(status), status)
}
