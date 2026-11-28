// Package daemon wires Wayfindr's packages into the running daemon and owns
// its lifecycle: startup, the reload of a new config, and shutdown (design
// §7.8). Events travel upward through the callbacks it wires; nothing below
// it imports it.
package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/catalog"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/daemonlock"
	"github.com/atomicpages/wayfindr/internal/embed"
	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/httpclient"
	"github.com/atomicpages/wayfindr/internal/httpd"
	"github.com/atomicpages/wayfindr/internal/oauthserver"
	"github.com/atomicpages/wayfindr/internal/paths"
	"github.com/atomicpages/wayfindr/internal/resolve"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/drivers"
	"github.com/atomicpages/wayfindr/internal/telemetry"
	"github.com/atomicpages/wayfindr/internal/upstream"
	"github.com/atomicpages/wayfindr/internal/webui"
)

const (
	// drainTimeout bounds how long shutdown waits for in-flight requests
	// (design §7.8).
	drainTimeout = 10 * time.Second
	// stopGrace is how long the server waits, once the drain has timed out
	// and it has cancelled the remaining requests, for them to return.
	// It is also the least time the cancelled catalog syncs get to return
	// when the drain has used up drainTimeout.
	stopGrace = time.Second
)

// Options is what the daemon reads from the host. Tests substitute fakes.
type Options struct {
	// Env is the process environment and home directory: where the config
	// and data directory are, and the master key and client token
	// variables.
	Env paths.Env
	// LookupEnv resolves ${env:…} references, with os.LookupEnv semantics.
	// Nil means os.LookupEnv.
	LookupEnv func(key string) (string, bool)
	// Keychain is the OS keychain the master key may be in. Nil means none
	// is available.
	Keychain secrets.Keychain
	Logger   zerolog.Logger
	// Version is reported by /healthz and sent to clients and upstreams.
	Version string
	// Commit is the build's commit. With Version, it decides whether the
	// build is a release, the only kind that sends the usage report.
	Commit string
	// Clock times the poll of HTTP connectors and the warning check. Nil
	// means the system clock.
	Clock Clock
	// Jitter draws from [0, 1) to spread each poll within its interval
	// (pollInterval), and to place the first usage report. Nil means
	// math/rand/v2.Float64.
	Jitter func() float64
	// ReportDifficulty is the usage report's proof-of-work. Zero means
	// telemetry.Difficulty; tests lower it.
	ReportDifficulty int
	// RemoteTestClient is the HTTP client the admin API's connection test
	// sends through (ADR 0021). Nil means the shared outbound client; tests
	// pass one that trusts their server's certificate.
	RemoteTestClient *http.Client
	// OAuthClient is the HTTP client the authorization server fetches
	// clients' ID metadata documents with (ADR 0022). Nil means
	// httpclient.PublicOnly(), which refuses private addresses; tests pass
	// one that reaches their TLS server.
	OAuthClient *http.Client
	// RemoteTestTimeout bounds one connection test, both probes together.
	// Zero means 10 seconds; tests shorten it.
	RemoteTestTimeout time.Duration
}

// Daemon is a started daemon: its listener is bound and its store open.
// Serve runs it and, when its context ends, shuts it down. Every Daemon
// that New returns must be served, or its resources are never released.
type Daemon struct {
	log      zerolog.Logger
	paths    paths.Paths
	loadOpts config.Options
	initial  *config.Config
	// lock is the daemon lock on the data directory, held from before the
	// bind until after the store has closed.
	lock *daemonlock.Lock

	store    store.Store
	userID   string
	manager  *upstream.Manager
	syncer   *catalog.Syncer
	gateway  *gateway.Gateway
	server   *httpd.Server
	resolver config.Resolver
	poller   *poller
	health   health
	calls    *callLog
	tokens   *clientTokens
	// oauth is Wayfindr's authorization server (ADR 0022), whose handlers
	// httpd serves; the admin API reaches its pending authorizations
	// through an adapter over it.
	oauth *oauthserver.Server
	// telemetry counts usage and sends the usage report while
	// telemetry.enabled is on (ADR 0020).
	telemetry *telemetry.Reporter
	// clock times the usage report, and jitter, drawing from [0, 1),
	// places its first.
	clock  Clock
	jitter func() float64
	// cfgState is what the admin status reports of the config.
	cfgState *configState

	// recovery runs the reconnects that credential changes, revokes, and
	// secret sets start; shutdown closes it, which cancels and waits for
	// the ones running in the background.
	recovery *recovery
	// flows is auth's flow entry; a reload tells it the config changed, and
	// it cancels the flows the change outdates.
	flows *auth.Flows
	// clients are the url connectors' HTTP clients; a reload drops those
	// of the connectors it removed.
	clients *httpclient.Clients
	// broker is the OAuth broker; shutdown closes it, which stops its
	// device code pollers.
	broker *auth.Broker
	// emb is the [embedding] the embedder was last built from.
	emb embeddingState

	// fileMu makes each reloadFile one step, reading the file and applying
	// it: without it, a reload that read the file earlier (the watcher's)
	// could apply after one that read it later (an add's or a public URL
	// set's), putting the older config back.
	fileMu sync.Mutex
	// mu serializes Reload with itself and with shutdown, and guards cfg
	// and closed.
	mu sync.Mutex
	// cfg is the one current config (design §7.8). It is nil until Serve's
	// initial apply.
	cfg    *config.Config
	closed bool
}

