package sqlite_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
)

// consistencyWords build random descriptions; every description also
// contains "catalog", so a "catalog" query matches every entry. Singulars
// and plurals of the same word are both present, along with words that fold
// in each way design §7.5 lists (queries, classes, indexes, branches) and
// words folding leaves alone or mangles (status, ties, series), so two docs
// can share a folded token through different surface forms.
func consistencyWords() []string {
	return []string{
		"issue", "issues", "project", "projects", "query", "queries", "branch", "branches",
		"class", "classes", "index", "indexes", "label", "labels", "status", "series", "ties",
		"channel", "message", "release",
	}
}

// consistencyQueries cover exact, folded, and typo-expanded lookups.
func consistencyQueries() []string {
	return append([]string{"catalog", "create issue", "projct", "serie", "brnches", "clases", "statuss", "id"},
		consistencyWords()...)
}

func consistencyConnectors() []string { return []string{"github", "linear", "slack"} }

func consistencyNames() []string { return []string{"create", "list", "get", "update", "delete"} }

// newRandomTool builds an entry whose content hash follows its description,
// as the catalog's content hash does. It does not take t, so writer
// goroutines can call it.
func newRandomTool(r *rand.Rand, connector, name string) (store.Tool, error) {
	words := consistencyWords()
	desc := "catalog"

	for range 1 + r.IntN(3) {
		desc += " " + words[r.IntN(len(words))]
	}

	hash := sha256.Sum256([]byte(desc))
	definition := fmt.Sprintf(`{"name":%q,"title":%q,"description":%q,"inputSchema":{"type":"object","properties":{"%s":{"type":"string","description":"%s id"}}}}`,
		name, words[r.IntN(len(words))], desc, words[r.IntN(len(words))], words[r.IntN(len(words))])

	return store.NewTool(connector, json.RawMessage(definition), hash[:])
}

func randomTool(t *testing.T, r *rand.Rand, connector, name string) store.Tool {
	t.Helper()
	tl, err := newRandomTool(r, connector, name)

	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}

	return tl
}

// catalogWrite is one random catalog write as randomWrite applied it.
type catalogWrite struct {
	op              string // replace, delete, or embed
	user, connector string
	names           []string // the connector's tool names after a replace
}

func (w catalogWrite) String() string { return w.op + " " + w.connector + " for " + w.user }

// randomWrite applies one random catalog write for a random user: replace a
// connector with a random subset of tools, delete a connector, or store
// embeddings for a few pending entries.
func randomWrite(ctx context.Context, s store.Store, r *rand.Rand, users []string) (catalogWrite, error) {
	connectors := consistencyConnectors()
	w := catalogWrite{user: users[r.IntN(len(users))], connector: connectors[r.IntN(len(connectors))]}

	switch r.IntN(4) {
	case 0, 1:
		w.op = "replace"
		w.names = []string{}
		var tools []store.Tool

		for _, name := range consistencyNames() {
			if r.IntN(2) == 0 {
				tl, err := newRandomTool(r, w.connector, name)

				if err != nil {
					return w, err
				}

				tools = append(tools, tl)
				w.names = append(w.names, name)
			}
		}

		if _, err := s.ApplySync(ctx, w.user, store.SyncOutcome{Connector: w.connector, Tools: tools}); err != nil {
			return w, fmt.Errorf("ApplySync: %w", err)
		}
	case 2:
		w.op = "delete"

		if err := s.DeleteCatalog(ctx, w.user, w.connector); err != nil {
			return w, fmt.Errorf("DeleteCatalog: %w", err)
		}
	case 3:
		w.op = "embed"
		pending, err := s.PendingEmbeddings(ctx, w.user, "m1", 1+r.IntN(4))

		if err != nil {
			return w, fmt.Errorf("PendingEmbeddings: %w", err)
		}

		var vecs []store.ToolVector

		for _, p := range pending {
			vecs = append(vecs, store.ToolVector{Connector: p.Connector, Name: p.Name, ContentHash: p.ContentHash,
				Vector: []float32{r.Float32(), r.Float32(), r.Float32()}})
		}

		if err := s.SetEmbeddings(ctx, w.user, "m1", vecs); err != nil {
			return w, fmt.Errorf("SetEmbeddings: %w", err)
		}
	}

	return w, nil
}

