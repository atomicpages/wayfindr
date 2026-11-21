package storetest

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/atomicpages/wayfindr/internal/store"
)

// embedAs stores vecs[i] as tools[i]'s embedding under modelID.
func embedAs(t *testing.T, s store.Store, userID, modelID string, tools []store.Tool, vecs ...[]float32) {
	t.Helper()

	if len(tools) != len(vecs) {
		t.Fatalf("%d tools, %d vectors", len(tools), len(vecs))
	}

	tvs := make([]store.ToolVector, len(tools))

	for i, tl := range tools {
		tvs[i] = store.ToolVector{Connector: tl.Connector, Name: tl.Name, ContentHash: tl.ContentHash, Vector: vecs[i]}
	}

	if err := s.SetEmbeddings(context.Background(), userID, modelID, tvs); err != nil {
		t.Fatalf("SetEmbeddings: %v", err)
	}
}

// Expected orders below are worked by hand from design §7.5: lexical and
// vector ranks fused as Σ 1/(5 + rank), ties by qualified name.
func testHybridSearchFusesLexicalAndVectorRanks(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	createIssue := def(t, "linear", `{"name":"create_issue","description":"Create a new issue."}`)
	listIssues := def(t, "linear", `{"name":"list_issues","description":"List issues in a team."}`)
	listTeams := def(t, "linear", `{"name":"list_teams","description":"List teams."}`)
	getUser := def(t, "linear", `{"name":"get_user","description":"Get a user."}`)
	listCycles := def(t, "linear", `{"name":"list_cycles","description":"List cycles."}`)
	tools := []store.Tool{createIssue, listIssues, listTeams, getUser, listCycles}
	apply(t, s, user, "linear", tools...)
	// Against the query vector (1, 0): list_teams has cosine 1 (vector rank
	// 1), list_issues 0.71 (rank 2), create_issue 0.32 (rank 3). get_user
	// (cosine -1) and list_cycles (cosine 0) are not vector candidates.
	embedAs(t, s, user, "m1", tools,
		[]float32{1, 3}, []float32{1, 1}, []float32{2, 0}, []float32{-1, 0}, []float32{0, 1})

	// Lexical ranks: create_issue 1 (both words), list_issues 2. ("an"
	// matches nothing; the query is not "create issue", which names
	// create_issue exactly and would rank it first by the boost alone.)
	// Fused: create_issue 1/6 + 1/8 = 0.2917, list_issues 2/7 = 0.2857,
	// list_teams 1/6 = 0.1667.
	page := search(t, s, store.SearchQuery{UserID: user, Text: "create an issue", Vector: []float32{1, 0}, ModelID: "m1", Page: 1, Size: 10})

	want := []string{"linear.create_issue", "linear.list_issues", "linear.list_teams"}

	if diff := cmp.Diff(want, qualified(page)); diff != "" {
		t.Errorf("results (-want +got):\n%s", diff)
	}
}

// explained returns the page's scores with each BM25F score checked and
// then zeroed: the drivers weight description fields differently, so only
// whether a result has a lexical score is driver-independent.
func explained(t *testing.T, page store.SearchPage) []store.Score {
	t.Helper()

	if len(page.Scores) != len(page.Tools) {
		t.Fatalf("%d scores for %d tools", len(page.Scores), len(page.Tools))
	}

	out := make([]store.Score, len(page.Scores))

	for i, sc := range page.Scores {
		if (sc.LexicalRank > 0) != (sc.Lexical > 0) {
			t.Errorf("%s: lexical rank %d with BM25F %v", qualified(page)[i], sc.LexicalRank, sc.Lexical)
		}

		sc.Lexical = 0
		out[i] = sc
	}

	return out
}

// approx compares cosines and fused scores, which the drivers compute in
// different precision and order (pgvector may sum in float4).
func approx() cmp.Option { return cmpopts.EquateApprox(1e-6, 0) }

