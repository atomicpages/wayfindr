package parity_test

import (
	"slices"
	"testing"

	"github.com/atomicpages/wayfindr/internal/store/golden"
)

// TestHybridParityOnGolden compares the drivers' hybrid top five on the
// golden fixture with real embeddings, at the lexical parity floors, and
// requires every name a lexical or hybrid query expects in Postgres's top
// five. It runs once per golden vectors file and skips when there is none
// (wayfindr-mvg.4).
func TestHybridParityOnGolden(t *testing.T) {
	files := golden.VectorsFiles(t)

	if len(files) == 0 {
		t.Skip("no golden vectors-*.json: generate one with a live provider (wayfindr-mvg.4; see internal/store/golden/README.md)")
	}

	for _, f := range files {
		t.Run(f, func(t *testing.T) { runHybridParity(t, golden.LoadVectors(t, f)) })
	}
}

func runHybridParity(t *testing.T, gv golden.Vectors) {
	lite, pg := newGoldenStores(t)
	gv.Embed(t, lite.s, lite.user, lite.tools)
	gv.Embed(t, pg.s, pg.user, pg.tools)

	queries := slices.Concat(golden.Queries(t), golden.HybridQueries(t))
	var sum float64

	for _, q := range queries {
		ov, _ := compare(t, lite, pg, q, gv.QueryVector(t, q.Query), gv.ModelID, minQueryOverlap)
		sum += ov
	}

	mean := sum / float64(len(queries))
	t.Logf("mean hybrid top-5 overlap %.3f over %d queries", mean, len(queries))

	if mean < minMeanOverlap {
		t.Errorf("mean hybrid top-5 overlap %.3f < %.1f", mean, minMeanOverlap)
	}
}
