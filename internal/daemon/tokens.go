package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/oauthserver"
	"github.com/atomicpages/wayfindr/internal/store"
)

const (
	// touchEvery is how often the daemon records when each scoped client
	// token was last used. Lookups only mark the registry; this bounds how
	// late a token's last_used_at is, and how often the store is written.
	touchEvery = time.Minute
	// touchTimeout bounds one flush of the touches, at shutdown included.
	touchTimeout = 5 * time.Second
	// oauthPruneEvery is how often the daemon deletes what OAuth no longer needs
	// (expired access tokens, long-spent refresh tokens, grants never
	// exchanged), and oauthPruneTimeout bounds one pass.
	oauthPruneEvery   = time.Hour
	oauthPruneTimeout = 10 * time.Second
	// oauthRetryTimeout bounds one retry of a failed applyOAuth, which run
	// makes every touchEvery until one succeeds.
	oauthRetryTimeout = 10 * time.Second
	// freeNameLimit is the highest suffix freeName tries: base-2 to base-99.
	freeNameLimit = 99
)

// clientTokens are the scoped client tokens (docs/internal/plans/
// scoped-client-tokens.md): the store keeps them, and the registry, which
// httpd resolves bearers with, holds them in memory. Every change goes to the
// store first, so a failed write leaves the registry as it was.
type clientTokens struct {
	store    store.Store
	userID   string
	registry *clienttoken.Registry
	clock    Clock
	log      zerolog.Logger

	// mu serializes every change to the tokens and grants, each held across
	// its store write and its registry update. Without it a revoke racing a
	// create of the same name could delete the new row and find nothing in
	// the registry yet, and the create's Add would then leave a live token
	// with no row; and a revoke racing a rotation of an OAuth grant could
	// delete the grant, after which the rotation's AddUntil would leave the
	// revoked grant's access token accepted for up to an hour.
	mu sync.Mutex
	// resource is the current oauthserver.Resource(issuer), "" while OAuth
	// is off. A grant approved for another resource is suspended.
	resource string
	// clients are the current server.oauth.clients; a grant of another
	// client is suspended.
	clients map[string]struct{}
	// live are the grant names whose access token this has put in the
	// registry, so applyOAuth can take them out again.
	live map[string]struct{}
	// stale is set when applyOAuth could not finish: no grant's access
	// token is accepted meanwhile, run retries the load every touchEvery,
	// and the next applyOAuth redoes it even if the config did not change.
	stale bool
}

var (
	_ admin.ClientTokens = (*clientTokens)(nil)
	_ oauthserver.Grants = (*clientTokens)(nil)
)

// loadClientTokens reads every scoped client token of userID into a new
// registry, so httpd accepts them from its first request. now, the daemon
// clock's, stamps the registry's touches.
func loadClientTokens(ctx context.Context, st store.Store, userID string, now func() time.Time) (*clienttoken.Registry, error) {
	rows, err := st.ListClientTokens(ctx, userID)

	if err != nil {
		return nil, fmt.Errorf("load client tokens: %w", err)
	}

	clients := make(map[[sha256.Size]byte]clienttoken.Client, len(rows))

	for _, t := range rows {
		clients[t.Digest] = registryClient(t)
	}

	return clienttoken.NewRegistry(clients, now), nil
}

// registryClient is what the registry keeps of a stored token: everything
// httpd needs to resolve its bearer, and its creation time, which tells it
// apart from a later token of the same name; nothing of its digest or its
// use.
func registryClient(t store.ClientToken) clienttoken.Client {
	return clienttoken.Client{Name: t.Name, Connectors: t.Connectors, Approval: t.Approval, Remote: t.Remote, Created: t.CreatedAt}
}

