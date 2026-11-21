package gateway_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// conceptEmbedder stands in for an embedding provider. Each dimension is a
// concept, and a text's vector counts its words of each concept. It knows a
// synonym the lexical index does not ("broadcast" is a messaging word), so
// hybrid search can find a tool that no query word names. It records how
// often it embedded each text.
type conceptEmbedder struct {
	model string

	mu       sync.Mutex
	embedded map[string]int
}

func newConceptEmbedder(model string) *conceptEmbedder {
	return &conceptEmbedder{model: model, embedded: map[string]int{}}
}

func (e *conceptEmbedder) ModelID() string { return e.model }

func (e *conceptEmbedder) Model() string { return "concept-" + e.model }

func (e *conceptEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	concepts := map[string]int{"broadcast": 0, "post": 0, "message": 0, "schedule": 1, "later": 1}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]float32, len(inputs))

	for i, in := range inputs {
		e.embedded[in]++
		v := make([]float32, 2)

		for _, w := range strings.FieldsFunc(strings.ToLower(in), func(r rune) bool { return !unicode.IsLetter(r) }) {
			if c, ok := concepts[w]; ok {
				v[c]++
			}
		}

		out[i] = v
	}

	return out, nil
}

// times is how often text was embedded.
func (e *conceptEmbedder) times(text string) int {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.embedded[text]
}

// slowEmbedder never answers: it waits for its deadline and records that
// it had one.
type slowEmbedder struct {
	mu          sync.Mutex
	hadDeadline bool
}

func (*slowEmbedder) ModelID() string { return "m1" }

func (*slowEmbedder) Model() string { return "slow" }

func (e *slowEmbedder) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	_, ok := ctx.Deadline()
	e.mu.Lock()
	e.hadDeadline = ok
	e.mu.Unlock()
	<-ctx.Done()

	return nil, ctx.Err()
}

// failingEmbedder is a provider that rejects every request.
type failingEmbedder struct{}

func (failingEmbedder) ModelID() string { return "m1" }

func (failingEmbedder) Model() string { return "failing" }

func (failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embed: provider returned HTTP 503: unavailable")
}

func slackTools() []fakeupstream.Option {
	return []fakeupstream.Option{fakeupstream.WithTools(
		&mcp.Tool{Name: "slack_post_message", Description: "Post a message to a channel.", InputSchema: map[string]any{"type": "object"}},
		&mcp.Tool{Name: "slack_schedule_message", Description: "Schedule a message to be sent to a channel later.", InputSchema: map[string]any{"type": "object"}},
	)}
}

// names is the qualified names in a search_tools response, in rank order.
func names(t *testing.T, resp map[string]any) []string {
	t.Helper()
	tools, _ := resp["tools"].([]any)
	out := []string{}

	for _, tl := range tools {
		m, _ := tl.(map[string]any)
		name, _ := m["name"].(string)
		out = append(out, name)
	}

	return out
}

// hybridEnv is a slack catalog embedded by catalogEmbedder, searched
// through a gateway that embeds queries with opts.Embedder.
func hybridEnv(t *testing.T, catalogEmbedder *conceptEmbedder, opts gateway.Options) *mcp.ClientSession {
	t.Helper()
	e := newEnvWith(t, envOptions{
		fakes:    map[string][]fakeupstream.Option{"slack": slackTools()},
		embedder: catalogEmbedder,
		gateway:  opts,
	})

	return connect(t, e.url, fakeupstream.V20251125)
}

func TestSearchFusesTheQueryEmbeddingWithLexicalMatches(t *testing.T) {
	e := newConceptEmbedder("m1")
	cs := hybridEnv(t, e, gateway.Options{Embedder: e})

	tests := []struct {
		query string
		want  []string
	}{
		// No tool says "broadcast"; only the embeddings connect it to
		// posting a message.
		{"broadcast", []string{"slack.slack_post_message", "slack.slack_schedule_message"}},
		// Lexically only schedule_message matches; the vector adds
		// post_message.
		{"broadcast schedule", []string{"slack.slack_schedule_message", "slack.slack_post_message"}},
	}

	for _, tt := range tests {
		if diff := cmp.Diff(tt.want, names(t, search(t, cs, map[string]any{"query": tt.query}))); diff != "" {
			t.Errorf("search %q (-want +got):\n%s", tt.query, diff)
		}
	}
}