// The same catalog as testHybridSearchFusesLexicalAndVectorRanks, and the
// same ranks: Explain reports them, each cosine against (1, 0), and the
// fused score Σ 1/(5 + rank). The query names create_issue exactly, so its
// score says so. Pages carry their own results' scores.
func testHybridSearchExplainsEachResultsRanks(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	createIssue := def(t, "linear", `{"name":"create_issue","description":"Create a new issue."}`)
	listIssues := def(t, "linear", `{"name":"list_issues","description":"List issues in a team."}`)
	listTeams := def(t, "linear", `{"name":"list_teams","description":"List teams."}`)
	getUser := def(t, "linear", `{"name":"get_user","description":"Get a user."}`)
	listCycles := def(t, "linear", `{"name":"list_cycles","description":"List cycles."}`)
	tools := []store.Tool{createIssue, listIssues, listTeams, getUser, listCycles}
	apply(t, s, user, "linear", tools...)
	embedAs(t, s, user, "m1", tools,
		[]float32{1, 3}, []float32{1, 1}, []float32{2, 0}, []float32{-1, 0}, []float32{0, 1})

	q := store.SearchQuery{UserID: user, Text: "create issue", Vector: []float32{1, 0}, ModelID: "m1", Page: 1, Size: 10, Explain: true}
	want := []store.Score{
		{LexicalRank: 1, VectorRank: 3, Cosine: 1 / math.Sqrt(10), Fused: 1.0/6 + 1.0/8, ExactName: true},
		{LexicalRank: 2, VectorRank: 2, Cosine: 1 / math.Sqrt(2), Fused: 2.0 / 7},
		{VectorRank: 1, Cosine: 1, Fused: 1.0 / 6},
	}

	if diff := cmp.Diff(want, explained(t, search(t, s, q)), approx()); diff != "" {
		t.Errorf("scores (-want +got):\n%s", diff)
	}

	q.Page, q.Size = 2, 1
	page := search(t, s, q)

	if diff := cmp.Diff([]string{"linear.list_issues"}, qualified(page)); diff != "" {
		t.Errorf("page 2 results (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff(want[1:2], explained(t, page), approx()); diff != "" {
		t.Errorf("page 2 scores (-want +got):\n%s", diff)
	}

	q.Explain = false

	if got := search(t, s, q).Scores; got != nil {
		t.Errorf("scores without Explain: %v, want nil", got)
	}
}

// Two docs whose ranks mirror each other fuse to the same score; the lower
// qualified name wins, not either list's order.
func testHybridSearchBreaksFusedTiesByQualifiedName(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	zeta := def(t, "linear", `{"name":"zeta_issue","description":"Issue."}`)
	alpha := def(t, "linear", `{"name":"alpha","description":"Mentions an issue among many other words in a long description."}`)
	apply(t, s, user, "linear", zeta, alpha)
	// Lexical: zeta 1, alpha 2. Vector: alpha 1, zeta 2.
	embedAs(t, s, user, "m1", []store.Tool{zeta, alpha}, []float32{1, 1}, []float32{1, 0})

	for range 3 {
		page := search(t, s, store.SearchQuery{UserID: user, Text: "issue", Vector: []float32{1, 0}, ModelID: "m1", Page: 1, Size: 10})

		if diff := cmp.Diff([]string{"linear.alpha", "linear.zeta_issue"}, qualified(page)); diff != "" {
			t.Errorf("results (-want +got):\n%s", diff)
		}
	}
}

// A query with no lexical match returns the vector candidates alone, and only
// vectors embedded under the query's model identity are compared.
func testHybridSearchComparesOnlyVectorsOfTheQueryModel(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	a := def(t, "notes", `{"name":"a","description":"First."}`)
	b := def(t, "notes", `{"name":"b","description":"Second."}`)
	apply(t, s, user, "notes", a, b)
	embedAs(t, s, user, "m-old", []store.Tool{a}, []float32{1, 0})
	embedAs(t, s, user, "m-new", []store.Tool{b}, []float32{1, 0})

	tests := []struct {
		model string
		want  []string
	}{
		{"m-new", []string{"notes.b"}},
		{"m-old", []string{"notes.a"}},
		{"m-other", []string{}},
	}

	for _, tt := range tests {
		// "zzzz" matches nothing lexically, so only vector candidates remain.
		page := search(t, s, store.SearchQuery{UserID: user, Text: "zzzz", Vector: []float32{1, 0}, ModelID: tt.model, Page: 1, Size: 10})

		if diff := cmp.Diff(tt.want, qualified(page)); diff != "" {
			t.Errorf("model %s (-want +got):\n%s", tt.model, diff)
		}
	}
}

func testHybridSearchKeepsTheTop50VectorCandidatesAboveZero(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	// Tool tNN has the vector (60-NN, NN): its cosine with (1, 0) falls as
	// NN grows, so t00 is the nearest and t59 the 60th.
	var tools []store.Tool
	var vecs [][]float32

	for i := range 60 {
		tools = append(tools, def(t, "notes", fmt.Sprintf(`{"name":"t%02d","description":"Note."}`, i)))
		vecs = append(vecs, []float32{float32(60 - i), float32(i)})
	}

	// A vector orthogonal or opposite to the query, or a zero vector, is
	// never a candidate.
	tools = append(tools,
		def(t, "notes", `{"name":"orthogonal","description":"Note."}`),
		def(t, "notes", `{"name":"opposite","description":"Note."}`),
		def(t, "notes", `{"name":"zero","description":"Note."}`))
	vecs = append(vecs, []float32{0, 1}, []float32{-1, 0}, []float32{0, 0})
	apply(t, s, user, "notes", tools...)
	embedAs(t, s, user, "m1", tools, vecs...)

	var got []string

	for p := 1; ; p++ {
		page := search(t, s, store.SearchQuery{UserID: user, Text: "zzzz", Vector: []float32{1, 0}, ModelID: "m1", Page: p, Size: 10})
		got = append(got, qualified(page)...)

		if !page.HasMore {
			break
		}
	}

	var want []string

	for i := range 50 {
		want = append(want, fmt.Sprintf("notes.t%02d", i))
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("results (-want +got):\n%s", diff)
	}
}

// Fusion ranks over the whole catalog, so filtering by connector only drops
// results: the filtered order is the unfiltered order restricted.
func testHybridSearchConnectorFilterRestrictsTheFusedRanking(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	gh := def(t, "github", `{"name":"create_issue","description":"Create an issue."}`)
	ln := def(t, "linear", `{"name":"create_issue","description":"Create an issue."}`)
	teams := def(t, "linear", `{"name":"list_teams","description":"List teams."}`)
	apply(t, s, user, "github", gh)
	apply(t, s, user, "linear", ln, teams)
	embedAs(t, s, user, "m1", []store.Tool{teams}, []float32{1, 0})

	// Not "create issue": that names create_issue exactly, and the boost
	// would rank both create_issue tools first.
	q := store.SearchQuery{UserID: user, Text: "create an issue", Vector: []float32{1, 0}, ModelID: "m1", Page: 1, Size: 10}
	// Lexical: github.create_issue 1, linear.create_issue 2 (equal scores,
	// by name). Vector: list_teams 1. Fused: github.create_issue and
	// list_teams 1/6 each, linear.create_issue 1/7.
	want := []string{"github.create_issue", "linear.list_teams", "linear.create_issue"}

	if diff := cmp.Diff(want, qualified(search(t, s, q))); diff != "" {
		t.Fatalf("unfiltered (-want +got):\n%s", diff)
	}

	// Ranking only linear's tools would move linear.create_issue to
	// lexical rank 1 and tie it with list_teams, which it beats by name.
	q.Connectors = []string{"linear"}

	if diff := cmp.Diff([]string{"linear.list_teams", "linear.create_issue"}, qualified(search(t, s, q))); diff != "" {
		t.Errorf("filtered (-want +got):\n%s", diff)
	}

	// Explained ranks are the unfiltered ones: linear.create_issue keeps
	// lexical rank 2.
	q.Explain = true
	wantScores := []store.Score{
		{VectorRank: 1, Cosine: 1, Fused: 1.0 / 6},
		{LexicalRank: 2, Fused: 1.0 / 7},
	}

	if diff := cmp.Diff(wantScores, explained(t, search(t, s, q)), approx()); diff != "" {
		t.Errorf("filtered scores (-want +got):\n%s", diff)
	}
}

func testHybridSearchIgnoresVectorsOfAnotherLength(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	a := def(t, "notes", `{"name":"a","description":"First."}`)
	b := def(t, "notes", `{"name":"b","description":"Second."}`)
	apply(t, s, user, "notes", a, b)
	embedAs(t, s, user, "m1", []store.Tool{a, b}, []float32{1, 0, 0}, []float32{1, 0})

	page := search(t, s, store.SearchQuery{UserID: user, Text: "zzzz", Vector: []float32{1, 0}, ModelID: "m1", Page: 1, Size: 10})

	if diff := cmp.Diff([]string{"notes.b"}, qualified(page)); diff != "" {
		t.Errorf("results (-want +got):\n%s", diff)
	}
}

// A query that names a tool exactly ranks it first (design §7.5): its words,
// by the tokenizer's rules and unfolded, equal those of the tool's name or
// qualified name. Without the boost, list_my_issues leads: it is vector rank
// 1 against (1, 0), linear.list_issues is rank 2, and github.list_issues is
// no vector candidate (cosine 0). Two exact names keep their fused order.
func testHybridSearchRanksAnExactToolNameFirst(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	gh := def(t, "github", `{"name":"list_issues","description":"List issues."}`)
	ln := def(t, "linear", `{"name":"list_issues","description":"List issues."}`)
	mine := def(t, "linear", `{"name":"list_my_issues","description":"List issues assigned to me."}`)
	apply(t, s, user, "github", gh)
	apply(t, s, user, "linear", ln, mine)
	embedAs(t, s, user, "m1", []store.Tool{gh, ln, mine}, []float32{0, 1}, []float32{0.1, 1}, []float32{1, 0})

	both := []string{"linear.list_issues", "github.list_issues", "linear.list_my_issues"}
	unboosted := []string{"linear.list_my_issues", "linear.list_issues", "github.list_issues"}

	for _, tc := range []struct {
		query string
		want  []string
		exact int // how many leading results are exact names
	}{
		{"list issues", both, 2},
		{"list_issues", both, 2},
		{"listIssues", both, 2},
		{"List-Issues", both, 2},
		{"github.list_issues", []string{"github.list_issues", "linear.list_my_issues", "linear.list_issues"}, 1},
		{"linear list issues", []string{"linear.list_issues", "linear.list_my_issues", "github.list_issues"}, 1},
		// Not exact: an extra word, another word order, a folded plural.
		{"list the issues", unboosted, 0},
		{"issues list", unboosted, 0},
		{"list issue", unboosted, 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			q := store.SearchQuery{UserID: user, Text: tc.query, Vector: []float32{1, 0}, ModelID: "m1", Page: 1, Size: 10, Explain: true}
			page := search(t, s, q)

			if diff := cmp.Diff(tc.want, qualified(page)); diff != "" {
				t.Errorf("results (-want +got):\n%s", diff)
			}

			got := make([]bool, len(page.Scores))
			want := make([]bool, len(page.Scores))

			for i, sc := range page.Scores {
				got[i], want[i] = sc.ExactName, i < tc.exact
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("ExactName (-want +got):\n%s", diff)
			}
		})
	}
}