// Run starts the daemon and serves until ctx ends or the process gets
// SIGINT or SIGTERM, then shuts it down. It is `wayfindr start --foreground`.
func Run(ctx context.Context, opts Options) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	d, err := New(ctx, opts)

	if err != nil {
		return err
	}

	return d.Serve(ctx)
}

// New performs startup up to serving (design §7.8 step 1): it loads the
// config, takes the daemon lock, binds the listener, opens the store,
// ensures the local user, loads the master key, reads the client token,
// and builds the broker, manager, syncer, and gateway. It takes the lock
// and binds before opening the store, so a second daemon on the data
// directory, at any server.listen, fails with "daemon already running"
// before it touches the store a running daemon writes (design §9). On
// failure it releases whatever it had opened.
func New(ctx context.Context, opts Options) (d *Daemon, err error) {
	log := opts.Logger
	p, err := paths.Resolve(opts.Env)

	if err != nil {
		return nil, err
	}

	loadOpts := config.Options{DataDir: p.DataDir, HomeDir: opts.Env.HomeDir}
	cfg, err := config.Load(ctx, p.ConfigFile, loadOpts)

	if err != nil {
		return nil, err
	}

	lock, err := daemonlock.Acquire(ctx, p.LockFile(), daemonlock.Holder{PID: os.Getpid(), Listen: cfg.Server.Listen})

	if held, ok := errors.AsType[*daemonlock.HeldError](err); ok {
		return nil, refused(held, cfg.Server.Listen)
	}

	if err != nil {
		return nil, err
	}

	cleanup := []func() error{lock.Release}
	defer func() {
		if err != nil {
			for i := len(cleanup) - 1; i >= 0; i-- {
				_ = cleanup[i]()
			}
		}
	}()

	ln, err := httpd.Listen(ctx, cfg.Server.Listen)

	if errors.Is(err, httpd.ErrAddrInUse) {
		return nil, fmt.Errorf("%s is in use: daemon already running? %w", cfg.Server.Listen, err)
	}

	if err != nil {
		return nil, err
	}

	cleanup = append(cleanup, ln.Close)

	st, err := drivers.Open(ctx, cfg.Storage.Driver, cfg.Storage.DSN)

	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	cleanup = append(cleanup, st.Close)
	userID, err := st.EnsureUser(ctx)

	if err != nil {
		return nil, fmt.Errorf("ensure user: %w", err)
	}

	sealer, source, err := secrets.Load(ctx, secrets.KeySources{
		Getenv:   opts.Env.Getenv,
		Keychain: opts.Keychain,
		KeyFile:  p.KeyFile(),
		Logger:   log,
	})

	if errors.Is(err, secrets.ErrNoKey) {
		return nil, fmt.Errorf("master key: %w: run `wayfindr init` first", err)
	}

	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}

	log.Info().Str("source", string(source)).Msg("master key loaded")
	token, err := clienttoken.Read(ctx, opts.Env, p)

	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("client token: %w: run `wayfindr init` first", err)
	}

	if err != nil {
		return nil, fmt.Errorf("client token: %w", err)
	}

	// A scoped value as the default token would give its holder every
	// connector and the admin API. It reaches here when a client's scoped
	// WAYFINDR_CLIENT_TOKEN leaks into the daemon's environment, or when a
	// scoped value is written to the file. The error names the source Read
	// took it from (the variable wins when set), never the value.
	if clienttoken.IsScoped(token) {
		if opts.Env.Getenv(clienttoken.EnvVar) != "" {
			return nil, fmt.Errorf("client token: %s holds a scoped client token, not the default one; unset it for the daemon", clienttoken.EnvVar)
		}

		return nil, fmt.Errorf("client token: %s holds a scoped client token, not the default one; "+
			"put the default token back, or delete the file and run `wayfindr init` to write a new one", p.ClientTokenFile())
	}

	// The bound address, not the configured one: they differ for port 0,
	// and the Host guard accepts both names of a loopback address.
	addr := ln.Addr().String()
	lookupEnv := opts.LookupEnv

	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	resolver := resolve.New(userID, lookupEnv, st, sealer, log)
	// One HTTP client per url connector, from its [tls] table: its MCP
	// traffic (the manager's base transport) and its authorization
	// server's (the broker's) both go through it.
	clients := httpclient.NewClients(resolver)

	// The broker reports a changed credential to the recovery module,
	// which reconnects the connector through the manager and waits for its
	// sync (design §7.8 step 5). The module comes first, since the broker
	// takes its hook, and is bound to the manager, the syncer, and the
	// broker below, once they exist: the one late binding here. Nothing
	// calls it before Serve.
	rec := newRecovery(log)
	broker := auth.NewBroker(auth.BrokerOptions{
		Store:    st,
		Sealer:   sealer,
		Resolver: resolver,
		Clients:  clients,
		Logger:   log,
		UserID:   userID,
		Listen:   addr,

		OnCredentialChanged: rec.credentialChanged,
	})
	cleanup = append(cleanup, func() error { broker.Close(); return nil })
	// The manager reports tool changes to the syncer, and the syncer lists
	// tools through the manager. The hook first fires in Serve's initial
	// apply, after syncer is set.
	hookCtx := context.WithoutCancel(ctx)
	var syncer *catalog.Syncer
	manager := upstream.New(upstream.Options{
		Logger:         log,
		Resolver:       resolver,
		Version:        opts.Version,
		OnToolsChanged: func(c string) { syncer.Request(hookCtx, c) },
		AuthTransport:  broker.Transport,
		AuthURL:        broker.Link,
		Clients:        clients,
	})
	cleanup = append(cleanup, func() error { return manager.Close(ctx) })
	// No embedder yet in the syncer or the gateway: the initial apply sets
	// one client in both, as every reload that changes [embedding] does.
	// The poll syncs through the syncer, and each sync that succeeds tells
	// the poll, which re-times the connector from the cache hint the sync
	// read (design §7.8 step 3).
	var poll *poller
	// The startup config, from before the server serves: Serve's initial
	// apply records it again, but a status request may come first. A reload
	// records the new config before it reconnects anything, so the sync a
	// changed tools list causes reads the new list.
	cfgState := &configState{}
	cfgState.applying(cfg)
	syncer = catalog.New(catalog.Options{
		Lister:   manager,
		Store:    st,
		UserID:   userID,
		Logger:   log,
		OnSynced: func(c string) { poll.synced(c) },
		Policy:   cfgState.toolPolicy,
	})
	clock := opts.Clock

	if clock == nil {
		clock = systemClock{}
	}

	poll = newPoller(clock, opts.Jitter, manager.Status, syncer.Sync, log)
	rec.bind(manager, syncer, broker)
	hl := health{broker: broker, lookup: cfgState.connector, status: manager.Status, now: time.Now, clock: clock, log: log}
	calls := newCallLog(st, userID, clock, cfg.Calls.Retention.Duration, log)
	// Off until the initial apply reads telemetry.enabled.
	reporter := telemetry.New(telemetry.Options{
		Endpoint:   cmp.Or(opts.Env.Getenv(telemetry.EndpointEnv), telemetry.Endpoint),
		IDFile:     p.TelemetryIDFile(),
		Version:    opts.Version,
		Commit:     opts.Commit,
		DoNotTrack: telemetry.DoNotTrack(opts.Env.Getenv(telemetry.DoNotTrackEnv)),
		Config:     cfgState.current,
		Now:        clock.Now,
		Difficulty: opts.ReportDifficulty,
		Logger:     log,
	})
	jitter := opts.Jitter

	if jitter == nil {
		jitter = rand.Float64
	}

	gw := gateway.New(gateway.Options{
		Upstreams: manager,
		Catalog:   st,
		UserID:    userID,
		Logger:    log,
		Version:   opts.Version,
		Warnings:  hl.urgent,
		Calls:     countedCalls{log: calls, report: reporter},
		OnSearch:  reporter.Search,
		Sealer:    sealer,
		Now:       clock.Now,
		Policy:    cfgState.toolPolicy,
		Profile:   cfgState.profile,
	})

	// The one flow entry for both doors: the browser's pages and the admin
	// API (design §7.3).
	flows := auth.NewFlows(broker, cfgState.connector)
	pages := auth.NewPages(flows)

	// Every scoped client token, loaded before the server serves, so none
	// is refused while the daemon starts.
	registry, err := loadClientTokens(ctx, st, userID, clock.Now)

	if err != nil {
		return nil, err
	}

	tokens := &clientTokens{
		store: st, userID: userID, registry: registry, clock: clock, log: log,
		clients: map[string]struct{}{}, live: map[string]struct{}{},
	}

	// Likewise every OAuth grant's access token, before httpd serves, so a
	// connected cloud agent is not refused while the daemon starts.
	if err := tokens.applyOAuth(ctx, cfg.Server); err != nil {
		return nil, err
	}

	// Wayfindr's authorization server (ADR 0022). It reads the running
	// [server] table on every request, so a reload turns it on and off.
	running := func() config.Server {
		if c := cfgState.current(); c != nil {
			return c.Server
		}

		return config.Server{}
	}
	// The approve page is on server.listen, the address `wayfindr ui`
	// opens, so the console's cookie is sent; a change to it needs a
	// restart, so the startup config's is the one served.
	listen := cfg.Server.Listen
	// Clients' documents are fetched through the public-only client; tests
	// pass their own.
	oauthClient := opts.OAuthClient

	if oauthClient == nil {
		oauthClient = httpclient.PublicOnly()
	}

	oauth := oauthserver.New(oauthserver.Options{
		Server: running,
		Grants: tokens,
		ApproveURL: func(id string) string {
			return "http://" + listen + webui.ApprovePath + "?req=" + url.QueryEscape(id)
		},
		Client: oauthClient,
		Now:    clock.Now,
		Logger: log,
	})

	// The connection test of a cloud agent's path goes out through the
	// shared outbound client, as a url connector without a [tls] table does.
	testClient := opts.RemoteTestClient

	if testClient == nil {
		testClient, err = clients.For(ctx, config.Connector{})

		if err != nil {
			return nil, err
		}
	}

	remoteTester := newRemoteTester(testClient, opts.RemoteTestTimeout, registry, log)

	// The web console's sessions: the CLI asks the admin API for a login,
	// and the console redeems it (ADR 0016).
	sessions := webui.NewSessions(clock.Now)
	// The console's way to add a connector: the daemon writes its own
	// config file (ADR 0017). Its reload is bound once the daemon exists.
	writer := &configWriter{
		file:      p.ConfigFile,
		opts:      loadOpts,
		putSecret: resolver.PutSecret,
		resolver:  resolver,
		config:    cfgState.snapshot,
		status:    manager.Status,
		awaitSync: rec.awaitSync,
		log:       log,
	}
	// A secret set also rebuilds the embedder when [embedding]'s api_key
	// refers to the secret; that is bound once the daemon exists.
	upstreams := &adminUpstreams{manager: manager, recovery: rec}
	adminAPI := admin.New(admin.Options{
		Upstreams: upstreams,
		Store:     st,
		UserID:    userID,
		// The resolver opens what it stores (design §7.1).
		PutSecret: resolver.PutSecret,
		DeleteSecret: func(ctx context.Context, name string) error {
			return st.DeleteSecret(ctx, userID, name)
		},
		ListSecrets: func(ctx context.Context) ([]string, error) {
			return st.ListSecrets(ctx, userID)
		},
		Version:       opts.Version,
		Config:        cfgState.snapshot,
		Flows:         authFlows{flows: flows, recovery: rec},
		Credentials:   credentials{recovery: rec},
		Health:        hl,
		CallRetention: calls.Retention,
		Search:        adminSearch(gw),
		Tools:         adminTools(manager, cfgState.toolPolicy),
		Call:          adminCall(gw, clienttoken.Digest(token), registry),
		Tokens:        tokens,
		OAuth:         oauthRequests{server: oauth, tokens: tokens},
		UILogin: func() admin.UILogin {
			path, expires := sessions.IssueLogin()

			return admin.UILogin{Path: path, ExpiresAt: expires}
		},
		AddConnector:    writer.add,
		SetPublicURL:    writer.setPublicURL,
		SetOAuthEnabled: writer.setOAuthEnabled,
		TestRemote:      remoteTester.test,
		Approvals:       adminApprovals{gw: gw},
		Logger:          log,
	})
	// The console reads through the admin API in process, so it and the
	// CLI get the same answers (design §7.10).
	console, err := webui.New(webui.Options{
		Admin:    admin.NewLocalClient(adminAPI),
		Sessions: sessions,
		Now:      clock.Now,
		Logger:   log,
	})

	if err != nil {
		return nil, err
	}

	server, err := httpd.NewServer(ln, httpd.Config{
		Token:        token,
		Clients:      registry,
		Version:      opts.Version,
		Logger:       log,
		CallbackPath: auth.CallbackPath,
		Handlers: httpd.Handlers{
			MCP:           gw,
			AuthConfirm:   http.HandlerFunc(pages.Confirm),
			AuthStart:     http.HandlerFunc(pages.Start),
			OAuthCallback: http.HandlerFunc(flows.HandleCallback),
			Admin:         adminAPI,
			UI:            console,

			OAuthResourceMetadata: http.HandlerFunc(oauth.ResourceMetadata),
			OAuthServerMetadata:   http.HandlerFunc(oauth.ServerMetadata),
			OAuthAuthorize:        http.HandlerFunc(oauth.Authorize),
			OAuthToken:            http.HandlerFunc(oauth.Token),
		},
		// While [server.oauth] is on, a remote /mcp request without a token
		// gets the 401 that names this URL, and the authorization server's
		// routes are served to remote requests.
		ResourceMetadataURL: func() string {
			s := running()

			if !s.OAuth.Enabled || s.Issuer() == "" {
				return ""
			}

			return oauthserver.ResourceMetadataURL(s.Issuer())
		},
	})

	if err != nil {
		return nil, err
	}

	built := &Daemon{
		cfgState:  cfgState,
		log:       log,
		paths:     p,
		loadOpts:  loadOpts,
		initial:   cfg,
		lock:      lock,
		store:     st,
		userID:    userID,
		manager:   manager,
		syncer:    syncer,
		gateway:   gw,
		server:    server,
		resolver:  resolver,
		poller:    poll,
		health:    hl,
		calls:     calls,
		tokens:    tokens,
		oauth:     oauth,
		telemetry: reporter,
		clock:     clock,
		jitter:    jitter,
		recovery:  rec,
		flows:     flows,
		clients:   clients,
		broker:    broker,
	}
	writer.reload = built.reloadFile
	upstreams.embedding = built.embeddingSecretSet

	return built, nil
}

