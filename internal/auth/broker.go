package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/httpclient"
	"github.com/atomicpages/wayfindr/internal/redact"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/store"
)

// BrokerOptions configures a [Broker].
type BrokerOptions struct {
	// Store holds credentials and cached metadata.
	Store store.Store
	// Sealer seals tokens and opens DCR clients.
	Sealer *secrets.Sealer
	// Resolver resolves connector URLs and config client secrets.
	Resolver config.Resolver
	// Client sends token and metadata requests when Clients is nil. Nil
	// means http.DefaultClient.
	Client *http.Client
	// Clients holds each connector's HTTP client, built from its [tls]
	// table: every request to a connector's authorization server (and
	// discovery's to its resource) goes through the connector's, the same
	// one its MCP traffic uses, so both trust its ca_file and present its
	// client certificate. Nil sends them all through Client.
	Clients *httpclient.Clients
	// Logger receives refresh outcomes. Tokens are never logged.
	Logger zerolog.Logger
	// UserID owns the credentials (the single local user).
	UserID string
	// Listen is the daemon's listen address, for the auth link in
	// a needs_auth [*Error]. Empty means config.DefaultListen.
	Listen string
	// Now is the clock token expiry is judged by. Nil means time.Now.
	Now func() time.Time

	// OnCredentialChanged is called after a connector's credential changed:
	// a token from a new grant was stored (storeGrant), or the credential
	// was deleted. A refresh renews the same grant and does not call it.
	// The daemon wires it to upstream.Manager.CredentialChanged, which
	// reconnects the connector; it must return quickly, because it runs on
	// the goroutine that stored the credential (the browser's callback
	// request, for the auth flow). Optional.
	OnCredentialChanged func(connector string)
}

// Broker is the OAuth broker (design §7.3). Its Transport authorizes
// upstream requests with the connector's stored token, refreshing lazily
// and single-flight per credential (ADR 0004).
type Broker struct {
	resolver config.Resolver
	client   *http.Client
	// clients is BrokerOptions.Clients; nil-able (see httpFor).
	clients  *httpclient.Clients
	log      zerolog.Logger
	userID   string
	listen   string
	now      func() time.Time
	discover *Discoverer
	// inspectors read every token response (see [oauthClient.exchange]).
	inspectors Inspectors
	// onCredentialChanged is BrokerOptions.OnCredentialChanged; nil-able.
	onCredentialChanged func(connector string)

	flights singleflight.Group
	// flows are the auth flows waiting for, or done with, their callback.
	flows *flowTable
	// creds is the one owner of the credentials rows: every read and write
	// of a row goes through it, the Registrar's included.
	creds *credentials
	// registrar chooses the OAuth client, for a refresh and for a flow.
	registrar *Registrar
	// unattended holds the unattended grants' tokens, in memory only.
	unattended *unattendedTable
	// nonces are the DPoP nonces servers sent, by origin and role.
	nonces *nonceCache

	// sleep waits between a device code flow's polls; tests replace it
	// (see export_test.go).
	sleep func(ctx context.Context, d time.Duration) error
	// pollers are the running device code pollers, which Close waits for.
	pollers sync.WaitGroup
	// mu guards closed.
	mu sync.Mutex
	// closed is set by Close: no poller starts from then on.
	closed bool
}

// NewBroker returns a Broker configured by o.
func NewBroker(o BrokerOptions) *Broker {
	b := &Broker{
		resolver:   o.Resolver,
		client:     o.Client,
		clients:    o.Clients,
		log:        o.Logger,
		userID:     o.UserID,
		listen:     o.Listen,
		now:        o.Now,
		inspectors: defaultInspectors(),
		flows:      newFlowTable(),
		sleep:      sleep,
		unattended: newUnattendedTable(),
		nonces:     newNonceCache(),
		creds:      &credentials{store: o.Store, sealer: o.Sealer, userID: o.UserID},

		onCredentialChanged: o.OnCredentialChanged,
	}

	if b.client == nil {
		b.client = http.DefaultClient
	}

	if b.listen == "" {
		b.listen = config.DefaultListen
	}

	if b.now == nil {
		b.now = time.Now
	}

	b.discover = NewDiscoverer(o.Store, b.client, b.log)
	b.discover.now = b.now
	b.registrar = newRegistrar(b.creds, b.resolver, b.client, b.log)

	return b
}

