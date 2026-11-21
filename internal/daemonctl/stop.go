package daemonctl

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/atomicpages/wayfindr/internal/daemonlock"
)

// stopTimeout is how long Stop waits for the daemon to let go of the lock:
// past its 10s drain of requests in flight, with time to stop its stdio
// connectors and close the store.
const stopTimeout = 15 * time.Second

// ErrNotRunning is Stop's error when no daemon holds the daemon lock.
var ErrNotRunning = errors.New("daemon is not running")

// Stop asks the daemon that holds the lock at lockFile to shut down, as
// SIGTERM does (on Windows, which has none, it is killed), and returns once
// the OS has dropped its lock. It returns the daemon it stopped, or
// ErrNotRunning when no daemon holds the lock.
//
// The lock names the daemon on this data directory whatever its
// server.listen, so Stop finds one that a config change left on an old
// address. A held lock means its pid is the live holder's, not a reused one.
func Stop(ctx context.Context, lockFile string) (daemonlock.Holder, error) {
	h, held, err := daemonlock.Probe(lockFile)

	if err != nil {
		return daemonlock.Holder{}, err
	}

	if !held {
		return daemonlock.Holder{}, ErrNotRunning
	}

	if h.PID == 0 {
		return h, errors.New("the daemon is starting and has not written its pid yet: try again in a moment")
	}

	if err := terminate(h.PID); err != nil {
		return h, fmt.Errorf("stop daemon (pid %d): %w", h.PID, err)
	}

	deadline := time.NewTimer(stopTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return h, ctx.Err()
		case <-deadline.C:
			return h, fmt.Errorf("the daemon (pid %d) did not stop within %s", h.PID, stopTimeout)
		case <-tick.C:
			if _, held, err := daemonlock.Probe(lockFile); err == nil && !held {
				return h, nil
			}
		}
	}
}
