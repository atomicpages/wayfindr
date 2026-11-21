package upstream_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// TestCallbacksMayCallBackIntoTheManager has each callback use the
// connector it was called for, and call Apply, while Apply is connecting
// it. The callbacks run with no lock held (Options), so none of this may
// deadlock.
func TestCallbacksMayCallBackIntoTheManager(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	var (
		m        *upstream.Manager
		errs     = make(chan error, 8)
		onReady  sync.Once
		onTools  sync.Once
		reentry  = make(chan struct{}, 2)
		reenters = func(f func() error) {
			errs <- f()
			reentry <- struct{}{}
		}
	)
	m, _ = newManager(t, nil, func(o *upstream.Options) {
		o.OnStateChanged = func(connector string, s upstream.State) {
			if s != upstream.StateReady {
				return
			}

			onReady.Do(func() {
				reenters(func() error {
					_, err := m.Call(context.Background(), connector, fakeupstream.ToolEcho, nil)

					return err
				})
			})
		}
		o.OnToolsChanged = func(connector string) {
			onTools.Do(func() {
				reenters(func() error {
					_ = m.Status()
					m.Apply(context.Background(), nil, config.Changes{})

					if err := m.Retry(context.Background(), connector); err != nil {
						return err
					}

					_, err := m.ListTools(context.Background(), connector)

					return err
				})
			})
		}
	})

	applied := make(chan struct{})
	go func() {
		defer close(applied)
		apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
	}()

	select {
	case <-applied:
	case <-time.After(10 * time.Second):
		t.Fatal("Apply did not return within 10s: a callback that called back into the manager deadlocked")
	}

	for range 2 {
		select {
		case <-reentry:
		case <-time.After(10 * time.Second):
			t.Fatal("a callback was not called, or did not return, within 10s")
		}

		if err := <-errs; err != nil {
			t.Errorf("call from a callback: %v", err)
		}
	}
}
