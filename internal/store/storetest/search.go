package storetest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/atomicpages/wayfindr/internal/store"
)

// Each search case has one right answer, worked by hand from design §7.5,
// which every driver must return exactly. How closely the drivers' rankings
// agree on a realistic catalog, where lexical ranking may differ in the
// lower places, is internal/store/parity's job.

// def builds a catalog entry from a literal tool definition.
func def(t *testing.T, connector, definition string) store.Tool {
	t.Helper()
	tl, err := store.NewTool(connector, json.RawMessage(definition), []byte(definition))

	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}

	return tl
}

func search(t *testing.T, s store.Store, q store.SearchQuery) store.SearchPage {
	t.Helper()
	page, err := s.Search(context.Background(), q)

	if err != nil {
		t.Fatalf("Search(%q): %v", q.Text, err)
	}

	return page
}

// qualified returns the page's qualified tool names in rank order.
func qualified(page store.SearchPage) []string {
	names := []string{}

	for _, tl := range page.Tools {
		names = append(names, tl.Connector+"."+tl.Name)
	}

	return names
}

func testSearchRanksNameMatchesAboveDescriptionMatches(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "linear",
		def(t, "linear", `{"name":"create_issue","title":"Create issue","description":"Create a new issue in a team."}`),
		def(t, "linear", `{"name":"list_teams","description":"List teams. Use before you create an issue to find the team ID."}`),
		def(t, "linear", `{"name":"list_projects","description":"List projects in the workspace."}`),
	)

	page := search(t, s, store.SearchQuery{UserID: user, Text: "create issue", Page: 1, Size: 5})

	want := []string{"linear.create_issue", "linear.list_teams"}

	if diff := cmp.Diff(want, qualified(page)); diff != "" {
		t.Errorf("results (-want +got):\n%s", diff)
	}

	if page.HasMore {
		t.Error("HasMore = true, want false")
	}
}

func testSearchReturnsFullCatalogEntries(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	entry := def(t, "linear", `{"name":"create_issue","title":"Create issue","description":"Create an issue.","inputSchema":{"type":"object","properties":{"team":{"type":"string","description":"Team ID"}}}}`)
	unembedded := def(t, "linear", `{"name":"archive_note","description":"Archive a note."}`)
	apply(t, s, user, "linear", entry, unembedded)
	embedAs(t, s, user, "m1", []store.Tool{entry}, []float32{1, 2})
	entry.Embedding, entry.EmbeddingModel = []float32{1, 2}, "m1"

	tests := []struct {
		text string
		want store.Tool
	}{
		{"create_issue", entry},
		{"archive_note", unembedded}, // no embedding comes back as none
	}

	for _, tt := range tests {
		page := search(t, s, store.SearchQuery{UserID: user, Text: tt.text, Page: 1, Size: 5})

		if diff := cmp.Diff([]store.Tool{tt.want}, page.Tools); diff != "" {
			t.Errorf("Search(%q) tools (-want +got):\n%s", tt.text, diff)
		}
	}
}

func testSearchIsScopedToTheUser(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "linear", def(t, "linear", `{"name":"create_issue","description":"Create an issue."}`))

	page := search(t, s, store.SearchQuery{UserID: unknownUser(t), Text: "create issue", Page: 1, Size: 5})

	if len(page.Tools) != 0 || page.HasMore {
		t.Errorf("results for another user = %v (has_more %v), want none", qualified(page), page.HasMore)
	}
}

func testSearchWithNoMatchingTokensReturnsAnEmptyPage(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "linear", def(t, "linear", `{"name":"create_issue","description":"Create an issue."}`))

	for _, text := range []string{"", "  ", "a", "zebra", "_-."} {
		page := search(t, s, store.SearchQuery{UserID: user, Text: text, Page: 1, Size: 5})

		if len(page.Tools) != 0 || page.HasMore {
			t.Errorf("Search(%q) = %v (has_more %v), want empty", text, qualified(page), page.HasMore)
		}
	}
}