// Addr is the address the daemon listens on.
func (d *Daemon) Addr() net.Addr { return d.server.Addr() }

// Serve serves HTTP, applies the startup config, which connects every
// connector in parallel and syncs each into the catalog, polls the HTTP
// connectors each refresh_interval, and serves until ctx ends. It then
// shuts the daemon down (design §7.8 step 4): it stops the poll and the
// usage reporter, stops
// accepting connections, drains in-flight requests for up to 10s, stops the
// recoveries, stops the syncer, closes every upstream (stopping stdio
// children), closes the store, and releases the daemon lock. No goroutine
// it started outlives it.
// Call it once.
func (d *Daemon) Serve(ctx context.Context) error {
	// Before serving, so no request finds the tools of a connector removed
	// while the daemon was stopped.
	d.dropStaleCatalogs(ctx)
	// Not ended by ctx: calls that finish during the drain are still
	// written. shutdown stops it, before the store closes.
	stopCalls := d.keepCallLog(context.WithoutCancel(ctx))
	// Likewise: uses during the drain are still recorded. shutdown stops
	// it, and it flushes, before the store closes.
	stopTouches := d.keepTouches(context.WithoutCancel(ctx))
	// Sends nothing until the startup apply turns telemetry on.
	stopTelemetry := d.keepTelemetry(ctx)
	served := make(chan error, 1)
	go func() { served <- d.server.Serve() }()
	d.log.Info().Str("listen", d.Addr().String()).Msg("daemon listening")

	// The watch starts before the startup apply, so an edit made while it
	// connects is queued, and is reloaded once it is done.
	cw := d.watchConfig()
	d.Reload(ctx, d.initial)
	stopWatch := d.reloadOnChange(ctx, cw)
	stopPoll := d.pollConnectors(ctx)
	stopHealth := d.watchHealth(ctx)

	var serveErr error

	select {
	case <-ctx.Done():
		d.log.Info().Msg("daemon shutting down")
	case serveErr = <-served:
		served = nil
		d.log.Error().Err(serveErr).Msg("http server stopped")
	}

	// No reload may start once shutdown has begun, and no poll.
	stopWatch()
	stopPoll()
	stopHealth()
	stopTelemetry()
	err := errors.Join(serveErr, d.shutdown(ctx, served, stopCalls, stopTouches))

	// Last, once the store has closed: from here another daemon may start
	// on the data directory.
	return errors.Join(err, d.lock.Release())
}

