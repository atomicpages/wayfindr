// Package catalog keeps the catalog in step with the upstreams: the Syncer
// fetches a connector's tool list, turns it into catalog entries with their
// embed text and content hash, and reconciles them into the store; its
// embedding pass then embeds the pending entries (design §7.4).
//
// The Syncer owns the catalog's lifecycle. The daemon wires upstream's
// OnToolsChanged hook, which fires for every new session and every
// list_changed, to Request; that syncs a connector when it is added,
// reconnected, retried, or changes its tools; after a reconnect it caused,
// the daemon's recovery waits for that sync with Await. When a reload removes a
// connector, the daemon applies the removal to the upstream.Manager first,
// so no new session can request a sync of it, then calls Remove, which
// drops its catalog and fences off any sync of it already under way.
//
// Shutdown order: Syncer.Close(ctx) first, which refuses new work (a later
// Request does nothing), cancels the running syncs and embedding pass, and
// waits for them until ctx ends, abandoning any that ignore the
// cancellation, which then write nothing; then the upstream.Manager, whose
// session close waits for the tools/list requests in flight; and only then
// the store they write to.
package catalog

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
	"github.com/atomicpages/wayfindr/internal/store"
)

// ErrClosed is returned by Sync, EmbedPending, and Remove once Close has
// begun.
var ErrClosed = errors.New("catalog syncer closed")

// ErrRemoved is returned by a Sync whose run Remove fenced off: the
// connector was removed after the run was requested, so its result was
// discarded.
var ErrRemoved = errors.New("connector removed from catalog")

// ToolLister lists the tools a connector serves. upstream.Manager satisfies
// it; catalog does not import upstream, as the two are siblings (design §2).
//
// A ListTools error whose chain carries UpstreamKind() == "unknown_connector"
// (upstream's *Error does) means the lister has no such connector: it was
// removed. A sync that fails so records nothing, so no last-sync row
// outlives the connector.
type ToolLister interface {
	ListTools(ctx context.Context, connector string) ([]*mcp.Tool, error)
}

// upstreamKinded is an error that names its upstream Kind. catalog matches
// upstream's errors through it, by behaviour, because it may not import
// upstream (CODING_STANDARDS: Seam errors).
type upstreamKinded interface {
	UpstreamKind() string
}

// kindUnknownConnector is upstream's Kind for a connector it doesn't have.
const kindUnknownConnector = "unknown_connector"

// unknownConnector reports whether err says the lister has no such
// connector.
func unknownConnector(err error) bool {
	var k upstreamKinded

	return errors.As(err, &k) && k.UpstreamKind() == kindUnknownConnector
}

// Embedder embeds texts with one model. *embed.Client satisfies it.
type Embedder interface {
	// Embed returns one vector per input, in input order.
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	// ModelID is the model identity stored with each vector; an entry
	// embedded under another identity is pending.
	ModelID() string
}

// Options configures a Syncer.
type Options struct {
	Lister ToolLister
	Store  store.Store
	// Embedder embeds the catalog. Nil means no embedding provider is
	// configured: entries stay pending and search is lexical only.
	Embedder Embedder
	// UserID owns the catalog rows. Local mode has exactly one user.
	UserID string
	Logger zerolog.Logger
	// Now stamps sync records. Nil means time.Now.
	Now func() time.Time
	// OnSynced is called after each successful sync of a connector has
	// written its catalog, whoever requested it, on the sync's goroutine
	// and with no Syncer lock held; it should return quickly. The daemon's
	// poll re-times the connector from the cache hint that sync's
	// tools/list carried (design §7.8 step 3). Optional.
	OnSynced func(connector string)
	// Policy returns a connector's tool policy as the config now has it;
	// the tools it hides never reach the catalog. Nil exposes every tool.
	Policy func(connector string) config.ToolPolicy
	// EmbedTimeout bounds one batch of the embedding pass. Zero means
	// DefaultEmbedTimeout.
	EmbedTimeout time.Duration
}

