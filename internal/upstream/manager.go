// Package upstream owns the daemon's live connections to connectors: their
// lifecycle, their state (GLOSSARY.md, "Connector states"), and the SDK
// client each one speaks through (docs/internal/design.md §7.2).
//
// The package sits below daemon. It reports upward only through the
// callbacks in [Options], which daemon wires up.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/httpclient"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// Lifecycle of stdio upstreams (docs/internal/design.md §7.2).
const (
	// reapInterval is how often the reaper looks for idle stdio upstreams.
	reapInterval = time.Minute
	// idleTimeout is how long a stdio upstream may sit unused before the
	// reaper stops its child.
	idleTimeout = 10 * time.Minute
	// defaultConnectTimeout bounds one connect: spawn or dial, plus the
	// initialize handshake. It is long enough for a first `npx`/`uvx` run
	// that downloads its package, and short enough that one black-holed
	// upstream can't hold Apply (and so startup) for long. A connect that
	// times out leaves the connector unreachable, and the next use retries.
	defaultConnectTimeout = 60 * time.Second
)

// Options configures a Manager.
type Options struct {
	// Logger receives connector lifecycle logs, and stdio children's stderr
	// at debug level.
	Logger zerolog.Logger
	// Resolver resolves ${env:…} and ${secret:…} references each time a
	// transport is built, so a changed secret takes effect on the next
	// connect without a reload.
	Resolver config.Resolver
	// Version is the Wayfindr version sent to upstreams as clientInfo. Empty
	// means "dev".
	Version string
	// Now is the clock that idle times and state times are read from. Nil
	// means time.Now.
	Now func() time.Time

	// OnToolsChanged is called when a connector's tool list may have changed:
	// on an upstream list_changed notification, and each time a new session
	// is opened, on any transport (sync-on-session): Apply's connect, a
	// stdio child's spawn, a reconnect after the upstream ended the session
	// or a connect failed, and Retry. A new session may serve different
	// tools than the last, and it is how the catalog learns that an upstream
	// it could not reach is back. The daemon wires it to
	// catalog.Syncer.Request. Optional.
	//
	// Both callbacks are called with no manager or connector lock held, so
	// they may call Status, Call, ListTools, Retry, and Apply, on any
	// connector, their own included. They must not call Close: one may run
	// on a session's own goroutine, which closing the session waits for.
	// Nor CredentialChanged or SecretChanged, which return only once the
	// callbacks their connects queued have been called, and so would wait
	// on the callback calling them.
	// Calls for one connector, of either callback, are made one at a time,
	// in the order the events happened; calls for different connectors may
	// run concurrently. A callback runs on the goroutine whose work caused
	// the event (Apply, Retry, Call, ListTools, or the session's
	// notification handler) once that work has released its locks, so it
	// should return quickly: Apply, for one, returns only once the
	// callbacks for its connectors have. Events that a callback's own calls
	// cause are delivered after it returns, not nested inside it.
	OnToolsChanged func(connector string)
	// OnStateChanged is called each time a connector enters a new state,
	// including the first (starting). See OnToolsChanged for how both
	// callbacks are called. Optional.
	OnStateChanged func(connector string, s State)

	// AuthTransport, if set, wraps the RoundTripper of each url connector's
	// HTTP client, outside the header injector (design §7.2). It is called
	// on every connect, with the config the connector is dialed with (Name
	// set to its key) and the chain built so far, and returns next itself
	// for a connector it has nothing to add to. A config change closes the
	// connection and dials again, so c is the connector's current config
	// for as long as the connection lasts. The OAuth broker plugs in here.
	// Optional.
	AuthTransport func(c config.Connector, next http.RoundTripper) http.RoundTripper

	// ConnectTimeout bounds each connect (spawn or dial, and the initialize
	// handshake). Zero means 60s.
	ConnectTimeout time.Duration

	// InteractionTTL is how long a parked question waits for the user's
	// answer. Zero means [InteractionTTL]; tests shorten it.
	InteractionTTL time.Duration

	// AuthURL returns the daemon's link that starts connector's auth flow
	// (/auth/<connector>). A needs_auth error whose cause carried no link
	// gets this one, and so does a needs_auth connector's status. Optional.
	AuthURL func(connector string) string

	// MaxMessageBytes bounds one upstream message (an HTTP response body,
	// an SSE event, a stdio line). Zero means 16 MiB. It is not a user
	// setting; tests lower it.
	MaxMessageBytes int
	// MaxSyncBytes bounds the tool definitions one ListTools keeps, across
	// its pages. Zero means 32 MiB. It is not a user setting; tests lower
	// it.
	MaxSyncBytes int

	// Clients holds each url connector's HTTP client, built from its [tls]
	// table: the base transport of its MCP traffic, under the header
	// injector and the auth hook, so the MCP server and the authorization
	// server see the same client certificate and are trusted by the same
	// CAs. Nil sends every connector through http.DefaultTransport.
	Clients *httpclient.Clients
}