// refused is New's error when another daemon holds the daemon lock. A
// running daemon started with a different server.listen is the case the
// bind check misses: the config was edited and the daemon not restarted.
func refused(held *daemonlock.HeldError, listen string) error {
	if held.Holder.Listen != "" && held.Holder.Listen != listen {
		return fmt.Errorf("%w; server.listen is now %s: restart that daemon to apply it", held, listen)
	}

	return held
}

// dropStaleCatalogs drops the catalog of every connector that has tools in
// the store but is not in the startup config: one removed from the config
// while the daemon was stopped, which the startup apply, a diff from no
// config, never sees as removed. Credentials stay; the status lists them as
// orphaned. A failure is logged, and the next startup tries again.
func (d *Daemon) dropStaleCatalogs(ctx context.Context) {
	counts, err := d.store.ToolCounts(ctx, d.userID)

	if err != nil {
		d.log.Error().Err(err).Msg("list catalog connectors at startup")

		return
	}

	for _, c := range slices.Sorted(maps.Keys(counts)) {
		if _, ok := d.initial.Connectors[c]; ok {
			continue
		}

		if err := d.syncer.Remove(ctx, c); err != nil {
			d.log.Error().Err(err).Str("connector", c).Msg("drop removed connector from catalog")
			continue
		}

		d.log.Info().Str("connector", c).Msg("dropped catalog of connector no longer configured")
	}
}

