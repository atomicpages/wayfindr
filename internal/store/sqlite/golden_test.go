package sqlite_test

import (
	"slices"
	"testing"

	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/golden"
)

// newGoldenStore opens a store holding the golden fixture's tools.
func newGoldenStore(t *testing.T) (store.Store, string, map[string][]store.Tool) {
	t.Helper()
	s, user := newStore(t)

	return s, user, golden.Load(t, s, user)
}

// TestSearchGolden checks lexical search quality (design §10) against the
// golden fixture of real-style tool definitions. SQLite is held to every
// expectation; Postgres to a top-5 overlap with SQLite (internal/store/parity).
func TestSearchGolden(t *testing.T) {
	s, user, tools := newGoldenStore(t)
	queries := golden.Queries(t)
	total := 0

	for _, ts := range tools {
		total += len(ts)
	}

	t.Logf("golden fixture: %d tools, %d queries", total, len(queries))

	for _, q := range queries {
		t.Run(q.Kind+"/"+q.Query, func(t *testing.T) {
			for _, f := range q.Failures(golden.TopFive(t, s, q.SearchQuery(user))) {
				t.Error(f)
			}
		})
	}
}

// TestSearchGoldenHybrid checks hybrid search quality (design §10) with
// real embeddings: every lexical expectation in queries.json still holds,
// and so does every expectation in hybrid_queries.json, which lexical
// search alone cannot meet. It runs once per golden vectors file and skips
// when there is none; TestGenerateHybridGolden in internal/store/golden
// makes one.
func TestSearchGoldenHybrid(t *testing.T) {
	files := golden.VectorsFiles(t)

	if len(files) == 0 {
		t.Skip("no golden vectors-*.json: generate one with a live provider (wayfindr-mvg.4; see internal/store/golden/README.md)")
	}

	for _, f := range files {
		t.Run(f, func(t *testing.T) { runHybridGolden(t, golden.LoadVectors(t, f)) })
	}
}

func runHybridGolden(t *testing.T, gv golden.Vectors) {
	s, user, tools := newGoldenStore(t)
	gv.Embed(t, s, user, tools)

	lexical, hybrid := golden.Queries(t), golden.HybridQueries(t)
	run := func(t *testing.T, q golden.Query, withVector bool) []string {
		t.Helper()
		sq := q.SearchQuery(user)

		if withVector {
			sq.Vector, sq.ModelID = gv.QueryVector(t, q.Query), gv.ModelID
		}

		return q.Failures(golden.TopFive(t, s, sq))
	}

	for _, q := range slices.Concat(lexical, hybrid) {
		t.Run(q.Kind+"/"+q.Query, func(t *testing.T) {
			for _, f := range run(t, q, true) {
				t.Error(f)
			}
		})
	}

	// The hybrid expectations must be ones lexical search misses, or they
	// show no improvement.
	lexicalMet := 0

	for _, q := range hybrid {
		if len(run(t, q, false)) == 0 {
			lexicalMet++
		}
	}

	t.Logf("lexical-only meets %d of %d hybrid expectations", lexicalMet, len(hybrid))

	if lexicalMet == len(hybrid) {
		t.Error("lexical-only search meets every hybrid expectation; hybrid_queries.json shows no improvement")
	}
}