// ConnectorStatus is one connector's row in Status. What it means to the
// user comes from [ConnectorStatus.Advice].
type ConnectorStatus struct {
	Connector string
	State     State
	// Reason is technical text for logs: why the connector is not ready.
	// It never contains a resolved secret, header value, or URL query, and
	// never reaches a user (Advice doesn't read it).
	Reason string
	// Since is when the connector entered State.
	Since time.Time
	// NotReadySince is when the connector was last ready, or was added:
	// unlike Since, a move between two states that are not ready keeps
	// it. Zero while ready.
	NotReadySince time.Time
	// Kind, Case, Ref, and ProviderCode are those of the *Error that put
	// the connector in State (see [Error]); empty in starting and ready,
	// and in a state entered without an error. Advice reads them, and
	// SecretChanged finds the connectors waiting on a secret by them.
	Kind         Kind
	Case         Case
	Ref          string
	ProviderCode string
	// Link starts the connector's auth flow: the link of the error that put
	// it in State, else, in needs_auth, the one Options.AuthURL gives.
	Link string
	// ToolsTTL is the cache hint (ttlMs, 2026-07-28 cacheable lists) of the
	// connector's last successful ListTools (PeekTools doesn't count): how
	// long its tool list stays fresh, the smallest of its pages' hints.
	// Zero when the upstream sent none or 0 ("immediately stale"), or the
	// connector has not listed its tools since it was configured. The
	// daemon's poll reads it (design §7.8 step 3).
	ToolsTTL time.Duration
}

// Manager holds one upstream per connector.
//
// Apply connects every connector it adds or changes, stdio ones included,
// so a connector reports ready only once its upstream has answered. A stdio
// child is then stopped once it has been idle for idleTimeout, and spawned
// again by the next use. Stopping an idle child leaves its connector ready.
type Manager struct {
	log            zerolog.Logger
	now            func() time.Time
	resolver       config.Resolver
	impl           *mcp.Implementation
	onToolsChanged func(connector string)
	onStateChanged func(connector string, s State)
	authTransport  func(c config.Connector, next http.RoundTripper) http.RoundTripper
	connectTimeout time.Duration
	authURL        func(connector string) string
	clients        *httpclient.Clients
	maxMessage     int
	maxSyncBytes   int

	// applyMu serializes Apply and Close, and guards closed.
	applyMu sync.Mutex
	// closed is set by Close; Apply does nothing after it.
	closed bool

	// mu guards the connectors map. Each connector guards itself.
	mu         sync.Mutex
	connectors map[string]*connector

	// halted is closed by halt, once Close's ctx has ended: every url
	// connector's requests then end at once (see haltTransport).
	halted chan struct{}
	halt   func()

	// Closing stopReaper stops the reaper goroutine, which then closes
	// reaperDone.
	stopReaper     chan struct{}
	stopReaperOnce sync.Once
	reaperDone     chan struct{}

	// interMu guards interactions, the parked questions by id (see park).
	interMu        sync.Mutex
	interactions   map[string]*interaction
	interactionTTL time.Duration
}

// New returns a Manager with no connectors and starts its idle reaper. Call
// Apply to add connectors, and Close to stop.
func New(opts Options) *Manager {
	version := opts.Version

	if version == "" {
		version = "dev"
	}

	connectTimeout := opts.ConnectTimeout

	if connectTimeout <= 0 {
		connectTimeout = defaultConnectTimeout
	}

	now := opts.Now

	if now == nil {
		now = time.Now
	}

	maxMessage := opts.MaxMessageBytes

	if maxMessage <= 0 {
		maxMessage = maxMessageBytes
	}

	maxSync := opts.MaxSyncBytes

	if maxSync <= 0 {
		maxSync = maxSyncBytes
	}

	interactionTTL := opts.InteractionTTL

	if interactionTTL <= 0 {
		interactionTTL = InteractionTTL
	}

	m := &Manager{
		log:            opts.Logger,
		now:            now,
		resolver:       opts.Resolver,
		impl:           &mcp.Implementation{Name: "wayfindr", Version: version},
		onToolsChanged: opts.OnToolsChanged,
		onStateChanged: opts.OnStateChanged,
		authTransport:  opts.AuthTransport,
		connectTimeout: connectTimeout,
		authURL:        opts.AuthURL,
		clients:        opts.Clients,
		maxMessage:     maxMessage,
		maxSyncBytes:   maxSync,
		connectors:     map[string]*connector{},
		stopReaper:     make(chan struct{}),
		reaperDone:     make(chan struct{}),
		halted:         make(chan struct{}),
		interactions:   map[string]*interaction{},
		interactionTTL: interactionTTL,
	}
	m.halt = sync.OnceFunc(func() { close(m.halted) })
	go m.runReaper()

	return m
}

// Apply carries out changes, which config.Diff computed between the config
// of the previous Apply (nil for the first) and the one whose connectors
// are set. The caller, the daemon's reload, owns the current config and
// diffs once; Apply keeps each connector's config only to dial it.
//
// Removed connectors are closed and dropped. A connector whose url changed
// and that uses OAuth goes to needs_auth without connecting, because its
// tokens are bound to the old url. Every other added or changed connector
// is rebuilt from set: its connection is closed, it enters starting, and it
// is connected (a stdio connector's child is spawned). The connects run in
// parallel, and Apply returns when each has settled or ctx is done.
// Unchanged connectors are left alone.
//
// Apply after Close does nothing but log a warning: the manager has
// stopped its reaper, and nothing would stop the children Apply spawned.
func (m *Manager) Apply(ctx context.Context, set config.Connectors, changes config.Changes) {
	// The callbacks for the connectors Apply touched run once it has
	// released applyMu (see Options.OnToolsChanged).
	for _, c := range m.applyLocked(ctx, set, changes) {
		m.notify(c)
	}
}

