package upstream_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// closeBudget is the ctx the hanging-DELETE tests give a close, and
// closeBound how long they let it take: well under the SDK's own 5s bound
// on the DELETE, so only the ctx can have cut it short.
const (
	closeBudget = 200 * time.Millisecond
	closeBound  = 2 * time.Second
)

// readyOnHangingDelete returns a manager with one connector, "c", ready on
// a stateful Streamable HTTP upstream that never answers the session
// DELETE; each with func adjusts the manager's options. The goroutines
// running before it are ignored by the leak check it registers, which runs
// once every other cleanup has.
func readyOnHangingDelete(t *testing.T, with ...func(*upstream.Options)) *upstream.Manager {
	t.Helper()
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125, fakeupstream.WithHangingDelete())
	m, _ := newManager(t, nil, with...)
	apply(t.Context(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})

	if got := statusOf(t, m, "c").State; got != upstream.StateReady {
		t.Fatalf("state = %s, want ready", got)
	}

	return m
}

func TestCloseGivesUpOnHangingSessionDeleteWhenCtxEnds(t *testing.T) {
	m := readyOnHangingDelete(t)

	ctx, cancel := context.WithTimeout(t.Context(), closeBudget)
	defer cancel()
	start := time.Now()
	err := m.Close(ctx)

	if took := time.Since(start); took > closeBound {
		t.Errorf("Close took %v, want under %v", took, closeBound)
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// A Drop stuck on the DELETE holds the connector's lock, which Close must
// take to close it: Close still returns once its ctx ends.
func TestCloseGivesUpOnDropHangingOnSessionDelete(t *testing.T) {
	deleting := make(chan struct{}, 1)
	m := readyOnHangingDelete(t, func(o *upstream.Options) {
		o.AuthTransport = func(_ config.Connector, next http.RoundTripper) http.RoundTripper {
			return roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodDelete {
					select {
					case deleting <- struct{}{}:
					default:
					}
				}

				return next.RoundTrip(r)
			})
		}
	})
	dropped := make(chan error, 1)
	go func() { dropped <- m.Drop(t.Context(), "c") }()
	<-deleting

	ctx, cancel := context.WithTimeout(t.Context(), closeBudget)
	defer cancel()
	start := time.Now()
	err := m.Close(ctx)

	if took := time.Since(start); took > closeBound {
		t.Errorf("Close took %v, want under %v", took, closeBound)
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close = %v, want it to wrap context.DeadlineExceeded", err)
	}

	if err := <-dropped; err != nil {
		t.Errorf("Drop = %v, want nil", err)
	}
}

func TestDropGivesUpOnHangingSessionDeleteWhenCtxEnds(t *testing.T) {
	m := readyOnHangingDelete(t)

	ctx, cancel := context.WithTimeout(t.Context(), closeBudget)
	defer cancel()
	start := time.Now()

	if err := m.Drop(ctx, "c"); err != nil {
		t.Errorf("Drop = %v, want nil", err)
	}

	if took := time.Since(start); took > closeBound {
		t.Errorf("Drop took %v, want under %v", took, closeBound)
	}
}