// addUser inserts a second user row. The store has no API for one (there is
// a single local user), but the index is keyed by user and must keep their
// catalogs apart.
func addUser(t *testing.T, s *sqlite.Store, id string) {
	t.Helper()

	if _, err := sqlite.DB(s).ExecContext(context.Background(),
		`INSERT INTO users (id, created_at) VALUES (?, 0)`, id); err != nil {
		t.Fatalf("insert user %s: %v", id, err)
	}
}

// twoUsers opens a store at path with the local user and one more.
func twoUsers(t *testing.T, path string) (*sqlite.Store, []string) {
	t.Helper()
	s := openStore(t, path)
	local, err := s.EnsureUser(context.Background())

	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	addUser(t, s, "second-user")

	return s, []string{local, "second-user"}
}

// searchAll returns every page of a query, concatenated.
func searchAll(t *testing.T, s store.Store, user, text string) []store.Tool {
	t.Helper()
	var all []store.Tool

	for p := 1; ; p++ {
		page := search(t, s, store.SearchQuery{UserID: user, Text: text, Page: p, Size: 3})
		all = append(all, page.Tools...)

		if !page.HasMore {
			return all
		}
	}
}

// checkMatchesRebuiltIndex fails unless the live index's bookkeeping (docs,
// document frequencies, field length sums, trigram vocabulary) equals that
// of an index rebuilt from the stored rows, for every user.
func checkMatchesRebuiltIndex(t *testing.T, s *sqlite.Store, after string) {
	t.Helper()
	live, rebuilt, err := sqlite.IndexStats(context.Background(), s)

	if err != nil {
		t.Fatalf("%s: IndexStats: %v", after, err)
	}

	if diff := cmp.Diff(rebuilt, live, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("%s: index internals live vs rebuilt from rows (-rebuilt +live):\n%s", after, diff)
	}
}

// checkMatchesReopenedSearch fails unless every query ranks the same for
// every user on s as on a store reopened from path, whose index is built
// from the rows.
func checkMatchesReopenedSearch(t *testing.T, s store.Store, path string, users []string, after string) {
	t.Helper()
	reopened := openStore(t, path)

	for _, user := range users {
		for _, q := range consistencyQueries() {
			got, want := searchAll(t, s, user, q), searchAll(t, reopened, user, q)

			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("%s: user %s: Search(%q) live vs reopened (-reopened +live):\n%s", after, user, q, diff)
			}
		}
	}

	if err := reopened.Close(); err != nil {
		t.Fatalf("Close reopened: %v", err)
	}
}

// TestSearchIndexStaysConsistentWithStoredRows applies seeded random
// sequences of catalog writes across two users. After each write, the live
// index's internals must equal those of an index rebuilt from the rows, and
// a query matching everything must return exactly each user's stored
// entries; every fifth write, search results must also equal those of a
// store reopened from disk.
func TestSearchIndexStaysConsistentWithStoredRows(t *testing.T) {
	ctx := context.Background()

	for seed := range uint64(4) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := rand.New(rand.NewPCG(seed, 0)) //nolint:gosec // seeded so failures reproduce.
			path := filepath.Join(t.TempDir(), "wayfindr.db")
			s, users := twoUsers(t, path)
			model := map[string]map[string][]string{} // user → connector → tool names in the catalog

			for _, u := range users {
				model[u] = map[string][]string{}
			}

			for step := range 25 {
				w, err := randomWrite(ctx, s, r, users)

				if err != nil {
					t.Fatalf("step %d (%s): %v", step, w, err)
				}

				switch w.op {
				case "replace":
					model[w.user][w.connector] = w.names
				case "delete":
					delete(model[w.user], w.connector)
				}

				after := fmt.Sprintf("step %d (%s)", step, w)

				checkMatchesRebuiltIndex(t, s, after)

				// Reopening is the slow check, and the internals above
				// already cover what ranking reads, so rank every few steps.
				if step%5 == 4 {
					checkMatchesReopenedSearch(t, s, path, users, after)
				}

				for _, user := range users {
					var wantNames []string

					for c, names := range model[user] {
						for _, name := range names {
							wantNames = append(wantNames, c+"."+name)
						}
					}

					slices.Sort(wantNames)
					var gotNames []string

					for _, tl := range searchAll(t, s, user, "catalog") {
						gotNames = append(gotNames, tl.Connector+"."+tl.Name)
						stored, err := s.GetTool(ctx, user, tl.Connector, tl.Name)

						if err != nil {
							t.Fatalf("%s: user %s: GetTool %s.%s: %v", after, user, tl.Connector, tl.Name, err)
						}

						if diff := cmp.Diff(stored, tl); diff != "" {
							t.Fatalf("%s: user %s: search result vs GetTool (-stored +search):\n%s", after, user, diff)
						}
					}

					slices.Sort(gotNames)

					if diff := cmp.Diff(wantNames, gotNames); diff != "" {
						t.Fatalf("%s: user %s: catalog query (-want +got):\n%s", after, user, diff)
					}
				}
			}
		})
	}
}

