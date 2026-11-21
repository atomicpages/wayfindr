package sqlite

import (
	"context"
	"database/sql"
	"slices"
)

// DB exposes the store's connection pool to the external test package, so
// tests can inspect per-connection settings without a production accessor.
func DB(s *Store) *sql.DB { return s.db }

// CorpusStats is the bookkeeping one user's corpus keeps up to date on every
// write: the entries, per-token document frequency, per-field length sums,
// and the trigram vocabulary that typo expansion scans.
type CorpusStats struct {
	Docs     []string            // qualified names, sorted
	DocFreq  map[string]int      // token → docs containing it
	LenSums  [numFields]int      // per-field token counts summed over docs
	Trigrams map[string][]string // vocabulary token → its trigrams, sorted
}

// IndexStats snapshots the live index and an index rebuilt from the stored
// rows the way Open builds one, both keyed by user ID. It holds the write
// lock, so the two describe the same committed state even while other
// goroutines write. A user whose corpus is empty is left out: the live index
// keeps an empty corpus after a delete, the rebuilt one has none.
func IndexStats(ctx context.Context, s *Store) (live, rebuilt map[string]CorpusStats, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	fresh, err := loadIndex(ctx, s.db)

	if err != nil {
		return nil, nil, err
	}

	return s.index.stats(), fresh.stats(), nil
}

func (x *index) stats() map[string]CorpusStats {
	x.mu.RLock()
	defer x.mu.RUnlock()
	out := map[string]CorpusStats{}

	for user, c := range x.users {
		st := CorpusStats{DocFreq: map[string]int{}, LenSums: c.lenSum, Trigrams: map[string][]string{}}

		for _, d := range c.docs {
			st.Docs = append(st.Docs, d.qualified)
		}

		slices.Sort(st.Docs)

		for tok, n := range c.df {
			st.DocFreq[tok] = n
		}

		for tok, grams := range c.grams {
			var g []string

			for gram := range grams {
				g = append(g, gram)
			}

			slices.Sort(g)
			st.Trigrams[tok] = g
		}

		if len(st.Docs) == 0 && len(st.DocFreq) == 0 && len(st.Trigrams) == 0 && st.LenSums == [numFields]int{} {
			continue
		}

		out[user] = st
	}

	return out
}