// shutdown releases everything New opened, in the order of design §7.8.
// served is the Serve goroutine's result, or nil if it has returned.
// stopCalls stops the call log's writer, and stopTouches the client tokens'
// touch flush.
func (d *Daemon) shutdown(ctx context.Context, served <-chan error, stopCalls, stopTouches func()) error {
	ctx = context.WithoutCancel(ctx)

	var errs []error
	// The drain budget covers the requests' drain and then the syncs'.
	deadline := time.Now().Add(drainTimeout)
	drain, cancel := context.WithDeadline(ctx, deadline)

	// The requests drain before the reload lock is taken: a connector add
	// holds it through its reload while Apply waits on connects, and only
	// the drain's end cancels that request (wayfindr-zqef). The watcher and
	// the poll have stopped, so no other reload can start meanwhile. Best
	// effort: the server cancels the requests still running after the
	// drain, and the process is about to exit anyway.
	if err := d.server.Shutdown(drain, stopGrace); err != nil {
		d.log.Warn().Err(err).Msg("requests still running after cancel")
	}

	cancel()

	if served != nil {
		if err := <-served; err != nil {
			errs = append(errs, err)
		}
	}

	// No request is left to start a reload; one a request began has
	// returned. From here none may.
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true

	// The broker's device code pollers next: no request is left to start
	// one, and Close waits for them, so none stores a token, or starts a
	// recovery, from here on.
	d.broker.Close()

	// The recoveries next: close cancels the background reconnects and
	// waits for them, which return once their connects have given up,
	// before the syncer and the manager close under them. No secret set
	// rebuilds the embedder from then on either.
	d.recovery.close()
	d.emb.close()

	// The syncer first, then the manager, then the store they write to.
	// The SDK's session close waits for the requests in flight on it, so
	// the manager can close only once no sync is listing tools; and once
	// the syncer has closed, the OnToolsChanged hook requests nothing.
	//
	// Close cancels the running syncs and has what is left of the drain
	// budget, at least stopGrace, to wait for them. A sync still running
	// then has a lister or embedder that ignores the cancellation; Close
	// abandons it, and it writes nothing when it does return, so the
	// store can close under it (wayfindr-t08.7).
	wait, cancel := context.WithTimeout(ctx, max(time.Until(deadline), stopGrace))

	if err := d.syncer.Close(wait); err != nil {
		d.log.Warn().Err(err).Msg("catalog syncs abandoned at shutdown; they ignored the cancellation")
	}

	cancel()
	// The manager gets the same: what is left of the drain budget, at least
	// stopGrace. When it ends, every url upstream request still in flight
	// ends at once, a session DELETE the upstream never answers included,
	// and so does a close waiting on a connector's lock held by one; Close
	// returns once its closes have, so none of their requests outlives it
	// into the store's close (wayfindr-t08.12). A token refresh one of them
	// started may: see StopRefreshes below.
	wait, cancel = context.WithTimeout(ctx, max(time.Until(deadline), stopGrace))

	if err := d.manager.Close(wait); err != nil {
		errs = append(errs, fmt.Errorf("close upstreams: %w", err))
	}

	cancel()
	// The broker's refreshes next: no upstream request is left to start
	// one, but a refresh is detached from the request that started it and
	// may still be storing a rotated refresh token, which must land before
	// the store closes (wayfindr-1j97).
	d.broker.StopRefreshes()
	// The call log last before the store: no request is left to record a
	// call, and the writer stores what is queued. The same holds for the
	// client tokens' last uses.
	stopCalls()
	stopTouches()

	if err := d.store.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close store: %w", err))
	}

	d.log.Info().Msg("daemon stopped")

	return errors.Join(errs...)
}

