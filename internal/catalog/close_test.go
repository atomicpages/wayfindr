package catalog_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/catalog"
	"github.com/atomicpages/wayfindr/internal/store"
)

// The tests in this file are the daemon's shutdown seen from the catalog
// (design §7.4, §7.8 step 4): Close has a budget, and once it returns, even
// with runs still going because their lister or embedder ignores the
// cancellation, the daemon closes the store under them. So nothing may
// write to the store after Close returns (wayfindr-t08.7).

// shutStore is a store.Store whose catalog writes fail the test once shut
// is set, as the daemon closing the store would make them fail. If gate is
// set, ApplySync announces itself on entered and then holds until gate
// is closed: a write in progress, which ends on its own.
type shutStore struct {
	store.Store
	t       *testing.T
	shut    atomic.Bool
	entered chan struct{}
	gate    chan struct{}
}

func (s *shutStore) check(op string) {
	if s.shut.Load() {
		s.t.Errorf("%s after Close returned and the store was closed", op)
	}
}

func (s *shutStore) ApplySync(ctx context.Context, userID string, o store.SyncOutcome) ([]string, error) {
	s.check("ApplySync")

	if s.gate != nil {
		s.entered <- struct{}{}
		<-s.gate
	}

	defer s.check("ApplySync, still writing,")

	return s.Store.ApplySync(ctx, userID, o)
}

func (s *shutStore) SetEmbeddings(ctx context.Context, userID, modelID string, vecs []store.ToolVector) error {
	s.check("SetEmbeddings")

	return s.Store.SetEmbeddings(ctx, userID, modelID, vecs)
}

func (s *shutStore) DeleteCatalog(ctx context.Context, userID, connector string) error {
	s.check("DeleteCatalog")

	return s.Store.DeleteCatalog(ctx, userID, connector)
}

// closeMargin is how far past its budget Close may return: scheduling, not
// waiting on a run.
const closeMargin = time.Second

// A run whose lister or embedder ignores the cancellation can't hold Close
// past its budget: Close gives up on it, and the run, when it does return,
// writes nothing, so the store can be closed as soon as Close returns.
func TestCloseAbandonsARunThatIgnoresCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		// start starts a run that blocks in s's lister or embedder, and
		// returns once it has.
		start func(t *testing.T, s *catalog.Syncer, l *lister, e *embedder)
	}{
		{"sync", func(_ *testing.T, s *catalog.Syncer, l *lister, _ *embedder) {
			l.mu.Lock()
			l.entered, l.gate, l.deaf = make(chan struct{}, 1), make(chan struct{}), true
			l.mu.Unlock()
			s.Request(context.Background(), "linear")
			<-l.entered
		}},
		{"embedding pass", func(t *testing.T, s *catalog.Syncer, _ *lister, e *embedder) {
			e.mu.Lock()
			e.entered, e.gate, e.deaf = make(chan struct{}, 1), make(chan struct{}), true
			e.mu.Unlock()

			// The sync stores the entry, then starts the pass that embeds it.
			if err := s.Sync(context.Background(), "linear"); err != nil {
				t.Fatalf("Sync: %v", err)
			}

			<-e.entered
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &lister{}
			l.set("linear", mcpTool("list_issues", "Lists issues.", nil))
			e := &embedder{model: "m1", version: 1}
			st := &shutStore{Store: openStore(t), t: t}
			s, _, _ := newSyncerWith(t, catalog.Options{Lister: l, Embedder: e, Store: st})
			tc.start(t, s, l, e)

			const budget = 100 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			began := time.Now()

			if err := s.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("Close with a run that ignores cancellation = %v, want %v", err, context.DeadlineExceeded)
			}

			if took := time.Since(began); took > budget+closeMargin {
				t.Errorf("Close took %v with a %v budget", took, budget)
			}

			st.shut.Store(true)

			// The run returns once released, and writes nothing.
			l.mu.Lock()

			if l.gate != nil {
				close(l.gate)
			}

			l.mu.Unlock()
			e.mu.Lock()

			if e.gate != nil {
				close(e.gate)
			}

			e.mu.Unlock()

			if err := s.Close(context.Background()); err != nil {
				t.Errorf("Close once the run was released: %v", err)
			}
		})
	}
}

// A write in progress when Close's budget runs out is waited for: writes
// end on their own, and the store must not close under one.
func TestCloseWaitsForAWriteInProgress(t *testing.T) {
	l := &lister{}
	l.set("linear", mcpTool("list_issues", "Lists issues.", nil))
	st := &shutStore{Store: openStore(t), t: t, entered: make(chan struct{}, 1), gate: make(chan struct{})}
	s, _, _ := newSyncerWith(t, catalog.Options{Lister: l, Store: st})

	synced := make(chan error, 1)
	go func() { synced <- s.Sync(context.Background(), "linear") }()
	<-st.entered

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- s.Close(ctx) }()

	select {
	case err := <-closed:
		t.Errorf("Close returned %v while a write was in progress", err)
		// The daemon would close the store now, under the write.
		st.shut.Store(true)
		close(st.gate)
	case <-time.After(200 * time.Millisecond):
		close(st.gate)

		if err := <-closed; err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Close = %v, want nil or %v", err, context.DeadlineExceeded)
		}

		st.shut.Store(true)
	}

	// The sync wrote before Close returned, or, if Close's cancel reached the
	// write, gave up: either way it has nothing left to write.
	if err := <-synced; err != nil && !errors.Is(err, catalog.ErrClosed) {
		t.Errorf("Sync = %v, want nil or %v", err, catalog.ErrClosed)
	}
}