// DefaultEmbedTimeout is how long one batch of the embedding pass may take:
// a provider that takes the request and never answers would otherwise stall
// the pass, and every rerun queued behind it, until Close (wayfindr-gxnw).
// It is generous next to embedding.timeout, which bounds one query, because
// a batch holds up to embed.BatchSize entries and a local model may be slow.
const DefaultEmbedTimeout = time.Minute

// Syncer syncs connectors into the catalog and embeds the pending entries.
// At most one sync runs per connector at a time, and at most one embedding
// pass at all; every request that arrives while one runs is coalesced into a
// single rerun, so a burst of triggers costs at most two runs.
//
// Each connector has a generation, which Remove bumps. A sync run belongs
// to the generation current when it was last requested, and writes only if
// that is still current when it writes, so a sync requested before a Remove
// can never write after it.
type Syncer struct {
	lister ToolLister
	store  store.Store
	userID string
	log    zerolog.Logger
	now    func() time.Time
	// onSynced is Options.OnSynced; nil for none.
	onSynced func(connector string)
	// policy is Options.Policy; nil for none.
	policy func(connector string) config.ToolPolicy
	// embedTimeout bounds one batch of the embedding pass.
	embedTimeout time.Duration

	wg       sync.WaitGroup
	mu       sync.Mutex
	embedder Embedder          // nil: no embedding provider
	flights  map[job]*flight   // jobs with a run in progress
	gens     map[string]uint64 // each connector's generation; absent is 0
	closed   bool              // Close has begun; no run may start
	closing  chan struct{}     // closed when Close begins; stops the embedding pass
	// drained is closed once every run has finished after Close began.
	drained   chan struct{}
	drainOnce sync.Once
	// writing holds one token while a sync writes its outcome, Remove
	// deletes a catalog, or the embedding pass stores a batch, so a sync's
	// generation check and its writes are one step that a Remove comes
	// wholly before or wholly after. Each checks closed once it holds the
	// token and writes nothing after Close has begun, so Close, by taking
	// the token once, knows no write follows (wayfindr-t08.7).
	writing chan struct{}
}

// job is what a flight runs: the sync of one connector, or the embedding
// pass.
type job struct {
	embed     bool   // the embedding pass; connector is empty
	connector string // the connector to sync
}

// flight is the run loop of one job: the run in progress and the rerun
// queued behind it, if any.
type flight struct {
	cur  *run
	next *run
}

// run is one run of a job. done is closed when it finishes, after which err
// holds its outcome.
type run struct {
	done    chan struct{}
	err     error
	waiters int    // Sync or EmbedPending calls waiting on this run
	gen     uint64 // a sync's connector generation, as of its latest request
}

// New returns a Syncer.
func New(opts Options) *Syncer {
	now := opts.Now

	if now == nil {
		now = time.Now
	}

	return &Syncer{
		lister:       opts.Lister,
		store:        opts.Store,
		embedder:     opts.Embedder,
		userID:       opts.UserID,
		log:          opts.Logger,
		now:          now,
		onSynced:     opts.OnSynced,
		policy:       opts.Policy,
		embedTimeout: cmp.Or(opts.EmbedTimeout, DefaultEmbedTimeout),
		flights:      map[job]*flight{},
		gens:         map[string]uint64{},
		closing:      make(chan struct{}),
		drained:      make(chan struct{}),
		writing:      make(chan struct{}, 1),
	}
}

// Sync syncs connector and waits for the outcome. If a sync of connector is
// already running, Sync waits for the rerun queued behind it, so the result
// always reflects the upstream as of some time after the call. ctx bounds
// only the wait: the sync itself runs to completion for the other callers
// coalesced into it, bounded by the lister's own timeout, unless Close
// cancels it. Once Close has begun, Sync returns ErrClosed; if Remove removes connector before the
// sync writes, Sync returns ErrRemoved.
//
// A successful sync requests an embedding pass but does not wait for it, so
// its outcome never depends on the embedding provider.
func (s *Syncer) Sync(ctx context.Context, connector string) error {
	return s.wait(ctx, s.enqueue(ctx, job{connector: connector}, true))
}

