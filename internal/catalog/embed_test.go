package catalog_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/catalog"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
)

// embedder is a stub catalog.Embedder. Each input's vector is
// {len(input), version}, so a test can tell which embedder produced it. If
// gate is set, each Embed call announces itself on entered and then waits
// for gate to be closed or ctx to end.
type embedder struct {
	model   string
	version float32

	mu      sync.Mutex
	err     error
	batches [][]string // inputs of each Embed call
	entered chan struct{}
	gate    chan struct{}
	deaf    bool // the gate wait ignores ctx, as a stuck provider might
}

func (e *embedder) ModelID() string { return e.model }

func (e *embedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	e.mu.Lock()
	e.batches = append(e.batches, inputs)
	entered, gate, deaf, err := e.entered, e.gate, e.deaf, e.err
	e.mu.Unlock()

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

	if err != nil {
		return nil, err
	}

	vecs := make([][]float32, len(inputs))

	for i, in := range inputs {
		vecs[i] = []float32{float32(len(in)), e.version}
	}

	return vecs, nil
}

func (e *embedder) batchSizes() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	var sizes []int

	for _, b := range e.batches {
		sizes = append(sizes, len(b))
	}

	return sizes
}

// newEmbeddingSyncer is newSyncer with an embedder configured.
func newEmbeddingSyncer(t *testing.T, l catalog.ToolLister, e catalog.Embedder) (*catalog.Syncer, store.Store, string) {
	t.Helper()

	return newEmbeddingSyncerWith(t, l, e, func(*catalog.Options) {})
}

// newEmbeddingSyncerWith is newEmbeddingSyncer with its options changed by
// with.
func newEmbeddingSyncerWith(t *testing.T, l catalog.ToolLister, e catalog.Embedder, with func(*catalog.Options)) (*catalog.Syncer, store.Store, string) {
	t.Helper()
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

	opts := catalog.Options{
		Lister:   l,
		Store:    st,
		Embedder: e,
		UserID:   user,
		Logger:   zerolog.Nop(),
		Now:      func() time.Time { return t0 },
	}
	with(&opts)
	s := catalog.New(opts)
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return s, st, user
}

// pending returns the qualified names of the entries not embedded by model.
func pending(t *testing.T, st store.Store, user, model string) []string {
	t.Helper()
	tools, err := st.PendingEmbeddings(context.Background(), user, model, 1000)

	if err != nil {
		t.Fatalf("PendingEmbeddings: %v", err)
	}

	var names []string

	for _, tl := range tools {
		names = append(names, tl.Connector+"."+tl.Name)
	}

	return names
}

func TestEmbedPendingEmbedsEntriesWithTheModelIdentity(t *testing.T) {
	l := &lister{}
	l.set("linear", mcpTool("create_issue", "Creates an issue.", map[string]string{"title": "Issue title"}))
	e := &embedder{model: "m1", version: 1}
	s, st, user := newEmbeddingSyncer(t, l, e)
	ctx := context.Background()

	if err := s.Sync(ctx, "linear"); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if err := s.EmbedPending(ctx); err != nil {
		t.Fatalf("EmbedPending: %v", err)
	}

	got, err := st.GetTool(ctx, user, "linear", "create_issue")

	if err != nil {
		t.Fatalf("GetTool: %v", err)
	}

	text := "linear.create_issue\n\nCreates an issue.\ntitle: Issue title\n"

	if diff := cmp.Diff([]float32{float32(len(text)), 1}, got.Embedding); diff != "" {
		t.Errorf("Embedding (-want +got):\n%s", diff)
	}

	if got.EmbeddingModel != "m1" {
		t.Errorf("EmbeddingModel = %q, want m1", got.EmbeddingModel)
	}

	if p := pending(t, st, user, "m1"); len(p) != 0 {
		t.Errorf("pending after EmbedPending = %v, want none", p)
	}
}

// manyTools returns n tools named t000, t001, ….
func manyTools(n int) []*mcp.Tool {
	tools := make([]*mcp.Tool, n)

	for i := range n {
		tools[i] = mcpTool(fmt.Sprintf("t%03d", i), "Does a thing.", nil)
	}

	return tools
}

func TestModelChangeReembedsEveryEntryInBatches(t *testing.T) {
	l := &lister{}
	l.set("linear", manyTools(70)...)
	s, st, user := newEmbeddingSyncer(t, l, &embedder{model: "m1", version: 1})
	ctx := context.Background()

	if err := s.Sync(ctx, "linear"); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if err := s.EmbedPending(ctx); err != nil {
		t.Fatalf("EmbedPending under m1: %v", err)
	}

	next := &embedder{model: "m2", version: 2}
	s.SetEmbedder(ctx, next)

	if err := s.EmbedPending(ctx); err != nil {
		t.Fatalf("EmbedPending under m2: %v", err)
	}

	if p := pending(t, st, user, "m2"); len(p) != 0 {
		t.Errorf("pending under m2 = %v, want none", p)
	}

	if diff := cmp.Diff([]int{64, 6}, next.batchSizes()); diff != "" {
		t.Errorf("m2 batch sizes (-want +got):\n%s", diff)
	}

	got, err := st.GetTool(ctx, user, "linear", "t069")

	if err != nil {
		t.Fatalf("GetTool: %v", err)
	}

	if got.EmbeddingModel != "m2" || len(got.Embedding) != 2 || got.Embedding[1] != 2 {
		t.Errorf("t069 embedding = %v from %q, want the m2 vector", got.Embedding, got.EmbeddingModel)
	}
}

