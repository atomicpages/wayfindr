package catalog_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/catalog"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

func TestMain(m *testing.M) {
	fakeupstream.ServeStdioIfChild()
	os.Exit(m.Run())
}

// stdioConnector is a connector that runs a stdio fake.
func stdioConnector(t *testing.T, version string, opts ...fakeupstream.Option) config.Connector {
	t.Helper()
	cmd := fakeupstream.StdioCommand(t, version, opts...)
	c := config.Connector{Command: cmd.Path, Env: map[string]config.Template{}}

	for _, a := range cmd.Args[1:] {
		c.Args = append(c.Args, config.Template(a))
	}

	for _, kv := range cmd.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "WAYFINDR_FAKEUPSTREAM_CONFIG" {
			c.Env[k] = config.Template(v)
		}
	}

	return c
}

// namesOf returns the sorted tool names of connector's catalog.
func namesOf(t *testing.T, st store.Store, user, connector string) []string {
	t.Helper()
	var names []string

	for n := range catalogOf(t, st, user, connector) {
		names = append(names, n)
	}

	sort.Strings(names)

	return names
}

// waitForCatalog waits until connector's catalog holds exactly want.
func waitForCatalog(t *testing.T, st store.Store, user, connector string, want []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)

	for {
		got := namesOf(t, st, user, connector)

		if cmp.Equal(want, got) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("catalog of %s after 5s (-want +got):\n%s", connector, cmp.Diff(want, got))
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// TestSyncerFollowsTheUpstreamLifecycle wires the Syncer to a real
// upstream.Manager the way the daemon does (OnToolsChanged → Request;
// removal → Manager.Apply, then Syncer.Remove) and walks one catalog
// through add, list_changed, removal during a sync, and re-add.
func TestSyncerFollowsTheUpstreamLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })
	user, err := st.EnsureUser(ctx)

	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	// Started first, so they outlive the Manager (cleanups run
	// last-registered first).
	httpSrv := fakeupstream.StartStreamable(t, fakeupstream.V20260728, fakeupstream.WithListChanged(),
		fakeupstream.WithTools(mcpTool("list_issues", "Lists issues.", nil)))
	// Every tools/list on slow takes a while, so a sync of it is still
	// fetching when the test removes it.
	slowSrv := fakeupstream.StartStreamable(t, fakeupstream.V20251125, fakeupstream.WithDelay(300*time.Millisecond),
		fakeupstream.WithTools(mcpTool("search_code", "Searches code.", nil)))

	// The daemon's wiring and shutdown order: Manager.Close, then
	// Syncer.Close, then the store.
	var s *catalog.Syncer
	m := upstream.New(upstream.Options{
		Logger:         zerolog.New(zerolog.NewTestWriter(t)),
		OnToolsChanged: func(connector string) { s.Request(ctx, connector) },
	})
	s = catalog.New(catalog.Options{Lister: m, Store: st, UserID: user, Logger: zerolog.New(zerolog.NewTestWriter(t))})
	t.Cleanup(func() {
		if err := s.Close(ctx); err != nil {
			t.Errorf("Syncer.Close: %v", err)
		}
	})
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Errorf("Manager.Close: %v", err)
		}
	})

	var applied config.Connectors
	apply := func(set config.Connectors) {
		t.Helper()
		m.Apply(ctx, set, config.Diff(&config.Config{Connectors: applied}, &config.Config{Connectors: set}))
		applied = set
	}
	linear := config.Connector{URL: config.Template(httpSrv.URL)}
	files := stdioConnector(t, fakeupstream.V20250618)
	slow := config.Connector{URL: config.Template(slowSrv.URL)}

	// Add: every new session, HTTP as well as stdio, syncs its connector.
	apply(config.Connectors{"linear": linear, "files": files})
	waitForCatalog(t, st, user, "linear", []string{fakeupstream.ToolChangeTools, "list_issues"})
	waitForCatalog(t, st, user, "files", []string{fakeupstream.ToolEcho})

	// list_changed: the upstream adds a tool and the catalog follows.
	if _, err := m.Call(ctx, "linear", fakeupstream.ToolChangeTools, nil); err != nil {
		t.Fatalf("Call %s: %v", fakeupstream.ToolChangeTools, err)
	}

	waitForCatalog(t, st, user, "linear", []string{"added_1", fakeupstream.ToolChangeTools, "list_issues"})

	// Remove during a sync: slow's first session requested a sync, which is
	// still fetching; a Sync joins the rerun queued behind it. Remove
	// fences both off, and neither may write the catalog or a sync record.
	apply(config.Connectors{"linear": linear, "files": files, "slow": slow})
	synced := make(chan error, 1)
	go func() { synced <- s.Sync(ctx, "slow") }()
	waitFor(t, func() bool { return catalog.Waiters(s, "slow") == 1 })

	if err := s.Remove(ctx, "slow"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if err := <-synced; !errors.Is(err, catalog.ErrRemoved) {
		t.Errorf("Sync fenced off by Remove = %v, want %v", err, catalog.ErrRemoved)
	}

	if got := namesOf(t, st, user, "slow"); len(got) != 0 {
		t.Errorf("catalog of slow after Remove = %v, want none (ghost tools)", got)
	}

	if _, err := st.LastSync(ctx, user, "slow"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LastSync of slow after Remove: err = %v, want %v", err, store.ErrNotFound)
	}

	// Once the Manager has dropped slow too, a sync of it fails as
	// unknown_connector and records nothing.
	apply(config.Connectors{"linear": linear, "files": files})
	err = s.Sync(ctx, "slow")

	if e, ok := upstream.AsError(err); !ok || e.Kind != upstream.KindUnknownConnector {
		t.Errorf("Sync of a removed connector = %v, want unknown_connector", err)
	}

	if _, err := st.LastSync(ctx, user, "slow"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LastSync of slow after a sync of it failed: err = %v, want %v", err, store.ErrNotFound)
	}

	// Re-add: the first new session syncs it again.
	apply(config.Connectors{"linear": linear, "files": files, "slow": slow})
	waitFor(t, func() bool {
		_, err := st.LastSync(ctx, user, "slow")

		return err == nil
	})

	if diff := cmp.Diff([]string{"search_code"}, namesOf(t, st, user, "slow")); diff != "" {
		t.Errorf("catalog of slow after re-add (-want +got):\n%s", diff)
	}

	// Remove after a sync: slow's catalog and its last sync both go, so
	// status shows no stale sync for a connector that is no longer there.
	apply(config.Connectors{"linear": linear, "files": files})

	if err := s.Remove(ctx, "slow"); err != nil {
		t.Fatalf("Remove after re-add: %v", err)
	}

	if got := namesOf(t, st, user, "slow"); len(got) != 0 {
		t.Errorf("catalog of slow after the second Remove = %v, want none", got)
	}

	if _, err := st.LastSync(ctx, user, "slow"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LastSync of slow after the second Remove: err = %v, want %v", err, store.ErrNotFound)
	}

	// The other connectors were never touched.
	waitForCatalog(t, st, user, "files", []string{fakeupstream.ToolEcho})
}