// Reload makes next the current config (design §7.8 step 2). It diffs once
// against the current config and acts on that one Changes: the manager
// stops the removed connectors, the syncer then drops their catalogs (their
// credentials stay), the manager applies the rest, a changed [embedding]
// swaps the embedder, and keys that need a restart are logged. Serve's
// startup apply is a Reload from no config, and the config watcher's
// reloads follow it. Reload after shutdown has begun does nothing.
//
// next must be a valid config loaded with LoadOptions; an invalid one is
// the caller's to log and record, and the current config keeps running.
func (d *Daemon) Reload(ctx context.Context, next *config.Config) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return
	}

	ch := config.Diff(d.cfg, next)

	for _, key := range ch.RestartRequired {
		d.log.Warn().Str("key", key).Msg("config change takes effect on restart")
	}

	// Before Apply, which waits for connects while the status is served:
	// the status must not list a configured connector's credential as
	// orphaned meanwhile.
	d.cfgState.applying(next)
	// Before any connect waits too: a change that suspends OAuth grants
	// (OAuth off, another public_url, a client removed) stops their access
	// tokens at once.
	if err := d.tokens.applyOAuth(ctx, next.Server); err != nil {
		d.log.Error().Err(err).Msg("apply oauth grants; their access tokens are refused until a retry, made each minute, succeeds")
	}
	// Likewise nothing armed, pending, or claimed, and no code not yet
	// exchanged, outlives the OAuth settings it was made under, even when a
	// later reload brings them back: the user selects Connect a client
	// again.
	if d.cfg != nil && oauthChanged(d.cfg.Server, next.Server) {
		d.oauth.Disarm()
	}
	// Once the running config is next: a late callback must not store a
	// token for a connector that is gone, or one granted under its old url
	// or [auth] table. auth decides which flows that ends.
	d.flows.ConfigChanged()
	// Before Apply too, which can wait long on connects: turning telemetry
	// off must hold at once, so no report goes out after it, and calls
	// during a startup apply count once it is on (ADR 0020).
	d.telemetry.SetEnabled(next.Telemetry.Enabled)
	// Removed connectors go first, and their catalogs with them, so their
	// tools leave search before Apply waits for the others to connect.
	d.manager.Apply(ctx, next.Connectors, config.Changes{Removed: ch.Removed})
	d.clients.Retain(next.Connectors)

	for _, c := range ch.Removed {
		// Not cancelled by shutdown: the connector is gone, and a catalog
		// left behind would outlive the daemon.
		if err := d.syncer.Remove(context.WithoutCancel(ctx), c); err != nil {
			d.log.Error().Err(err).Str("connector", c).Msg("drop removed connector from catalog")
		}
	}

	rest := ch
	rest.Removed = nil
	d.manager.Apply(ctx, next.Connectors, rest)

	if ch.EmbeddingChanged {
		d.setEmbedder(ctx, next.Embedding)
	}

	d.calls.setRetention(next.Calls.Retention.Duration)
	d.poller.apply(next, ch)
	d.cfg = next
}

