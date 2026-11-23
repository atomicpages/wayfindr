package oauthserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
)

const (
	grantAuthorizationCode = "authorization_code"
	grantRefreshToken      = "refresh_token"
)

// revokeTimeout bounds a revoke on reuse, which outlives its request.
const revokeTimeout = 5 * time.Second

// refusal is a token error answer (RFC 6749 §5.2): a code and a fixed
// description, never an input.
type refusal struct {
	status      int
	code        string
	description string
}

func invalidRequest(description string) refusal {
	return refusal{http.StatusBadRequest, "invalid_request", description}
}

func invalidGrant(description string) refusal {
	return refusal{http.StatusBadRequest, "invalid_grant", description}
}

func invalidTarget() refusal {
	return refusal{http.StatusBadRequest, "invalid_target", "The resource is not the one this grant is for."}
}

func invalidClient(description string) refusal {
	return refusal{http.StatusUnauthorized, "invalid_client", description}
}

func serverError() refusal {
	return refusal{http.StatusInternalServerError, "server_error", "Wayfindr could not complete the request."}
}

// tokenResponse is a successful token answer.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// Token serves POST TokenPath: the authorization_code and refresh_token
// grants for public clients with PKCE (design §7.11). 404 while off.
func (s *Server) Token(w http.ResponseWriter, r *http.Request) {
	cfg, on := s.running()
	if !on {
		http.NotFound(w, r)

		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)

		return
	}

	// A body sent slowly holds the request no longer than
	// TokenReadTimeout, wall time. A ResponseWriter that can't set a
	// deadline (a test's recorder) reads without one.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(TokenReadTimeout))
	r.Body = http.MaxBytesReader(w, r.Body, MaxTokenBody)

	form, ok := readForm(r)
	if !ok {
		s.refuse(w, r, "", invalidRequest("The request body is not one urlencoded form of at most 16 KiB with each parameter once."))

		return
	}

	grantType := form.Get("grant_type")

	if _, ok := r.Header["Authorization"]; ok || form.Has("client_secret") || form.Has("client_assertion") || form.Has("client_assertion_type") {
		s.refuse(w, r, grantType, invalidClient("Wayfindr accepts only public clients (token endpoint authentication none)."))

		return
	}

	clientID := form.Get("client_id")
	if clientID == "" {
		s.refuse(w, r, grantType, invalidRequest("client_id is missing."))

		return
	}

	if !slices.Contains(cfg.OAuth.Clients, clientID) {
		s.refuse(w, r, grantType, invalidClient("This client is not allowed."))

		return
	}

	var (
		resp tokenResponse
		ref  *refusal
	)

	switch grantType {
	case grantAuthorizationCode:
		resp, ref = s.exchange(r.Context(), cfg, form, clientID)
	case grantRefreshToken:
		resp, ref = s.refresh(r.Context(), cfg, form, clientID)
	default:
		ref = &refusal{http.StatusBadRequest, "unsupported_grant_type", "Only authorization_code and refresh_token are supported."}
	}

	if ref != nil {
		s.refuse(w, r, grantType, *ref)

		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// exchange runs the authorization_code grant.
func (s *Server) exchange(ctx context.Context, cfg config.Server, form url.Values, clientID string) (tokenResponse, *refusal) {
	value, redirectURI, verifier := form.Get("code"), form.Get("redirect_uri"), form.Get("code_verifier")
	if value == "" || redirectURI == "" || verifier == "" {
		return refused(invalidRequest("code, redirect_uri, and code_verifier are required."))
	}

	d := sha256.Sum256([]byte(value))
	now := s.now()

	s.mu.Lock()
	s.sweepLocked(now)

	if sc, ok := s.spent[d]; ok {
		s.mu.Unlock()
		s.revokeReplayed(ctx, sc)

		return refused(invalidGrant("The code was already used."))
	}

	c, ok := s.codes[d]
	if ok {
		// Spent by this first presentation, whatever comes of it.
		delete(s.codes, d)
		s.spent[d] = spentCode{
			grant:        c.grant,
			grantCreated: c.grantCreated,
			clientHost:   c.clientHost,
			until:        now.Add(PendingTTL),
		}
	}

	s.mu.Unlock()

	if !ok || !now.Before(c.expires) {
		return refused(invalidGrant("The code is unknown or expired."))
	}

	if clientID != c.clientID || redirectURI != c.redirectURI || !pkceMatches(verifier, c.challenge) {
		return refused(invalidGrant("The code was not issued for this request."))
	}

	if form.Has("resource") && form.Get("resource") != c.resource {
		return refused(invalidTarget())
	}

	if c.resource != Resource(cfg.Issuer()) {
		return refused(invalidGrant("The grant is suspended."))
	}

	t, err := newTokens(c.grant, s.now())
	if err != nil {
		s.log.Error().Err(err).Msg("oauth token generation failed")

		return refused(serverError())
	}

	err = s.grants.IssueGrantTokens(ctx, c.grantCreated, t.access, t.refresh)

	switch {
	case errors.Is(err, ErrSuspended) || errors.Is(err, store.ErrNotFound):
		return refused(invalidGrant("The grant is suspended or revoked."))
	case err != nil:
		s.log.Error().Err(err).Str("grant", c.grant).Msg("oauth token issue failed")

		return refused(serverError())
	}

	s.log.Info().Str("grant", c.grant).Str("client_host", c.clientHost).Msg("oauth tokens issued")

	return t.response(), nil
}

// refresh runs the refresh_token grant.
func (s *Server) refresh(ctx context.Context, cfg config.Server, form url.Values, clientID string) (tokenResponse, *refusal) {
	value := form.Get("refresh_token")
	if value == "" {
		return refused(invalidRequest("refresh_token is required."))
	}

	d := clienttoken.Digest(value)

	tok, err := s.grants.GrantToken(ctx, d)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return refused(invalidGrant("The refresh token is unknown."))
	case err != nil:
		s.log.Error().Err(err).Msg("oauth refresh token lookup failed")

		return refused(serverError())
	}

	if tok.Kind != store.OAuthRefresh {
		return refused(invalidGrant("The refresh token is unknown."))
	}

	if tok.UsedAt != nil {
		// A reuse. Within the grace it is a client's own concurrent or
		// retried refresh, refused only; later it revokes the grant.
		if s.now().Sub(*tok.UsedAt) >= RefreshGrace {
			s.revokeReused(ctx, tok)
		}

		return refused(invalidGrant("The refresh token was already used."))
	}

	g, err := s.grants.Grant(ctx, tok.Grant)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return refused(invalidGrant("The grant was revoked."))
	case err != nil:
		s.log.Error().Err(err).Str("grant", tok.Grant).Msg("oauth grant lookup failed")

		return refused(serverError())
	}

	if clientID != g.ClientID {
		return refused(invalidGrant("The refresh token was not issued to this client."))
	}

	if form.Has("resource") && form.Get("resource") != g.Resource {
		return refused(invalidTarget())
	}

	if g.Resource != Resource(cfg.Issuer()) {
		return refused(invalidGrant("The grant is suspended."))
	}

	t, err := newTokens(g.Name, s.now())
	if err != nil {
		s.log.Error().Err(err).Msg("oauth token generation failed")

		return refused(serverError())
	}

	err = s.grants.RotateGrantTokens(ctx, d, t.access, t.refresh)

	switch {
	case errors.Is(err, store.ErrSpent):
		// A rotation that raced this one won: spent just now, so within
		// the grace. Nothing is revoked.
		return refused(invalidGrant("The refresh token was already used."))
	case errors.Is(err, ErrSuspended) || errors.Is(err, store.ErrNotFound):
		return refused(invalidGrant("The grant is suspended or revoked."))
	case err != nil:
		s.log.Error().Err(err).Str("grant", g.Name).Msg("oauth token rotation failed")

		return refused(serverError())
	}

	s.log.Info().Str("grant", g.Name).Str("client_host", hostOf(g.ClientID)).Msg("oauth tokens rotated")

	return t.response(), nil
}

