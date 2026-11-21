package daemon

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/store"
)

const (
	// callQueue is how many calls may wait for the writer. Beyond it, a
	// call is dropped rather than slowing the call_tool that made it.
	callQueue = 256
	// callWriteTimeout bounds one call-log write, so a stuck store holds
	// up the queue for at most this long per call.
	callWriteTimeout = time.Second
	// callFlushTimeout bounds writing the queued calls at shutdown.
	callFlushTimeout = 2 * time.Second
	// callPruneTimeout bounds one prune. The writer waits for it, so a
	// prune held up by a lock or a large first delete must not stall the
	// queue for long; one that times out deletes nothing, and the next
	// prune tries again.
	callPruneTimeout = 10 * time.Second
	// pruneEvery is how often calls older than the retention are deleted.
	pruneEvery = time.Hour
)

// callLog is the call log (GLOSSARY: Call log). The gateway hands it every
// call_tool call through Record, which never blocks; one writer stores the
// calls and, every pruneEvery, deletes those older than the retention.
// A retention of 0 turns the log off: nothing is recorded or pruned.
type callLog struct {
	store  store.Store
	userID string
	clock  Clock
	log    zerolog.Logger

	// retention is calls.retention in nanoseconds; a reload sets it.
	retention atomic.Int64
	queue     chan store.Call
	// dropped counts the calls dropped on a full queue since the writer
	// last reported them.
	dropped atomic.Int64
}

func newCallLog(st store.Store, userID string, clock Clock, retention time.Duration, log zerolog.Logger) *callLog {
	l := &callLog{store: st, userID: userID, clock: clock, log: log, queue: make(chan store.Call, callQueue)}
	l.setRetention(retention)

	return l
}

// Record implements gateway.Calls.
func (l *callLog) Record(c store.Call) {
	if l.Retention() <= 0 {
		return
	}

	select {
	case l.queue <- c:
	default:
		l.dropped.Add(1)
	}
}

// Retention is the current calls.retention.
func (l *callLog) Retention() time.Duration { return time.Duration(l.retention.Load()) }

// setRetention applies calls.retention, as on a reload. A shorter one
// takes effect for pruning at the next prune.
func (l *callLog) setRetention(d time.Duration) { l.retention.Store(int64(d)) }

// run writes the queued calls and prunes, once at the start and then
// every pruneEvery, until ctx ends. It then writes what is still queued,
// within callFlushTimeout, and returns.
func (l *callLog) run(ctx context.Context) {
	l.prune(ctx)
	// One timer across writes: a steady stream of calls must not keep
	// putting the prune off.
	timer := l.clock.NewTimer(pruneEvery)

	for {
		select {
		case c := <-l.queue:
			// Not ctx: when shutdown begins with calls queued, select may
			// still pick this case, and that call must be stored like the
			// ones flush writes. callWriteTimeout bounds the write.
			l.write(context.WithoutCancel(ctx), c)
		case <-timer.C():
			l.prune(ctx)
			timer = l.clock.NewTimer(pruneEvery)
		case <-ctx.Done():
			timer.Stop()
			l.flush(context.WithoutCancel(ctx))

			return
		}
	}
}

// flush writes the queued calls until the queue is empty or
// callFlushTimeout has passed; the rest are dropped.
func (l *callLog) flush(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, callFlushTimeout)
	defer cancel()

	for {
		select {
		case c := <-l.queue:
			if ctx.Err() != nil {
				l.dropped.Add(1)

				continue
			}

			l.write(ctx, c)
		default:
			l.reportDropped()

			return
		}
	}
}

func (l *callLog) write(ctx context.Context, c store.Call) {
	l.reportDropped()
	ctx, cancel := context.WithTimeout(ctx, callWriteTimeout)
	defer cancel()

	if err := l.store.RecordCall(ctx, c); err != nil {
		l.log.Warn().Err(err).Str("connector", c.Connector).Str("tool", c.Tool).Msg("record call")
	}
}

// reportDropped logs the calls dropped since the last report, if any, so
// a burst of drops is one log line.
func (l *callLog) reportDropped() {
	if n := l.dropped.Swap(0); n > 0 {
		l.log.Warn().Int64("dropped", n).Msg("call log full; calls not recorded")
	}
}

func (l *callLog) prune(ctx context.Context) {
	retention := l.Retention()

	if retention <= 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, callPruneTimeout)
	defer cancel()
	n, err := l.store.PruneCalls(ctx, l.userID, l.clock.Now().Add(-retention))

	if err != nil {
		l.log.Warn().Err(err).Msg("prune call log")

		return
	}

	if n > 0 {
		l.log.Debug().Int64("calls", n).Msg("pruned call log")
	}
}

// keepCallLog runs the call log's writer until ctx ends or stop is called;
// stop waits for it to write what is queued.
func (d *Daemon) keepCallLog(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		d.calls.run(ctx)
	}()

	return func() {
		cancel()
		<-done
	}
}