// applyLocked carries out Apply under applyMu and returns the connectors
// it reconfigured, whose callbacks it leaves queued.
func (m *Manager) applyLocked(ctx context.Context, set config.Connectors, changes config.Changes) []*connector {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	if m.closed {
		m.log.Warn().Msg("apply after close ignored")

		return nil
	}

	for _, name := range changes.Removed {
		m.remove(ctx, name)
	}

	var touched, connect []*connector

	for _, name := range changes.Added {
		c := m.add(name)
		m.reconfigure(ctx, c, dialConfig(set, name), EventConfigured, "", nil)
		touched, connect = append(touched, c), append(connect, c)
	}

	for _, name := range changes.URLChanged {
		c, cfg := m.add(name), dialConfig(set, name)
		touched = append(touched, c)

		if cfg.UsesOAuth() {
			m.reconfigure(ctx, c, cfg, EventURLChanged, "url changed; run `wayfindr auth login "+name+"`",
				&Error{Connector: name, Kind: KindNeedsAuth, Case: CaseURLChanged})
			continue
		}

		m.reconfigure(ctx, c, cfg, EventConfigured, "", nil)
		connect = append(connect, c)
	}

	for _, name := range changes.Changed {
		c := m.add(name)
		m.reconfigure(ctx, c, dialConfig(set, name), EventConfigured, "", nil)
		touched, connect = append(touched, c), append(connect, c)
	}

	var wg sync.WaitGroup

	for _, c := range connect {
		wg.Go(func() {
			// The outcome is recorded in the connector's state.
			if l, err := m.acquire(ctx, c); err == nil {
				l.release()
			}
		})
	}

	wg.Wait()

	return touched
}

// Status returns every connector's state, sorted by connector name.
func (m *Manager) Status() []ConnectorStatus {
	m.mu.Lock()
	cs := slices.Collect(maps.Values(m.connectors))
	m.mu.Unlock()

	out := make([]ConnectorStatus, 0, len(cs))

	for _, c := range cs {
		out = append(out, m.statusOf(c))
	}

	slices.SortFunc(out, func(a, b ConnectorStatus) int { return strings.Compare(a.Connector, b.Connector) })

	return out
}

// statusOf is c's row in Status.
func (m *Manager) statusOf(c *connector) ConnectorStatus {
	s := c.status()

	if s.State == StateNeedsAuth && s.Link == "" {
		s.Link = m.authLink(c.name)
	}

	return s
}

// Close stops the reaper, closes every upstream, and removes every
// connector. Closing a stdio upstream stops its child and waits for it to
// exit, killing it if it doesn't exit on its own; the upstreams close in
// parallel. Close may be called more than once.
//
// ctx bounds the url connectors' closes: the session DELETE of each, and
// whatever holds a connector's lock meanwhile (a connect, or a Drop's
// DELETE). Once ctx ends, every request to a url upstream still in flight
// ends at once, and Close returns, wrapping ctx's error, when the closes
// have; a stdio child's stop has its own bound and is waited for, since a
// child Close left behind would outlive the daemon (wayfindr-t08.12).
func (m *Manager) Close(ctx context.Context) error {
	// No one will answer a parked question now.
	m.endParked(ctx)
	halt := context.AfterFunc(ctx, m.halt)
	m.stopReaperOnce.Do(func() { close(m.stopReaper) })
	<-m.reaperDone

	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.closed = true

	m.mu.Lock()
	cs := m.connectors
	m.connectors = map[string]*connector{}
	m.mu.Unlock()

	var (
		wg   sync.WaitGroup
		emu  sync.Mutex
		errs []error
	)

	for _, c := range cs {
		wg.Go(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.removed = true

			if err := c.closeSession(ctx); err != nil {
				emu.Lock()
				errs = append(errs, fmt.Errorf("connector %s: %w", c.name, redactErr(err)))
				emu.Unlock()
			}
		})
	}

	wg.Wait()

	if !halt() {
		errs = append(errs, fmt.Errorf("upstream closes cut short: %w", ctx.Err()))
	}

	return errors.Join(errs...)
}

// runReaper runs reap every reapInterval until Close.
func (m *Manager) runReaper() {
	defer close(m.reaperDone)
	t := time.NewTicker(reapInterval)
	defer t.Stop()

	for {
		select {
		case <-m.stopReaper:
			return
		case <-t.C:
			m.reap()
		}
	}
}

// reap stops every stdio child whose session has had no use in progress
// for longer than idleTimeout. The connector keeps its state, and its next
// use spawns a new child.
func (m *Manager) reap() {
	m.mu.Lock()
	cs := slices.Collect(maps.Values(m.connectors))
	m.mu.Unlock()

	now := m.now()

	type idleSession struct {
		s    *mcp.ClientSession
		mask redact.Values
	}

	var idle []idleSession

	for _, c := range cs {
		// A connector whose lock is held is connecting or starting a use,
		// so it isn't idle; the next pass looks again.
		if !c.mu.TryLock() {
			continue
		}

		if c.session != nil && c.cfg.IsStdio() && c.inUse == 0 && now.Sub(c.lastUsed) > idleTimeout {
			idle = append(idle, idleSession{c.session, c.mask})
			c.session, c.sessionDone, c.halt, c.overflow = nil, nil, nil, nil
			m.log.Info().Str("connector", c.name).Msg("stopping idle stdio upstream")
		}

		c.mu.Unlock()
	}

	// Close outside the locks: a child that ignores its closed stdin takes
	// seconds to stop, and its connector may spawn a new one meanwhile.
	var wg sync.WaitGroup

	for _, s := range idle {
		wg.Go(func() {
			if err := s.s.Close(); err != nil {
				m.log.Debug().Err(redactErr(s.mask.Err(err))).Msg("close idle stdio upstream")
			}
		})
	}

	wg.Wait()
}

