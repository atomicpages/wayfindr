package auth_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

func TestDiscoverIssuerMismatch(t *testing.T) {
	t.Run("validation on is auth_error", func(t *testing.T) {
		s := fakeas.Start(t, fakeas.WithIssuerMismatch())
		h := newHarness(t)
		_, err := h.d.Discover(t.Context(), auth.DiscoverRequest{Connector: "c", ResourceURL: s.ResourceURL, Auth: defaultAuth()})
		e, ok := auth.AsError(err)

		if !ok {
			t.Fatalf("Discover error = %v, want an *auth.Error", err)
		}

		if diff := cmp.Diff([]any{auth.KindConfig, auth.CaseIssuerMismatch}, []any{e.Kind, e.Case}); diff != "" {
			t.Errorf("Kind, Case (-want +got):\n%s", diff)
		}

		for _, want := range []string{s.Issuer, s.MetadataIssuer, s.MetadataURL} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error text %q lacks %q", err, want)
			}
		}
	})

	t.Run("validation off proceeds and warns on every skip", func(t *testing.T) {
		s := fakeas.Start(t, fakeas.WithIssuerMismatch())
		h := newHarness(t)
		a := defaultAuth()
		a.ValidateIssuer = false
		req := auth.DiscoverRequest{Connector: "c", ResourceURL: s.ResourceURL, Auth: a}

		for i := range 2 {
			got, err := h.d.Discover(t.Context(), req)

			if err != nil {
				t.Fatalf("Discover %d: %v", i, err)
			}

			if got.Metadata == nil || got.Metadata.Issuer != s.MetadataIssuer {
				t.Fatalf("Discover %d metadata = %+v, want issuer %q", i, got.Metadata, s.MetadataIssuer)
			}

			if diff := cmp.Diff(metadataEndpoints(s), got.Endpoints); diff != "" {
				t.Errorf("endpoints (-want +got):\n%s", diff)
			}
		}

		if n := strings.Count(h.log.String(), `"message":"issuer validation skipped"`); n != 2 {
			t.Errorf("issuer-skip warnings = %d, want 2 (one per discovery)\nlog:\n%s", n, h.log)
		}

		if !strings.Contains(h.log.String(), `"level":"warn"`) {
			t.Errorf("skip was not logged at warn level:\n%s", h.log)
		}
	})

	t.Run("a trailing slash in PRM is the same issuer", func(t *testing.T) {
		s := fakeas.Start(t, fakeas.WithPRMIssuerTrailingSlash())
		h := newHarness(t)
		got, err := h.d.Discover(t.Context(), auth.DiscoverRequest{Connector: "c", ResourceURL: s.ResourceURL, Auth: defaultAuth()})

		if err != nil {
			t.Fatalf("Discover: %v", err)
		}

		if diff := cmp.Diff(metadataEndpoints(s), got.Endpoints); diff != "" {
			t.Errorf("endpoints (-want +got):\n%s", diff)
		}

		if strings.Contains(h.log.String(), "issuer validation skipped") {
			t.Errorf("unexpected skip warning:\n%s", h.log)
		}
	})

	t.Run("validation on with a matching issuer does not warn", func(t *testing.T) {
		s := fakeas.Start(t)
		h := newHarness(t)

		if _, err := h.d.Discover(t.Context(), auth.DiscoverRequest{Connector: "c", ResourceURL: s.ResourceURL, Auth: defaultAuth()}); err != nil {
			t.Fatalf("Discover: %v", err)
		}

		if strings.Contains(h.log.String(), "issuer validation skipped") {
			t.Errorf("unexpected skip warning:\n%s", h.log)
		}
	})
}
