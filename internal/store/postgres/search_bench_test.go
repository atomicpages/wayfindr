package postgres_test

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/golden"
	"github.com/atomicpages/wayfindr/internal/store/postgres/pgtest"
)

// scaleVariants is how many copies of the golden fixture (about 200 tools)
// the search benchmark loads: 25 copies make a catalog of about 5,000 tools.
const scaleVariants = 25

// variantSuffixes name the copies' connectors, so each copy adds its own
// connector tokens to the vocabulary, as distinct accounts or tenants of one
// server would. The first copy keeps the fixture's names.
func variantSuffixes() []string {
	return []string{"", "acme", "prod", "staging", "eu", "us", "internal", "legacy", "beta",
		"team", "ops", "data", "sales", "support", "infra", "mobile", "web", "billing",
		"partner", "sandbox", "archive", "research", "finance", "people", "labs"}
}

// scaledCatalog copies the golden fixture scaleVariants times, by connector.
// Every copy's descriptions gain four words from a pool of pseudo-words, so
// the vocabulary grows with the catalog (to over 7,000 tokens) instead of
// staying at the fixture's, which matters to typo expansion: it compares a
// misspelt word with every vocabulary token.
func scaledCatalog(b *testing.B) map[string][]store.Tool {
	b.Helper()
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic fixture data, not security
	pool := make([]string, 6000)

	for i := range pool {
		var w strings.Builder

		for range 5 + rng.IntN(5) {
			w.WriteByte("abcdefghijklmnopqrstuvwxyz"[rng.IntN(26)])
		}

		pool[i] = w.String()
	}

	base := golden.Tools(b)
	connectors := make([]string, 0, len(base))

	for c := range base {
		connectors = append(connectors, c)
	}

	slices.Sort(connectors)

	out := map[string][]store.Tool{}

	for _, suffix := range variantSuffixes()[:scaleVariants] {
		for _, c := range connectors {
			connector := c

			if suffix != "" {
				connector = c + "_" + suffix
			}

			for _, tl := range base[c] {
				var def map[string]any

				if err := json.Unmarshal(tl.Definition, &def); err != nil {
					b.Fatalf("%s.%s: %v", c, tl.Name, err)
				}

				desc, _ := def["description"].(string)
				extra := make([]string, 4)

				for i := range extra {
					extra[i] = pool[rng.IntN(len(pool))]
				}

				def["description"] = desc + " " + strings.Join(extra, " ")
				raw, err := json.Marshal(def)

				if err != nil {
					b.Fatalf("%s.%s: %v", c, tl.Name, err)
				}

				nt, err := store.NewTool(connector, raw, raw)

				if err != nil {
					b.Fatalf("%s.%s: %v", connector, tl.Name, err)
				}

				out[connector] = append(out[connector], nt)
			}
		}
	}

	return out
}

// BenchmarkSearchAt5k measures lexical search latency on a catalog of about
// 5,000 tools (design §7.5), cycling through the golden queries, and reports
// the 50th and 95th percentile of single searches in milliseconds. The
// budget is a p95 under 50 ms (design §7.5 has the measurements). It needs
// WAYFINDR_TEST_PG_DSN; `make bench-postgres` runs it on a throwaway
// pgvector container.
func BenchmarkSearchAt5k(b *testing.B) {
	ctx := context.Background()
	s := openStore(b, pgtest.NewDatabase(b))
	user, err := s.EnsureUser(ctx)

	if err != nil {
		b.Fatalf("EnsureUser: %v", err)
	}

	n, catalog := 0, scaledCatalog(b)
	start := time.Now()

	for connector, ts := range catalog {
		if _, err := s.ApplySync(ctx, user, store.SyncOutcome{Connector: connector, Tools: ts}); err != nil {
			b.Fatalf("ApplySync %s: %v", connector, err)
		}

		n += len(ts)
	}

	queries := golden.Queries(b)
	b.Logf("catalog of %d tools in %d connectors written in %v; %d golden queries",
		n, len(catalog), time.Since(start).Round(time.Millisecond), len(queries))

	b.Run("lexical", func(b *testing.B) {
		for _, q := range queries { // warm the plan cache and the buffers
			if _, err := s.Search(ctx, q.SearchQuery(user)); err != nil {
				b.Fatalf("Search(%q): %v", q.Query, err)
			}
		}

		lat := make([]time.Duration, 0, b.N)
		b.ResetTimer()

		for i := range b.N {
			q := queries[i%len(queries)]
			start := time.Now()

			if _, err := s.Search(ctx, q.SearchQuery(user)); err != nil {
				b.Fatalf("Search(%q): %v", q.Query, err)
			}

			lat = append(lat, time.Since(start))
		}

		b.StopTimer()
		slices.Sort(lat)
		ms := func(p float64) float64 {
			return float64(lat[min(len(lat)-1, int(p*float64(len(lat))))].Microseconds()) / 1000
		}
		b.ReportMetric(ms(0.50), "p50-ms")
		b.ReportMetric(ms(0.95), "p95-ms")
		b.ReportMetric(ms(1), "max-ms")
	})
}
