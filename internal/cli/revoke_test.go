package cli

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// fakeCredentials is the daemon's side of a revoke: it answers report and
// records the connector.
type fakeCredentials struct {
	mu      sync.Mutex
	report  admin.RevokeReport
	revoked []string
}

func (f *fakeCredentials) Revoke(_ context.Context, connector string) (admin.RevokeReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, connector)

	return f.report, nil
}

// revokedConnectors returns the connectors Revoke was called for, in order.
func (f *fakeCredentials) revokedConnectors() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.revoked)
}

// revokeHost serves the admin API over creds and returns a host that
// reaches it.
func revokeHost(t *testing.T, creds admin.Credentials) Host {
	t.Helper()
	addr := serveAdminAPI(t, admin.Options{
		Upstreams:   &fakeUpstreams{},
		Store:       &fakeStore{},
		UserID:      "local",
		Config:      func() admin.ConfigState { return admin.ConfigState{} },
		Credentials: creds,
		Logger:      zerolog.New(zerolog.NewTestWriter(t)),
	})
	host, root := testHost(t)
	writeStatusHost(t, root, addr)

	return host
}

func TestAuthLogoutSaysHowTheConnectorCameOut(t *testing.T) {
	creds := &fakeCredentials{report: admin.RevokeReport{Found: true, Status: &upstream.ConnectorStatus{
		Connector: "linear", State: upstream.StateNeedsAuth,
		Kind: upstream.KindNeedsAuth, Case: upstream.CaseNoCredential, Link: "http://127.0.0.1:7777/auth/linear",
	}}}
	host := revokeHost(t, creds)
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"auth", "logout", "linear"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want := `Revoked the credential of linear.
linear: The connector has not been authorized yet.
  Open the link to authorize it.
  http://127.0.0.1:7777/auth/linear
`

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff([]string{"linear"}, creds.revokedConnectors()); diff != "" {
		t.Errorf("revoked (-want +got):\n%s", diff)
	}
}

func TestAuthLogoutOfAnOrphanedCredential(t *testing.T) {
	host := revokeHost(t, &fakeCredentials{report: admin.RevokeReport{Found: true}})
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"auth", "logout", "old"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if diff := cmp.Diff("Revoked the credential of old, which is no longer configured.\n", stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

func TestAuthLogoutWithoutACredentialSaysSoAndSucceeds(t *testing.T) {
	host := revokeHost(t, &fakeCredentials{})
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"auth", "logout", "linear"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if diff := cmp.Diff("linear has no stored credential; nothing to revoke.\n", stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

// `auth revoke` is the old name of `auth logout`, kept hidden.
func TestAuthLogoutAndTheOldRevokeBothDeleteTheCredential(t *testing.T) {
	for _, name := range []string{"logout", "revoke"} {
		t.Run(name, func(t *testing.T) {
			creds := &fakeCredentials{report: admin.RevokeReport{Found: true}}
			host := revokeHost(t, creds)
			var stdout, stderr bytes.Buffer

			if code := RunWith([]string{"auth", name, "linear"}, &stdout, &stderr, host); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			if diff := cmp.Diff([]string{"linear"}, creds.revokedConnectors()); diff != "" {
				t.Errorf("revoked (-want +got):\n%s", diff)
			}
		})
	}
}
