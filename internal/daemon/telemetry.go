package daemon

import (
	"context"
	"time"

	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/telemetry"
)

const (
	// reportFirstDelay and reportFirstSpread place the first usage report a
	// random time 5 to 60 minutes after startup, so a fleet of daemons
	// restarted together doesn't report at once, and a daemon started only
	// briefly sends nothing.
	reportFirstDelay  = 5 * time.Minute
	reportFirstSpread = 55 * time.Minute
	// reportEvery is how often the usage report is sent after the first.
	reportEvery = 24 * time.Hour
)

// countedCalls is the gateway's call log: it counts each call for the usage
// report, then records it in the call log. The count comes first, so a call
// log turned off (calls.retention = 0s) doesn't zero the report's counts.
type countedCalls struct {
	log    *callLog
	report *telemetry.Reporter
}

// Record implements gateway.Calls.
func (c countedCalls) Record(call store.Call) {
	c.report.Call(call.Client, call.Result)
	c.log.Record(call)
}

// keepTelemetry sends the usage report on its schedule until ctx ends or
// stop is called; stop waits for it to return. Each send does nothing
// while telemetry is off, which each config apply decides (ADR 0020). A
// report that fails is logged at debug level; its counts wait for the
// next.
func (d *Daemon) keepTelemetry(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	wait := reportFirstDelay + time.Duration(float64(reportFirstSpread)*d.jitter())

	go func() {
		defer close(done)

		for {
			t := d.clock.NewTimer(wait)

			select {
			case <-ctx.Done():
				t.Stop()

				return
			case <-t.C():
			}

			if err := d.telemetry.Send(ctx); err != nil {
				d.log.Debug().Err(err).Msg("usage report not sent")
			}

			wait = reportEvery
		}
	}()

	return func() {
		cancel()
		<-done
	}
}