// revokeReplayed revokes the grant a replayed code was for, if it is still
// the grant the code was made for: one re-created under the same name
// since is left alone.
func (s *Server) revokeReplayed(ctx context.Context, sc spentCode) {
	ctx, cancel := revokeContext(ctx)
	defer cancel()

	g, err := s.grants.Grant(ctx, sc.grant)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error().Err(err).Str("grant", sc.grant).Msg("oauth grant lookup failed")
		}

		return
	}

	if !g.CreatedAt.Truncate(time.Millisecond).Equal(sc.grantCreated) {
		return
	}

	err = s.grants.RevokeGrant(ctx, sc.grant)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error().Err(err).Str("grant", sc.grant).Msg("oauth grant revoke failed")
		}

		return
	}

	s.log.Warn().Str("grant", sc.grant).Str("client_host", sc.clientHost).Msg("oauth grant revoked: code reused")
}

// revokeReused revokes the grant whose spent refresh token tok was
// presented again after the grace, if it is still the grant tok was issued
// for: a grant created after tok, re-created under the same name since, is
// left alone, as revokeReplayed leaves one.
func (s *Server) revokeReused(ctx context.Context, tok store.OAuthToken) {
	ctx, cancel := revokeContext(ctx)
	defer cancel()

	grant := tok.Grant

	g, err := s.grants.Grant(ctx, grant)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error().Err(err).Str("grant", grant).Msg("oauth grant lookup failed")
		}

		return
	}

	// The store keeps both times to the millisecond; a grant's tokens are
	// made at or after it.
	if g.CreatedAt.Truncate(time.Millisecond).After(tok.CreatedAt.Truncate(time.Millisecond)) {
		return
	}

	err = s.grants.RevokeGrant(ctx, grant)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error().Err(err).Str("grant", grant).Msg("oauth grant revoke failed")
		}

		return
	}

	s.log.Warn().Str("grant", grant).Msg("oauth grant revoked: refresh token reused")
}

