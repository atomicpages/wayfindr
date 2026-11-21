package auth_test

import (
	"errors"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

func TestDiscoverCacheThenRefetch(t *testing.T) {
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	var (
		cold auth.Discovery
		req  auth.DiscoverRequest
	)
	// PRM path-inserted, then AS metadata at the root issuer.
	coldPaths := []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server"}

	// The fake lives only for this subtest, so it is gone afterwards.
	t.Run("server up", func(t *testing.T) {
		s := fakeas.Start(t)
		req = auth.DiscoverRequest{Connector: "c", ResourceURL: s.ResourceURL, Auth: defaultAuth()}

		h := newHarnessWithStore(t, st)
		cold, err = h.d.Discover(t.Context(), req)

		if err != nil {
			t.Fatalf("cold Discover: %v", err)
		}

		if diff := cmp.Diff(coldPaths, h.rec.take()); diff != "" {
			t.Errorf("cold requests (-want +got):\n%s", diff)
		}

		// A fresh Discoverer over the same store: everything is cached.
		h2 := newHarnessWithStore(t, st)
		warm, err := h2.d.Discover(t.Context(), req)

		if err != nil {
			t.Fatalf("cached Discover: %v", err)
		}

		if got := h2.rec.take(); len(got) != 0 {
			t.Errorf("cached Discover made requests %v, want none", got)
		}

		if diff := cmp.Diff(cold, warm); diff != "" {
			t.Errorf("cached discovery differs from fetched (-fetched +cached):\n%s", diff)
		}

		// After an auth failure the caller refetches once.
		refetch := req
		refetch.Refetch = true
		again, err := h2.d.Discover(t.Context(), refetch)

		if err != nil {
			t.Fatalf("refetch Discover: %v", err)
		}

		if diff := cmp.Diff(coldPaths, h2.rec.take()); diff != "" {
			t.Errorf("refetch requests (-want +got):\n%s", diff)
		}

		if diff := cmp.Diff(cold, again); diff != "" {
			t.Errorf("refetched discovery (-want +got):\n%s", diff)
		}
	})

	t.Run("server gone", func(t *testing.T) {
		h := newHarnessWithStore(t, st)
		got, err := h.d.Discover(t.Context(), req)

		if err != nil {
			t.Fatalf("cached Discover with the server gone: %v", err)
		}

		if diff := cmp.Diff(cold, got); diff != "" {
			t.Errorf("cached discovery (-want +got):\n%s", diff)
		}

		refetch := req
		refetch.Refetch = true
		_, err = h.d.Discover(t.Context(), refetch)

		if err == nil {
			t.Fatal("refetch with the server gone succeeded, want a transport error")
		}

		if e, ok := auth.AsError(err); !ok || e.Kind != auth.KindUnreachable {
			t.Errorf("refetch error %v, want kind unreachable", err)
		}

		var ue *url.Error

		if !errors.As(err, &ue) {
			t.Errorf("refetch error %v does not wrap the transport failure", err)
		}
	})
}