// Close ends every pending auth flow, as [CaseFlowCancelled], which stops
// the device code pollers, and waits for them to return. No poller starts
// once it has begun, and none stores a token after it returns. The daemon
// calls it at shutdown, before it closes the store the pollers write to.
func (b *Broker) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()

	b.flows.endAll(func(f *flow) error {
		return b.fail(CaseFlowCancelled, f.Connector, "the daemon is stopping", "", nil)
	})
	b.pollers.Wait()
}

// startPoller runs poll on a goroutine of its own, the one poller of
// pending flow f. Its context keeps ctx's values but not its cancellation:
// it ends when f does (a revoke, a config change, the poll's own end) or
// the broker closes. A flow that started as the broker closed, or that
// ended meanwhile, is not polled; the error is the one it ended with.
func (b *Broker) startPoller(ctx context.Context, f *flow, poll func(ctx context.Context)) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		res, _ := b.flows.finish(f, FlowFailed, b.fail(CaseFlowCancelled, f.Connector, "the daemon is stopping", "", nil))

		return res.Err
	}

	pctx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	if err := b.flows.attach(f, cancel); err != nil {
		return err
	}

	b.pollers.Go(func() {
		defer cancel()
		poll(pctx)
	})

	return nil
}

// sleep waits d, or until ctx ends, which it reports as ctx's error.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Transport wraps next with the OAuth credential of c, the config the
// connector is being dialed with. It returns next itself for a connector
// that doesn't use OAuth, which matches upstream.Options.AuthTransport.
//
// The broker keeps no copy of the config: upstream calls Transport on every
// connect, and a config change rebuilds the connection, so c is never stale
// and a new OAuth connector's first connect is already authorized.
//
// It is the one selector of the token source: an unattended grant's tokens
// are held in memory ([unattendedTokens]), and an auth flow's are stored
// ([storedTokens]). Nothing else in the transport knows the grant.
//
// Its sender sends each token as its grant's sender bound it: the grant's
// [Broker.oauthClient] chose bearer or DPoP with [chooseSender], and the
// token records the outcome, its DPoP key and its token_type. So upstream
// requests agree with the token requests that got the token, a token from
// before a config change included, and a token the authorization server
// did not bind goes as a bearer token.
func (b *Broker) Transport(c config.Connector, next http.RoundTripper) http.RoundTripper {
	if !c.UsesOAuth() {
		return next
	}

	var tokens tokenSource = &storedTokens{b: b, conn: c}

	if c.Auth.Grant.Unattended() {
		tokens = &unattendedTokens{b: b, conn: c, grant: b.unattendedGrantOf(c)}
	}

	return &roundTripper{
		connector: c.Name,
		link:      b.Link(c.Name),
		tokens:    tokens,
		sender:    dpop{nonces: b.nonces, now: b.now},
		next:      next,
	}
}

// urlMask is the mask of connector c's url: each value its references
// resolve to, replaced by the reference. Discovery takes it, so what it
// logs, returns, and caches never holds a secret from the url, such as one
// in its path (wayfindr-wt2v). A url that can't be resolved masks nothing;
// nothing was resolved.
func (b *Broker) urlMask(ctx context.Context, c config.Connector) redact.Values {
	_, used, err := c.URL.ResolveRefs(ctx, b.resolver)

	if err != nil {
		return redact.Values{}
	}

	return config.Mask(used)
}

// httpFor returns connector c's HTTP client, which its requests to the
// authorization server, and discovery's to its resource, go through (see
// BrokerOptions.Clients). A client that cannot be built is a [KindConfig]
// error (see [clientSetupCase]); an unresolvable tls key keeps its
// *config.ResolveError, so upstream names the reference.
func (b *Broker) httpFor(ctx context.Context, c config.Connector) (*http.Client, error) {
	if b.clients == nil {
		return b.client, nil
	}

	hc, err := b.clients.For(ctx, c)

	if err != nil {
		return nil, b.fail(clientSetupCase(err), c.Name, "connector tls", "", err)
	}

	return hc, nil
}

// Link is the auth link of connector, http://<listen>/auth/<connector>
// with the name escaped: the browser page that starts its auth flow. It is
// the one builder of the link: every needs_auth [*Error] carries it, and
// the daemon gives it to upstream as the link of a connector that needs
// auth.
func (b *Broker) Link(connector string) string {
	return "http://" + b.listen + "/auth/" + url.PathEscape(connector)
}

