package auth

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// Flows is the one entry to the auth flows (design §7.3), the authorization
// code's and the device code's, for both doors: the browser's pages ([NewPages]) and the admin API, which
// the daemon adapts to it. It finds the connector in the running config,
// checks that it uses OAuth, and starts, waits on, and completes its
// flows on the broker. It also owns the rule for which config changes end
// a pending flow ([Flows.ConfigChanged]). The broker itself still keeps no
// copy of the config and has no lookup into it.
type Flows struct {
	b *Broker
	// lookup returns the connector's config in the running config.
	lookup func(name string) (config.Connector, bool)
}

// NewFlows returns the flows of b. lookup returns a connector's config in
// the running config, and false for a name it doesn't have; the daemon
// passes its running config, which a reload swaps before it calls
// ConfigChanged.
func NewFlows(b *Broker, lookup func(name string) (config.Connector, bool)) *Flows {
	return &Flows{b: b, lookup: lookup}
}

// StartOptions are how a caller asks for an auth flow. The zero value
// starts the flow the connector's grant names.
type StartOptions struct {
	// Device asks for the device code flow whatever the connector's grant:
	// the user enters a user code on any device.
	Device bool
}

// Start starts an auth flow of the connector called name, in its current
// config, and returns what the user is shown: the URL to open for the
// authorization code, or the user code and where to enter it for the
// device code. opts.Device asks for the device code; otherwise the
// connector's grant decides (see [selectFlowKind]). Errors are [*Error]s:
// [KindRequest] for a name the running config doesn't have
// ([CaseUnknownConnector]) and for a connector that does not use OAuth
// ([CaseNotOAuth]) or has an unattended grant ([CaseNotInteractive]);
// [KindConfig] when the connector cannot be authorized
// as configured, such as [CasePKCEUnsupported] or [CaseDeviceUnsupported];
// the discovery and registration errors; and [CaseFlowCancelled] when a
// revoke or a config change ended the flow while it started. A needs_auth
// one carries the auth link. [HTTPStatus] says how either door answers
// one.
func (fl *Flows) Start(ctx context.Context, name string, opts StartOptions) (Flow, error) {
	c, err := fl.connector(name)

	if err != nil {
		return Flow{}, err
	}

	f, err := fl.b.startFlow(ctx, c, selectFlowKind(opts, *c.Auth, fl.stale))

	return f, fl.b.withLink(err)
}

// connector returns the running config of the connector called name, if
// it has an auth flow; otherwise a [KindRequest] error. An unattended grant
// has none of any kind, device code included: Wayfindr gets its token
// itself. The auth link's pages check through it too, so they say so
// before offering to continue.
func (fl *Flows) connector(name string) (config.Connector, error) {
	c, ok := fl.lookup(name)

	switch {
	case !ok:
		return config.Connector{}, fl.b.fail(CaseUnknownConnector, name, "the running config has no such connector", "", nil)
	case !c.UsesOAuth():
		return config.Connector{}, fl.b.fail(CaseNotOAuth, name, "the connector has no auth table of type oauth", "", nil)
	case c.Auth.Grant.Unattended():
		return config.Connector{}, fl.b.fail(CaseNotInteractive, name, "grant "+string(c.Auth.Grant)+" has no auth flow", "", nil)
	}

	return c, nil
}

// Wait waits until the flow called id leaves pending, it expires, or ctx
// ends, and returns how it stands then: a flow still pending when ctx ends
// is reported as pending. It reports false for an id it doesn't know,
// which includes a flow that ended more than [FlowTTL] ago.
func (fl *Flows) Wait(ctx context.Context, id string) (FlowResult, bool) {
	return fl.b.waitFlow(ctx, id)
}

// HandleCallback serves GET [CallbackPath], where the authorization server
// sends the browser back (design §7.3). Before it exchanges the code, and
// again under the row lock as it stores the token, it ends the flow as
// [CaseFlowCancelled] if its connector's config changed since the flow
// started (see [Flows.ConfigChanged]): the check here is the
// authoritative one, since a reload may sweep only after the callback
// began.
func (fl *Flows) HandleCallback(w http.ResponseWriter, r *http.Request) {
	fl.b.handleCallback(w, r, fl.stale)
}

