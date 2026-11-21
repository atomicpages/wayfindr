// Package parity_test compares the store drivers' search rankings (design
// §7.5, ADR 0005). Search behaviour with one right answer is in the shared
// suite (internal/store/storetest), which every driver runs exactly. What is
// left here is what only a comparison can check: lexical ranking may differ
// in the lower places, so on the golden fixture (internal/store/golden)
// Postgres is held to a top-5 overlap with SQLite, lexical and hybrid.
package parity_test

import (
	"slices"
	"testing"

	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/golden"
	"github.com/atomicpages/wayfindr/internal/store/storetest"
)

// goldenStore is the golden fixture loaded into one driver's store.
type goldenStore struct {
	s     store.Store
	user  string
	tools map[string][]store.Tool
}

// newGoldenStores loads the fixture into a SQLite and a Postgres store. It
// skips t without a Postgres test database. The Postgres C-ctype run is left
// out: the fixture is ASCII, so another locale adds nothing.
func newGoldenStores(t *testing.T) (lite, pg goldenStore) {
	t.Helper()

	for _, d := range storetest.Drivers() {
		var g *goldenStore

		switch d.Name {
		case storetest.SQLite:
			g = &lite
		case storetest.Postgres:
			g = &pg
		default:
			continue
		}

		g.s, g.user = storetest.OpenStore(t, d)
		g.tools = golden.Load(t, g.s, g.user)
	}

	return lite, pg
}

// Parity floors for the golden comparisons. Postgres computes SQLite's BM25F
// over its tsvector, but the tsvector merges the description and the
// parameter descriptions into one field and its tokenizer does not split
// letter/digit or acronym boundaries, so the lower places of a top five can
// differ. When these were set, the mean lexical overlap was 0.92, every
// query but three overlapped by at least 0.8 (4 of 5), and the three
// lexicalNearTies overlapped by 0.6 (3 of 5). The floors leave room for that
// and still catch a broken idf, plural folding, or typo expansion, each of
// which sinks whole queries.
//
// Hybrid parity has no recorded evidence yet (no golden vectors file is
// checked in), so it holds every query to minQueryOverlap.
const (
	minMeanOverlap         = 0.8
	minQueryOverlap        = 0.6 // lexicalNearTies, and every hybrid query
	minLexicalQueryOverlap = 0.8 // every other lexical query
)

// lexicalNearTies are the golden queries whose lexical top five shares only
// 3 of 5 names with SQLite's. In each, places three to five are near-ties
// among tools that share the query's terms (pull-request tools, create_*
// tools reached by typo expansion, memory graph tools), which the merged
// tsvector field orders differently; the places a query expects agree.
func lexicalNearTies() []string {
	return []string{
		"pull request diff",
		"creat_isue",
		"knowlege graph", //nolint:misspell // a golden typo query, misspelt on purpose
	}
}

// overlap is the share of a's names that b also has; two empty lists agree.
func overlap(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}

	n := 0

	for _, name := range a {
		if slices.Contains(b, name) {
			n++
		}
	}

	return float64(n) / float64(max(len(a), len(b)))
}

// compare runs q on both stores, with vec as the query vector under modelID
// when vec is set. It fails t if the top fives overlap less than floor or
// Postgres's misses a name q expects, and returns the overlap and Postgres's
// top five.
func compare(t *testing.T, lite, pg goldenStore, q golden.Query, vec []float32, modelID string, floor float64) (float64, []string) {
	t.Helper()
	lq, pq := q.SearchQuery(lite.user), q.SearchQuery(pg.user)

	if vec != nil {
		lq.Vector, lq.ModelID = vec, modelID
		pq.Vector, pq.ModelID = vec, modelID
	}

	liteTop, pgTop := golden.TopFive(t, lite.s, lq), golden.TopFive(t, pg.s, pq)
	ov := overlap(liteTop, pgTop)

	if ov < floor {
		t.Errorf("%s %q: top-5 overlap %.1f < %.1f\n sqlite   %v\n postgres %v", q.Kind, q.Query, ov, floor, liteTop, pgTop)
	}

	for _, want := range q.Expected() {
		if !slices.Contains(pgTop, want) {
			t.Errorf("%s %q: postgres top 5 %v is missing %s", q.Kind, q.Query, pgTop, want)
		}
	}

	return ov, pgTop
}