// configState is the config's part of the admin status: the connectors the
// running config names, its profiles, and why the last reload failed. It
// has its own lock, so a status request never waits for a Reload that is
// connecting. The running config is the one being applied, from before the
// apply starts.
type configState struct {
	mu sync.Mutex
	// cfg is the running config, for the usage report; nil before the
	// first apply.
	cfg        *config.Config
	connectors []string
	// configs are the running config's connectors, for the auth flow,
	// which the broker keeps no copy of.
	configs config.Connectors
	// profiles are the running config's profiles, which the gateway
	// resolves a request's profile name against (ADR 0011). A config is
	// never changed once loaded, so they are shared, not copied.
	profiles map[string]*config.Scope
	// err is why the last reload failed; a successful Reload clears it.
	err string
	// secretUsers is what in the running config refers to each secret
	// (`wayfindr secret list`). It is never changed once built, so it is
	// shared, not copied.
	secretUsers map[string][]string
	// publicURL is the running config's server.public_url (ADR 0021).
	publicURL string
	// oauth is the running config's server.oauth (ADR 0022).
	oauth config.OAuth
}

func (s *configState) snapshot() admin.ConfigState {
	s.mu.Lock()
	defer s.mu.Unlock()

	return admin.ConfigState{
		Connectors:  slices.Clone(s.connectors),
		Profiles:    maps.Clone(s.profiles),
		ConfigError: s.err,
		SecretUsers: s.secretUsers,
		PublicURL:   s.publicURL,
		OAuth:       s.oauth,
	}
}

// applying records cfg as the running config, as its apply begins.
func (s *configState) applying(cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	s.connectors = slices.Sorted(maps.Keys(cfg.Connectors))
	s.configs = cfg.Connectors
	s.profiles = cfg.Profiles
	s.secretUsers = cfg.SecretUsers()
	s.publicURL = cfg.Server.PublicURL
	s.oauth = cfg.Server.OAuth
	s.err = ""
}

