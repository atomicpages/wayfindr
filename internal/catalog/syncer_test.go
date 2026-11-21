package catalog_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/catalog"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// lister is a stub catalog.ToolLister serving a settable tool list per
// connector. If gate is set, each ListTools call announces itself on entered
// and then waits for gate to be closed, or for its ctx to end unless deaf
// is set.
//
// It stands in for fakeupstream behind an upstream.Manager because the
// coalescing and Close tests must hold a sync inside ListTools, count the
// calls, and fail or change the list on demand, none of which the fake
// exposes. A test that needs none of that uses the real path; see
// TestSyncStoresToolsWithContentHashOfEmbedText.
type lister struct {
	mu      sync.Mutex
	tools   map[string][]*mcp.Tool
	err     error
	calls   int
	last    time.Time // when the latest ListTools call began
	entered chan struct{}
	gate    chan struct{}
	deaf    bool // the gate wait ignores ctx, as a stuck upstream might
}

func (l *lister) set(connector string, tools ...*mcp.Tool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.tools == nil {
		l.tools = map[string][]*mcp.Tool{}
	}

	l.tools[connector] = tools
}

func (l *lister) ListTools(ctx context.Context, connector string) ([]*mcp.Tool, error) {
	l.mu.Lock()
	l.calls++
	l.last = time.Now()
	entered, gate, deaf := l.entered, l.gate, l.deaf
	l.mu.Unlock()

	if entered != nil {
		entered <- struct{}{}
	}

	if gate != nil && deaf {
		<-gate
	}

	if gate != nil && !deaf {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.err != nil {
		return nil, l.err
	}

	return l.tools[connector], nil
}

func (l *lister) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.calls
}

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func newSyncer(t *testing.T, l catalog.ToolLister) (*catalog.Syncer, store.Store, string) {
	t.Helper()

	return newSyncerWith(t, catalog.Options{Lister: l})
}

// newSyncerWith is newSyncer with opts, whose UserID, Logger, and Now it
// fills in, and its Store unless set.
func newSyncerWith(t *testing.T, opts catalog.Options) (*catalog.Syncer, store.Store, string) {
	t.Helper()
	ctx := context.Background()
	st := opts.Store

	if st == nil {
		st = openStore(t)
	}

	user, err := st.EnsureUser(ctx)

	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	opts.Store, opts.UserID, opts.Logger = st, user, zerolog.Nop()
	opts.Now = func() time.Time { return t0 }
	s := catalog.New(opts)
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return s, st, user
}

// openStore opens a SQLite store in a temporary directory, closed when the
// test ends.
func openStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	return st
}

func mcpTool(name, description string, params map[string]string) *mcp.Tool {
	props := map[string]any{}

	for p, d := range params {
		props[p] = map[string]any{"type": "string", "description": d}
	}

	return &mcp.Tool{
		Name:        name,
		Description: description,
		InputSchema: map[string]any{"type": "object", "properties": props},
	}
}

func sha(text string) []byte {
	sum := sha256.Sum256([]byte(text))

	return sum[:]
}

// catalogOf returns the connector's catalog as name → content hash. Nothing
// is embedded yet, so every entry is pending under any model.
func catalogOf(t *testing.T, st store.Store, user, connector string) map[string][]byte {
	t.Helper()
	tools, err := st.PendingEmbeddings(context.Background(), user, "any-model", 1000)

	if err != nil {
		t.Fatalf("PendingEmbeddings: %v", err)
	}

	got := map[string][]byte{}

	for _, tl := range tools {
		if tl.Connector == connector {
			got[tl.Name] = tl.ContentHash
		}
	}

	return got
}