// ConfigChanged ends, as failed with [CaseFlowCancelled], every pending
// flow, starting or not, whose connector the running config no longer
// names, or names with another url, [auth], or [tls] table (see
// [outdated]). The
// daemon calls it on every reload, once lookup returns the new config, in
// place of choosing connectors itself. It runs under the row lock, so a
// callback storing its token either stored it first, under the config its
// flow was checked against, or stores nothing.
func (fl *Flows) ConfigChanged() {
	var ended []error
	fl.b.creds.locked(func() { ended = fl.b.flows.sweep(fl.stale) })

	for _, err := range ended {
		if e, ok := AsError(err); ok {
			fl.b.log.Info().Str("connector", e.Connector).Str("reason", e.Detail).Msg("oauth flow cancelled")
		}
	}
}

// stale returns the [CaseFlowCancelled] error that ends f if its
// connector's running config no longer matches the one it started with,
// and nil if f may complete.
func (fl *Flows) stale(f *flow) error {
	now, ok := fl.lookup(f.Connector)

	if detail := outdated(f.conn, now, ok); detail != "" {
		return fl.b.fail(CaseFlowCancelled, f.Connector, detail, "", nil)
	}

	return nil
}

// outdated is the one rule for which config changes end a pending auth
// flow (design §7.3, wayfindr-zgp.15): the connector was removed, or its
// url, its [auth] table, or its [tls] table changed. The token would be
// bound to the old url, granted under the old client, scopes, endpoints,
// or redirect URI, or bound to the old client certificate. Any other
// change, such as its timeout, headers, or env, leaves the flow alone.
// started is the config the flow started with; now is the running one, if
// configured. It returns why the flow is outdated, or "".
func outdated(started, now config.Connector, configured bool) string {
	switch {
	case !configured:
		return "the connector was removed from the config"
	case now.URL != started.URL:
		return "the connector's url changed"
	case !config.AuthEqual(started.Auth, now.Auth):
		return "the connector's [auth] table changed"
	case !config.TLSEqual(started.TLS, now.TLS):
		return "the connector's [tls] table changed"
	}

	return ""
}

// startFailed renders the page of a flow that cannot start, with the
// status the admin API answers the same error with ([HTTPStatus]).
func startFailed(w http.ResponseWriter, connector string, err error) {
	renderAdvice(w, HTTPStatus(err), "Cannot authorize "+connector, err)
}

// FlowTTL is how long an auth flow waits for its callback, or a device
// code flow for the user's approval, at most (design §7.3). A finished or
// expired flow's result stays readable for as long again.
const FlowTTL = 10 * time.Minute

// FlowState is where an auth flow stands.
type FlowState string

// Flow states.
const (
	// FlowPending: waiting for the browser to come back to the callback,
	// or for the user of a device code flow to approve.
	FlowPending FlowState = "pending"
	// FlowDone: the code was exchanged and the token stored.
	FlowDone FlowState = "done"
	// FlowFailed: the authorization server refused, the callback failed a
	// check, the code exchange failed, or the flow was cancelled. The
	// result's Err says why.
	FlowFailed FlowState = "failed"
	// FlowExpired: no callback within [FlowTTL].
	FlowExpired FlowState = "expired"
)

// Flow is a started auth flow: the user follows its [Prompt] before
// ExpiresAt.
type Flow struct {
	// ID names the flow for [Flows.Wait]. It is not the OAuth state.
	ID        string
	Connector string
	Prompt
	ExpiresAt time.Time
}

// Prompt is what the user is shown to take part in an auth flow: for the
// authorization code, the URL to open in a browser; for the device code
// (RFC 8628 §3.2), the user code and where to enter it.
type Prompt struct {
	// AuthorizeURL is the authorization code flow's; empty for a device
	// code flow.
	AuthorizeURL string
	// UserCode is the code the user of a device code flow enters at
	// VerificationURI; empty for an authorization code flow.
	UserCode string
	// VerificationURI is where the user enters UserCode. It is https, or
	// http on a loopback host.
	VerificationURI string
	// VerificationURIComplete, if the server sent one, is VerificationURI
	// with UserCode in it, for a link or a QR code. It is https, or http on
	// a loopback host.
	VerificationURIComplete string
}

