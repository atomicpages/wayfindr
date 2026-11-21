package auth

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// unattendedGrant builds the form of one unattended grant's token request
// (GLOSSARY: Unattended grant) asking for scopes, as oc's client: the
// grant_type and what the grant presents. [unattendedTokens] adds the
// resource indicator, and [oauthClient.exchange] the client
// authentication. Each grant is one such function, in a file of its own.
type unattendedGrant func(ctx context.Context, oc oauthClient, scopes []string) (url.Values, error)

// unattendedGrantOf is the grant function of connector c's unattended
// grant; nil for a grant with a user, which has none.
func (b *Broker) unattendedGrantOf(c config.Connector) unattendedGrant {
	switch c.Auth.Grant {
	case config.GrantClientCredentials:
		return clientCredentials
	case config.GrantJWTBearer:
		return jwtBearer(c.Auth.Assertion, b.resolver)
	case config.GrantAuthorizationCode, config.GrantDeviceCode:
	}

	return nil
}

// heldToken is one connector's token from an unattended grant, in memory.
type heldToken struct {
	tok token
	// scopes are the scopes the grant that got tok asked for.
	scopes []string
	// auth and resource are the connector's [auth] table and resolved url
	// the grant ran under: a token got under another config is not used.
	auth     config.Auth
	resource string
	// key is the grant's DPoP key, made by its first grant proved with
	// DPoP and kept for every later one; nil until then.
	key *dpopKey
}

// unattendedTable holds the unattended grants' tokens, keyed
// userID|connector. It is the only state of an unattended connector: such
// a connector has no credentials row (ADR 0018).
type unattendedTable struct {
	mu sync.Mutex
	// held has an entry for every connector whose grant ran, with a zero
	// token when it has not succeeded, so [Broker.Revoke] can retry it.
	held map[string]heldToken
	// epoch counts the forgets of each key, so a grant that a revoke
	// overtook does not put its token back.
	epoch map[string]uint64
}

func newUnattendedTable() *unattendedTable {
	return &unattendedTable{held: map[string]heldToken{}, epoch: map[string]uint64{}}
}

// begin marks that key's grant runs, and returns its entry and the epoch
// a later put must match.
func (t *unattendedTable) begin(key string) (heldToken, uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h, ok := t.held[key]

	if !ok {
		t.held[key] = h
	}

	return h, t.epoch[key]
}

// get returns key's entry.
func (t *unattendedTable) get(key string) heldToken {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.held[key]
}

// put stores h as key's entry, unless key was forgotten since epoch.
func (t *unattendedTable) put(key string, epoch uint64, h heldToken) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.epoch[key] != epoch {
		return false
	}

	t.held[key] = h

	return true
}

// forget drops key's entry and reports whether it had one.
func (t *unattendedTable) forget(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.held[key]
	delete(t.held, key)
	t.epoch[key]++

	return ok
}

// unattendedTokens is the [tokenSource] of a connector with an unattended
// grant: Wayfindr runs the grant itself when a call needs a token, holds
// the token in memory only, and runs it again when the token nears expiry
// or the upstream rejects it. It never touches the credentials module, so
// such a connector has no stored credential by construction. Any refresh
// token in the answer is ignored: the grant itself is the renewal.
type unattendedTokens struct {
	b *Broker
	// conn is the config the connector is being dialed with.
	conn  config.Connector
	grant unattendedGrant
}

// grantRequest is one call's need for a token from the grant.
type grantRequest struct {
	resource string
	// seen is the access token the caller holds. A flight that finds a
	// different one held returns it without a grant.
	seen string
	// force runs the grant even when the held token is not near expiry
	// (after a 401, or for a step-up).
	force bool
	// challenge is the 401's WWW-Authenticate, for discovery. When set,
	// cached metadata is fetched again (design §7.3).
	challenge []string
	// scopes is a step-up's larger scope set; nil chooses the scopes as
	// [unattendedTokens.scopes] says.
	scopes []string
}

// key is the connector's key in the broker's flights and unattended
// table.
func (u *unattendedTokens) key() string { return u.b.userID + "|" + u.conn.Name }

// token returns the held token, or runs the grant when there is none or it
// is near expiry.
func (u *unattendedTokens) token(ctx context.Context) (token, error) {
	resource, err := u.resource(ctx)

	if err != nil {
		return token{}, err
	}

	h, ok := u.current(resource)

	if ok && !u.b.expiring(h.tok) {
		return h.tok, nil
	}

	return u.run(ctx, grantRequest{resource: resource, seen: h.tok.Access})
}

