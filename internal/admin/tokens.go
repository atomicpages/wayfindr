package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
)

// MaxTokenBody bounds the body of POST /admin/tokens: a name and a list of
// connector names.
const MaxTokenBody = 64 << 10

// ClientTokens creates, lists, and revokes scoped client tokens
// (docs/internal/plans/scoped-client-tokens.md). The daemon adapts the
// store and the registry httpd resolves bearers with to it, so a token is
// accepted from its creation and refused from its revoke. Admin checks the
// request before it calls Create; ClientTokens checks nothing of it.
type ClientTokens interface {
	// Create generates a token called name for connectors, stores its
	// digest, and starts accepting it. It returns the stored row and the
	// token's value, which nothing keeps. A name already taken is an error
	// wrapping store.ErrExists.
	Create(ctx context.Context, name string, connectors []string) (store.ClientToken, string, error)
	// List returns every scoped token, ordered by name.
	List(ctx context.Context) ([]store.ClientToken, error)
	// Revoke deletes the token called name and stops accepting it. An
	// unknown name is an error wrapping store.ErrNotFound.
	Revoke(ctx context.Context, name string) error
}

// TokenSpec is the body of POST /admin/tokens.
type TokenSpec struct {
	// Name follows the connector-name rule and is never "default".
	Name string `json:"name"`
	// Connectors are the configured connectors the token may use; at least
	// one. A name given twice counts once.
	Connectors []string `json:"connectors"`
}

