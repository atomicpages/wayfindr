package admin_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

func TestUILogin(t *testing.T) {
	st, user := newStore(t)
	expires := time.Date(2026, 11, 12, 10, 1, 0, 0, time.UTC)
	want := admin.UILogin{Path: "/ui/login#code", ExpiresAt: expires}

	t.Run("issues a login", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
		calls := 0
		opts.UILogin = func() admin.UILogin {
			calls++

			return want
		}
		c, _ := serve(t, opts)
		got, err := c.UILogin(context.Background())

		if err != nil {
			t.Fatalf("UILogin: %v", err)
		}

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("UILogin (-want +got):\n%s", diff)
		}

		if calls != 1 {
			t.Errorf("issued %d logins, want 1", calls)
		}
	})

	t.Run("no console is 501", func(t *testing.T) {
		c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))
		_, err := c.UILogin(context.Background())

		if got := statusOf(err); got != http.StatusNotImplemented {
			t.Errorf("status = %d (%v), want 501", got, err)
		}
	})
}

// TestLocalClient calls the admin handler in process: the same answers,
// and the same errors, as over a socket.
func TestLocalClient(t *testing.T) {
	st, user := newStore(t)
	u := &fakeUpstreams{states: []upstream.ConnectorStatus{{Connector: "github", State: upstream.StateReady}}}
	c := admin.NewLocalClient(admin.New(options(t, st, user, u, admin.ConfigState{Connectors: []string{"github"}})))

	s, err := c.Status(context.Background())

	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if len(s.Connectors) != 1 || s.Connectors[0].Name != "github" || s.Connectors[0].State != upstream.StateReady {
		t.Errorf("Status connectors = %+v, want github ready", s.Connectors)
	}

	_, err = c.UILogin(context.Background())

	if got := statusOf(err); got != http.StatusNotImplemented {
		t.Errorf("UILogin status = %d (%v), want 501", got, err)
	}
}
