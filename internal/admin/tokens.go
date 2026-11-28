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
// (docs/internal/plans/scoped-client-tokens.md), and lists and revokes the
// OAuth grants that share their name space (ADR 0022). The daemon adapts the
// store and the registry httpd resolves bearers with to it, so a token is
// accepted from its creation and refused from its revoke. Admin checks the
// request before it calls Create; ClientTokens checks nothing of it.
type ClientTokens interface {
	// Create generates a token called name for connectors, stores its
	// digest, and starts accepting it. It returns the stored row and the
	// token's value, which nothing keeps. remote makes the token one that
	// may reach /mcp through a tunnel (ADR 0021); it cannot be changed
	// later. approval is the token's approval entries (ADR 0024), stored as
	// given; nil gates nothing, and admin passes nil, never an empty list. A name already taken is an error wrapping
	// store.ErrExists.
	Create(ctx context.Context, name string, connectors []string, remote bool, approval []string) (store.ClientToken, string, error)
	// List returns every scoped token, ordered by name.
	List(ctx context.Context) ([]store.ClientToken, error)
	// ListGrants returns every OAuth grant, ordered by name.
	ListGrants(ctx context.Context) ([]store.OAuthGrant, error)
	// Revoke deletes the scoped token or else the OAuth grant called name
	// and stops accepting it. An unknown name is an error wrapping
	// store.ErrNotFound.
	Revoke(ctx context.Context, name string) error
}

// TokenSpec is the body of POST /admin/tokens.
type TokenSpec struct {
	// Name follows the connector-name rule and is never "default".
	Name string `json:"name"`
	// Connectors are the configured connectors the token may use; at least
	// one. A name given twice counts once.
	Connectors []string `json:"connectors"`
	// Remote makes the token one that may reach /mcp through a tunnel (ADR
	// 0021). It is fixed at creation; to change it, revoke the token and
	// create another. Absent means false.
	Remote bool `json:"remote,omitempty"`
	// Approval are the token's approval entries (ADR 0024,
	// config.ParseApproval): the calls that wait for the owner's approval.
	// Only a remote token takes them, and a <connector>.<glob> entry must
	// name one of Connectors. Fixed at creation, like Remote. Absent gates
	// nothing.
	Approval []string `json:"approval,omitempty"`
}