// Device reports whether p is a device code flow's prompt.
func (p Prompt) Device() bool { return p.UserCode != "" }

// flowKind is how an auth flow gets the user's consent once its OAuth
// client is known: the authorization code ([authCode]) or the device code
// ([deviceCode]). The kinds never call each other; [selectFlowKind] is the
// one place a kind is chosen.
type flowKind interface {
	// grantType is the grant the flow ends with, which its OAuth client
	// must be registered for.
	grantType() string
	// check reports, as a [KindConfig] error, why a connector with auth a,
	// whose discovery is disc, can never be authorized by this kind. A
	// starting flow applies it before choosing a client, and the Transport
	// before answering needs_auth (see [Broker.unauthorizable]).
	check(b *Broker, connector string, a config.Auth, disc Discovery) error
	// begin readies f, a starting flow whose fields up to scope are set,
	// to be armed, and returns what the user is shown.
	begin(ctx context.Context, b *Broker, f *flow, oc oauthClient) (Prompt, error)
	// armed runs once f is armed, with the client begin had: the device
	// code starts its poller here. ctx is the start's. An error ends the
	// start.
	armed(ctx context.Context, b *Broker, f *flow, oc oauthClient) error
}

// selectFlowKind is the one selector of an auth flow's kind: the device
// code when opts asks for it or the connector's grant is device_code, the
// authorization code otherwise. stale is the config rule ([Flows.stale])
// a device code flow's poller checks as it stores its token; it may be nil
// when only check is called.
func selectFlowKind(opts StartOptions, a config.Auth, stale func(*flow) error) flowKind {
	if opts.Device || a.Grant == config.GrantDeviceCode {
		return &deviceCode{stale: stale}
	}

	return authCode{}
}

// FlowResult is how a flow stands, or how it ended.
type FlowResult struct {
	ID        string
	Connector string
	State     FlowState
	// Err is why a failed or expired flow ended, an [*Error] (narrow it
	// with [AsError]); nil otherwise.
	Err error
}

// flow is a flow's in-memory record. It enters the flow table as its start
// begins (see [Broker.startFlow]): ID, Connector, conn, auth, and done are
// set then and never change, and ExpiresAt changes only under flowTable.mu
// while it starts (see flowTable.expireBy). The other fields are set while
// it starts, before it is armed, which is when a callback can first find
// it. res, granted, and stop are guarded by flowTable.mu.
type flow struct {
	Flow
	// conn is the connector's config the flow started with; [outdated]
	// compares it with the running config.
	conn     config.Connector
	auth     config.Auth
	stateKey [sha256.Size]byte
	disc     Discovery
	client   Registration
	// http is the connector's HTTP client the flow started with (see
	// [Broker.httpFor]); a [tls] change outdates the flow.
	http *http.Client
	// seen is the credentials row when the flow started: a rejected DCR
	// client is forgotten only if no token was stored since.
	seen grantView
	// verifier is the PKCE code verifier; empty when PKCE is off.
	verifier string
	// dpopKey is the grant's DPoP key; nil when its sender is bearer.
	dpopKey *dpopKey
	// redirectURI is the redirect URI the OAuth client is registered with,
	// which the authorization code flow sends.
	redirectURI string
	resource    string
	scope       string
	// grantType is the flow kind's grant, for logs.
	grantType string

	// res is the flow's result; done is closed when it leaves pending.
	res  FlowResult
	done chan struct{}
	// granted is set once the callback's token passed its checks under the
	// row lock and is being stored: from then on only a revoke, which
	// deletes the token, ends the flow, not a config change that came
	// after the token (see flowTable.sweep).
	granted bool
	// stop, when set, cancels the flow's poller; ending the flow calls it.
	stop context.CancelFunc
}

// flowTable holds the flows in memory: by ID from the start of the flow,
// for Wait and for the cancellations; and by the digest of their OAuth
// state once armed, for the callback, which consumes it.
type flowTable struct {
	mu      sync.Mutex
	byID    map[string]*flow
	byState map[[sha256.Size]byte]*flow
}