// renew runs the grant again for a token the upstream answered 401 to,
// with discovery fetched again.
func (u *unattendedTokens) renew(ctx context.Context, why rejection) (token, error) {
	resource, err := u.resource(ctx)

	if err != nil {
		return token{}, err
	}

	// A grant with a challenge, even an empty one, fetches the cached
	// metadata again.
	challenge := why.challenge

	if challenge == nil {
		challenge = []string{}
	}

	return u.run(ctx, grantRequest{resource: resource, seen: why.held.Access, force: true, challenge: challenge})
}

// stepUp runs the grant again with the union of the scopes the last grant
// asked for and scopes, for the request to be replayed with. When scopes
// adds nothing to what the grant already asked for, another grant cannot
// help: it fails scope_denied without one. So each step-up asks for a
// strictly larger set, and the transport replays only once.
func (u *unattendedTokens) stepUp(ctx context.Context, scopes []string) (token, error) {
	resource, err := u.resource(ctx)

	if err != nil {
		return token{}, err
	}

	h, _ := u.current(resource)
	union := slices.Clone(h.scopes)

	for _, s := range scopes {
		if !slices.Contains(union, s) {
			union = append(union, s)
		}
	}

	if len(union) == len(h.scopes) {
		return token{}, u.b.fail(CaseScopeDenied, u.conn.Name,
			"the upstream requires scopes "+strings.Join(scopes, " ")+", which the grant already asked for", "", nil)
	}

	u.b.log.Info().Str("connector", u.conn.Name).Str("scopes", strings.Join(union, " ")).Msg("oauth step-up regrant")

	return u.run(ctx, grantRequest{resource: resource, seen: h.tok.Access, force: true, scopes: union})
}

// refused is the error for a token fresh from the grant that the upstream
// refused: no auth flow can fix it, so the connector is auth_error.
func (u *unattendedTokens) refused(_ context.Context, why rejection) error {
	if why.status == http.StatusForbidden {
		h := u.b.unattended.get(u.key())

		return u.b.fail(CaseScopeDenied, u.conn.Name,
			"the upstream answered 403 insufficient_scope to a token granted for scopes "+strings.Join(h.scopes, " "), "", nil)
	}

	return u.b.fail(CaseGrantRejected, u.conn.Name, "the upstream answered 401 to a token fresh from the grant", "", nil)
}

// resource resolves the connector's URL, which its token is bound to.
func (u *unattendedTokens) resource(ctx context.Context) (string, error) {
	resource, err := u.conn.URL.Resolve(ctx, u.b.resolver)

	if err != nil {
		return "", u.b.fail(CaseUnresolvedValue, u.conn.Name, "connector url", "", err)
	}

	return resource, nil
}

// current returns the held entry if its token was got under the
// connector's current config for resource.
func (u *unattendedTokens) current(resource string) (heldToken, bool) {
	h := u.b.unattended.get(u.key())

	if h.tok.Access == "" || h.resource != resource || !config.AuthEqual(&h.auth, u.conn.Auth) {
		return heldToken{}, false
	}

	return h, true
}

// run returns a token from the grant fresher than req.seen, running the
// grant at most once per connector at a time: concurrent callers share the
// flight's result. The flight is detached from the caller's context, with
// the refresh's timeout, so a grant the server answered is held even if the
// caller gave up.
func (u *unattendedTokens) run(ctx context.Context, req grantRequest) (token, error) {
	var h heldToken

	// A forced caller that joined a lazy flight which found the token
	// still fresh, or one for fewer scopes, needs a flight of its own.
	for range 2 {
		ch := u.b.flights.DoChan(u.key(), func() (any, error) {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
			defer cancel()

			return u.once(fctx, req)
		})
		var res singleflight.Result

		select {
		case res = <-ch:
		case <-ctx.Done():
			return token{}, ctx.Err()
		}

		if res.Err != nil {
			return token{}, res.Err
		}

		h, _ = res.Val.(heldToken)

		if (!req.force || h.tok.Access != req.seen) && covers(h.scopes, req.scopes) {
			break
		}
	}

	return h.tok, nil
}

// covers reports whether have includes every scope of want.
func covers(have, want []string) bool {
	for _, s := range want {
		if !slices.Contains(have, s) {
			return false
		}
	}

	return true
}