// Request starts a sync of connector in the background and returns at once;
// a failure is logged. It is what upstream's OnToolsChanged hook calls, on
// every new session and every list_changed. ctx supplies values only; the
// sync is not cancelled with it. Once Close has begun, Request does
// nothing.
func (s *Syncer) Request(ctx context.Context, connector string) {
	if s.enqueue(ctx, job{connector: connector}, false) == nil {
		s.log.Debug().Str("connector", connector).Msg("catalog sync request after close ignored")
	}
}

// Await waits for the sync of connector already requested, if any, and
// returns its outcome; it starts none. That is the rerun queued behind a
// running sync, else the running sync, so a sync requested before the
// call is complete when it returns; with none queued or running it returns
// nil at once. The daemon's recovery calls it once a reconnect has
// returned, by when the new session's OnToolsChanged has requested its
// sync, so a connector that recovered is reported with its tools without a
// second tools/list (wayfindr-zgp.12). ctx bounds only the wait. Once
// Close has begun, Await returns ErrClosed.
func (s *Syncer) Await(ctx context.Context, connector string) error {
	s.mu.Lock()

	if s.closed {
		s.mu.Unlock()

		return ErrClosed
	}

	f, ok := s.flights[job{connector: connector}]

	if !ok {
		s.mu.Unlock()

		return nil
	}

	r := f.next

	if r == nil {
		r = f.cur
	}

	// A failure is then the caller's to report.
	r.waiters++
	s.mu.Unlock()

	return s.wait(ctx, r)
}

// Remove drops connector's catalog and its last-sync record, as when a
// reload removes the connector; its credential stays. It is a fence: a sync of connector requested before Remove, whether
// running or queued, writes nothing after it, neither tools nor a sync
// record, and its Sync callers get ErrRemoved. Remove abandons such a sync
// rather than waiting for its fetch; it waits only for a write already in
// progress. A sync requested after Remove belongs to the connector's next
// life: re-adding the connector syncs it on its first new session.
//
// Call it after the upstream.Manager has dropped the connector, so no new
// session can request a sync of it in between. A sync that runs anyway
// fails as unknown_connector and records nothing. ctx bounds the wait and
// the delete; if it ends first, the fence still holds and Remove can be
// called again. Once Close has begun, Remove returns ErrClosed.
//
// Embeddings need no fence: the embedding pass stores a vector only on a
// row with the content hash it read, so one racing Remove writes nothing
// for the deleted rows.
func (s *Syncer) Remove(ctx context.Context, connector string) error {
	s.mu.Lock()

	if s.closed {
		s.mu.Unlock()

		return ErrClosed
	}

	s.gens[connector]++
	s.mu.Unlock()

	select {
	case s.writing <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}

	defer func() { <-s.writing }()

	if s.isClosed() {
		// Close began while Remove waited for the slot: the store may be
		// closed once Close returns.
		return ErrClosed
	}

	if err := s.store.DeleteCatalog(ctx, s.userID, connector); err != nil {
		return fmt.Errorf("remove %s: %w", connector, err)
	}

	s.log.Debug().Str("connector", connector).Msg("catalog removed")

	return nil
}