func newFlowTable() *flowTable {
	return &flowTable{byID: map[string]*flow{}, byState: map[[sha256.Size]byte]*flow{}}
}

// begin adds f, a flow that is starting: a revoke or a config change can
// end it from now on, but no callback can find it until it is armed. It
// drops the flows whose result has been readable for FlowTTL past their
// expiry.
func (t *flowTable) begin(f *flow, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for id, old := range t.byID {
		if now.After(old.ExpiresAt.Add(FlowTTL)) {
			delete(t.byID, id)
			delete(t.byState, old.stateKey)
		}
	}

	t.byID[f.ID] = f
}

// arm indexes a started flow by its state, so its callback finds it. A
// flow that was ended while it started is not armed, and arm returns the
// error it ended with.
func (t *flowTable) arm(f *flow) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.pendingLocked(f); err != nil {
		return err
	}

	// A device code flow has no state: no callback comes for it.
	if f.stateKey != ([sha256.Size]byte{}) {
		t.byState[f.stateKey] = f
	}

	return nil
}

// attach sets stop as pending f's poller cancellation, so that ending f
// stops its poller. A flow that has already ended is not attached: stop
// is called at once, and attach returns the error f ended with.
func (t *flowTable) attach(f *flow, stop context.CancelFunc) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.pendingLocked(f); err != nil {
		stop()

		return err
	}

	f.stop = stop

	return nil
}

// expireBy brings a starting flow's expiry forward to at, when that is
// earlier: a device code that expires before [FlowTTL] ends the flow then.
func (t *flowTable) expireBy(f *flow, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if at.Before(f.ExpiresAt) {
		f.ExpiresAt = at
	}
}

// endAll ends every pending flow, starting or not, as failed with the
// error reason returns for it; [Broker.Close] calls it, so every poller
// stops.
func (t *flowTable) endAll(reason func(*flow) error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, f := range t.byID {
		if f.res.State == FlowPending {
			t.finishLocked(f, FlowFailed, reason(f))
		}
	}
}

// drop removes a flow whose start failed; its ID was never handed out.
func (t *flowTable) drop(f *flow) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byID, f.ID)
}

func (t *flowTable) get(id string) (*flow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.byID[id]

	return f, ok
}

// consume removes state from the table and returns its flow, so a state
// is accepted once at most. The digest keeps the raw state out of memory
// once it has been handed out.
func (t *flowTable) consume(state string) (*flow, bool) {
	if state == "" {
		return nil, false
	}

	key := sha256.Sum256([]byte(state))
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.byState[key]
	delete(t.byState, key)

	return f, ok
}

// result returns f's result, first expiring it with expired if it is
// pending past its expiry.
func (t *flowTable) result(f *flow, now time.Time, expired error) FlowResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	if f.res.State == FlowPending && !now.Before(f.ExpiresAt) {
		t.finishLocked(f, FlowExpired, expired)
	}

	return f.res
}

// finish ends a pending flow with state and err, and reports whether it
// did: a flow that has already ended keeps its result. Either way it
// returns the flow's result.
func (t *flowTable) finish(f *flow, state FlowState, err error) (FlowResult, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ok := t.finishLocked(f, state, err)

	return f.res, ok
}

// pending returns nil while f is pending, and the error it ended with
// otherwise.
func (t *flowTable) pending(f *flow) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.pendingLocked(f)
}

func (t *flowTable) pendingLocked(f *flow) error {
	if f.res.State == FlowPending {
		return nil
	}

	if f.res.Err != nil {
		return f.res.Err
	}

	return errors.New("the auth flow has ended")
}

// grant marks pending f as storing its token once check passes, so that a
// config change from then on no longer ends it (see sweep). It returns the
// error f ended with, or check's. The callback calls it under the row
// lock, which the cancellations hold too.
func (t *flowTable) grant(f *flow, check func() error) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.pendingLocked(f); err != nil {
		return err
	}

	if err := check(); err != nil {
		return err
	}

	f.granted = true

	return nil
}