// once is the body of a grant flight.
func (u *unattendedTokens) once(ctx context.Context, req grantRequest) (heldToken, error) {
	key, name, a := u.key(), u.conn.Name, *u.conn.Auth
	h, epoch := u.b.unattended.begin(key)
	current := h.tok.Access != "" && h.resource == req.resource && config.AuthEqual(&h.auth, u.conn.Auth)

	if current && covers(h.scopes, req.scopes) &&
		(h.tok.Access != req.seen || (!req.force && !u.b.expiring(h.tok))) {
		return h, nil // another flight already granted
	}

	// The connector's own HTTP client, as its MCP traffic's: discovery and
	// the token request trust its ca_file and present its certificate.
	hc, err := u.b.httpFor(ctx, u.conn)

	if err != nil {
		return heldToken{}, err
	}

	disc, err := u.discover(ctx, req, hc)

	if err != nil {
		return heldToken{}, err
	}

	scopes := u.scopes(req, h, current, disc)
	reg, ok, err := u.b.registrar.configured(ctx, RegisterRequest{Connector: name, Auth: a, Discovery: disc})

	switch {
	case err != nil:
		return heldToken{}, err
	case !ok: // validation requires client_id
		return heldToken{}, u.b.fail(CaseNoRegistration, name, "an unattended grant needs client_id", "", nil)
	}

	oc := u.b.oauthClient(u.conn, hc, disc, reg, h.key)
	form, err := u.grant(ctx, oc, scopes)

	if err != nil {
		return heldToken{}, err
	}

	if a.ResourceIndicator {
		form.Set("resource", req.resource)
	}

	tok, _, err := oc.exchange(ctx, form)

	if err != nil {
		err = grantRejected(err)
		u.logGrant(disc, err)

		return heldToken{}, err
	}

	u.logGrant(disc, nil)

	// The grant itself is the renewal: a refresh token is never kept.
	tok.Refresh, tok.RefreshExpiry = "", time.Time{}
	dk := h.key

	if oc.dpopKey != nil {
		dk = oc.dpopKey
	}

	h = heldToken{tok: tok, scopes: slices.Clone(scopes), auth: a, resource: req.resource, key: dk}

	// A revoke meanwhile forgot the connector: the token serves this call
	// only.
	u.b.unattended.put(key, epoch, h)

	return h, nil
}

// logGrant logs a grant's outcome, err (nil for a token), without any
// response body or token.
func (u *unattendedTokens) logGrant(disc Discovery, err error) {
	ev := u.b.log.Info().Str("outcome", "ok")

	if err != nil {
		ev = u.b.log.Warn().Str("outcome", outcome(err))

		if e, ok := AsError(err); ok {
			ev = ev.Str("case", string(e.Case)).Str("provider_code", e.ProviderCode)
		}
	}

	ev.Str("connector", u.conn.Name).Str("issuer", redact.URL(disc.Issuer)).
		Str("grant_type", string(u.conn.Auth.Grant)).Msg("oauth unattended grant")
}

// scopes are the scopes a grant asks for: a step-up's set; else the set
// the held token was granted for; else the connector's scopes; else the
// scope of the upstream's challenge; else none. The protected resource
// metadata's scopes_supported is never used: it lists what the server
// knows, not what this client may have.
func (u *unattendedTokens) scopes(req grantRequest, h heldToken, current bool, disc Discovery) []string {
	switch {
	case req.scopes != nil:
		return req.scopes
	case current && len(h.scopes) > 0:
		return h.scopes
	case len(u.conn.Auth.Scopes) > 0:
		return u.conn.Auth.Scopes
	}

	return disc.ChallengeScopes
}

// discover runs discovery as a refresh does: from cached documents, and
// fetched again after a 401 (req.challenge set). Discovery takes
// token_endpoint from config only together with authorization_endpoint,
// which an unattended grant has no use for, so a token_endpoint alone
// stands in for both.
func (u *unattendedTokens) discover(ctx context.Context, req grantRequest, hc *http.Client) (Discovery, error) {
	a := *u.conn.Auth

	if a.TokenEndpoint != "" && a.AuthorizationEndpoint == "" {
		a.AuthorizationEndpoint = a.TokenEndpoint
	}

	return u.b.discover.Discover(ctx, DiscoverRequest{
		Connector: u.conn.Name, ResourceURL: req.resource, Auth: a,
		Challenge: req.challenge, Refetch: req.challenge != nil, Client: hc, Mask: u.b.urlMask(ctx, u.conn),
	})
}

// grantRejected is the one translation of a token endpoint's refusal of an
// unattended grant. A refusal that would send an auth flow's connector
// back through the flow (needs_auth), and unauthorized_client (the client
// may not use this grant type), become [CaseGrantRejected], a config
// error with the provider code kept: there is no flow to go through.
// client_rejected, transient errors, and other config errors pass through.
func grantRejected(err error) error {
	e, ok := AsError(err)

	if !ok || (e.Kind != KindNeedsAuth && (e.Case != CaseClientRejected || e.ProviderCode != "unauthorized_client")) {
		return err
	}

	g := newError(CaseGrantRejected, e.Connector, e.Detail, e.Err)
	g.ProviderCode = e.ProviderCode

	return g.seam()
}
