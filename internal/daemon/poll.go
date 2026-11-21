package daemon

import (
	"context"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// The poll of HTTP connectors (design §7.8 step 3). A connector's next poll
// is due one interval after its last, or after the reload that added,
// changed, or re-timed it: refresh_interval, unless its last tools/list
// carried a cache hint, within a jitter that keeps a fleet of connectors
// from polling in lockstep.
const (
	// pollJitter is the spread of an unhinted interval: ±10% of
	// refresh_interval.
	pollJitter = 10 // percent
	// minHintedPoll and maxHintedPoll bound a cache hint before it is used
	// as an interval. The floor keeps a tiny or mistaken ttlMs from turning
	// the poll into a busy loop against the upstream and the store; the
	// ceiling has a connector that claims a long freshness still checked
	// daily, so a wrong hint can't leave its catalog stale for long.
	minHintedPoll = time.Minute
	maxHintedPoll = 24 * time.Hour
)

// Clock is the time source of the poll. Options.Clock substitutes one in
// tests; nil means the system clock.
type Clock interface {
	Now() time.Time
	// NewTimer returns a timer that fires once d has passed.
	NewTimer(d time.Duration) Timer
}

// Timer is a Clock's one-shot timer, as *time.Timer.
type Timer interface {
	C() <-chan time.Time
	// Stop prevents the timer from firing; false if it already has.
	Stop() bool
}

type systemClock struct{}

func (systemClock) Now() time.Time                 { return time.Now() }
func (systemClock) NewTimer(d time.Duration) Timer { return systemTimer{time.NewTimer(d)} }

type systemTimer struct{ t *time.Timer }

func (t systemTimer) C() <-chan time.Time { return t.t.C }
func (t systemTimer) Stop() bool          { return t.t.Stop() }

// pollInterval is how long after a poll the next one of a connector is due,
// for refresh_interval base, the connector's cache hint (0 for none), and a
// jitter draw j in [0, 1).
//
// Unhinted, it is base ±10%. A hint replaces base, bounded to
// [minHintedPoll, maxHintedPoll], and the jitter only lengthens it, to up to
// 20% more: a 2026-07-28 session serves tools/list from the SDK's cache
// until the hint runs out, so a poll before then would fetch nothing new.
func pollInterval(base, hint time.Duration, j float64) time.Duration {
	if hint <= 0 {
		spread := base * pollJitter / 100

		return base - spread + time.Duration(float64(2*spread)*j)
	}

	hint = min(max(hint, minHintedPoll), maxHintedPoll)

	return hint + time.Duration(float64(2*hint*pollJitter/100)*j)
}

// poller syncs every HTTP connector in ready or unreachable each interval
// (design §7.8 step 3). Stdio connectors are never polled: their tools come
// with each new child, which syncs. A connector in starting, needs_auth, or
// auth_error is skipped: it syncs when it recovers, since recovery opens a
// new session. The state is read at the poll, not when it was scheduled.
//
// Reload tells it of each config (apply); run polls until its context ends.
type poller struct {
	clock  Clock
	jitter func() float64
	status func() []upstream.ConnectorStatus
	sync   func(ctx context.Context, connector string) error
	log    zerolog.Logger

	// changed wakes run after apply has rescheduled.
	changed chan struct{}

	mu       sync.Mutex
	interval time.Duration
	// due is when each HTTP connector's next poll is due; it lists no other
	// connector.
	due map[string]time.Time
	// hints is the cache hint each connector's due time was set from (0 for
	// none), so a sync that reads another can re-time it (synced).
	hints map[string]time.Duration
	// gen counts the reschedules of apply and synced. A poll that finishes
	// after one of them rescheduled its connector leaves that schedule
	// alone.
	gen  uint64
	gens map[string]uint64
}

func newPoller(clock Clock, jitter func() float64, status func() []upstream.ConnectorStatus,
	sync func(context.Context, string) error, log zerolog.Logger,
) *poller {
	if clock == nil {
		clock = systemClock{}
	}

	if jitter == nil {
		jitter = rand.Float64
	}

	return &poller{
		clock:   clock,
		jitter:  jitter,
		status:  status,
		sync:    sync,
		log:     log,
		changed: make(chan struct{}, 1),
		due:     map[string]time.Time{},
		hints:   map[string]time.Duration{},
		gens:    map[string]uint64{},
	}
}

// hintsNow is each connector's cache hint as its last tools/list left it.
func (p *poller) hintsNow() map[string]time.Duration {
	hints := map[string]time.Duration{}

	for _, s := range p.status() {
		hints[s.Connector] = s.ToolsTTL
	}

	return hints
}

// schedule makes connector's next poll due one interval, from hint, after
// now. The caller holds mu.
func (p *poller) schedule(connector string, now time.Time, hint time.Duration) {
	p.gen++
	p.gens[connector] = p.gen
	p.hints[connector] = hint
	p.due[connector] = now.Add(pollInterval(p.interval, hint, p.jitter()))
}

// synced follows a catalog sync of connector that succeeded, whoever
// requested it (catalog.Options.OnSynced). If the sync's tools/list read a
// cache hint other than the one the connector's poll was scheduled from,
// the next poll is due one interval, from the new hint, after it. So a new
// connector, scheduled by the reload before its connect's sync finished,
// first polls when the hint that sync read runs out, not after
// refresh_interval (wayfindr-t08.6). Another sync leaves the schedule
// alone, as a reload leaves an unchanged connector's.
func (p *poller) synced(connector string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// The hint is read under mu, so this and apply, which reads it under mu
	// too, see the sync in one order.
	hint := p.hintsNow()[connector]

	if _, ok := p.due[connector]; !ok || p.hints[connector] == hint {
		return
	}

	p.schedule(connector, p.clock.Now(), hint)
	p.wake()
}

// wake has run re-read the schedule.
func (p *poller) wake() {
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

// apply follows a reload to cfg, whose changes from the last config are ch.
// Each HTTP connector added, changed, or turned from stdio to HTTP is due
// one interval from now: its connect has just synced it. A changed
// refresh_interval reschedules every connector that way. Removed connectors,
// and those that became stdio, leave the schedule; the rest keep theirs.
func (p *poller) apply(cfg *config.Config, ch config.Changes) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock.Now()
	retimed := cfg.Index.RefreshInterval.Duration != p.interval
	p.interval = cfg.Index.RefreshInterval.Duration
	rebuilt := slices.Concat(ch.Added, ch.URLChanged, ch.Changed)

	for name := range p.due {
		if c, ok := cfg.Connectors[name]; !ok || c.IsStdio() {
			delete(p.due, name)
			delete(p.hints, name)
			delete(p.gens, name)
		}
	}

	// A rebuilt connector's hint was cleared with its old upstream; one
	// its connect's sync has already read is used here, and one read
	// later re-times it (synced).
	hints := p.hintsNow()

	for name, c := range cfg.Connectors {
		if c.IsStdio() {
			continue
		}

		if _, ok := p.due[name]; ok && !retimed && !slices.Contains(rebuilt, name) {
			continue
		}

		p.schedule(name, now, hints[name])
	}

	p.wake()
}

// run polls each connector when it is due, until ctx ends. It waits for the
// polls it started before it returns.
func (p *poller) run(ctx context.Context) {
	for {
		var fire <-chan time.Time
		var timer Timer

		if next, ok := p.next(); ok {
			timer = p.clock.NewTimer(max(next.Sub(p.clock.Now()), 0))
			fire = timer.C()
		}

		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}

			return
		case <-p.changed:
			if timer != nil {
				timer.Stop()
			}
		case <-fire:
			p.pollDue(ctx)
		}
	}
}