// TestSearchIndexStaysConsistentUnderConcurrentWrites runs seeded random
// catalog writes from several goroutines for two users while others search.
// Once they finish, the index must match one rebuilt from the rows, and
// every query must rank as it does on a store reopened from disk. Run under
// -race it also checks that searches and writes share the index safely.
func TestSearchIndexStaysConsistentUnderConcurrentWrites(t *testing.T) {
	ctx := context.Background()

	for seed := range uint64(2) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wayfindr.db")
			s, users := twoUsers(t, path)

			var writers, searchers sync.WaitGroup
			done := make(chan struct{})

			for g := range uint64(4) {
				r := rand.New(rand.NewPCG(seed, g+1)) //nolint:gosec // seeded so failures reproduce.
				writers.Go(func() {
					for range 15 {
						if w, err := randomWrite(ctx, s, r, users); err != nil {
							t.Errorf("writer %d (%s): %v", g, w, err)

							return
						}
					}
				})
			}

			for g := range 3 {
				searchers.Go(func() {
					queries := consistencyQueries()

					for i := 0; ; i++ {
						select {
						case <-done:
							return
						default:
						}

						q := store.SearchQuery{UserID: users[i%len(users)], Text: queries[(g+i)%len(queries)], Page: 1, Size: 5}

						if _, err := s.Search(ctx, q); err != nil {
							t.Errorf("Search(%q): %v", q.Text, err)

							return
						}
					}
				})
			}

			writers.Wait()
			close(done)
			searchers.Wait()

			if t.Failed() {
				return
			}

			checkMatchesRebuiltIndex(t, s, "after concurrent writes")
			checkMatchesReopenedSearch(t, s, path, users, "after concurrent writes")
		})
	}
}

// TestSearchRunsConcurrentlyWithCatalogWrites exists for the race detector.
func TestSearchRunsConcurrentlyWithCatalogWrites(t *testing.T) {
	ctx := context.Background()
	s, user := newStore(t)
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // seeded so failures reproduce.
	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			for range 20 {
				if _, err := s.Search(ctx, store.SearchQuery{UserID: user, Text: "catalog issue", Page: 1, Size: 5}); err != nil {
					t.Errorf("Search: %v", err)

					return
				}
			}
		})
	}

	for i := range 10 {
		connector := []string{"github", "linear"}[i%2]
		apply(t, s, user, connector, randomTool(t, r, connector, "create"), randomTool(t, r, connector, "list"))

		if i%3 == 0 {
			if err := s.DeleteCatalog(ctx, user, connector); err != nil {
				t.Fatalf("DeleteCatalog: %v", err)
			}
		}
	}

	wg.Wait()

	if t.Failed() {
		return
	}

	// The last write per connector: github was replaced at i=8, linear
	// deleted at i=9.
	var got []string

	for _, tl := range searchAll(t, s, user, "catalog") {
		got = append(got, tl.Connector+"."+tl.Name)
	}

	if diff := cmp.Diff([]string{"github.create", "github.list"}, got, cmpopts.SortSlices(strings.Compare)); diff != "" {
		t.Errorf("after writes, catalog query (-want +got):\n%s", diff)
	}
}