// cancel ends every pending flow of connector with err, starting or not,
// and returns how many it ended. Their states stay in the table, so a late
// callback is answered with err's advice.
func (t *flowTable) cancel(connector string, err error) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0

	for _, f := range t.byID {
		if f.Connector == connector && t.finishLocked(f, FlowFailed, err) {
			n++
		}
	}

	return n
}

// sweep ends as failed every pending flow, starting or not, for which
// stale returns an error, with that error, and returns those errors. A
// flow whose callback is storing its token is left alone: the token was
// granted under the config it was checked against.
func (t *flowTable) sweep(stale func(*flow) error) []error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ended []error

	for _, f := range t.byID {
		if f.res.State != FlowPending || f.granted {
			continue
		}

		if err := stale(f); err != nil {
			t.finishLocked(f, FlowFailed, err)
			ended = append(ended, err)
		}
	}

	return ended
}

func (t *flowTable) finishLocked(f *flow, state FlowState, err error) bool {
	if f.res.State != FlowPending {
		return false
	}

	f.res.State, f.res.Err = state, err

	// The poller stops before anyone waiting hears the flow ended.
	if f.stop != nil {
		f.stop()
	}

	close(f.done)

	return true
}

// startFlow starts an auth flow of kind for c, the connector's current
// config, which uses OAuth ([Flows.Start] checks both). The flow enters
// the flow table at once, as starting, so a revoke or a config change
// while it discovers and registers ends it. It then runs discovery, picks
// the OAuth client (registering one if needed, once per connector at a
// time) once kind's check passes, picks the scopes, lets kind begin it,
// and arms the flow, returning what the user is shown. The flow waits in
// memory for its completion for [FlowTTL] at most.
//
// Errors are [*Error]s: [KindConfig] when the connector cannot be
// authorized as configured, such as [CasePKCEUnsupported], the discovery
// and registration errors, and [CaseFlowCancelled] for a flow ended while
// it started. A flow that fails to start leaves the table.
func (b *Broker) startFlow(ctx context.Context, c config.Connector, kind flowKind) (Flow, error) {
	now := b.now()
	// The flow's own copy of the auth table: [outdated] compares it with
	// the running config's.
	a := *c.Auth
	c.Auth = &a
	f := &flow{conn: c, auth: a, grantType: kind.grantType(), done: make(chan struct{})}
	f.Flow = Flow{ID: random(16), Connector: c.Name, ExpiresAt: now.Add(FlowTTL)}
	f.res = FlowResult{ID: f.ID, Connector: c.Name, State: FlowPending}
	b.flows.begin(f, now)

	if err := b.prepare(ctx, f, kind); err != nil {
		b.flows.drop(f)

		return Flow{}, err
	}

	b.log.Info().Str("connector", f.Connector).Str("issuer", redact.URL(f.disc.Issuer)).Str("grant_type", f.grantType).
		Str("method", string(f.client.Method)).Bool("pkce", f.verifier != "").Msg("oauth flow started")

	return f.Flow, nil
}