func TestSyncStoresToolsWithContentHashOfEmbedText(t *testing.T) {
	ctx := context.Background()
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125, fakeupstream.WithTools(
		mcpTool("create_issue", "Creates an issue.", map[string]string{"title": "Issue title"}),
	))
	m := upstream.New(upstream.Options{Logger: zerolog.Nop()})
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Errorf("Manager.Close: %v", err)
		}
	})
	cfg := &config.Config{Connectors: config.Connectors{"linear": {Name: "linear", URL: config.Template(srv.URL)}}}
	m.Apply(ctx, cfg.Connectors, config.Diff(nil, cfg))
	s, st, user := newSyncer(t, m)

	if err := s.Sync(ctx, "linear"); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	got, err := st.GetTool(ctx, user, "linear", "create_issue")

	if err != nil {
		t.Fatalf("GetTool: %v", err)
	}

	want := store.Tool{
		Connector:   "linear",
		Name:        "create_issue",
		Description: "Creates an issue.",
		Params:      []store.Param{{Name: "title", Description: "Issue title"}},
		ContentHash: sha("linear.create_issue\n\nCreates an issue.\ntitle: Issue title\n"),
	}
	got.Definition = nil

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetTool (-want +got):\n%s", diff)
	}

	rec, err := st.LastSync(ctx, user, "linear")

	if err != nil {
		t.Fatalf("LastSync: %v", err)
	}

	if diff := cmp.Diff(store.SyncRecord{At: t0}, rec, cmp.Comparer(time.Time.Equal)); diff != "" {
		t.Errorf("LastSync (-want +got):\n%s", diff)
	}
}

