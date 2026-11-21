package gateway_test

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// hitNames is the qualified names of a Search result, in rank order.
func hitNames(res gateway.SearchResult) []string {
	out := []string{}

	for _, h := range res.Hits {
		out = append(out, h.Name)
	}

	return out
}

// Search ranks as search_tools does: the same query gets the same tools in
// the same order.
func TestSearchAPIRanksAsSearchToolsDoes(t *testing.T) {
	m1 := newConceptEmbedder("m1")
	e := newEnvWith(t, envOptions{
		fakes:    map[string][]fakeupstream.Option{"slack": slackTools()},
		embedder: m1,
		gateway:  gateway.Options{Embedder: m1},
	})
	cs := connect(t, e.url, fakeupstream.V20251125)

	for _, query := range []string{"broadcast", "broadcast schedule", "channel", "zzzz"} {
		res, err := e.gateway.Search(context.Background(), gateway.SearchRequest{Query: query})

		if err != nil {
			t.Fatalf("Search(%q): %v", query, err)
		}

		if diff := cmp.Diff(names(t, search(t, cs, map[string]any{"query": query})), hitNames(res)); diff != "" {
			t.Errorf("Search(%q) (-search_tools +Search):\n%s", query, diff)
		}
	}
}

// With Explain, each hit carries its ranks and fused score. "broadcast
// schedule" matches schedule_message lexically, and its vector is nearer
// the query's than post_message's (see TestSearchFusesTheQueryEmbedding…).
func TestSearchAPIExplainsRanks(t *testing.T) {
	m1 := newConceptEmbedder("m1")
	e := newEnvWith(t, envOptions{
		fakes:    map[string][]fakeupstream.Option{"slack": slackTools()},
		embedder: m1,
		gateway:  gateway.Options{Embedder: m1},
	})

	res, err := e.gateway.Search(context.Background(), gateway.SearchRequest{Query: "broadcast schedule", Explain: true})

	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if diff := cmp.Diff([]string{"slack.slack_schedule_message", "slack.slack_post_message"}, hitNames(res)); diff != "" {
		t.Fatalf("hits (-want +got):\n%s", diff)
	}

	// BM25F and cosine depend on the embed text; ranks and RRF do not.
	ignore := cmpopts.IgnoreFields(store.Score{}, "Lexical", "Cosine")
	want := []store.Score{
		{LexicalRank: 1, VectorRank: 1, Fused: 2.0 / 6},
		{VectorRank: 2, Fused: 1.0 / 7},
	}
	got := []store.Score{res.Hits[0].Score, res.Hits[1].Score}

	if diff := cmp.Diff(want, got, ignore, cmpopts.EquateApprox(1e-9, 0)); diff != "" {
		t.Errorf("scores (-want +got):\n%s", diff)
	}

	if got[0].Lexical <= 0 || got[0].Cosine <= 0 || got[1].Cosine <= 0 {
		t.Errorf("scores %+v: want a positive BM25F for the lexical hit and positive cosines", got)
	}

	if want := (gateway.QueryEmbedding{Mode: gateway.EmbedOn, Model: "concept-m1"}); res.Embedding != want {
		t.Errorf("embedding = %+v, want %+v", res.Embedding, want)
	}

	if res.Page != 1 || res.Size != 5 || res.HasMore {
		t.Errorf("page %d size %d has_more %v, want 1, 5, false", res.Page, res.Size, res.HasMore)
	}
}

func TestSearchAPIReportsHowTheQueryWasEmbedded(t *testing.T) {
	tests := []struct {
		name     string
		embedder gateway.Embedder
		want     gateway.QueryEmbedding
	}{
		{"no embedder", nil, gateway.QueryEmbedding{Mode: gateway.EmbedOff}},
		{"provider fails", failingEmbedder{}, gateway.QueryEmbedding{
			Mode: gateway.EmbedFailed, Model: "failing", Reason: "embed: provider returned HTTP 503: unavailable",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnvWith(t, envOptions{
				fakes:    map[string][]fakeupstream.Option{"slack": slackTools()},
				embedder: newConceptEmbedder("m1"),
				gateway:  gateway.Options{Embedder: tt.embedder},
			})

			res, err := e.gateway.Search(context.Background(), gateway.SearchRequest{Query: "schedule"})

			if err != nil {
				t.Fatalf("Search: %v", err)
			}

			if res.Embedding != tt.want {
				t.Errorf("embedding = %+v, want %+v", res.Embedding, tt.want)
			}

			if diff := cmp.Diff([]string{"slack.slack_schedule_message"}, hitNames(res)); diff != "" {
				t.Errorf("lexical hits (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSearchAPIRefusesWhatSearchToolsRefuses(t *testing.T) {
	e := newEnv(t, map[string][]fakeupstream.Option{"slack": slackTools()})

	tests := []struct {
		name string
		req  gateway.SearchRequest
		want gateway.Case
	}{
		{"blank query", gateway.SearchRequest{Query: "  "}, gateway.CaseMissingQuery},
		{"size over 10", gateway.SearchRequest{Query: "post", Size: 11}, gateway.CaseBadSize},
		{"negative page", gateway.SearchRequest{Query: "post", Page: -1}, gateway.CaseBadPage},
		{"unknown connector", gateway.SearchRequest{Query: "post", Connectors: []string{"jira"}}, gateway.CaseUnknownConnector},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.gateway.Search(context.Background(), tt.req)
			ge, ok := gateway.AsError(err)

			if !ok || ge.Kind != gateway.KindInvalidSearch || ge.Case != tt.want {
				t.Errorf("Search error = %v, want %s/%s", err, gateway.KindInvalidSearch, tt.want)
			}
		})
	}
}