func testSearchPagesPartitionTheRankingAndAreStable(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	// Seven tools with identical text tie on score, so the order is the
	// qualified-name tie-break. Several query words make each score a
	// sum of several terms, which must add up the same for every tool.
	var tools []store.Tool

	for _, name := range []string{"g", "c", "a", "f", "e", "b", "d"} {
		tools = append(tools, def(t, "notes", `{"name":"`+name+`","description":"Search the notes, the lists, and the notebooks."}`))
	}

	apply(t, s, user, "notes", tools...)

	tests := []struct {
		page        int
		wantTools   []string
		wantHasMore bool
	}{
		{1, []string{"notes.a", "notes.b", "notes.c"}, true},
		{2, []string{"notes.d", "notes.e", "notes.f"}, true},
		{3, []string{"notes.g"}, false},
		{4, []string{}, false},
		{1 << 62, []string{}, false}, // an offset past any catalog
	}

	for _, tt := range tests {
		for range 3 { // the same page again: pages are stable across calls
			page := search(t, s, store.SearchQuery{UserID: user, Text: "search notes lists notebook", Page: tt.page, Size: 3})

			if diff := cmp.Diff(tt.wantTools, qualified(page)); diff != "" {
				t.Errorf("page %d (-want +got):\n%s", tt.page, diff)
			}

			if page.HasMore != tt.wantHasMore {
				t.Errorf("page %d HasMore = %v, want %v", tt.page, page.HasMore, tt.wantHasMore)
			}
		}
	}
}

func testSearchHasMoreIsFalseWhenTheLastPageIsExactlyFull(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "notes",
		def(t, "notes", `{"name":"a","description":"Search the notes."}`),
		def(t, "notes", `{"name":"b","description":"Search the notes."}`),
	)

	page := search(t, s, store.SearchQuery{UserID: user, Text: "notes", Page: 1, Size: 2})

	if len(page.Tools) != 2 || page.HasMore {
		t.Errorf("got %v has_more %v, want 2 tools and has_more false", qualified(page), page.HasMore)
	}
}

func testSearchRejectsNonPositivePageOrSize(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)

	for _, q := range []store.SearchQuery{
		{UserID: user, Text: "x", Page: 0, Size: 5},
		{UserID: user, Text: "x", Page: 1, Size: 0},
	} {
		if _, err := s.Search(context.Background(), q); err == nil {
			t.Errorf("Search(page %d, size %d): err = nil, want error", q.Page, q.Size)
		}
	}
}

func testSearchConnectorFilterKeepsOnlyListedConnectors(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "github", def(t, "github", `{"name":"create_issue","description":"Create a new issue in a GitHub repository."}`))
	apply(t, s, user, "linear", def(t, "linear", `{"name":"create_issue","description":"Create a new issue in Linear."}`))
	apply(t, s, user, "sentry", def(t, "sentry", `{"name":"update_issue","description":"Update an issue's status."}`))

	tests := []struct {
		connectors []string
		want       []string
	}{
		{nil, []string{"github.create_issue", "linear.create_issue", "sentry.update_issue"}},
		{[]string{}, []string{"github.create_issue", "linear.create_issue", "sentry.update_issue"}},
		{[]string{"linear"}, []string{"linear.create_issue"}},
		{[]string{"sentry", "github"}, []string{"github.create_issue", "sentry.update_issue"}},
		{[]string{"slack"}, []string{}},
	}

	for _, tt := range tests {
		page := search(t, s, store.SearchQuery{UserID: user, Text: "create issue", Connectors: tt.connectors, Page: 1, Size: 10})

		if diff := cmp.Diff(tt.want, qualified(page), cmpopts.SortSlices(strings.Compare)); diff != "" {
			t.Errorf("connectors %v (-want +got):\n%s", tt.connectors, diff)
		}
	}
}

