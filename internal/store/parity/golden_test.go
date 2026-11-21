package parity_test

import (
	"slices"
	"testing"

	"github.com/atomicpages/wayfindr/internal/store/golden"
)

// TestLexicalParityOnGolden compares Postgres's lexical top five with
// SQLite's for every golden query (design §7.5: parity is top-5 overlap, not
// identical order). Every name a query expects, its first included, must
// also be in Postgres's top five, and Postgres must meet every golden
// expectation strictly (a query's first result first): it met 61 of 61
// when this floor was set, so any miss is a regression. Overlap floors are
// per query: minLexicalQueryOverlap, or minQueryOverlap for the named
// lexicalNearTies, whose overlaps are logged.
func TestLexicalParityOnGolden(t *testing.T) {
	lite, pg := newGoldenStores(t)
	queries := golden.Queries(t)

	var sum float64
	strict := 0

	for _, q := range queries {
		floor := minLexicalQueryOverlap
		nearTie := slices.Contains(lexicalNearTies(), q.Query)

		if nearTie {
			floor = minQueryOverlap
		}

		ov, pgTop := compare(t, lite, pg, q, nil, "", floor)
		sum += ov

		if nearTie {
			t.Logf("near-tie %s %q: top-5 overlap %.1f (floor %.1f)", q.Kind, q.Query, ov, floor)
		}

		if failures := q.Failures(pgTop); len(failures) > 0 {
			t.Errorf("%s %q: postgres misses golden expectations strictly: %v (top 5 %v)", q.Kind, q.Query, failures, pgTop)
			continue
		}

		strict++
	}

	for _, nt := range lexicalNearTies() {
		if !slices.ContainsFunc(queries, func(q golden.Query) bool { return q.Query == nt }) {
			t.Errorf("near-tie %q is not a golden query", nt)
		}
	}

	mean := sum / float64(len(queries))
	t.Logf("mean top-5 overlap %.3f; postgres meets %d of %d golden expectations strictly", mean, strict, len(queries))

	if mean < minMeanOverlap {
		t.Errorf("mean top-5 overlap %.3f < %.1f", mean, minMeanOverlap)
	}
}