// TokenCreated is the response to POST /admin/tokens. It is the only
// response that ever holds a token's value.
type TokenCreated struct {
	Name string `json:"name"`
	// Connectors are the request's, without repeats, in the order given.
	Connectors []string `json:"connectors"`
	// Token is the value a client presents as its bearer. It cannot be
	// read back: only its SHA-256 is stored.
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

// TokenList is the response to GET /admin/tokens.
type TokenList struct {
	// Tokens are the scoped tokens, ordered by name. Always an array.
	Tokens []Token `json:"tokens"`
}

// Token is one scoped client token as GET /admin/tokens lists it, without
// its value.
type Token struct {
	Name       string    `json:"name"`
	Connectors []string  `json:"connectors"`
	CreatedAt  time.Time `json:"created_at"`
	// LastUsedAt is when a client last presented the token, as the daemon
	// last recorded it (at most a minute late); absent until first use.
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// TokenRevoked is the response to DELETE /admin/tokens/{name}.
type TokenRevoked struct {
	Name string `json:"name"`
}

// withTokens serves h, a /admin/tokens route, only when the daemon passed
// Options.Tokens; otherwise it answers 501.
func (a api) withTokens(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Tokens == nil {
			http.Error(w, "client tokens are not available", http.StatusNotImplemented)

			return
		}

		h(w, r)
	}
}

// createToken serves POST /admin/tokens. It never logs the token's value,
// and answers it only in the response body.
func (a api) createToken(w http.ResponseWriter, r *http.Request) {
	var spec TokenSpec

	if err := readJSON(w, r, MaxTokenBody, &spec); err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			http.Error(w, "request body is too large", http.StatusRequestEntityTooLarge)

			return
		}

		http.Error(w, `body must be {"name": "…", "connectors": ["…"]}`, http.StatusBadRequest)

		return
	}

	connectors, msg := a.checkTokenSpec(spec)

	if msg != "" {
		http.Error(w, msg, http.StatusBadRequest)

		return
	}

	row, value, err := a.Tokens.Create(r.Context(), spec.Name, connectors)

	if errors.Is(err, store.ErrExists) {
		http.Error(w, fmt.Sprintf("a client token called %q already exists; revoke it first to replace it", spec.Name), http.StatusConflict)

		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Str("client_token", spec.Name).Msg("create client token")
		http.Error(w, "creating the client token failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	a.Logger.Info().Str("client_token", row.Name).Strs("connectors", row.Connectors).Msg("client token created")
	writeJSON(w, TokenCreated{Name: row.Name, Connectors: row.Connectors, Token: value, CreatedAt: row.CreatedAt.UTC()})
}

// checkTokenSpec checks spec against the naming rule and the running
// config. It returns spec's connectors without repeats, in the order
// given, or msg saying what is wrong.
func (a api) checkTokenSpec(spec TokenSpec) (connectors []string, msg string) {
	switch {
	case spec.Name == clienttoken.DefaultName:
		return nil, fmt.Sprintf("%q is reserved for the default client token; choose another name", clienttoken.DefaultName)
	case !config.IsConnectorName(spec.Name):
		// %.64q bounds the echoed name.
		return nil, fmt.Sprintf("client token name %.64q must match %s", spec.Name, config.NameGrammar)
	case len(spec.Connectors) == 0:
		return nil, "a client token needs at least one connector"
	}

	configured := a.Config().Connectors

	for _, c := range spec.Connectors {
		if slices.Contains(connectors, c) {
			continue
		}

		if !slices.Contains(configured, c) {
			return nil, unknownConnector(c, configured)
		}

		connectors = append(connectors, c)
	}

	return connectors, ""
}

// unknownConnector says that no connector is called name, and which are.
func unknownConnector(name string, configured []string) string {
	// %.64q bounds the echoed name.
	msg := fmt.Sprintf("no connector is called %.64q", name)

	if len(configured) == 0 {
		return msg + "; no connectors are configured"
	}

	return msg + "; configured: " + strings.Join(slices.Sorted(slices.Values(configured)), ", ")
}

// listTokens serves GET /admin/tokens.
func (a api) listTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := a.Tokens.List(r.Context())

	if err != nil {
		a.Logger.Error().Err(err).Msg("list client tokens")
		http.Error(w, "listing client tokens failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	res := TokenList{Tokens: make([]Token, 0, len(rows))}

	for _, t := range rows {
		tok := Token{Name: t.Name, Connectors: t.Connectors, CreatedAt: t.CreatedAt.UTC()}

		if t.LastUsedAt != nil {
			at := t.LastUsedAt.UTC()
			tok.LastUsedAt = &at
		}

		res.Tokens = append(res.Tokens, tok)
	}

	writeJSON(w, res)
}

// revokeToken serves DELETE /admin/tokens/{name}. An unknown name is 404.
func (a api) revokeToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := a.Tokens.Revoke(r.Context(), name)

	if errors.Is(err, store.ErrNotFound) {
		// %.64q bounds the echoed name.
		http.Error(w, fmt.Sprintf("no client token is called %.64q", name), http.StatusNotFound)

		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Str("client_token", name).Msg("revoke client token")
		http.Error(w, "revoking the client token failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	a.Logger.Info().Str("client_token", name).Msg("client token revoked")
	writeJSON(w, TokenRevoked{Name: name})
}

// CreateToken calls POST /admin/tokens: it creates a scoped client token
// called name for connectors, and returns it with its value, which the
// daemon never shows again.
func (c *Client) CreateToken(ctx context.Context, name string, connectors []string) (TokenCreated, error) {
	var res TokenCreated
	err := c.do(ctx, http.MethodPost, pathTokens, TokenSpec{Name: name, Connectors: connectors}, &res)

	return res, err
}

// ListTokens calls GET /admin/tokens: the scoped client tokens, without
// their values.
func (c *Client) ListTokens(ctx context.Context) (TokenList, error) {
	var res TokenList
	err := c.do(ctx, http.MethodGet, pathTokens, nil, &res)

	return res, err
}

// RevokeToken calls DELETE /admin/tokens/{name}. An unknown name is an
// error with status 404.
func (c *Client) RevokeToken(ctx context.Context, name string) (TokenRevoked, error) {
	var res TokenRevoked
	err := c.do(ctx, http.MethodDelete, pathTokens+"/"+url.PathEscape(name), nil, &res)

	return res, err
}
