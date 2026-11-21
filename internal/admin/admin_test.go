package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// fakeUpstreams stands in for the manager: it reports states, and a
// SecretChanged reports the connectors in retried under the secret's name,
// in their states after the retry, and records the name.
type fakeUpstreams struct {
	mu      sync.Mutex
	states  []upstream.ConnectorStatus
	retried map[string][]upstream.ConnectorStatus
	changed []string
}

func (u *fakeUpstreams) Status() []upstream.ConnectorStatus {
	u.mu.Lock()
	defer u.mu.Unlock()

	return slices.Clone(u.states)
}

func (u *fakeUpstreams) SecretChanged(_ context.Context, name string) []upstream.ConnectorStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.changed = append(u.changed, name)

	return slices.Clone(u.retried[name])
}

// secretsChanged are the names SecretChanged was called with, in order.
func (u *fakeUpstreams) secretsChanged() []string {
	u.mu.Lock()
	defer u.mu.Unlock()

	return slices.Clone(u.changed)
}

// newStore opens a SQLite store in a temp dir and returns it with the
// local user.
func newStore(t *testing.T) (store.Store, string) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })
	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	return st, user
}

func tool(t *testing.T, connector, name string) store.Tool {
	t.Helper()
	tl, err := store.NewTool(connector, json.RawMessage(`{"name":"`+name+`","inputSchema":{"type":"object"}}`), []byte(name))

	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}

	return tl
}

// options are admin.Options over st and u, with a PutSecret that stores
// the value prefixed by the secret's name, so a test can see which name it
// was stored under.
func options(t *testing.T, st store.Store, user string, u admin.Upstreams, cfg admin.ConfigState) admin.Options {
	t.Helper()

	return admin.Options{
		Upstreams: u,
		Store:     st,
		UserID:    user,
		PutSecret: func(ctx context.Context, name, value string) error {
			return st.PutSecret(ctx, user, name, []byte(name+":"+value))
		},
		DeleteSecret: func(ctx context.Context, name string) error {
			return st.DeleteSecret(ctx, user, name)
		},
		ListSecrets: func(ctx context.Context) ([]string, error) {
			return st.ListSecrets(ctx, user)
		},
		Config: func() admin.ConfigState { return cfg },
		Logger: zerolog.New(zerolog.NewTestWriter(t)),
	}
}

// serve serves admin.New(opts) and returns a client for it with no
// bearer: the admin API itself checks none (httpd does).
func serve(t *testing.T, opts admin.Options) (*admin.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(admin.New(opts))
	t.Cleanup(srv.Close)

	return admin.NewClient(srv.URL, "", srv.Client()), srv
}

// statusOf is the HTTP status of an *admin.APIError in err's chain, or 0.
func statusOf(err error) int {
	if ae := (*admin.APIError)(nil); errors.As(err, &ae) {
		return ae.Status
	}

	return 0
}
