package daemon

import (
	"context"
	"sync"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/catalog"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// recovery owns what follows each "the user fixed it" event (design §7.8
// step 5): a credential stored or deleted, which the broker reports
// (credentialChanged), a revoke (revoke), and a secret set
// (secretChanged). Each reconnects the connectors the change affects
// through the manager and, for each that is then ready, waits for the
// catalog sync its new session requested (catalog.Syncer.Await), so
// whoever reports a connector afterwards reports it with its tools, and
// with one tools/list (wayfindr-zgp.12). The auth flow's long-poll and the
// revoke's answer wait for a connector's latest credential recovery (wait).
//
// It is built before the broker, whose OnCredentialChanged is
// credentialChanged, and bound to the manager, the syncer, and the broker
// once they exist (bind): the one late binding in New. Nothing calls it
// before Serve. Shutdown closes it after the HTTP server's drain and
// before the syncer and the manager close.
type recovery struct {
	log zerolog.Logger

	// Set by bind.
	manager *upstream.Manager
	syncer  *catalog.Syncer
	broker  *auth.Broker

	// stopped is closed by close; it cancels the background reconnects.
	stopped chan struct{}
	// wg counts the background reconnects.
	wg sync.WaitGroup

	// mu guards closed and conns.
	mu     sync.Mutex
	closed bool
	conns  map[string]*connRecovery
}

// connRecovery is one connector's recoveries.
type connRecovery struct {
	// turn is held by the connector's running reconnect, and by a revoke
	// from before it drops the session until the credential is deleted, so
	// none of the connector's reconnects runs in between with the token
	// the revoke deletes.
	turn chan struct{}
	// latest is the connector's most recent credential recovery.
	latest *attempt
}

// attempt is one credential change's recovery. done is closed when it has
// settled, after which status and ok are set.
type attempt struct {
	done   chan struct{}
	status upstream.ConnectorStatus
	// ok is false for a recovery that shutdown abandoned before it ran.
	ok bool
}

func newRecovery(log zerolog.Logger) *recovery {
	return &recovery{log: log, stopped: make(chan struct{}), conns: map[string]*connRecovery{}}
}

// bind gives r what it acts through. New calls it once, before Serve.
func (r *recovery) bind(manager *upstream.Manager, syncer *catalog.Syncer, broker *auth.Broker) {
	r.manager, r.syncer, r.broker = manager, syncer, broker
}

// conn returns connector's recoveries, creating them; the caller holds mu.
func (r *recovery) conn(connector string) *connRecovery {
	c, ok := r.conns[connector]

	if !ok {
		c = &connRecovery{turn: make(chan struct{}, 1)}
		r.conns[connector] = c
	}

	return c
}

// credentialChanged is the broker's OnCredentialChanged: a token was
// stored, or the credential deleted. It starts the connector's recovery in
// the background and returns at once, since the broker calls it from the
// browser's callback request, which must not wait on a connect. The
// recovery is the connector's latest from now on. After close it does
// nothing.
func (r *recovery) credentialChanged(connector string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	c := r.conn(connector)
	a := &attempt{done: make(chan struct{})}
	c.latest = a
	r.wg.Go(func() {
		defer close(a.done)
		ctx, cancel := r.context()
		defer cancel()

		if !take(ctx, c.turn) {
			return
		}

		defer func() { <-c.turn }()
		a.status, a.ok = r.reconnect(ctx, connector), true
	})
}

// reconnect has the manager reconnect connector after its credential
// changed and, once it is ready, waits for the sync its new session
// requested. By the time CredentialChanged returns, that request has been
// made (upstream's OnToolsChanged has been called), so Await finds it.
func (r *recovery) reconnect(ctx context.Context, connector string) upstream.ConnectorStatus {
	s := r.manager.CredentialChanged(ctx, connector)

	if s.State == upstream.StateReady {
		r.awaitSync(ctx, connector)
	}

	r.log.Info().Str("connector", connector).Str("state", string(s.State)).Str("kind", string(s.Kind)).
		Msg("connector reconnected after credential change")

	return s
}

// awaitSync waits for connector's requested sync; its failure is logged
// here, since the syncer leaves a failure someone waits for to them.
func (r *recovery) awaitSync(ctx context.Context, connector string) {
	if err := r.syncer.Await(ctx, connector); err != nil {
		r.log.Warn().Err(err).Str("connector", connector).Msg("catalog sync after recovery")
	}
}

// secretChanged has the manager retry every connector waiting on the
// stored secret called name, which was just set, and, for each that is
// then ready, waits for the sync its new session requested, in parallel.
// It returns their statuses as Manager.SecretChanged does, so the rows
// built from them show the connectors' tools.
func (r *recovery) secretChanged(ctx context.Context, name string) []upstream.ConnectorStatus {
	statuses := r.manager.SecretChanged(ctx, name)
	var wg sync.WaitGroup

	for _, s := range statuses {
		if s.State == upstream.StateReady {
			wg.Go(func() { r.awaitSync(ctx, s.Connector) })
		}
	}

	wg.Wait()

	return statuses
}

// revoke forgets connector's credential and reports how the connector
// settled. It closes the connector's session while the token still exists
// (Manager.Drop), so a stateful upstream's session DELETE carries it
// (wayfindr-zgp.13), and only then has the broker delete the credential,
// which reports the change: the recovery that starts reconnects without a
// token and settles the connector in needs_auth. found is false when there
// was no credential; status is the settled one, nil for a connector the
// config no longer names or when ctx ends before it settles.
//
// The order holds against the connector's other recoveries: revoke takes
// the connector's turn before the drop and gives it up only once the
// credential is gone, so a recovery already running finishes first (its
// session is the one dropped), and none, its own included, connects in
// between. Nor does a use of the connector (a call, or a poll's sync)
// racing the revoke: the revoke holds the connector in the manager
// (Manager.Hold) from before the drop until the delete, so such a use
// waits and connects after the delete, without the token, and every
// session opened with the token is closed with it (wayfindr-t08.10).
//
// A connector with no credential (404) has its session dropped too; its
// next use reconnects.
func (r *recovery) revoke(ctx context.Context, connector string) (bool, *upstream.ConnectorStatus, error) {
	r.mu.Lock()
	c := r.conn(connector)
	r.mu.Unlock()

	if !take(ctx, c.turn) {
		return false, nil, ctx.Err()
	}

	found, err := r.dropThenRevoke(ctx, connector)
	<-c.turn

	if err != nil || !found {
		return found, nil, err
	}

	// The broker started the recovery before Revoke returned.
	s, ok := r.wait(ctx, connector)

	if !ok || s.Kind == upstream.KindUnknownConnector {
		return true, nil, nil
	}

	return true, &s, nil
}

// dropThenRevoke drops connector's session, then deletes its credential,
// holding the connector throughout (Manager.Hold), so no use opens a
// session in between. The caller holds the connector's turn.
func (r *recovery) dropThenRevoke(ctx context.Context, connector string) (bool, error) {
	release, err := r.manager.Hold(ctx, connector)

	if err != nil {
		return false, err
	}

	defer release()

	if err := r.manager.Drop(ctx, connector); err != nil {
		return false, err
	}

	return r.broker.Revoke(ctx, connector)
}

// wait waits for connector's latest credential recovery and returns the
// status it left the connector in. It reports false if there is none, it
// was abandoned at shutdown, or ctx ends first. One that has already
// settled is reported even if ctx has ended: a poll without wait runs with
// a ctx that is done from the start, and a select would pick between the
// two at random (wayfindr-zgp.18).
func (r *recovery) wait(ctx context.Context, connector string) (upstream.ConnectorStatus, bool) {
	r.mu.Lock()
	var a *attempt

	if c, ok := r.conns[connector]; ok {
		a = c.latest
	}

	r.mu.Unlock()

	if a == nil {
		return upstream.ConnectorStatus{}, false
	}

	select {
	case <-a.done:
		return a.status, a.ok
	default:
	}

	select {
	case <-a.done:
		return a.status, a.ok
	case <-ctx.Done():
		return upstream.ConnectorStatus{}, false
	}
}

// close cancels the background recoveries and waits for them; none starts
// after it. Shutdown calls it once the server has drained, before the
// syncer and the manager close under the recoveries.
func (r *recovery) close() {
	r.mu.Lock()

	if !r.closed {
		r.closed = true
		close(r.stopped)
	}

	r.mu.Unlock()
	r.wg.Wait()
}

// context returns a context for a background recovery, which close
// cancels.
func (r *recovery) context() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-r.stopped:
			cancel()
		case <-ctx.Done():
		}
	}()

	return ctx, cancel
}

// take takes turn, or reports false once ctx has ended, even if the turn
// was free.
func take(ctx context.Context, turn chan struct{}) bool {
	select {
	case turn <- struct{}{}:
	case <-ctx.Done():
		return false
	}

	if ctx.Err() != nil {
		<-turn

		return false
	}

	return true
}
