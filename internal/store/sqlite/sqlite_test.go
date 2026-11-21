package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
	"github.com/atomicpages/wayfindr/internal/store/storetest"
)

// TestStore runs the shared Store suite, search included. The tests in this
// package cover what only SQLite has: the in-memory search index, its
// tokenizer, and the connection pragmas.
func TestStore(t *testing.T) {
	storetest.RunDriver(t, storetest.SQLite)
}

func openStore(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), path)

	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

// apply stores a successful sync that found tools as the connector's
// catalog, and returns the names it reports changed.
func apply(t *testing.T, s store.Store, userID, connector string, tools ...store.Tool) []string {
	t.Helper()
	changed, err := s.ApplySync(context.Background(), userID,
		store.SyncOutcome{Connector: connector, At: time.UnixMilli(1_762_000_000_000), Tools: tools})

	if err != nil {
		t.Fatalf("ApplySync: %v", err)
	}

	return changed
}

func search(t *testing.T, s store.Store, q store.SearchQuery) store.SearchPage {
	t.Helper()
	page, err := s.Search(context.Background(), q)

	if err != nil {
		t.Fatalf("Search(%q): %v", q.Text, err)
	}

	return page
}

// newStore opens a fresh store in a temp dir and returns it with the local user.
func newStore(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	s := openStore(t, filepath.Join(t.TempDir(), "wayfindr.db"))
	userID, err := s.EnsureUser(context.Background())

	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	return s, userID
}

// A new database and its WAL file are private to the user, like every
// other file in the data dir (wayfindr-pzz9).
func TestOpenCreatesTheDatabase0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}

	path := filepath.Join(t.TempDir(), "wayfindr.db")
	s := openStore(t, path)

	if _, err := s.EnsureUser(context.Background()); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	for _, p := range []string{path, path + "-wal"} {
		fi, err := os.Stat(p)

		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}

		if m := fi.Mode().Perm(); m != 0o600 {
			t.Errorf("%s mode = %v, want 0600", filepath.Base(p), m)
		}
	}
}