func (m *Manager) lookup(name string) *connector {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.connectors[name]
}

// add returns the connector called name, creating it if the manager has
// none. Apply reconfigures it next.
func (m *Manager) add(name string) *connector {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.connectors[name]

	if c == nil {
		c = &connector{name: name}
		c.idle.L = &c.noticeMu
		m.connectors[name] = c
	}

	return c
}

// dialConfig is the config Apply keeps for the connector called name, to
// dial it with: its entry in set, with Name set to the key.
func dialConfig(set config.Connectors, name string) config.Connector {
	c := set[name]
	c.Name = name

	return c
}

// remove deletes a connector and closes its upstream; ctx's end cuts the
// close short (see closeSession).
func (m *Manager) remove(ctx context.Context, name string) {
	m.mu.Lock()
	c := m.connectors[name]
	delete(m.connectors, name)
	m.mu.Unlock()

	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.removed = true

	if err := c.closeSession(ctx); err != nil {
		m.log.Debug().Str("connector", name).Err(redactErr(err)).Msg("close upstream")
	}

	m.log.Info().Str("connector", name).Msg("connector removed")
}

// reconfigure replaces c's config, drops its upstream, and applies event,
// with the reason and the error (nil or an *Error) that explain it. ctx's
// end cuts the upstream's close short (see closeSession).
func (m *Manager) reconfigure(ctx context.Context, c *connector, cfg config.Connector, event Event, reason string, cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.closeSession(ctx); err != nil {
		m.log.Debug().Str("connector", c.name).Err(redactErr(err)).Msg("close upstream")
	}

	c.cfg = cfg
	c.sse = false
	c.stateMu.Lock()
	// The hint was the old upstream's.
	c.toolsTTL = 0
	c.stateMu.Unlock()
	m.transition(c, event, reason, cause)
}

// acquire returns c's upstream session for one use (a call or a sync),
// connecting first if there is none or the upstream has ended it (a
// transport error); for a stdio connector, that spawns its child. Every new
// session queues OnToolsChanged, whatever the transport, and the caller
// delivers what acquire queued with notify once it holds no lock. A failed connect
// applies its verdict (unreachable, for most), returns the verdict's
// *Error, and the next acquire tries again. A connector removed by Apply or
// Close reads as unknown_connector, so a use racing a removal can't spawn a
// child nothing would stop. The caller must call release when the use
// ends. Until then the reaper leaves the session alone; the end of the use
// is what the idle time is measured from.
//
// Only acquire creates sessions, under the per-connector mutex (design
// §7.2), so one goroutine creates a connector's session and the rest wait
// for it. Uses then run concurrently over the session.
func (m *Manager) acquire(ctx context.Context, c *connector) (l lease, err error) {
	if err := c.lockUnheld(ctx); err != nil {
		// No connect was tried: the state is left alone.
		return lease{}, judge(contextOutcome(ctx, outcome{connector: c.name, phase: phaseUse, err: err})).err
	}

	if !c.removed && c.sessionEnded() {
		// The upstream ended the session: its child exited, or a transport
		// error closed the connection. Recreate it (design §7.2).
		m.log.Info().Str("connector", c.name).Msg("upstream session ended; reconnecting")

		if cerr := c.closeSession(ctx); cerr != nil {
			m.log.Debug().Str("connector", c.name).Err(redactErr(cerr)).Msg("close ended upstream session")
		}
	}

	switch {
	case c.removed:
		err = &Error{Connector: c.name, Kind: KindUnknownConnector}
	case c.session == nil:
		if err = m.connect(ctx, c); err == nil {
			// Sync-on-session: a new session may serve different tools
			// than the last, whether a new child or a reconnected upstream
			// serves it.
			m.toolsChanged(c)
		}
	}

	if err == nil {
		l.session, l.epoch, l.mask, l.overflow = c.session, c.sessionEpoch, c.mask, c.overflow
		c.inUse++
	}

	c.mu.Unlock()

	if err != nil {
		return lease{}, err
	}

	var once sync.Once
	l.release = func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.inUse--
			c.lastUsed = m.now()
		})
	}

	return l, nil
}

// lease is one use's hold on a connector's session, from acquire.
type lease struct {
	session *mcp.ClientSession
	// epoch is the connector's epoch when session was opened (see
	// connector.epoch).
	epoch uint64
	// mask is the connector's mask when the use began, for the use's
	// verdict, which is judged without the connector's lock.
	mask redact.Values
	// overflow is session's overflow record, for the use's verdict.
	overflow *overflow
	// release ends the use.
	release func()
}