// Close stops the Syncer: from the moment it is called no sync or embedding
// pass starts, a rerun queued behind a running one is dropped, its callers
// getting ErrClosed, and the running syncs and embedding pass are
// cancelled. A sync writes nothing once Close has begun, not even a failed
// sync record, so a shutdown never records the cancellation as an upstream
// failure. Close then waits for the runs to return.
//
// If ctx ends first, as when a lister or embedder ignores the cancellation,
// Close abandons the runs still going, logs them, and returns ctx.Err()
// (wayfindr-t08.7). Every catalog write checks, in the write slot, that
// Close has not begun, so Close only waits for a write already in
// progress, which the cancellation ends, and from then on an abandoned run
// writes nothing when it does return. So the store may be closed as soon
// as Close returns, whatever it returns. The abandoned runs carry on until
// their lister or embedder returns; a later Close waits for them again.
// Close is idempotent.
func (s *Syncer) Close(ctx context.Context) error {
	s.mu.Lock()

	if !s.closed {
		s.closed = true
		close(s.closing)
	}

	s.mu.Unlock()
	// No wg.Go can follow: enqueue checks closed under mu before calling it.
	s.drainOnce.Do(func() {
		go func() {
			s.wg.Wait()
			close(s.drained)
		}()
	})

	select {
	case <-s.drained:
		return nil
	case <-ctx.Done():
	}

	// A write that took the slot before Close began may still be under way;
	// any later one finds the Syncer closed and writes nothing.
	s.writing <- struct{}{}
	<-s.writing

	select {
	case <-s.drained:
		return nil
	default:
	}

	s.mu.Lock()

	for j := range s.flights {
		s.logJob(s.log.Warn(), j).Msg("catalog run abandoned at close; it ignored the cancellation")
	}

	s.mu.Unlock()

	return ctx.Err()
}

// isClosed reports whether Close has begun.
func (s *Syncer) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.closed
}

// wait waits for r and returns its outcome, or ctx.Err() if ctx ends first.
// A nil r means the request was refused because Close has begun.
func (s *Syncer) wait(ctx context.Context, r *run) error {
	if r == nil {
		return ErrClosed
	}

	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// enqueue returns the run of j that will satisfy a request made now: a new
// run if j is idle, otherwise the rerun queued behind the running one. It
// returns nil once Close has begun.
func (s *Syncer) enqueue(ctx context.Context, j job, wait bool) *run {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	// A sync joins its connector's current generation; for the embedding
	// pass, whose connector is empty, gen is unused.
	gen := s.gens[j.connector]

	if f, ok := s.flights[j]; ok {
		if f.next == nil {
			f.next = newRun()
		}

		if wait {
			f.next.waiters++
		}

		// The rerun has not started, so it fetches after this request: its
		// result belongs to the generation this request was made in.
		f.next.gen = gen

		return f.next
	}

	r := newRun()
	r.gen = gen

	if wait {
		r.waiters++
	}

	f := &flight{cur: r}
	s.flights[j] = f
	ctx = context.WithoutCancel(ctx)
	s.wg.Go(func() { s.loop(ctx, j, f, r) })

	return r
}

func newRun() *run { return &run{done: make(chan struct{})} }

// loop runs r, then each rerun queued while the previous run was in
// progress, until none is queued or Close has begun. Close cancels the run
// in progress. A failure nobody waits for is logged.
func (s *Syncer) loop(ctx context.Context, j job, f *flight, r *run) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.closing:
			cancel()
		case <-ctx.Done():
		}
	}()

	for {
		var err error

		if j.embed {
			err = s.embedOnce(ctx)
		} else {
			err = s.syncOnce(ctx, j.connector, r.gen)

			if err == nil && s.onSynced != nil {
				s.onSynced(j.connector)
			}
		}

		s.mu.Lock()
		waiters := r.waiters
		s.mu.Unlock()

		if err != nil && waiters == 0 {
			s.logFailure(j, err)
		}

		r.err = err
		close(r.done)

		s.mu.Lock()
		next := f.next
		f.next = nil

		if next == nil || s.closed {
			delete(s.flights, j)
			s.mu.Unlock()

			if next != nil {
				s.logJob(s.log.Debug(), j).Msg("catalog rerun dropped at close")
				next.err = ErrClosed
				close(next.done)
			}

			return
		}

		r = next
		f.cur = next
		s.mu.Unlock()
	}
}

// logFailure logs a failed run of j that nobody waits for.
func (s *Syncer) logFailure(j job, err error) {
	switch {
	case errors.Is(err, ErrClosed):
		s.logJob(s.log.Debug(), j).Msg("catalog run stopped at close")
	case errors.Is(err, ErrRemoved), unknownConnector(err):
		s.logJob(s.log.Debug(), j).Msg("catalog sync of removed connector discarded")
	case j.embed:
		s.log.Error().Err(err).Msg("catalog embedding pass failed")
	default:
		s.log.Error().Err(err).Str("connector", j.connector).Msg("catalog sync failed")
	}
}