func TestSearchWithoutAnEmbedderIsLexicalOnly(t *testing.T) {
	cs := hybridEnv(t, newConceptEmbedder("m1"), gateway.Options{})

	if got := names(t, search(t, cs, map[string]any{"query": "broadcast"})); len(got) != 0 {
		t.Errorf("search broadcast = %v, want no results", got)
	}
}

func TestSearchFallsBackToLexicalWhenTheQueryEmbeddingTimesOut(t *testing.T) {
	slow := &slowEmbedder{}
	cs := hybridEnv(t, newConceptEmbedder("m1"), gateway.Options{Embedder: slow, EmbedTimeout: 20 * time.Millisecond})

	got := names(t, search(t, cs, map[string]any{"query": "broadcast schedule"}))

	if diff := cmp.Diff([]string{"slack.slack_schedule_message"}, got); diff != "" {
		t.Errorf("results (-want +got):\n%s", diff)
	}

	slow.mu.Lock()
	defer slow.mu.Unlock()

	if !slow.hadDeadline {
		t.Error("query embedding ran without a deadline")
	}
}

func TestSearchFallsBackToLexicalWhenTheProviderFails(t *testing.T) {
	cs := hybridEnv(t, newConceptEmbedder("m1"), gateway.Options{Embedder: failingEmbedder{}})

	got := names(t, search(t, cs, map[string]any{"query": "broadcast schedule"}))

	if diff := cmp.Diff([]string{"slack.slack_schedule_message"}, got); diff != "" {
		t.Errorf("results (-want +got):\n%s", diff)
	}
}

func TestSearchEmbedsARepeatedQueryOnce(t *testing.T) {
	e := newConceptEmbedder("m1")
	cs := hybridEnv(t, e, gateway.Options{Embedder: e})

	for range 3 {
		search(t, cs, map[string]any{"query": "broadcast"})
	}

	if n := e.times("broadcast"); n != 1 {
		t.Errorf("query embedded %d times, want 1", n)
	}
}

// The cache is keyed by model identity as well as query: a vector from one
// model is never compared with another model's.
func TestSearchCachesQueryEmbeddingsPerModelIdentity(t *testing.T) {
	m1 := newConceptEmbedder("m1")
	e := newEnvWith(t, envOptions{
		fakes:    map[string][]fakeupstream.Option{"slack": slackTools()},
		embedder: m1,
		gateway:  gateway.Options{Embedder: m1},
	})
	cs := connect(t, e.url, fakeupstream.V20251125)
	search(t, cs, map[string]any{"query": "broadcast"})

	m2 := newConceptEmbedder("m2")
	e.gateway.SetEmbedder(m2, time.Second)

	// The catalog has no m2 vectors yet, so the search is lexical in effect.
	if got := names(t, search(t, cs, map[string]any{"query": "broadcast"})); len(got) != 0 {
		t.Errorf("search under m2 = %v, want no results", got)
	}

	if n := m2.times("broadcast"); n != 1 {
		t.Errorf("m2 embedded the query %d times, want 1", n)
	}

	// Back to m1 through a new client: the m1 vector is still cached.
	again := newConceptEmbedder("m1")
	e.gateway.SetEmbedder(again, time.Second)

	if diff := cmp.Diff([]string{"slack.slack_post_message", "slack.slack_schedule_message"}, names(t, search(t, cs, map[string]any{"query": "broadcast"}))); diff != "" {
		t.Errorf("search under m1 again (-want +got):\n%s", diff)
	}

	if n := again.times("broadcast"); n != 0 {
		t.Errorf("m1 re-embedded a cached query %d times, want 0", n)
	}
}

// The cache holds 256 query embeddings (design §7.5) and evicts the least
// recently used.
func TestSearchQueryCacheEvictsBeyond256Entries(t *testing.T) {
	e := newConceptEmbedder("m1")
	cs := hybridEnv(t, e, gateway.Options{Embedder: e})

	for i := range 257 {
		search(t, cs, map[string]any{"query": fmt.Sprintf("query %d", i)})
	}

	// query 0 was the least recently used of 257, so it was evicted;
	// query 256 is still cached.
	search(t, cs, map[string]any{"query": "query 256"})
	search(t, cs, map[string]any{"query": "query 0"})

	if n := e.times("query 256"); n != 1 {
		t.Errorf("query 256 embedded %d times, want 1 (cached)", n)
	}

	if n := e.times("query 0"); n != 2 {
		t.Errorf("query 0 embedded %d times, want 2 (evicted)", n)
	}
}