// Retry connects the connector called name again, after the user may have
// fixed what kept it from working: authorized it, set a missing secret, or
// brought its upstream back. From needs_auth, auth_error, or unreachable it
// drops the session, moves the connector to starting (EventAuthorized),
// and connects, waiting until the connect settles: at most the connect
// timeout, or until ctx is done. It returns the connect's *Error, or nil
// once the connector is ready; the new session fires OnToolsChanged, as
// every new session does. A ready connector is left alone, and Retry
// returns nil. A connector in starting is not restarted: Retry waits for
// the connect in progress and returns how it ended.
func (m *Manager) Retry(ctx context.Context, name string) error {
	c := m.lookup(name)

	if c == nil {
		return &Error{Connector: name, Kind: KindUnknownConnector}
	}

	defer m.notify(c)
	c.mu.Lock()

	if c.status().State == StateReady {
		c.mu.Unlock()

		return nil
	}

	m.restart(ctx, c)
	c.mu.Unlock()

	l, err := m.acquire(ctx, c)

	if err != nil {
		return err
	}

	l.release()

	return nil
}

// CredentialChanged reconnects the connector called name after its OAuth
// credential changed: a token was stored, or the credential was deleted.
// In any state it drops the session, so the next connect reads the
// credential as it is now, and connects at once, moving the connector
// through starting (EventAuthorized) from needs_auth, auth_error, or
// unreachable, as Retry does. One rule serves both directions: a new token
// makes the connector ready, and a deleted one puts it in needs_auth
// (no_credential) now rather than on its next use. It waits for the
// connect to settle (at most the connect timeout, or until ctx is done)
// and returns the connector's status then, as Status shows it. A name the
// manager doesn't know (a credential of a connector no longer configured)
// gets a status with no State and Kind unknown_connector, whose Advice
// says so.
//
// When it returns, the callbacks its connect queued have been called,
// OnToolsChanged for the new session among them, even if another goroutine
// was delivering the connector's callbacks: the daemon then waits for the
// sync that call requested rather than start one (wayfindr-t08.9). So,
// unlike Retry, a callback must not call it: it would wait on its own
// delivery.
func (m *Manager) CredentialChanged(ctx context.Context, name string) ConnectorStatus {
	unknown := ConnectorStatus{Connector: name, Kind: KindUnknownConnector}
	c := m.lookup(name)

	if c == nil {
		return unknown
	}

	defer m.flush(c)
	c.mu.Lock()
	m.restart(ctx, c)
	c.mu.Unlock()

	l, err := m.acquire(ctx, c)

	if err == nil {
		l.release()
	} else if e, ok := AsError(err); ok && e.Kind == KindUnknownConnector {
		// Removed while it connected.
		return unknown
	}

	return m.statusOf(c)
}

// Drop closes the session of the connector called name, if it has one,
// and leaves its state alone: the next use connects again. A revoke drops
// the connector before its credential is deleted, so the session's close,
// which for a Streamable HTTP session is a DELETE through the broker's
// transport, still carries the token, and a stateful upstream ends the
// session rather than keep it until its own timeout (wayfindr-zgp.13). A
// name the manager doesn't know (an orphaned credential's connector) has
// nothing to drop. It waits for a connect or a session start in progress
// on the connector; it returns ctx's error without dropping anything if
// ctx has ended by then. A stdio connector's child is stopped, as the
// reaper does.
func (m *Manager) Drop(ctx context.Context, name string) error {
	c := m.lookup(name)

	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := c.closeSession(ctx); err != nil {
		m.log.Debug().Str("connector", name).Err(redactErr(err)).Msg("close upstream")
	}

	return nil
}

// Hold keeps every use of the connector called name from starting, and so
// from opening a session, until the returned release is called: acquire
// waits for it, and Call, ListTools, Retry, CredentialChanged, and Apply's
// connects with it. Uses already under way carry on. A revoke holds the
// connector from before its Drop until the credential is deleted, so no
// use racing it opens a session with the token in between, which would
// then be closed without it (wayfindr-t08.10); a use that came meanwhile
// connects after the delete, into needs_auth. One hold at a time: Hold
// waits for one already taken, and returns ctx's error if ctx ends first.
// A name the manager doesn't know has nothing to hold. release is
// idempotent. It waits for a connect in progress, as Drop does.
func (m *Manager) Hold(ctx context.Context, name string) (release func(), err error) {
	c := m.lookup(name)

	if c == nil {
		return func() {}, nil
	}

	if err := c.lockUnheld(ctx); err != nil {
		return nil, err
	}

	held := make(chan struct{})
	c.held = held
	c.mu.Unlock()

	return sync.OnceFunc(func() {
		c.mu.Lock()
		c.held = nil
		c.mu.Unlock()
		close(held)
	}), nil
}