// prepare is the body of startFlow: it fills in the starting flow f, lets
// kind begin it, arms it, and tells kind it is armed. After each slow step
// it stops if f was ended meanwhile.
func (b *Broker) prepare(ctx context.Context, f *flow, kind flowKind) error {
	name, a := f.Connector, f.auth
	resource, err := f.conn.URL.Resolve(ctx, b.resolver)

	if err != nil {
		return b.fail(CaseUnresolvedValue, name, "connector url", "", err)
	}

	hc, err := b.httpFor(ctx, f.conn)

	if err != nil {
		return err
	}

	// A flow is the user's answer to an auth failure, so every document is
	// fetched again (design §7.3).
	disc, err := b.discover.Discover(ctx, DiscoverRequest{
		Connector: name, ResourceURL: resource, Auth: a, Probe: true, Refetch: true, Client: hc,
		Mask: b.urlMask(ctx, f.conn),
	})

	if err != nil {
		return err
	}

	if err := b.flows.pending(f); err != nil {
		return err
	}

	if err := kind.check(b, name, a, disc); err != nil {
		return err
	}

	// DCR registers every client with a redirect URI, the one an
	// authorization code flow sends, so a device code flow's client serves
	// both kinds.
	redirect := b.redirectURI(a)
	// A registration goes to the endpoint the client's token requests would
	// use (see [endpointsFor]).
	regDisc := disc
	regDisc.Endpoints = endpointsFor(f.conn, a.TokenEndpointAuthMethod, disc)
	client, err := b.registrar.obtain(ctx, RegisterRequest{
		Connector: name, Auth: a, Discovery: regDisc, RedirectURI: redirect, GrantType: kind.grantType(), Client: hc,
	})

	if err != nil {
		return err
	}

	if err := b.flows.pending(f); err != nil {
		return err
	}

	seen, err := b.creds.stored(ctx, name)

	if err != nil {
		return err
	}

	f.disc, f.client, f.seen, f.redirectURI, f.resource, f.http = disc, client, seen, redirect, resource, hc
	f.scope = flowScope(a, disc, seen.RequestedScopes)

	oc := b.oauthClient(f.conn, hc, disc, client, nil)

	if err := oc.usable(); err != nil {
		return err
	}

	// The grant's DPoP key, when its sender is DPoP: the authorization
	// request names it (dpop_jkt), the token requests prove with it, and it
	// is sealed with the token.
	f.dpopKey = oc.dpopKey

	if f.Prompt, err = kind.begin(ctx, b, f, oc); err != nil {
		return err
	}

	if err := b.flows.arm(f); err != nil {
		return err
	}

	return kind.armed(ctx, b, f, oc)
}

// completeFlow stores tok, the token flow f was granted, through
// storeGrant, which reports the changed credential, with scope as granted
// (the requested one when the response has none). stale is the config rule
// ([Flows.stale]); it is checked again under the row lock, which a revoke
// and a config change's sweep hold too, so a flow they end before the
// token is stored stores nothing.
func (b *Broker) completeFlow(ctx context.Context, f *flow, tok token, scope string, stale func(*flow) error) error {
	// RFC 6749 §5.1: no scope in the response means the requested one.
	granted := cmp.Or(scope, f.scope)
	tok.TokenEndpoint = f.disc.Endpoints.Token
	tok.PRMURL, tok.PRMKnown = f.disc.PRMURL, true

	return b.storeGrant(ctx, f.Connector, tok, f.resource, f.client.ClientID, granted, func() error {
		return b.flows.grant(f, func() error { return stale(f) })
	})
}

// flowScope picks the scopes to ask for (ADR 0007, ADR 0004): the
// challenge's scope, else PRM scopes_supported, else the config scopes,
// then requested, the scopes a step-up added to requested_scopes.
func flowScope(a config.Auth, disc Discovery, requested string) string {
	var base []string

	switch {
	case len(disc.ChallengeScopes) > 0:
		base = disc.ChallengeScopes
	case disc.PRM != nil && len(disc.PRM.ScopesSupported) > 0:
		base = disc.PRM.ScopesSupported
	default:
		base = a.Scopes
	}

	var out []string

	for _, s := range slices.Concat(base, strings.Fields(requested)) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}

	return strings.Join(out, " ")
}

// random returns n random bytes, base64url-encoded.
func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails (crypto/rand)

	return base64.RawURLEncoding.EncodeToString(b)
}

// waitFlow is [Flows.Wait].
func (b *Broker) waitFlow(ctx context.Context, id string) (FlowResult, bool) {
	f, ok := b.flows.get(id)

	if !ok {
		return FlowResult{}, false
	}

	if res := b.flowResult(f); res.State != FlowPending {
		return res, true
	}

	timer := time.NewTimer(f.ExpiresAt.Sub(b.now()))
	defer timer.Stop()

	select {
	case <-f.done:
	case <-timer.C:
	case <-ctx.Done():
	}

	return b.flowResult(f), true
}

// flowResult is f's result, expired if its time is up. Every error a flow
// ends with is the broker's, so a needs_auth one carries the auth link.
func (b *Broker) flowResult(f *flow) FlowResult {
	expired := b.fail(CaseFlowExpired, f.Connector, "no callback within "+FlowTTL.String(), "", nil)

	return b.flows.result(f, b.now(), expired)
}