// revokeContext is what a revoke on reuse runs under: ctx's values but not
// its cancellation, bounded by revokeTimeout. A client that disconnects once
// it has presented a replayed code or a reused refresh token must not call
// the revoke off.
func revokeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
}

// tokens is a new access and refresh token pair: the values for the
// client, the rows for the store.
type tokens struct {
	accessValue, refreshValue string
	access, refresh           store.OAuthToken
}

func newTokens(grant string, now time.Time) (tokens, error) {
	accessValue, err := clienttoken.Generate()
	if err != nil {
		return tokens{}, err
	}

	refreshValue, err := clienttoken.Generate()
	if err != nil {
		return tokens{}, err
	}

	expires := now.Add(AccessTTL)

	return tokens{
		accessValue:  accessValue,
		refreshValue: refreshValue,
		access: store.OAuthToken{
			Digest:    clienttoken.Digest(accessValue),
			Grant:     grant,
			Kind:      store.OAuthAccess,
			CreatedAt: now,
			ExpiresAt: &expires,
		},
		refresh: store.OAuthToken{
			Digest:    clienttoken.Digest(refreshValue),
			Grant:     grant,
			Kind:      store.OAuthRefresh,
			CreatedAt: now,
		},
	}, nil
}

func (t tokens) response() tokenResponse {
	return tokenResponse{
		AccessToken:  t.accessValue,
		TokenType:    "Bearer",
		ExpiresIn:    int(AccessTTL / time.Second),
		RefreshToken: t.refreshValue,
	}
}

func refused(r refusal) (tokenResponse, *refusal) { return tokenResponse{}, &r }

// pkceMatches reports whether verifier is 43 to 128 unreserved characters
// (RFC 7636 §4.1) whose S256 is challenge, compared in constant time.
func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}

	for i := range len(verifier) {
		c := verifier[i]
		if !isAlnum(c) && c != '-' && c != '.' && c != '_' && c != '~' {
			return false
		}
	}

	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])

	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// readForm reads a token request's body: application/x-www-form-urlencoded,
// at most MaxTokenBody bytes, each parameter at most once. The URL's own
// query is ignored.
func readForm(r *http.Request) (url.Values, bool) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/x-www-form-urlencoded" {
		return nil, false
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxTokenBody+1))
	if err != nil || len(body) > MaxTokenBody {
		return nil, false
	}

	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, false
	}

	for _, v := range form {
		if len(v) != 1 {
			return nil, false
		}
	}

	return form, true
}

// refuse answers a token error and logs it at debug: its code and grant
// type, nothing else.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, grantType string, ref refusal) {
	if grantType != grantAuthorizationCode && grantType != grantRefreshToken && grantType != "" {
		grantType = "other"
	}

	s.log.Debug().Str("error", ref.code).Str("grant_type", grantType).Msg("oauth token request refused")

	if _, ok := r.Header["Authorization"]; ok && ref.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Basic")
	}

	writeJSON(w, ref.status, struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}{ref.code, ref.description})
}

// writeJSON answers v as a token endpoint answer: never cached.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