// lockUnheld locks c.mu once c is not held (see Hold), or returns ctx's
// error, unlocked, if ctx ends first.
func (c *connector) lockUnheld(ctx context.Context) error {
	for {
		c.mu.Lock()
		held := c.held

		if held == nil {
			return nil
		}

		c.mu.Unlock()

		select {
		case <-held:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// restart drops c's session, so the next acquire connects afresh, and
// moves c to starting from a state a fix leaves (needs_auth, auth_error,
// unreachable). In starting it does nothing: the connect pending there
// has not read anything yet. ctx's end cuts the session's close short (see
// closeSession). The caller holds c.mu.
func (m *Manager) restart(ctx context.Context, c *connector) {
	if c.removed {
		return
	}

	var event Event

	switch c.status().State {
	case StateStarting:
		return
	case StateReady:
	case StateNeedsAuth, StateAuthError, StateUnreachable:
		event = EventAuthorized
	}

	if err := c.closeSession(ctx); err != nil {
		m.log.Debug().Str("connector", c.name).Err(redactErr(err)).Msg("close upstream")
	}

	if event != 0 {
		m.transition(c, event, "", nil)
	}
}

// SecretChanged reconnects, in parallel, every connector waiting on the
// stored secret called name, after it was set: those whose last error is
// missing_value or secret_unreadable on Ref "secret:<name>". It returns
// their statuses, sorted by connector name, once each connect has settled
// (see Retry); none when no connector was waiting. A connector that
// resolved an older value keeps its session and picks the new value up on
// its next connect. ${env:…} references are not covered: the daemon's
// environment is fixed for its lifetime.
//
// As with CredentialChanged, the retried connectors' callbacks have been
// called when it returns, so a callback must not call it.
func (m *Manager) SecretChanged(ctx context.Context, name string) []ConnectorStatus {
	ref := string(config.RefSecret) + ":" + name
	var waiting []string

	for _, s := range m.Status() {
		if s.waitsOn(ref) {
			waiting = append(waiting, s.Connector)
		}
	}

	if len(waiting) == 0 {
		return nil
	}

	var wg sync.WaitGroup

	for _, c := range waiting {
		wg.Go(func() {
			// The outcome is in the connector's status, read below.
			if err := m.Retry(ctx, c); err != nil {
				m.log.Debug().Err(err).Str("connector", c).Msg("connector still failing after secret set")
			}
		})
	}

	wg.Wait()

	for _, name := range waiting {
		if c := m.lookup(name); c != nil {
			m.flush(c)
		}
	}

	out := make([]ConnectorStatus, 0, len(waiting))

	for _, s := range m.Status() {
		if slices.Contains(waiting, s.Connector) {
			out = append(out, s)
		}
	}

	return out
}

// waitsOn reports whether the connector is kept from working by the
// reference ref ("secret:<name>") alone: it is missing, or can't be
// decrypted, so storing it again is the fix.
func (s ConnectorStatus) waitsOn(ref string) bool {
	if s.Ref != ref {
		return false
	}

	return s.Kind == KindMissingValue || s.Kind == KindAuthConfig && s.Case == CaseSecretUnreadable
}

// connect opens c's session, judges the outcome, and records it in c's
// state. A failure is recorded even when the caller gave up (its ctx
// ended): the upstream hadn't answered by then, and a connector left as it
// was would sit in starting, or read ready with no session, until the next
// use. The caller holds c.mu.
func (m *Manager) connect(ctx context.Context, c *connector) error {
	d, err := m.dial(ctx, c.name, c.cfg, c.sse)

	if err != nil {
		return m.connectFailed(ctx, c, err)
	}

	c.mask = d.mask

	client := mcp.NewClient(m.impl, m.clientOptions(c))
	client.AddReceivingMiddleware(declineInput())
	s, err := m.connectBounded(ctx, client, d.transport)

	if d.probe != nil {
		if err == nil {
			d.probe.settle()
		} else {
			err = d.probe.failure(err)
		}
	}

	if err != nil && d.fallsBack() {
		m.log.Info().Str("connector", c.name).Int("status", d.probe.initializeStatus()).
			Msg("streamable http turned away; falling back to sse")
		var serr error

		if s, serr = m.connectBounded(ctx, client, d.fallback); serr == nil {
			// Remembered for the connector's lifetime (design §7.2).
			c.sse = true
			err = nil
		} else {
			err = fmt.Errorf("streamable http: %w; sse fallback: %w", err, serr)
		}
	}

	if err != nil {
		return m.connectFailed(ctx, c, err)
	}

	// Only what comes over the session from here on is its own overflow.
	d.overflow.reset()

	done := make(chan struct{})
	go func() {
		// Wait ends when the session closes, by closeSession or by the
		// upstream; its error is the transport's and says nothing more.
		_ = s.Wait()
		close(done)
	}()
	c.session, c.sessionDone, c.sessionEpoch, c.halt, c.overflow = s, done, c.epoch, d.halt, d.overflow
	c.lastUsed = m.now()
	m.record(c, EventSynced, "", nil, c.sessionEpoch)

	return nil
}

// clientOptions are the SDK client options for one connector. The client
// advertises elicitation, form and URL, and nothing else (the SDK default
// would advertise roots). Multi round-trip handling is off, so Call sees
// an upstream's input_required and passes it to the user rather than the
// SDK answering it; an elicitation/create during a call goes to elicit.
func (m *Manager) clientOptions(c *connector) *mcp.ClientOptions {
	opts := &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{
			Form: &mcp.FormElicitationCapabilities{},
			URL:  &mcp.URLElicitationCapabilities{},
		}},
		ElicitationHandler: m.elicit(c),
		MultiRoundTrip:     &mcp.MultiRoundTripOptions{Disabled: true},
	}

	if m.onToolsChanged != nil {
		opts.ToolListChangedHandler = func(context.Context, *mcp.ToolListChangedRequest) {
			// Queued with the connector's other events, to keep their order.
			m.toolsChanged(c)
			m.notify(c)
		}
	}

	return opts
}

// connectBounded connects client over t within the connect timeout.
//
// The SDK ties some transports' long-lived streams (the HTTP+SSE GET) to
// the context Connect gets, and a use's context ends with the use. So the
// connect runs under a context detached from ctx, which ctx's end or the
// connect timeout cancel only while the connect is in progress.
//
// A connect given up on returns at once: the SDK's cleanup of a failed
// connect can wait seconds on an unresponsive upstream, so it finishes in
// the background.
func (m *Manager) connectBounded(ctx context.Context, client *mcp.Client, t mcp.Transport) (*mcp.ClientSession, error) {
	connCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	type result struct {
		s   *mcp.ClientSession
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := client.Connect(connCtx, t, nil)
		done <- result{s, err}
	}()
	timer := time.NewTimer(m.connectTimeout)
	defer timer.Stop()

	var err error

	select {
	case r := <-done:
		if r.err != nil {
			cancel()

			return nil, r.err
		}

		// connCtx lives as long as the session, and closing the session
		// ends everything that used it, so cancel isn't needed past here.
		_ = cancel

		return r.s, nil
	case <-timer.C:
		err = fmt.Errorf("%w after %v", errConnectTimeout, m.connectTimeout)
	case <-ctx.Done():
		err = fmt.Errorf("connect given up before the upstream answered: %w", context.Cause(ctx))
	}

	cancel()
	go func() {
		if r := <-done; r.err == nil {
			// The connect finished as it was given up on.
			_ = r.s.Close()
		}
	}()

	return nil, err
}

// declineInput answers the upstream's sampling requests with a
// codeInputDeclined error, which Call finds in the failed call's own
// error. The client doesn't advertise sampling, but an upstream may ask
// anyway.
func declineInput() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "sampling/createMessage" {
				return next(ctx, method, req)
			}

			return nil, &jsonrpc.Error{Code: codeInputDeclined, Message: declineMessage}
		}
	}
}