// current returns the running config. A config is never changed once
// loaded, so it is shared, not copied.
func (s *configState) current() *config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cfg
}

// profile returns the running config's profile called name, or false when
// it defines none by that name.
func (s *configState) profile(name string) (*config.Scope, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[name]

	return p, ok
}

// connector returns the running config of the connector called name.
func (s *configState) connector(name string) (config.Connector, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.configs[name]

	return c, ok
}

// toolPolicy returns the running tools list of the connector called name;
// nil, which exposes every tool, for a connector not in the config.
func (s *configState) toolPolicy(name string) config.ToolPolicy {
	c, _ := s.connector(name)

	return c.Tools
}

// failed records why a reload failed; the running config is unchanged.
func (s *configState) failed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err.Error()
}

// embeddingState is the [embedding] table the embedder was last built
// from. Its lock serializes the builds: a Reload that changes [embedding],
// and a secret set that its api_key refers to, which takes only this lock,
// so it never waits for a Reload's connects or for shutdown's drain.
type embeddingState struct {
	mu  sync.Mutex
	cfg *config.Embedding
	// closed is set by shutdown; no build follows it.
	closed bool
}

// close ends the builds: none runs after it returns.
func (s *embeddingState) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// setEmbedder builds the embedder for e, the [embedding] table of the
// config being applied, and records e as the one later secret sets rebuild
// from.
func (d *Daemon) setEmbedder(ctx context.Context, e *config.Embedding) {
	d.emb.mu.Lock()
	defer d.emb.mu.Unlock()

	if d.emb.closed {
		return
	}

	d.emb.cfg = e
	d.buildEmbedder(ctx, e)
}

// embeddingSecretSet rebuilds the embedder after the stored secret called
// name was set, when the running [embedding]'s api_key refers to it, so
// the new value applies without a reload or a restart (design §4,
// wayfindr-9ydc). The rebuild is a changed [embedding]'s: it resolves the
// key again and starts the embedding pass, which embeds what the catalog
// still lacks, such as everything synced while the key was missing. A set
// of any other secret does nothing here.
func (d *Daemon) embeddingSecretSet(ctx context.Context, name string) {
	d.emb.mu.Lock()
	defer d.emb.mu.Unlock()
	e := d.emb.cfg

	if d.emb.closed || e == nil || !slices.Contains(e.Secrets(), name) {
		return
	}

	d.log.Info().Str("secret", name).Msg("embedding api_key secret set; rebuilding the embedding client")
	// ctx is the admin request's. Once the secret is stored, the rebuild
	// must not fail because the client gave up: nothing retries it, and a
	// failed key resolve would replace a working embedder with none.
	d.buildEmbedder(context.WithoutCancel(ctx), e)
}

// buildEmbedder gives the catalog and the gateway's query embedding one
// client for e, or removes it from both when there is none. Each receives
// an untyped nil then, never a nil *embed.Client. The caller holds
// d.emb.mu.
func (d *Daemon) buildEmbedder(ctx context.Context, e *config.Embedding) {
	var (
		forCatalog catalog.Embedder
		forQueries gateway.Embedder
		timeout    time.Duration
	)

	if c := d.embedder(ctx, e); c != nil {
		forCatalog, forQueries, timeout = c, c, e.Timeout.Duration
	}

	d.syncer.SetEmbedder(ctx, forCatalog)
	d.gateway.SetEmbedder(forQueries, timeout)
}

// LoadOptions are the options a reload passes to config.Load, the same as
// startup's.
func (d *Daemon) LoadOptions() config.Options { return d.loadOpts }

// ConfigFile is the path of the config file the daemon loaded.
func (d *Daemon) ConfigFile() string { return d.paths.ConfigFile }

// embedder builds the embedding client for e. It returns nil when e is nil
// or the client can't be built; search is then lexical only.
func (d *Daemon) embedder(ctx context.Context, e *config.Embedding) *embed.Client {
	if e == nil {
		return nil
	}

	key, err := e.APIKey.Resolve(ctx, d.resolver)

	if err != nil {
		d.log.Error().Err(err).Msg("embedding api_key unresolved; search is lexical only")

		return nil
	}

	c, err := embed.New(embed.Config{BaseURL: e.BaseURL, Model: e.Model, Dimensions: e.Dimensions, APIKey: key}, &http.Client{}, d.log)

	if err != nil {
		d.log.Error().Err(err).Msg("embedding client; search is lexical only")

		return nil
	}

	return c
}
