package catalog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/catalog"
)

// The daemon's recovery waits with Await for the sync a connector's new
// session requested, instead of starting a second one (wayfindr-zgp.12).

func TestAwaitWaitsForTheRequestedSyncAndStartsNone(t *testing.T) {
	l := &lister{entered: make(chan struct{}, 10), gate: make(chan struct{})}
	l.set("linear", mcpTool("create_issue", "Creates an issue.", nil))
	s, st, user := newSyncer(t, l)
	ctx := context.Background()

	s.Request(ctx, "linear")
	<-l.entered // the requested sync is inside ListTools
	awaited := make(chan error, 1)
	go func() { awaited <- s.Await(ctx, "linear") }()
	waitFor(t, func() bool { return catalog.Waiters(s, "linear") == 1 })

	select {
	case err := <-awaited:
		t.Fatalf("Await = %v before the running sync finished, want it to wait", err)
	default:
	}

	close(l.gate)

	if err := <-awaited; err != nil {
		t.Errorf("Await: %v", err)
	}

	if got := catalogOf(t, st, user, "linear"); len(got) != 1 {
		t.Errorf("catalog once Await returned has %d tools, want 1: the sync it waited for wrote first", len(got))
	}

	if got := l.callCount(); got != 1 {
		t.Errorf("ListTools calls = %d, want 1: Await starts no sync", got)
	}
}

func TestAwaitWaitsForTheRerunQueuedBehindARunningSync(t *testing.T) {
	l := &lister{entered: make(chan struct{}, 10), gate: make(chan struct{})}
	l.set("linear", mcpTool("v1", "first", nil))
	s, st, user := newSyncer(t, l)
	ctx := context.Background()

	// A poll's sync is running when a new session requests another, which
	// queues a rerun: Await must wait for that rerun, which lists the new
	// session's tools.
	go func() { _ = s.Sync(ctx, "linear") }()
	<-l.entered
	l.set("linear", mcpTool("v2", "second", nil))
	s.Request(ctx, "linear")
	awaited := make(chan error, 1)
	go func() { awaited <- s.Await(ctx, "linear") }()
	waitFor(t, func() bool { return catalog.Waiters(s, "linear") == 1 })
	close(l.gate)

	if err := <-awaited; err != nil {
		t.Errorf("Await: %v", err)
	}

	var names []string

	for n := range catalogOf(t, st, user, "linear") {
		names = append(names, n)
	}

	if diff := cmp.Diff([]string{"v2"}, names); diff != "" {
		t.Errorf("catalog once Await returned (-want +got):\n%s", diff)
	}

	if got := l.callCount(); got != 2 {
		t.Errorf("ListTools calls = %d, want 2: the run and its rerun, none from Await", got)
	}
}

func TestAwaitWithNoSyncReturnsAtOnce(t *testing.T) {
	l := &lister{}
	s, _, _ := newSyncer(t, l)

	if err := s.Await(context.Background(), "linear"); err != nil {
		t.Errorf("Await = %v, want nil: there was nothing to wait for", err)
	}

	if got := l.callCount(); got != 0 {
		t.Errorf("ListTools calls = %d, want 0: Await starts no sync", got)
	}
}

func TestAwaitReturnsTheSyncsFailure(t *testing.T) {
	l := &lister{entered: make(chan struct{}, 10), gate: make(chan struct{})}
	l.err = errors.New("connection refused")
	s, _, _ := newSyncer(t, l)
	ctx := context.Background()

	s.Request(ctx, "linear")
	<-l.entered
	awaited := make(chan error, 1)
	go func() { awaited <- s.Await(ctx, "linear") }()
	waitFor(t, func() bool { return catalog.Waiters(s, "linear") == 1 })
	close(l.gate)

	if err := <-awaited; err == nil {
		t.Error("Await = nil, want the failed sync's error")
	}
}