// connectFailed judges a failed connect, records the verdict in c's state,
// and returns its error. The caller holds c.mu.
func (m *Manager) connectFailed(ctx context.Context, c *connector, err error) error {
	v := judge(contextOutcome(ctx, outcome{connector: c.name, phase: phaseConnect, err: err, mask: c.mask}))
	m.transition(c, v.event, v.reason, v.err)

	return v.err
}

// record applies event, from a connect or a use over a session opened at
// epoch, to c. A success clears needs_auth or auth_error only when the
// session was opened after c entered that state (epoch is c's epoch): the
// upstream accepted what the user fixed. It moves c through starting to
// ready, as Retry does, by applying EventAuthorized first. A success over
// an older session is stale, and Transition refuses it. The caller holds
// c.mu.
func (m *Manager) record(c *connector, event Event, reason string, cause error, epoch uint64) {
	if event == EventSynced && epoch == c.epoch {
		if s := c.status().State; s == StateNeedsAuth || s == StateAuthError {
			m.transition(c, EventAuthorized, "", nil)
		}
	}

	m.transition(c, event, reason, cause)
}

// transition applies event to c, with the reason and the error (nil or an
// *Error) that explain it. Entering needs_auth or auth_error, even from
// itself, starts a new epoch. The caller holds c.mu, which keeps one
// connector's transitions, and so its queued OnStateChanged calls, in
// order; it calls notify once it has released its locks.
func (m *Manager) transition(c *connector, event Event, reason string, cause error) {
	// The cause is also what the use returns, so the link reaches the
	// caller whether or not the event applies.
	if e, ok := AsError(cause); ok && e.Kind == KindNeedsAuth && e.Link == "" {
		e.Link = m.authLink(e.Connector)
	}

	c.stateMu.Lock()
	from := c.state
	to, ok := Transition(from, event)

	if !ok {
		c.stateMu.Unlock()
		m.log.Debug().Str("connector", c.name).Str("state", string(from)).
			Stringer("event", event).Msg("connector event ignored")

		return
	}

	c.state, c.reason, c.err = to, reason, nil

	switch to {
	case StateNeedsAuth, StateAuthError:
		c.epoch++
		c.err, _ = AsError(cause)
	case StateUnreachable:
		c.err, _ = AsError(cause)
	case StateStarting, StateReady:
	}

	if to != from {
		c.since = m.now()
	}

	switch {
	case to == StateReady:
		c.notReadySince = time.Time{}
	case c.notReadySince.IsZero():
		c.notReadySince = c.since
	}

	c.stateMu.Unlock()

	if to == from {
		return
	}

	l := m.log.Info()

	if to != StateReady && to != StateStarting {
		l = m.log.Warn()
	}

	l.Str("connector", c.name).Str("state", string(to)).Str("reason", reason).Msg("connector state changed")

	if m.onStateChanged != nil {
		c.queue(notice{state: to})
	}
}

// authLink is Options.AuthURL's link for connector; "" without one.
func (m *Manager) authLink(connector string) string {
	if m.authURL == nil {
		return ""
	}

	return m.authURL(connector)
}

// toolsChanged queues an OnToolsChanged call for c, if the manager has the
// callback.
func (m *Manager) toolsChanged(c *connector) {
	if m.onToolsChanged != nil {
		c.queue(notice{toolsChanged: true})
	}
}