// logJob adds what identifies j to e.
func (s *Syncer) logJob(e *zerolog.Event, j job) *zerolog.Event {
	if j.embed {
		return e.Bool("embedding_pass", true)
	}

	return e.Str("connector", j.connector)
}

// syncOnce lists connector's tools and stores the outcome, the catalog
// they make on success and the sync record either way, in one store write,
// unless connector has left generation gen (Remove came first), the lister
// no longer has it, or Close has begun.
func (s *Syncer) syncOnce(ctx context.Context, connector string, gen uint64) error {
	start := s.now()
	tools, hidden, fetchErr := s.fetch(ctx, connector)

	if unknownConnector(fetchErr) {
		// The connector was removed; clearing its catalog is Remove's job.
		return fmt.Errorf("sync %s: %w", connector, fetchErr)
	}

	s.writing <- struct{}{}
	defer func() { <-s.writing }()
	s.mu.Lock()
	removed := s.gens[connector] != gen
	closed := s.closed
	s.mu.Unlock()

	if closed {
		return fmt.Errorf("sync %s: %w", connector, ErrClosed)
	}

	if removed {
		return fmt.Errorf("sync %s: %w", connector, ErrRemoved)
	}

	o := store.SyncOutcome{Connector: connector, At: s.now(), Tools: tools, Hidden: hidden}

	if fetchErr != nil {
		// Status displays the stored error (design §9).
		o.Error = redact.URLQueries(fetchErr.Error())
	}

	changed, applyErr := s.store.ApplySync(ctx, s.userID, o)

	if err := errors.Join(fetchErr, applyErr); err != nil {
		// applyErr may be a write that Close cancelled midway.
		return s.passErr(fmt.Errorf("sync %s: %w", connector, err))
	}

	s.log.Debug().Str("connector", connector).Int("tools", len(tools)).
		Int("hidden", len(hidden)).Int("changed", len(changed)).Int64("duration_ms", s.now().Sub(start).Milliseconds()).
		Msg("catalog synced")
	s.RequestEmbed(ctx)

	return nil
}

// fetch lists connector's tools as catalog entries, and the tools its
// policy hides, which it leaves out of them.
func (s *Syncer) fetch(ctx context.Context, connector string) ([]store.Tool, []store.HiddenTool, error) {
	listed, err := s.lister.ListTools(ctx, connector)

	if err != nil {
		return nil, nil, err
	}

	var policy config.ToolPolicy

	if s.policy != nil {
		policy = s.policy(connector)
	}

	tools := make([]store.Tool, 0, len(listed))
	exposed := make(map[string]bool, len(listed))

	var hidden []store.HiddenTool

	for _, lt := range listed {
		if lt == nil {
			continue
		}

		// A hidden tool skips store.NewTool's validation on purpose: it never
		// enters the catalog, and hiding a malformed tool (one without a
		// name, say) is how the rest of its connector still syncs.
		if class := config.ClassOf(lt.Annotations); !policy.Exposes(lt.Name, class) {
			hidden = append(hidden, store.HiddenTool{Name: lt.Name, Class: string(class)})

			continue
		}

		def, err := json.Marshal(lt)

		if err != nil {
			return nil, nil, fmt.Errorf("encode tool %s: %w", lt.Name, err)
		}

		t, err := store.NewTool(connector, def, nil)

		if err != nil {
			return nil, nil, fmt.Errorf("tool %s: %w", lt.Name, err)
		}

		// Checked here rather than left to the store, so a duplicate fails
		// the fetch and is recorded like any other failed sync: a list
		// ApplySync rejects writes nothing, not even the sync record.
		if exposed[t.Name] {
			return nil, nil, fmt.Errorf("duplicate tool %s", t.Name)
		}

		exposed[t.Name] = true
		t.ContentHash = contentHash(t)
		tools = append(tools, t)
	}

	return tools, hidden, nil
}