// fail returns an [*Error] about connector as the error the seam returns.
// A needs_auth one carries the auth link.
func (b *Broker) fail(c Case, connector, detail, providerCode string, cause error) error {
	return linkedFail(b.Link(connector), c, connector, detail, providerCode, cause)
}

// linkedFail is [Broker.fail] for code that holds the connector's auth link
// rather than the broker.
func linkedFail(link string, c Case, connector, detail, providerCode string, cause error) error {
	e := newError(c, connector, detail, cause)
	e.ProviderCode = providerCode

	if e.Kind == KindNeedsAuth {
		e.Link = link
	}

	return e.seam()
}

// withLink returns a needs_auth error made without the broker (the
// Registrar's, or the credentials module's) with the auth link added. It
// copies the error, which a single flight may share.
func (b *Broker) withLink(err error) error {
	if e, ok := AsError(err); ok && e.Kind == KindNeedsAuth && e.Link == "" {
		linked := *e
		linked.Link = b.Link(e.Connector)

		return linked.seam()
	}

	return err
}

// load returns the connector's grant for use at resource, its configured
// URL (see [credentials.load]); a needs_auth error carries the auth link.
func (b *Broker) load(ctx context.Context, connector, resource string, auth config.Auth) (grantView, error) {
	v, err := b.creds.load(ctx, connector, resource, auth)

	return v, b.withLink(err)
}

// storeGrant stores tok, a token from a new grant, bound to resource with
// the granted scopes and the client that obtained it, starting the row if
// there is none (see
// [credentials.grant]); then it reports the changed credential
// (credentialChanged). A non-nil check runs first under the row lock: if it
// returns an error, nothing is written and that error is returned. Every
// path that stores a token the connector didn't have goes through it: an
// auth flow's completion ([Broker.completeFlow], wayfindr-421) stores its
// token here, with a check that its flow was not cancelled meanwhile. A refresh
// renews the same grant, and stores its token only over the one it
// refreshed (see refreshOnce).
func (b *Broker) storeGrant(ctx context.Context, connector string, tok token, resource, clientID, granted string, check func() error) error {
	if err := b.creds.grant(ctx, connector, tok, resource, clientID, granted, check); err != nil {
		return err
	}

	b.credentialChanged(connector)

	return nil
}

// credentialChanged calls OnCredentialChanged for connector, if set. It
// follows every change of a connector's credential other than a refresh:
// storeGrant calls it, and so does Revoke, once the row is gone.
func (b *Broker) credentialChanged(connector string) {
	if b.onCredentialChanged != nil {
		b.onCredentialChanged(connector)
	}
}

// forgetRejectedDCR handles a token endpoint that rejected the client a
// refresh or a code exchange used, read from seen. A client Wayfindr
// registered through DCR is cleared from the credentials row, so the next
// auth flow registers a new one instead of reusing it forever, and the
// error becomes [CaseRegistrationExpired], which needs auth. A config or
// CIMD client is the user's to fix: err is returned as it is.
//
// The client is kept if a token was stored since seen (see
// [credentials.forgetClient]): the server accepted it then. The error is
// still registration_expired, but wraps errTokenReplaced, so a refresh
// uses the newer token instead.
func (b *Broker) forgetRejectedDCR(ctx context.Context, connector string, client Registration, seen grantView, err error) error {
	e, ok := AsError(err)

	if !ok || e.Case != CaseClientRejected || (client.Method != MethodStoredDCR && client.Method != MethodDCR) {
		return err
	}

	// With no row (revoked meanwhile), there is no client to forget, and the
	// error is no_credential.
	switch ferr := b.creds.forgetClient(ctx, connector, client.ClientID, seen); {
	case errors.Is(ferr, errTokenReplaced):
		return b.fail(CaseRegistrationExpired, connector, e.Detail+"; the oauth client was kept", e.ProviderCode, ferr)
	case ferr != nil:
		return b.withLink(ferr)
	}

	return b.fail(CaseRegistrationExpired, connector, e.Detail+"; the stored oauth client was forgotten", e.ProviderCode, nil)
}

// redirectURI is the effective redirect URI of a connector (ADR 0007): its
// redirect_uri, else the daemon's own callback. DCR registers it, and the
// authorize and token requests send it.
func (b *Broker) redirectURI(a config.Auth) string {
	if a.RedirectURI != "" {
		return a.RedirectURI
	}

	return "http://" + b.listen + CallbackPath
}
