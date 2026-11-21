package postgres_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/golden"
	"github.com/atomicpages/wayfindr/internal/store/postgres/pgtest"
)

// TestSearchIndexFollowsConcurrentCatalogWrites: search reads an index that
// catalog writes maintain (design §7.5), and the writes of one user's
// connectors share its vocabulary. Concurrent replacements and deletions,
// of different connectors and of the same one, must all succeed (no
// deadlock) and leave search ranking exactly as on the same catalog written
// serially.
func TestSearchIndexFollowsConcurrentCatalogWrites(t *testing.T) {
	ctx := context.Background()
	tools := golden.Tools(t)

	serial := openStore(t, pgtest.NewDatabase(t))
	serialUser, err := serial.EnsureUser(ctx)

	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	golden.Load(t, serial, serialUser)

	churned := openStore(t, pgtest.NewDatabase(t))
	user, err := churned.EnsureUser(ctx)

	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 1000)

	for connector, ts := range tools {
		// Each connector goes through full, half, deleted, and full again,
		// while another writer deletes and restores github's catalog.
		for _, writer := range []string{connector, "github"} {
			wg.Go(func() {
				for range 3 {
					steps := []func() error{
						func() error {
							_, err := churned.ApplySync(ctx, user, store.SyncOutcome{Connector: connector, Tools: ts})
							return err
						},
						func() error {
							_, err := churned.ApplySync(ctx, user, store.SyncOutcome{Connector: connector, Tools: ts[:len(ts)/2]})
							return err
						},
						func() error { return churned.DeleteCatalog(ctx, user, writer) },
					}

					for _, step := range steps {
						if err := step(); err != nil {
							errs <- err
						}
					}
				}
			})
		}
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent write: %v", err)
	}

	golden.Load(t, churned, user)

	// TopFive returns the qualified names of whatever page it is asked for.
	for _, q := range golden.Queries(t) {
		want := golden.TopFive(t, serial, store.SearchQuery{UserID: serialUser, Text: q.Query, Page: 1, Size: 50})
		got := golden.TopFive(t, churned, store.SearchQuery{UserID: user, Text: q.Query, Page: 1, Size: 50})

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Search(%q) after concurrent writes (-serial +concurrent):\n%s", q.Query, diff)
		}
	}
}