// TokenCreated is the response to POST /admin/tokens. It is the only
// response that ever holds a token's value.
type TokenCreated struct {
	Name string `json:"name"`
	// Connectors are the request's, without repeats, in the order given.
	Connectors []string `json:"connectors"`
	// Remote is whether the token may reach /mcp through a tunnel.
	Remote bool `json:"remote"`
	// Approval are the request's approval entries; absent when it had none.
	Approval []string `json:"approval,omitempty"`
	// Token is the value a client presents as its bearer. It cannot be
	// read back: only its SHA-256 is stored.
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

// TokenKind is what a listed Token is.
type TokenKind string

const (
	// TokenKindToken is a scoped client token.
	TokenKindToken TokenKind = "token"
	// TokenKindOAuth is an OAuth grant (ADR 0022).
	TokenKindOAuth TokenKind = "oauth"
)

// TokenList is the response to GET /admin/tokens.
type TokenList struct {
	// Tokens are the scoped tokens and the OAuth grants, ordered by name.
	// Always an array.
	Tokens []Token `json:"tokens"`
}

// Token is one scoped client token or OAuth grant as GET /admin/tokens lists
// it, without its value.
type Token struct {
	Name string `json:"name"`
	// Kind is TokenKindToken, or TokenKindOAuth for an OAuth grant.
	Kind       TokenKind `json:"kind"`
	Connectors []string  `json:"connectors"`
	// Client is an OAuth grant's client ID; absent for a scoped token.
	Client string `json:"client,omitempty"`
	// Remote is whether the token may reach /mcp through a tunnel; always
	// true for an OAuth grant.
	Remote bool `json:"remote"`
	// Approval are the approval entries (ADR 0024); absent when there are
	// none.
	Approval  []string  `json:"approval,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// LastUsedAt is when a client last presented the token (for a grant, its
	// access token), as the daemon
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

	connectors, msg := a.checkTokenSpec(spec, "client token")

	var approval []string

	if msg == "" {
		approval, msg = checkApproval(spec.Approval, spec.Remote, connectors, "client token")
	}

	if msg != "" {
		http.Error(w, msg, http.StatusBadRequest)

		return
	}

	row, value, err := a.Tokens.Create(r.Context(), spec.Name, connectors, spec.Remote, approval)

	if errors.Is(err, store.ErrExists) {
		http.Error(w, fmt.Sprintf("a client token or OAuth grant called %q already exists", spec.Name), http.StatusConflict)

		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Str("client_token", spec.Name).Msg("create client token")
		http.Error(w, "creating the client token failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	a.Logger.Info().Str("client_token", row.Name).Strs("connectors", row.Connectors).Bool("remote", row.Remote).
		Strs("approval_entries", row.Approval).Msg("client token created")
	writeJSON(w, TokenCreated{
		Name: row.Name, Connectors: row.Connectors, Remote: row.Remote, Approval: row.Approval,
		Token: value, CreatedAt: row.CreatedAt.UTC(),
	})
}

// LocalApprovalMessage is the 400 body of approval entries on a token that
// isn't remote (ADR 0024's threat model). The CLI checks the same rule
// before the request and says the same words.
const LocalApprovalMessage = "Approval gates protect only remote clients: " +
	"a local agent can approve its own calls with the default token."

// checkApproval checks a client's approval entries (ADR 0024) against its
// remote flag and its connectors, already checked: entries that parse, on a
// remote client, naming only its connectors, since an entry for another
// would gate nothing. It returns the entries to store, nil for none, so
// an empty list is stored as no list; or msg saying what is wrong. noun
// names the client in msg, as for checkTokenSpec: "client token", or
// "connection" for an OAuth grant, which is always remote.
func checkApproval(entries []string, remote bool, connectors []string, noun string) (approval []string, msg string) {
	if len(entries) == 0 {
		return nil, ""
	}

	if !remote {
		return nil, LocalApprovalMessage
	}

	rule, err := config.ParseApproval(entries)

	if err != nil {
		return nil, err.Error()
	}

	for _, c := range rule.Connectors() {
		if !slices.Contains(connectors, c) {
			// %.64q bounds the echoed name.
			return nil, fmt.Sprintf("an approval entry names connector %.64q, which this %s can't use; its connectors: %s",
				c, noun, strings.Join(connectors, ", "))
		}
	}

	return entries, ""
}

// checkTokenSpec checks spec against the naming rule and the running
// config. It returns spec's connectors without repeats, in the order
// given, or msg saying what is wrong. subject names what is being made in
// msg: "client token", or "connection" on the OAuth approve page.
func (a api) checkTokenSpec(spec TokenSpec, subject string) (connectors []string, msg string) {
	switch {
	case spec.Name == clienttoken.DefaultName:
		return nil, fmt.Sprintf("%q is reserved for the default client token; choose another name", clienttoken.DefaultName)
	case !config.IsConnectorName(spec.Name):
		// %.64q bounds the echoed name.
		return nil, fmt.Sprintf("%s name %.64q must match %s", subject, spec.Name, config.NameGrammar)
	case len(spec.Connectors) == 0:
		return nil, "a " + subject + " needs at least one connector"
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

// listTokens serves GET /admin/tokens: the scoped tokens and the OAuth
// grants, merged and ordered by name.
func (a api) listTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := a.Tokens.List(r.Context())

	if err != nil {
		a.listTokensFailed(w, err)

		return
	}

	grants, err := a.Tokens.ListGrants(r.Context())

	if err != nil {
		a.listTokensFailed(w, err)

		return
	}

	tokens := make([]Token, 0, len(rows))

	for _, t := range rows {
		tokens = append(tokens, Token{
			Name: t.Name, Kind: TokenKindToken, Connectors: t.Connectors, Remote: t.Remote, Approval: t.Approval,
			CreatedAt: t.CreatedAt.UTC(), LastUsedAt: utcPtr(t.LastUsedAt),
		})
	}

	oauth := make([]Token, 0, len(grants))

	for _, g := range grants {
		oauth = append(oauth, Token{
			Name: g.Name, Kind: TokenKindOAuth, Connectors: g.Connectors, Client: g.ClientID, Remote: true, Approval: g.Approval,
			CreatedAt: g.CreatedAt.UTC(), LastUsedAt: utcPtr(g.LastUsedAt),
		})
	}

	writeJSON(w, TokenList{Tokens: mergeByName(tokens, oauth)})
}

// mergeByName merges two lists, each in name order as the store returns it,
// into one. The names are unique across both: tokens and grants share a name
// space.
func mergeByName(a, b []Token) []Token {
	out := make([]Token, 0, len(a)+len(b))

	for len(a) > 0 && len(b) > 0 {
		if strings.Compare(b[0].Name, a[0].Name) < 0 {
			out, b = append(out, b[0]), b[1:]
		} else {
			out, a = append(out, a[0]), a[1:]
		}
	}

	return append(append(out, a...), b...)
}

func (a api) listTokensFailed(w http.ResponseWriter, err error) {
	a.Logger.Error().Err(err).Msg("list client tokens and oauth grants")
	http.Error(w, "listing client tokens and OAuth grants failed; see the daemon log", http.StatusInternalServerError)
}

// utcPtr is t in UTC, nil when t is.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}

	at := t.UTC()

	return &at
}

// revokeToken serves DELETE /admin/tokens/{name}, for a scoped token or an
// OAuth grant. An unknown name is 404.
func (a api) revokeToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := a.Tokens.Revoke(r.Context(), name)

	if errors.Is(err, store.ErrNotFound) {
		// %.64q bounds the echoed name.
		http.Error(w, fmt.Sprintf("no client token or OAuth grant is called %.64q", name), http.StatusNotFound)

		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Str("client_token", name).Msg("revoke client token or oauth grant")
		http.Error(w, "revoking the client token or OAuth grant failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	a.Logger.Info().Str("client_token", name).Msg("client token or oauth grant revoked")
	writeJSON(w, TokenRevoked{Name: name})
}

// CreateToken calls POST /admin/tokens: it creates the scoped client token
// spec describes and returns it with its value, which the daemon never shows
// again.
func (c *Client) CreateToken(ctx context.Context, spec TokenSpec) (TokenCreated, error) {
	var res TokenCreated
	err := c.do(ctx, http.MethodPost, pathTokens, spec, &res)

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
