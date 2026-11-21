package admin_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// fakeCredentials stands in for the daemon's adapter of the auth broker:
// a revoke answers report (or err) and is recorded.
type fakeCredentials struct {
	mu      sync.Mutex
	report  admin.RevokeReport
	err     error
	revoked []string
}

func (f *fakeCredentials) Revoke(_ context.Context, connector string) (admin.RevokeReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, connector)

	return f.report, f.err
}

func serveCredentials(t *testing.T, f *fakeCredentials) *admin.Client {
	t.Helper()
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
	opts.Credentials = f
	c, _ := serve(t, opts)

	return c
}

func TestRevokeAnswersTheConnectorsRowAfterIt(t *testing.T) {
	since := time.Date(2026, 11, 5, 12, 1, 0, 0, time.UTC)
	f := &fakeCredentials{report: admin.RevokeReport{Found: true, Status: &upstream.ConnectorStatus{
		Connector: "linear", State: upstream.StateNeedsAuth, Since: since,
		Kind: upstream.KindNeedsAuth, Case: upstream.CaseNoCredential, Link: "http://127.0.0.1:7777/auth/linear",
	}}}
	c := serveCredentials(t, f)

	got, err := c.Revoke(t.Context(), "linear")

	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	want := admin.Revoked{Connector: "linear", Result: &admin.Connector{
		Name: "linear", State: upstream.StateNeedsAuth, Since: since,
		Advice: &admin.Advice{
			Summary: "The connector has not been authorized yet.",
			Action:  "Open the link to authorize it.",
			Link:    "http://127.0.0.1:7777/auth/linear",
		},
	}}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Revoke (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff([]string{"linear"}, f.revoked); diff != "" {
		t.Errorf("revoked (-want +got):\n%s", diff)
	}
}

// An orphaned credential's connector is no longer configured, so the
// answer has no row for it.
func TestRevokeAnOrphanedCredentialAnswersNoRow(t *testing.T) {
	c := serveCredentials(t, &fakeCredentials{report: admin.RevokeReport{Found: true}})

	got, err := c.Revoke(t.Context(), "old")

	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if diff := cmp.Diff(admin.Revoked{Connector: "old"}, got); diff != "" {
		t.Errorf("Revoke (-want +got):\n%s", diff)
	}
}

func TestRevokeErrors(t *testing.T) {
	tests := []struct {
		name   string
		f      *fakeCredentials
		status int
		want   string
	}{
		{"no credential", &fakeCredentials{}, http.StatusNotFound, `"linear" has no stored credential`},
		{"store failure", &fakeCredentials{err: errors.New("database is locked")},
			http.StatusInternalServerError, "revoking the credential failed; see the daemon log"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := serveCredentials(t, tt.f)

			_, err := c.Revoke(t.Context(), "linear")

			if err == nil {
				t.Fatal("Revoke: want an error")
			}

			if got := statusOf(err); got != tt.status {
				t.Errorf("status = %d, want %d", got, tt.status)
			}

			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestRevokeWithoutCredentialsIsNotImplemented(t *testing.T) {
	st, user := newStore(t)
	c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))

	if _, err := c.Revoke(t.Context(), "linear"); statusOf(err) != http.StatusNotImplemented {
		t.Errorf("Revoke = %v, want 501", err)
	}
}