func testSearchFoldsPluralsAndToleratesTyposInLongTokens(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "linear",
		def(t, "linear", `{"name":"create_issue","description":"Create a new issue."}`),
		def(t, "linear", `{"name":"list_projects","description":"List projects."}`),
		def(t, "linear", `{"name":"list_series","description":"List time series."}`),
		def(t, "linear", `{"name":"list_queries","description":"List saved queries."}`),
	)

	tests := []struct {
		text string
		want []string
	}{
		{"project", []string{"linear.list_projects"}},
		{"query", []string{"linear.list_queries"}},
		{"issues", []string{"linear.create_issue"}},
		{"creat_isue", []string{"linear.create_issue"}},
		{"projcts", []string{"linear.list_projects"}},
		// series folds to sery; the typo still reaches it through the
		// surface form.
		{"serie", []string{"linear.list_series"}},
		// Tokens under four characters are never expanded.
		{"lst", []string{}},
	}

	for _, tt := range tests {
		page := search(t, s, store.SearchQuery{UserID: user, Text: tt.text, Page: 1, Size: 5})

		if diff := cmp.Diff(tt.want, qualified(page)); diff != "" {
			t.Errorf("Search(%q) (-want +got):\n%s", tt.text, diff)
		}
	}
}

// A plural query word is one word: it must not outweigh a singular one just
// because folding gives it a second token (design §7.5).
func testSearchCountsAPluralQueryWordOnce(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "git",
		def(t, "git", `{"name":"tool_a","description":"Lists commits in the repository"}`),
		def(t, "git", `{"name":"tool_b","description":"Shows history"}`),
	)

	// Each tool matches one query word, and both words have the same idf.
	// tool_b's match is in a much shorter description, so it ranks first.
	page := search(t, s, store.SearchQuery{UserID: user, Text: "commits history", Page: 1, Size: 5})

	want := []string{"git.tool_b", "git.tool_a"}

	if diff := cmp.Diff(want, qualified(page)); diff != "" {
		t.Errorf("results (-want +got):\n%s", diff)
	}
}

// Search splits, lowercases, and typo-matches non-ASCII letters by Unicode
// rules (design §7.5), the same on every driver and whatever the database's
// locale: a C ctype database must not strip or fail to lowercase é, ü, ñ, or
// CJK.
func testSearchMatchesNonASCIILetters(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "docs",
		def(t, "docs", `{"name":"créer_tâche","title":"Créer une tâche","description":"Crée une tâche dans le projet."}`),
		def(t, "docs", `{"name":"zeigeÄnderung","description":"Zeigt die Grüße eines Teams."}`),
		def(t, "docs", `{"name":"añadir_comentario","description":"Añade un comentario al artículo."}`),
		def(t, "docs", `{"name":"wiki_検索","description":"全文検索 for the wiki."}`),
		def(t, "docs", `{"name":"list_projects","description":"List the projects."}`),
	)

	tests := []struct {
		text string
		want []string
	}{
		{"tâche", []string{"docs.créer_tâche"}},
		// The accented letter is part of the word: tèche shares only 3
		// of 9 trigrams with tâche (0.33), so it matches nothing.
		{"tèche", []string{}},
		// Uppercase non-ASCII letters lowercase like ASCII ones.
		{"TÂCHE", []string{"docs.créer_tâche"}},
		{"AÑADIR", []string{"docs.añadir_comentario"}},
		// A capital non-ASCII letter is a camelCase boundary.
		{"änderung", []string{"docs.zeigeÄnderung"}},
		{"Grüße", []string{"docs.zeigeÄnderung"}},
		{"artículo", []string{"docs.añadir_comentario"}},
		{"検索", []string{"docs.wiki_検索"}},
		// Typo expansion: 全文検査 shares 3 of 7 trigrams with 全文検索
		// (0.43), and articulo 6 of 12 with artículo (0.5).
		{"全文検査", []string{"docs.wiki_検索"}},
		{"articulo", []string{"docs.añadir_comentario"}},
	}

	for _, tt := range tests {
		page := search(t, s, store.SearchQuery{UserID: user, Text: tt.text, Page: 1, Size: 5})

		if diff := cmp.Diff(tt.want, qualified(page)); diff != "" {
			t.Errorf("Search(%q) (-want +got):\n%s", tt.text, diff)
		}
	}
}