// notify delivers c's queued callbacks, in order. The caller holds no
// lock, so a callback may call back into the manager. If another goroutine
// is delivering c's callbacks already, notify leaves the queue to it and
// returns at once: that keeps one connector's callbacks one at a time and
// in order, and a callback whose own calls queue more is not reentered.
func (m *Manager) notify(c *connector) { m.deliver(c, false) }

// flush is notify for a caller that must know, when it returns, that the
// callbacks it queued have been called: if another goroutine is delivering
// c's callbacks, it waits until that goroutine has emptied the queue, which
// includes what the caller queued, and then delivers what is left. A
// callback must not reach it: it would wait on its own delivery.
func (m *Manager) flush(c *connector) { m.deliver(c, true) }

// deliver is notify, or with wait, flush.
func (m *Manager) deliver(c *connector, wait bool) {
	c.noticeMu.Lock()
	defer c.noticeMu.Unlock()

	for wait && c.notifying {
		c.idle.Wait()
	}

	if c.notifying {
		return
	}

	c.notifying = true

	for len(c.notices) > 0 {
		n := c.notices[0]
		c.notices = c.notices[1:]
		c.noticeMu.Unlock()

		if n.toolsChanged {
			m.onToolsChanged(c.name)
		} else {
			m.onStateChanged(c.name, n.state)
		}

		c.noticeMu.Lock()
	}

	c.notifying = false
	c.idle.Broadcast()
}

// notice is one queued callback call: OnToolsChanged, or OnStateChanged
// with state.
type notice struct {
	toolsChanged bool
	state        State
}

// connector is one configured upstream.
type connector struct {
	name string

	// mu guards session creation and use tracking: the fields up to
	// stateMu.
	mu      sync.Mutex
	cfg     config.Connector
	session *mcp.ClientSession
	// sessionDone is closed when session ends. Nil when session is.
	sessionDone <-chan struct{}
	// halt ends every request of session's transport (see dialed.halt).
	// Nil when session is.
	halt func()
	// overflow records a message session's transport cut off for being
	// over the limit (see dialed.overflow). Nil when session is.
	overflow *overflow
	// mask hides the values of the references the connector was last
	// dialed with (see dialed.mask) in the text of its errors.
	mask redact.Values
	// removed is set when the connector leaves the manager; it can't be
	// used after that.
	removed bool
	// held, while Hold's hold lasts, is closed when it is released.
	held chan struct{}
	// sse is set once a url connector has fallen back to HTTP+SSE, so later
	// connects skip the Streamable attempt. A config change clears it.
	sse bool
	// epoch counts the connector's entries into needs_auth and auth_error,
	// and sessionEpoch is its value when session was opened. A success
	// clears either state only over a session opened since (see record).
	epoch        uint64
	sessionEpoch uint64
	// inUse counts the uses in progress. lastUsed is when the last use
	// ended, or when session was opened. The reaper reads both.
	inUse    int
	lastUsed time.Time

	// stateMu guards the state fields, so Status never waits on a connect.
	stateMu sync.Mutex
	state   State
	reason  string
	since   time.Time
	// notReadySince is ConnectorStatus.NotReadySince.
	notReadySince time.Time
	// err is the error that put the connector in state; nil in starting
	// and ready.
	err *Error
	// toolsTTL is ConnectorStatus.ToolsTTL.
	toolsTTL time.Duration

	// noticeMu guards the callback queue: the notices not yet delivered,
	// and whether a goroutine is delivering them (see notify). idle, on
	// noticeMu, is broadcast each time a delivery ends (see flush).
	noticeMu  sync.Mutex
	notices   []notice
	notifying bool
	idle      sync.Cond

	// runsMu guards runs, the tools/calls in flight, which the elicitation
	// handler reads to tie a question to its call.
	runsMu sync.Mutex
	runs   map[*run]struct{}
}

// queue adds n to c's callback queue, for notify to deliver.
func (c *connector) queue(n notice) {
	c.noticeMu.Lock()
	defer c.noticeMu.Unlock()
	c.notices = append(c.notices, n)
}

func (c *connector) status() ConnectorStatus {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	s := ConnectorStatus{
		Connector: c.name, State: c.state, Reason: c.reason, Since: c.since, NotReadySince: c.notReadySince,
		ToolsTTL: c.toolsTTL,
	}

	if c.err != nil {
		e := c.err
		s.Kind, s.Case, s.Ref, s.ProviderCode, s.Link = e.Kind, e.Case, e.Ref, e.ProviderCode, e.Link
	}

	return s
}

// closeSession closes c's upstream, if any. The SDK's close takes no ctx,
// so ctx's end halts the session's transport instead, which ends a url
// upstream's session DELETE at once; a stdio child's stop has its own
// bound. Once closed, the transport is halted anyway: nothing is left to
// send on it. The caller holds c.mu.
func (c *connector) closeSession(ctx context.Context) error {
	if c.session == nil {
		return nil
	}

	c.endRuns()
	stop := context.AfterFunc(ctx, c.halt)
	err := c.session.Close()
	stop()
	c.halt()
	c.session, c.sessionDone, c.halt, c.overflow = nil, nil, nil, nil

	return c.mask.Err(err)
}

// sessionEnded reports whether c has a session that has ended without
// closeSession. The caller holds c.mu.
func (c *connector) sessionEnded() bool {
	if c.session == nil {
		return false
	}

	select {
	case <-c.sessionDone:
		return true
	default:
		return false
	}
}