// Create implements admin.ClientTokens: it generates the value, stores its
// digest, and only then adds it to the registry.
func (t *clientTokens) Create(ctx context.Context, name string, connectors []string, remote bool, approval []string) (store.ClientToken, string, error) {
	value, err := clienttoken.Generate()

	if err != nil {
		return store.ClientToken{}, "", err
	}

	row := store.ClientToken{
		UserID:     t.userID,
		Name:       name,
		Digest:     clienttoken.Digest(value),
		Connectors: slices.Clone(connectors),
		Remote:     remote,
		// Nil for none: admin never passes an empty list, so the registry
		// holds what the store reads back after a restart.
		Approval: slices.Clone(approval),
		// The store keeps milliseconds: truncating here makes the create's
		// answer the time a list reads back.
		CreatedAt: t.clock.Now().UTC().Truncate(time.Millisecond),
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.store.CreateClientToken(ctx, row); err != nil {
		return store.ClientToken{}, "", err
	}

	t.registry.Add(row.Digest, registryClient(row))

	return row, value, nil
}

// List implements admin.ClientTokens.
func (t *clientTokens) List(ctx context.Context) ([]store.ClientToken, error) {
	return t.store.ListClientTokens(ctx, t.userID)
}

// ListGrants implements admin.ClientTokens.
func (t *clientTokens) ListGrants(ctx context.Context) ([]store.OAuthGrant, error) {
	return t.store.ListOAuthGrants(ctx, t.userID)
}

// Revoke implements admin.ClientTokens: it deletes the client token called
// name, or else the OAuth grant of that name (with its tokens), from the
// store, then from the registry, which refuses it from the next request on.
// A name neither table has is an error wrapping store.ErrNotFound.
func (t *clientTokens) Revoke(ctx context.Context, name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	err := t.store.DeleteClientToken(ctx, t.userID, name)

	if errors.Is(err, store.ErrNotFound) {
		err = t.store.DeleteOAuthGrant(ctx, t.userID, name)
	}

	if err != nil {
		return err
	}

	t.drop(name)

	return nil
}

// drop stops accepting the token called name. Callers hold mu.
func (t *clientTokens) drop(name string) {
	t.registry.Remove(name)
	delete(t.live, name)
}

// CreateGrant implements oauthserver.Grants. A grant's name is in the space
// client tokens' are, so it follows the same rule: a connector name, and
// never the default token's.
func (t *clientTokens) CreateGrant(ctx context.Context, g store.OAuthGrant) error {
	if !config.IsConnectorName(g.Name) {
		return fmt.Errorf("grant name %q: lowercase letters, digits, and hyphens only: %w", g.Name, oauthserver.ErrInvalidName)
	}

	if g.Name == clienttoken.DefaultName {
		return fmt.Errorf("grant name %q is the default token's: %w", g.Name, oauthserver.ErrInvalidName)
	}

	g.UserID = t.userID
	g.Connectors = slices.Clone(g.Connectors)

	t.mu.Lock()
	defer t.mu.Unlock()

	return t.store.CreateOAuthGrant(ctx, g)
}

// Grant implements oauthserver.Grants.
func (t *clientTokens) Grant(ctx context.Context, name string) (store.OAuthGrant, error) {
	return t.store.GetOAuthGrant(ctx, t.userID, name)
}

// GrantToken implements oauthserver.Grants.
func (t *clientTokens) GrantToken(ctx context.Context, d [sha256.Size]byte) (store.OAuthToken, error) {
	return t.store.GetOAuthToken(ctx, t.userID, d)
}

// IssueGrantTokens implements oauthserver.Grants: it stores a grant's first
// tokens, and only then starts accepting its access token. The grant is read
// under the lock that Revoke and applyOAuth take, so a suspension or a revoke
// cannot fall between the check and the registry update. created is the
// CreatedAt of the grant the code was made for: a grant of that name created
// at another time is another grant, re-created since, and gets nothing.
func (t *clientTokens) IssueGrantTokens(ctx context.Context, created time.Time, access, refresh store.OAuthToken) error {
	access.UserID, refresh.UserID = t.userID, t.userID

	t.mu.Lock()
	defer t.mu.Unlock()

	g, err := t.liveGrant(ctx, access, &created)

	if err != nil {
		return err
	}

	if err := t.store.IssueOAuthTokens(ctx, t.userID, access, refresh); err != nil {
		return err
	}

	t.accept(g, access)

	return nil
}

// RotateGrantTokens implements oauthserver.Grants: it spends the refresh
// token spent and stores its successors, then accepts the new access token in
// place of the grant's previous one.
func (t *clientTokens) RotateGrantTokens(ctx context.Context, spent [sha256.Size]byte, access, refresh store.OAuthToken) error {
	access.UserID, refresh.UserID = t.userID, t.userID

	t.mu.Lock()
	defer t.mu.Unlock()

	g, err := t.liveGrant(ctx, access, nil)

	if err != nil {
		return err
	}

	if err := t.store.RotateOAuthTokens(ctx, t.userID, spent, t.clock.Now().UTC().Truncate(time.Millisecond), access, refresh); err != nil {
		return err
	}

	t.accept(g, access)

	return nil
}

// RevokeGrant implements oauthserver.Grants. Unlike Revoke it never deletes a
// client token.
func (t *clientTokens) RevokeGrant(ctx context.Context, name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.store.DeleteOAuthGrant(ctx, t.userID, name); err != nil {
		return err
	}

	t.drop(name)

	return nil
}

// liveGrant reads the grant that access belongs to, for issuing or rotating:
// it is store.ErrNotFound when the grant is gone or, with created set, was
// created at another time (to the millisecond, as the store keeps it), and
// oauthserver.ErrSuspended when it is suspended. Callers hold mu.
func (t *clientTokens) liveGrant(ctx context.Context, access store.OAuthToken, created *time.Time) (store.OAuthGrant, error) {
	if access.ExpiresAt == nil {
		return store.OAuthGrant{}, errors.New("access token has no expiry")
	}

	g, err := t.store.GetOAuthGrant(ctx, t.userID, access.Grant)

	if err != nil {
		return store.OAuthGrant{}, err
	}

	if created != nil && !g.CreatedAt.Truncate(time.Millisecond).Equal(created.Truncate(time.Millisecond)) {
		return store.OAuthGrant{}, fmt.Errorf("grant %q was re-created: %w", g.Name, store.ErrNotFound)
	}

	if t.suspended(g) {
		return store.OAuthGrant{}, oauthserver.ErrSuspended
	}

	return g, nil
}

// accept registers access, a stored access token of g, until it expires. It
// replaces g's previous access token, if any. Callers hold mu.
func (t *clientTokens) accept(g store.OAuthGrant, access store.OAuthToken) {
	t.registry.AddUntil(access.Digest, grantClient(g), *access.ExpiresAt)
	t.live[g.Name] = struct{}{}
}

// grantClient is what httpd needs to resolve a grant's access token: a remote
// scoped client, so it reaches /mcp through the tunnel and nothing of
// /admin. It carries the grant's creation time, not the access token's,
// which rotates, so every access token of one grant is one instance.
func grantClient(g store.OAuthGrant) clienttoken.Client {
	return clienttoken.Client{Name: g.Name, Connectors: g.Connectors, Approval: g.Approval, Remote: true, Created: g.CreatedAt}
}

// suspended reports whether g is suspended: OAuth is off, its resource is not
// the current one (public_url changed), or its client is no longer allowed.
// Undoing the change lifts it. Callers hold mu.
func (t *clientTokens) suspended(g store.OAuthGrant) bool {
	if t.resource == "" || g.Resource != t.resource {
		return true
	}

	_, ok := t.clients[g.ClientID]

	return !ok
}

// applyOAuth makes the registry agree with s, the running [server] table.
// When the resource or the allowed clients changed it takes every grant's
// access token out of the registry, then, with OAuth on, puts back the
// unexpired ones of the grants that are not suspended. So turning OAuth off,
// changing public_url, or removing a client stops those grants at once, and
// undoing the change brings back the tokens not yet expired. A store error
// is returned and leaves no grant's access token accepted (it fails closed)
// until a retry loads them: run's every touchEvery, or the next call's.
func (t *clientTokens) applyOAuth(ctx context.Context, s config.Server) error {
	resource := ""
	clients := map[string]struct{}{}

	if s.OAuth.Enabled {
		resource = oauthserver.Resource(s.Issuer())

		for _, c := range s.OAuth.Clients {
			clients[c] = struct{}{}
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.stale && resource == t.resource && maps.Equal(clients, t.clients) {
		return nil
	}

	t.resource, t.clients = resource, clients

	return t.loadLocked(ctx)
}

// retryOAuth redoes the load of a failed applyOAuth for the [server] table
// it was given, which resource and clients still hold; it does nothing when
// the last one succeeded. A failure is logged, and run retries again a
// touchEvery later.
func (t *clientTokens) retryOAuth(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.stale {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, oauthRetryTimeout)
	defer cancel()

	if err := t.loadLocked(ctx); err != nil {
		t.log.Warn().Err(err).Msg("retry oauth grants; their access tokens stay refused")

		return
	}

	t.log.Info().Msg("oauth grants applied on retry")
}

// loadLocked takes every grant's access token out of the registry, then,
// with OAuth on, puts back the unexpired ones of the grants that resource
// and clients do not suspend. It sets stale until the load succeeds.
// Callers hold mu.
func (t *clientTokens) loadLocked(ctx context.Context) error {
	for name := range t.live {
		t.registry.Remove(name)
	}

	clear(t.live)

	if t.resource == "" {
		t.stale = false

		return nil
	}

	// Until the load succeeds, a retry starts over.
	t.stale = true

	tokens, err := t.store.ListOAuthAccessTokens(ctx, t.userID, t.clock.Now())

	if err != nil {
		return fmt.Errorf("load oauth access tokens: %w", err)
	}

	grants, err := t.store.ListOAuthGrants(ctx, t.userID)

	if err != nil {
		return fmt.Errorf("load oauth grants: %w", err)
	}

	byName := make(map[string]store.OAuthGrant, len(grants))

	for _, g := range grants {
		byName[g.Name] = g
	}

	for _, tok := range tokens {
		g, ok := byName[tok.Grant]

		if !ok || tok.ExpiresAt == nil || t.suspended(g) {
			continue
		}

		t.accept(g, tok)
	}

	t.stale = false

	return nil
}

// errNoFreeName is freeName's answer when base and base-2 to base-99 are all
// taken.
var errNoFreeName = errors.New("no free name")

// freeName returns base, or else base-2 to base-99, whichever is the first
// that neither a client token nor an OAuth grant has, and is not the default
// token's name. It is a suggestion: the name may be taken before it is used.
func (t *clientTokens) freeName(ctx context.Context, base string) (string, error) {
	tokens, err := t.store.ListClientTokens(ctx, t.userID)

	if err != nil {
		return "", err
	}

	grants, err := t.store.ListOAuthGrants(ctx, t.userID)

	if err != nil {
		return "", err
	}

	taken := map[string]struct{}{clienttoken.DefaultName: {}}

	for _, tok := range tokens {
		taken[tok.Name] = struct{}{}
	}

	for _, g := range grants {
		taken[g.Name] = struct{}{}
	}

	for i := 1; i <= freeNameLimit; i++ {
		name := base

		if i > 1 {
			name = base + "-" + strconv.Itoa(i)
		}

		if _, ok := taken[name]; !ok {
			return name, nil
		}
	}

	return "", fmt.Errorf("%w from %q to %q", errNoFreeName, base, base+"-"+strconv.Itoa(freeNameLimit))
}

// run records the registry's touches every touchEvery until ctx ends, then
// once more, so the uses of the last minute are not lost at shutdown. On the
// same timer it retries a failed applyOAuth, and it prunes OAuth's rows when
// it starts and then every oauthPruneEvery.
func (t *clientTokens) run(ctx context.Context) {
	pruned := t.clock.Now()
	t.prune(ctx)
	timer := t.clock.NewTimer(touchEvery)

	for {
		select {
		case <-timer.C():
			t.flush(ctx)
			t.retryOAuth(ctx)

			if now := t.clock.Now(); now.Sub(pruned) >= oauthPruneEvery {
				pruned = now
				t.prune(ctx)
			}

			timer = t.clock.NewTimer(touchEvery)
		case <-ctx.Done():
			timer.Stop()
			t.flush(context.WithoutCancel(ctx))

			return
		}
	}
}

// flush writes the touches since the last flush. Each touch goes to both
// tables: each store call ignores a digest that is not its own. A failed
// write is logged and dropped: it costs only some tokens' last_used_at.
func (t *clientTokens) flush(ctx context.Context) {
	used := t.registry.Touches()

	if len(used) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, touchTimeout)
	defer cancel()

	if err := t.store.TouchClientTokens(ctx, t.userID, used); err != nil {
		t.log.Warn().Err(err).Int("client_tokens", len(used)).Msg("record client token use")
	}

	if err := t.store.TouchOAuthGrants(ctx, t.userID, used); err != nil {
		t.log.Warn().Err(err).Int("client_tokens", len(used)).Msg("record oauth grant use")
	}
}

// prune deletes the OAuth rows nothing needs any more. A failure is logged
// and retried an hour later.
func (t *clientTokens) prune(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, oauthPruneTimeout)
	defer cancel()

	if _, err := t.store.PruneOAuth(ctx, t.userID, t.clock.Now()); err != nil {
		t.log.Warn().Err(err).Msg("prune oauth tokens")
	}
}

// keepTouches runs the touch flush until ctx ends or stop is called; stop
// waits for the last flush.
func (d *Daemon) keepTouches(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		d.tokens.run(ctx)
	}()

	return func() {
		cancel()
		<-done
	}
}