// search returns the qualified names of a lexical search's first page.
func search(t *testing.T, st store.Store, user, text string) []string {
	t.Helper()
	page, err := st.Search(context.Background(), store.SearchQuery{UserID: user, Text: text, Page: 1, Size: 5})

	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	var names []string

	for _, tl := range page.Tools {
		names = append(names, tl.Connector+"."+tl.Name)
	}

	return names
}

// A provider that takes the request and never answers stalls one batch,
// not the embedding pass for good (wayfindr-gxnw): the batch times out, the
// pass stops with its entries pending, and the rerun a reload queued embeds
// them with the new embedder.
func TestAStuckProviderTimesOutAndANewEmbedderTakesOver(t *testing.T) {
	l := &lister{}
	l.set("linear", mcpTool("create_issue", "Creates an issue.", nil))
	stuck := &embedder{model: "m1", entered: make(chan struct{}, 8), gate: make(chan struct{})}
	s, st, user := newEmbeddingSyncerWith(t, l, stuck, func(o *catalog.Options) { o.EmbedTimeout = 50 * time.Millisecond })
	ctx := context.Background()

	if err := s.Sync(ctx, "linear"); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	<-stuck.entered // the pass Sync started waits on its first batch
	s.SetEmbedder(ctx, &embedder{model: "m2"})

	for deadline := time.Now().Add(5 * time.Second); len(pending(t, st, user, "m2")) != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("entries still pending for the new embedder after 5s: the stuck batch never ended")
		}
	}
}

func TestProviderFailureStopsThePassAndLeavesEntriesPending(t *testing.T) {
	l := &lister{}
	l.set("linear", append(manyTools(69), mcpTool("create_issue", "Creates an issue.", nil))...)
	providerErr := errors.New("embed: provider returned HTTP 503")
	e := &embedder{model: "m1", err: providerErr}
	s, st, user := newEmbeddingSyncer(t, l, e)
	ctx := context.Background()

	if err := s.Sync(ctx, "linear"); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if err := s.EmbedPending(ctx); !errors.Is(err, providerErr) {
		t.Fatalf("EmbedPending = %v, want it to wrap %v", err, providerErr)
	}

	// Each pass (the one Sync started and ours) stops at its first batch.
	for i, n := range e.batchSizes() {
		if n != 64 {
			t.Errorf("batch %d has %d inputs, want 64 (no batch after a failure)", i, n)
		}
	}

	if got := len(e.batchSizes()); got > 2 {
		t.Errorf("Embed calls = %d, want at most 2 (one per pass)", got)
	}

	if p := pending(t, st, user, "m1"); len(p) != 70 {
		t.Errorf("pending after failure = %d entries, want all 70", len(p))
	}

	if diff := cmp.Diff([]string{"linear.create_issue"}, search(t, st, user, "create issue")); diff != "" {
		t.Errorf("lexical search after failure (-want +got):\n%s", diff)
	}

	// The next pass, once the provider recovers, embeds them.
	e.mu.Lock()
	e.err = nil
	e.mu.Unlock()

	if err := s.EmbedPending(ctx); err != nil {
		t.Fatalf("EmbedPending after recovery: %v", err)
	}

	if p := pending(t, st, user, "m1"); len(p) != 0 {
		t.Errorf("pending after recovery = %v, want none", p)
	}
}

func TestSearchAndSyncKeepWorkingWhileABackgroundPassRuns(t *testing.T) {
	l := &lister{}
	l.set("linear", mcpTool("create_issue", "Creates an issue.", nil))
	l.set("github", mcpTool("search_code", "Searches code.", nil))
	e := &embedder{model: "m1", entered: make(chan struct{}, 10), gate: make(chan struct{})}
	s, st, user := newEmbeddingSyncer(t, l, e)
	ctx := context.Background()

	if err := s.Sync(ctx, "linear"); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	<-e.entered // the pass the sync started is inside Embed

	if diff := cmp.Diff([]string{"linear.create_issue"}, search(t, st, user, "create issue")); diff != "" {
		t.Errorf("search during the pass (-want +got):\n%s", diff)
	}

	if err := s.Sync(ctx, "github"); err != nil {
		t.Errorf("Sync during the pass: %v", err)
	}

	if diff := cmp.Diff([]string{"github.search_code"}, search(t, st, user, "search code")); diff != "" {
		t.Errorf("search for a tool synced during the pass (-want +got):\n%s", diff)
	}

	close(e.gate)
	waitFor(t, func() bool { return len(pending(t, st, user, "m1")) == 0 })
}

func TestCloseStopsARunningPass(t *testing.T) {
	l := &lister{}
	l.set("linear", mcpTool("create_issue", "Creates an issue.", nil))
	// The gate is never opened: only Close can end the Embed call.
	e := &embedder{model: "m1", entered: make(chan struct{}, 10), gate: make(chan struct{})}
	s, st, user := newEmbeddingSyncer(t, l, e)
	ctx := context.Background()

	if err := s.Sync(ctx, "linear"); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	<-e.entered
	waiting := make(chan error, 1)
	go func() { waiting <- s.EmbedPending(ctx) }() // joins the rerun

	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := s.Close(bounded); err != nil {
		t.Fatalf("Close = %v, want the pass stopped and drained", err)
	}

	if err := <-waiting; !errors.Is(err, catalog.ErrClosed) {
		t.Errorf("EmbedPending waiting at Close = %v, want %v", err, catalog.ErrClosed)
	}

	if err := s.EmbedPending(ctx); !errors.Is(err, catalog.ErrClosed) {
		t.Errorf("EmbedPending after Close = %v, want %v", err, catalog.ErrClosed)
	}

	if p := pending(t, st, user, "m1"); len(p) != 1 {
		t.Errorf("pending after Close = %v, want the 1 entry", p)
	}
}