// next is when the earliest poll is due; false when no connector is
// scheduled.
func (p *poller) next() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var first time.Time

	for _, at := range p.due {
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}

	return first, !first.IsZero()
}

// pollDue syncs every connector now due that is ready or unreachable, in
// parallel, and waits for them; then schedules each due connector's next
// poll, from its cache hint as the sync left it.
func (p *poller) pollDue(ctx context.Context) {
	p.mu.Lock()
	now := p.clock.Now()
	due := map[string]uint64{}

	for name, at := range p.due {
		if !at.After(now) {
			due[name] = p.gens[name]
		}
	}

	p.mu.Unlock()

	var wg sync.WaitGroup

	for _, s := range p.status() {
		if _, ok := due[s.Connector]; !ok {
			continue
		}

		switch s.State {
		case upstream.StateReady, upstream.StateUnreachable:
		case upstream.StateStarting, upstream.StateNeedsAuth, upstream.StateAuthError:
			// Each syncs when it recovers, which opens a new session.
			p.log.Debug().Str("connector", s.Connector).Str("state", string(s.State)).Msg("poll skipped")
			continue
		}

		wg.Go(func() {
			p.log.Debug().Str("connector", s.Connector).Msg("polling connector")

			// The syncer logs only the failures nobody waits for, so the
			// poll logs its own; the next poll tries again.
			if err := p.sync(ctx, s.Connector); err != nil && ctx.Err() == nil {
				p.log.Warn().Err(err).Str("connector", s.Connector).Msg("poll sync failed")
			}
		})
	}

	wg.Wait()

	if ctx.Err() != nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	hints := p.hintsNow()
	now = p.clock.Now()

	for name, gen := range due {
		if p.gens[name] != gen {
			continue // removed, or rescheduled meanwhile by a reload or a new hint
		}

		p.schedule(name, now, hints[name])
	}
}

// pollConnectors runs the poll until the returned stop is called; stop
// waits for it, including the syncs it is waiting on, which its cancel
// ends.
func (d *Daemon) pollConnectors(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.poller.run(ctx)
	}()

	return func() {
		cancel()
		<-done
	}
}
