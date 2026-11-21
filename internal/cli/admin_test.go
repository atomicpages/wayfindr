package cli

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/httpd"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// The CLI tests run against the real admin API (admin.New behind httpd's
// guards) over stub dependencies, so they speak the wire the daemon does.

// fakeUpstreams stands in for the manager: it reports states, and a
// SecretChanged reports the connectors in retried under the secret's name,
// in their states after the retry.
type fakeUpstreams struct {
	states  []upstream.ConnectorStatus
	retried map[string][]upstream.ConnectorStatus
}

func (u *fakeUpstreams) Status() []upstream.ConnectorStatus { return slices.Clone(u.states) }

func (u *fakeUpstreams) SecretChanged(_ context.Context, name string) []upstream.ConnectorStatus {
	return slices.Clone(u.retried[name])
}

// fakeStore is the part of the store the admin API uses, in memory.
type fakeStore struct {
	mu      sync.Mutex
	tools   map[string]int
	syncs   map[string]store.SyncRecord
	creds   []string
	secrets map[string]string
	// deleted are the secrets DeleteSecret removed, in order.
	deleted []string
	// deleteErr fails every DeleteSecret.
	deleteErr error
	calls     []store.Call
	// filter is the last ListCalls filter.
	filter store.CallFilter
}

// ListCalls returns the fake's calls as they are, up to f.Limit; the real
// filtering is the store's, tested there.
func (s *fakeStore) ListCalls(_ context.Context, f store.CallFilter) ([]store.Call, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filter = f

	return s.calls[:min(len(s.calls), f.Limit)], nil
}

// lastFilter returns the filter of the last ListCalls.
func (s *fakeStore) lastFilter() store.CallFilter {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.filter
}

func (s *fakeStore) ToolCounts(context.Context, string) (map[string]int, error) {
	return s.tools, nil
}

func (s *fakeStore) LastSync(_ context.Context, _, connector string) (store.SyncRecord, error) {
	rec, ok := s.syncs[connector]

	if !ok {
		return store.SyncRecord{}, store.ErrNotFound
	}

	return rec, nil
}

func (s *fakeStore) ListCredentialConnectors(context.Context, string) ([]string, error) {
	return s.creds, nil
}

// PutSecret is the daemon's secret writer: it stores value as it is.
func (s *fakeStore) PutSecret(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.secrets == nil {
		s.secrets = map[string]string{}
	}

	s.secrets[name] = value

	return nil
}

// DeleteSecret is the daemon's secret remover: a secret it doesn't hold
// is store.ErrNotFound.
func (s *fakeStore) DeleteSecret(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.deleteErr != nil {
		return s.deleteErr
	}

	if _, ok := s.secrets[name]; !ok {
		return fmt.Errorf("secret %s: %w", name, store.ErrNotFound)
	}

	delete(s.secrets, name)
	s.deleted = append(s.deleted, name)

	return nil
}

// ListSecrets is the daemon's secret lister: the names it holds, sorted.
func (s *fakeStore) ListSecrets(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Sorted(maps.Keys(s.secrets)), nil
}

// deletedSecrets returns the secrets the daemon deleted, in order.
func (s *fakeStore) deletedSecrets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.deleted)
}

// stored returns the secrets the daemon stored, by name.
func (s *fakeStore) stored() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return maps.Clone(s.secrets)
}

// adminDaemonOver serves the admin API over u, st, and cfg behind httpd's
// guards with the test client token, and returns its host:port. Secrets
// are stored in, listed from, and deleted from st as they are. Each with changes the
// options first, to add credentials or a version, say.
func adminDaemonOver(t *testing.T, u admin.Upstreams, st *fakeStore, cfg admin.ConfigState, with ...func(*admin.Options)) string {
	t.Helper()
	opts := admin.Options{
		Upstreams:    u,
		Store:        st,
		UserID:       "local",
		PutSecret:    st.PutSecret,
		DeleteSecret: st.DeleteSecret,
		ListSecrets:  st.ListSecrets,
		Config:       func() admin.ConfigState { return cfg },
		Logger:       zerolog.New(zerolog.NewTestWriter(t)),
	}

	for _, w := range with {
		w(&opts)
	}

	return serveAdminAPI(t, opts)
}

// serveAdminAPI serves admin.New(opts) behind httpd's guards with the test
// client token, and returns its host:port.
func serveAdminAPI(t *testing.T, opts admin.Options) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	h, err := httpd.NewHandler(httpd.Config{
		Port:     srv.Listener.Addr().(*net.TCPAddr).Port,
		Token:    testToken,
		Logger:   zerolog.Nop(),
		Handlers: httpd.Handlers{Admin: admin.New(opts)},
	})

	if err != nil {
		t.Fatalf("httpd.NewHandler: %v", err)
	}

	srv.Config.Handler = h
	srv.Start()
	t.Cleanup(srv.Close)

	return srv.Listener.Addr().String()
}