func TestSyncReconcilesUpstreamChanges(t *testing.T) {
	l := &lister{}
	keep := mcpTool("list_issues", "Lists issues.", map[string]string{"team": "Team key"})
	l.set("linear",
		keep,
		mcpTool("create_issue", "Creates an issue.", nil),
		mcpTool("delete_issue", "Deletes an issue.", nil),
	)
	s, st, user := newSyncer(t, l)
	ctx := context.Background()

	if err := s.Sync(ctx, "linear"); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	before := catalogOf(t, st, user, "linear")

	l.set("linear",
		keep, // unchanged
		mcpTool("create_issue", "Creates a new issue.", nil), // changed
		mcpTool("archive_issue", "Archives an issue.", nil),  // added
		// delete_issue removed
	)

	if err := s.Sync(ctx, "linear"); err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	want := map[string][]byte{
		"list_issues":   sha("linear.list_issues\n\nLists issues.\nteam: Team key\n"),
		"create_issue":  sha("linear.create_issue\n\nCreates a new issue.\n"),
		"archive_issue": sha("linear.archive_issue\n\nArchives an issue.\n"),
	}
	after := catalogOf(t, st, user, "linear")

	if diff := cmp.Diff(want, after); diff != "" {
		t.Errorf("catalog after resync (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff(before["list_issues"], after["list_issues"]); diff != "" {
		t.Errorf("unchanged tool's hash changed (-before +after):\n%s", diff)
	}
}

// TestSyncLeavesHiddenToolsOutOfTheCatalog: the tools the connector's
// policy hides never reach the catalog, the sync record lists them with
// their class in the upstream's order, and a sync under a changed policy
// brings them back.
func TestSyncLeavesHiddenToolsOutOfTheCatalog(t *testing.T) {
	no := false
	annotated := func(name string, a *mcp.ToolAnnotations) *mcp.Tool {
		tl := mcpTool(name, "Does "+name+".", nil)
		tl.Annotations = a

		return tl
	}
	l := &lister{}
	l.set("gmail",
		annotated("get_thread", &mcp.ToolAnnotations{ReadOnlyHint: true}),
		annotated("create_draft", &mcp.ToolAnnotations{DestructiveHint: &no}),
		annotated("send_message", &mcp.ToolAnnotations{DestructiveHint: &no}),
		annotated("delete_label", &mcp.ToolAnnotations{ReadOnlyHint: true}),
		annotated("purge", nil),
	)
	policy := config.ToolPolicy{"@read", "create_draft", "!delete_*"}
	s, st, user := newSyncerWith(t, catalog.Options{Lister: l, Policy: func(c string) config.ToolPolicy {
		if c != "gmail" {
			t.Errorf("Policy(%q), want gmail", c)
		}

		return policy
	}})
	ctx := context.Background()

	if err := s.Sync(ctx, "gmail"); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if diff := cmp.Diff([]string{"create_draft", "get_thread"}, slices.Sorted(maps.Keys(catalogOf(t, st, user, "gmail")))); diff != "" {
		t.Errorf("catalog (-want +got):\n%s", diff)
	}

	rec, err := st.LastSync(ctx, user, "gmail")

	if err != nil {
		t.Fatalf("LastSync: %v", err)
	}

	wantHidden := []store.HiddenTool{
		{Name: "send_message", Class: "write"},
		{Name: "delete_label", Class: "read"},
		{Name: "purge", Class: "destructive"},
	}

	if diff := cmp.Diff(wantHidden, rec.Hidden); diff != "" {
		t.Errorf("LastSync.Hidden (-want +got):\n%s", diff)
	}

	policy = nil

	if err := s.Sync(ctx, "gmail"); err != nil {
		t.Fatalf("Sync after the policy changed: %v", err)
	}

	if got := catalogOf(t, st, user, "gmail"); len(got) != 5 {
		t.Errorf("catalog after the policy was removed has %d tools, want 5", len(got))
	}

	if rec, err := st.LastSync(ctx, user, "gmail"); err != nil || rec.Hidden != nil {
		t.Errorf("LastSync after the policy was removed = %+v, %v; want no hidden tools", rec, err)
	}
}

// A malformed tool (here, one without a name) fails the sync while the
// connector's policy exposes it, and doesn't once the policy hides it: a
// hidden tool never reaches the catalog, so hiding a broken tool is how the
// rest of its connector syncs. The sync record lists it as hidden. A !glob
// that matches an empty name matches every name, so the ! case hides
// get_thread too.
func TestSyncSkipsAMalformedHiddenTool(t *testing.T) {
	tests := []struct {
		name        string
		policy      config.ToolPolicy
		wantErr     bool
		wantCatalog []string
		wantHidden  []store.HiddenTool
	}{
		{"exposed, the sync fails", nil, true, nil, nil},
		{"hidden by a ! entry", config.ToolPolicy{"!*"}, false, []string{},
			[]store.HiddenTool{{Name: "get_thread", Class: "read"}, {Name: "", Class: "destructive"}}},
		{"hidden by a class", config.ToolPolicy{"@read"}, false, []string{"get_thread"},
			[]store.HiddenTool{{Name: "", Class: "destructive"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			read := mcpTool("get_thread", "Gets a thread.", nil)
			read.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true}
			l := &lister{}
			l.set("gmail", read, mcpTool("", "Has no name.", nil))
			s, st, user := newSyncerWith(t, catalog.Options{Lister: l, Policy: func(string) config.ToolPolicy { return tt.policy }})
			ctx := context.Background()
			err := s.Sync(ctx, "gmail")

			if tt.wantErr {
				if err == nil {
					t.Fatal("Sync succeeded, want it to fail on the tool without a name")
				}

				if got := catalogOf(t, st, user, "gmail"); len(got) != 0 {
					t.Errorf("catalog after the failed sync has %d tools, want none", len(got))
				}

				if rec, err := st.LastSync(ctx, user, "gmail"); err != nil || rec.Error == "" {
					t.Errorf("LastSync = %+v, %v; want the failure recorded", rec, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("Sync: %v", err)
			}

			if diff := cmp.Diff(tt.wantCatalog, slices.Sorted(maps.Keys(catalogOf(t, st, user, "gmail"))), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("catalog (-want +got):\n%s", diff)
			}

			rec, err := st.LastSync(ctx, user, "gmail")

			if err != nil {
				t.Fatalf("LastSync: %v", err)
			}

			if diff := cmp.Diff(tt.wantHidden, rec.Hidden); diff != "" {
				t.Errorf("LastSync.Hidden (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSyncFailureIsRecordedAndKeepsCatalog(t *testing.T) {
	l := &lister{}
	l.set("linear", mcpTool("list_issues", "Lists issues.", nil))
	s, st, user := newSyncer(t, l)
	ctx := context.Background()

	if err := s.Sync(ctx, "linear"); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	upstreamErr := errors.New("connection refused")
	l.mu.Lock()
	l.err = upstreamErr
	l.mu.Unlock()

	if err := s.Sync(ctx, "linear"); !errors.Is(err, upstreamErr) {
		t.Fatalf("Sync error = %v, want it to wrap %v", err, upstreamErr)
	}

	if got := catalogOf(t, st, user, "linear"); len(got) != 1 {
		t.Errorf("catalog after failed sync has %d tools, want the 1 from before", len(got))
	}

	rec, err := st.LastSync(ctx, user, "linear")

	if err != nil {
		t.Fatalf("LastSync: %v", err)
	}

	if rec.Error == "" {
		t.Error("LastSync.Error is empty after a failed sync")
	}
}

// The stored sync error is shown by status, so a secret in an upstream
// URL's query never reaches it (design §9).
func TestSyncRecordsItsFailureWithURLQueriesRedacted(t *testing.T) {
	l := &lister{}
	l.err = fmt.Errorf("list tools: %w", &url.Error{Op: "Post", URL: "https://x.example.com/mcp?key=hunter2", Err: io.EOF})
	s, st, user := newSyncer(t, l)
	ctx := context.Background()

	if err := s.Sync(ctx, "linear"); err == nil {
		t.Fatal("Sync succeeded, want the lister's failure")
	}

	rec, err := st.LastSync(ctx, user, "linear")

	if err != nil {
		t.Fatalf("LastSync: %v", err)
	}

	want := store.SyncRecord{At: t0, Error: `list tools: Post "https://x.example.com/mcp?REDACTED": EOF`}

	if diff := cmp.Diff(want, rec, cmp.Comparer(time.Time.Equal)); diff != "" {
		t.Errorf("LastSync (-want +got):\n%s", diff)
	}
}

// An upstream listing two tools under one name can't become a catalog; the
// sync fails, keeps the catalog it had, and records the failure, so status
// shows why.
func TestSyncOfAListWithADuplicateNameFailsAndIsRecorded(t *testing.T) {
	l := &lister{}
	l.set("linear", mcpTool("list_issues", "Lists issues.", nil))
	s, st, user := newSyncer(t, l)
	ctx := context.Background()

	if err := s.Sync(ctx, "linear"); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	l.set("linear", mcpTool("create_issue", "Creates an issue.", nil), mcpTool("create_issue", "Creates it again.", nil))

	if err := s.Sync(ctx, "linear"); err == nil {
		t.Fatal("Sync succeeded, want it to fail on the duplicate name")
	}

	if diff := cmp.Diff([]string{"list_issues"}, slices.Sorted(maps.Keys(catalogOf(t, st, user, "linear")))); diff != "" {
		t.Errorf("catalog after the failed sync (-want +got):\n%s", diff)
	}

	if rec, err := st.LastSync(ctx, user, "linear"); err != nil || !strings.Contains(rec.Error, "create_issue") {
		t.Errorf("LastSync = %+v, %v; want the failure, naming the duplicate, recorded", rec, err)
	}
}

func TestConcurrentSyncsCoalesceIntoOneRerun(t *testing.T) {
	l := &lister{entered: make(chan struct{}, 10), gate: make(chan struct{})}
	l.set("linear", mcpTool("v1", "first", nil))
	s, st, user := newSyncer(t, l)
	ctx := context.Background()

	errs := make(chan error, 4)
	go func() { errs <- s.Sync(ctx, "linear") }()
	<-l.entered // the first sync is inside ListTools

	// Three more requests arrive while it runs; the upstream changes too.
	l.set("linear", mcpTool("v2", "second", nil))

	for range 3 {
		go func() { errs <- s.Sync(ctx, "linear") }()
	}

	s.Request(ctx, "linear")
	// Release the first run only once all three callers wait on the rerun.
	waitFor(t, func() bool { return catalog.Waiters(s, "linear") == 3 })
	close(l.gate)

	for range 4 {
		if err := <-errs; err != nil {
			t.Errorf("Sync: %v", err)
		}
	}

	if got := l.callCount(); got != 2 {
		t.Errorf("ListTools calls = %d, want 2 (one run plus one coalesced rerun)", got)
	}

	var names []string

	for n := range catalogOf(t, st, user, "linear") {
		names = append(names, n)
	}

	sort.Strings(names)

	if diff := cmp.Diff([]string{"v2"}, names); diff != "" {
		t.Errorf("catalog after rerun (-want +got):\n%s", diff)
	}
}

func TestRequestSyncsInTheBackground(t *testing.T) {
	l := &lister{}
	l.set("github", mcpTool("search_code", "Searches code.", nil))
	s, st, user := newSyncer(t, l)

	s.Request(context.Background(), "github")

	waitFor(t, func() bool { return len(catalogOf(t, st, user, "github")) == 1 })
}

// OnSynced reports each sync that wrote its catalog, a background one
// included, by the time its Sync callers return, and not a failed one: the
// daemon's poll re-times a connector from the sync's cache hint
// (wayfindr-t08.6).
func TestOnSyncedReportsEachSuccessfulSync(t *testing.T) {
	l := &lister{}
	l.set("github", mcpTool("search_code", "Searches code.", nil))
	var mu sync.Mutex
	var got []string
	synced := func() []string {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(got)
	}
	s, _, _ := newSyncerWith(t, catalog.Options{Lister: l, OnSynced: func(c string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, c)
	}})
	ctx := context.Background()

	if err := s.Sync(ctx, "github"); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if diff := cmp.Diff([]string{"github"}, synced()); diff != "" {
		t.Errorf("OnSynced after Sync (-want +got):\n%s", diff)
	}

	s.Request(ctx, "github")
	waitFor(t, func() bool { return len(synced()) == 2 })

	l.mu.Lock()
	l.err = errors.New("connection refused")
	l.mu.Unlock()

	if err := s.Sync(ctx, "github"); err == nil {
		t.Fatal("Sync with a failing lister succeeded")
	}

	if diff := cmp.Diff([]string{"github", "github"}, synced()); diff != "" {
		t.Errorf("OnSynced after a failed Sync (-want +got):\n%s", diff)
	}
}

func TestEmbedPendingWithoutAnEmbedderLeavesEntriesPending(t *testing.T) {
	l := &lister{}
	l.set("linear", mcpTool("list_issues", "Lists issues.", nil))
	s, st, user := newSyncer(t, l)
	ctx := context.Background()

	if err := s.Sync(ctx, "linear"); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if err := s.EmbedPending(ctx); err != nil {
		t.Errorf("EmbedPending: %v", err)
	}

	if got := catalogOf(t, st, user, "linear"); len(got) != 1 {
		t.Errorf("pending entries = %d, want 1", len(got))
	}
}

// lastCall reports when the latest ListTools call began.
func (l *lister) lastCall() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.last
}

func TestRequestAndSyncDuringCloseNeverStartASync(t *testing.T) {
	l := &lister{}
	l.set("linear", mcpTool("list_issues", "Lists issues.", nil))
	l.set("github", mcpTool("search_code", "Searches code.", nil))
	s, _, _ := newSyncer(t, l)
	ctx := context.Background()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := range 8 {
		connector := []string{"linear", "github"}[i%2]
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}

				if i%4 < 2 {
					s.Request(ctx, connector)
					continue
				}

				if err := s.Sync(ctx, connector); err != nil && !errors.Is(err, catalog.ErrClosed) {
					t.Errorf("Sync: %v", err)
				}
			}
		})
	}

	waitFor(t, func() bool { return l.callCount() >= 4 })

	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	closed := time.Now()
	// Keep the callers going past Close: none of their calls may sync.
	time.Sleep(20 * time.Millisecond)

	if err := s.Sync(ctx, "linear"); !errors.Is(err, catalog.ErrClosed) {
		t.Errorf("Sync after Close = %v, want %v", err, catalog.ErrClosed)
	}

	close(stop)
	wg.Wait()

	if last := l.lastCall(); last.After(closed) {
		t.Errorf("ListTools began %v after Close returned", last.Sub(closed))
	}
}

// TestCloseCancelsASyncBlockedInListToolsAndDropsItsQueuedRerun is the
// shutdown of a daemon whose upstream hangs a tools/list: Close cancels the
// sync instead of waiting on the upstream, and the cancelled sync records
// nothing, so the store can be closed once Close returns.
func TestCloseCancelsASyncBlockedInListToolsAndDropsItsQueuedRerun(t *testing.T) {
	l := &lister{entered: make(chan struct{}, 10), gate: make(chan struct{})}
	defer close(l.gate)
	l.set("linear", mcpTool("list_issues", "Lists issues.", nil))
	s, st, user := newSyncer(t, l)
	ctx := context.Background()

	first := make(chan error, 1)
	go func() { first <- s.Sync(ctx, "linear") }()
	<-l.entered
	s.Request(ctx, "linear") // queues a rerun behind the running sync

	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := s.Close(wait); err != nil {
		t.Fatalf("Close with a sync blocked in ListTools: %v", err)
	}

	if err := <-first; !errors.Is(err, catalog.ErrClosed) {
		t.Errorf("in-flight Sync = %v, want %v", err, catalog.ErrClosed)
	}

	if got := l.callCount(); got != 1 {
		t.Errorf("ListTools calls = %d, want 1 (the queued rerun must not run after Close began)", got)
	}

	if got := catalogOf(t, st, user, "linear"); len(got) != 0 {
		t.Errorf("catalog after a cancelled sync has %d tools, want 0", len(got))
	}

	if _, err := st.LastSync(ctx, user, "linear"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LastSync after a cancelled sync = %v, want %v", err, store.ErrNotFound)
	}
}

// TestCloseGivesUpWhenCtxExpiresWhileASyncRuns covers a lister that ignores
// the cancellation: Close returns ctx.Err(), and the sync, when it does
// return, still writes nothing.
func TestCloseGivesUpWhenCtxExpiresWhileASyncRuns(t *testing.T) {
	l := &lister{entered: make(chan struct{}, 10), gate: make(chan struct{}), deaf: true}
	l.set("linear", mcpTool("list_issues", "Lists issues.", nil))
	s, st, user := newSyncer(t, l)
	ctx := context.Background()

	s.Request(ctx, "linear")
	<-l.entered

	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()

	if err := s.Close(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close with expiring ctx = %v, want %v", err, context.DeadlineExceeded)
	}

	// A second Close, once the sync is released, waits for it and succeeds.
	close(l.gate)

	if err := s.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}

	if got := l.callCount(); got != 1 {
		t.Errorf("ListTools calls = %d, want 1", got)
	}

	if got := catalogOf(t, st, user, "linear"); len(got) != 0 {
		t.Errorf("catalog after a sync that outlived Close's ctx has %d tools, want 0", len(got))
	}

	if _, err := st.LastSync(ctx, user, "linear"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LastSync after a sync that outlived Close's ctx = %v, want %v", err, store.ErrNotFound)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)

	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}

		time.Sleep(time.Millisecond)
	}
}
