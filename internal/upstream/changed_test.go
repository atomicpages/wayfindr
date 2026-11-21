package upstream_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/resolve"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// secretStore is a store with the local user, and a sealer under the
// current master key, for tests that store secrets the resolver opens.
type secretStore struct {
	st     store.Store
	user   string
	sealer *secrets.Sealer
}

func newSecretStore(t *testing.T) secretStore {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })
	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}

	sealer, err := secrets.NewSealer([]byte("0123456789abcdef0123456789abcdef"))

	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}

	return secretStore{st: st, user: user, sealer: sealer}
}

// put stores value as secret name, sealed by sealer, as a resolver under
// that master key does.
func (s secretStore) put(t *testing.T, sealer *secrets.Sealer, name, value string) {
	t.Helper()

	if err := resolve.New(s.user, os.LookupEnv, s.st, sealer, zerolog.Nop()).PutSecret(t.Context(), name, value); err != nil {
		t.Fatalf("put secret: %v", err)
	}
}

func (s secretStore) resolver() config.Resolver {
	return resolve.New(s.user, os.LookupEnv, s.st, s.sealer, zerolog.Nop())
}

func TestSecretChangedRetriesTheConnectorsWaitingOnTheSecret(t *testing.T) {
	oldKey, err := secrets.NewSealer([]byte("fedcba9876543210fedcba9876543210"))

	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}

	for _, tt := range []struct {
		name string
		// before stores what the secret is before it is set: nothing
		// (missing_value), or a value sealed under an old master key
		// (secret_unreadable).
		before   func(t *testing.T, s secretStore)
		wantKind upstream.Kind
		wantCase upstream.Case
	}{
		{"missing", func(*testing.T, secretStore) {}, upstream.KindMissingValue, upstream.CaseSecret},
		{"unreadable", func(t *testing.T, s secretStore) { s.put(t, oldKey, "token", "stale") },
			upstream.KindAuthConfig, upstream.CaseSecretUnreadable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newSecretStore(t)
			tt.before(t, s)
			m, rec := newManager(t, s.resolver())
			waiting := stdioConnector(t, fakeupstream.V20251125)
			waiting.Env["TOKEN"] = "${secret:token}"
			other := stdioConnector(t, fakeupstream.V20251125)
			other.Env["TOKEN"] = "${secret:other}"
			apply(context.Background(), m, nil, config.Connectors{
				"gh": waiting, "notes": other, "plain": stdioConnector(t, fakeupstream.V20251125),
			})

			if got := statusOf(t, m, "gh"); got.Kind != tt.wantKind || got.Case != tt.wantCase || got.Ref != "secret:token" {
				t.Fatalf("status before the secret is set = %+v, want %s (%s) on secret:token", got, tt.wantKind, tt.wantCase)
			}

			rec.reset()

			// What `wayfindr secret set token` does.
			s.put(t, s.sealer, "token", "s3cret")
			got := m.SecretChanged(context.Background(), "token")

			want := []upstream.ConnectorStatus{{Connector: "gh", State: upstream.StateReady}}

			if diff := cmp.Diff(want, got, ignoreSince, ignoreReason); diff != "" {
				t.Errorf("SecretChanged (-want +got):\n%s", diff)
			}

			for _, c := range []string{"notes", "plain"} {
				if got := rec.of(c); len(got) != 0 {
					t.Errorf("%s: OnStateChanged = %v, want none: it was not waiting on the secret", c, got)
				}
			}

			if got := statusOf(t, m, "notes"); got.Kind != upstream.KindMissingValue || got.Ref != "secret:other" {
				t.Errorf("notes: status = %+v, want still missing_value on secret:other", got)
			}
		})
	}
}

// unresolvedErr is what the auth RoundTripper returns when a value its
// config names (a client_secret, say) won't resolve: an auth config error
// wrapping the resolver's *config.ResolveError.
type unresolvedErr struct{ err error }

func (e unresolvedErr) Error() string    { return "unresolved value: " + e.err.Error() }
func (e unresolvedErr) AuthConfig() bool { return true }
func (e unresolvedErr) AuthCase() string { return "unresolved_value" }
func (e unresolvedErr) Unwrap() error    { return e.err }

// A connector whose auth config names a missing secret (client_secret =
// "${secret:cs}") waits on that secret like one whose headers do: setting
// it reconnects the connector (wayfindr-jrc4).
func TestSecretChangedRetriesAConnectorWhoseAuthWaitsOnTheSecret(t *testing.T) {
	s := newSecretStore(t)
	_, rerr := config.Template("${secret:cs}").Resolve(t.Context(), s.resolver())

	if rerr == nil {
		t.Fatal("resolve ${secret:cs} before it is set = nil error, want not found")
	}

	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	stub := &authStub{}
	stub.fail(unresolvedErr{rerr})
	m, _ := newManager(t, s.resolver(), func(o *upstream.Options) { o.AuthTransport = stub.transport })
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})

	if got := statusOf(t, m, "c"); got.Kind != upstream.KindMissingValue || got.Case != upstream.CaseSecret || got.Ref != "secret:cs" {
		t.Fatalf("status before the secret is set = %+v, want missing_value (secret) on secret:cs", got)
	}

	// What `wayfindr secret set cs` does; auth now resolves it.
	s.put(t, s.sealer, "cs", "s3cret")
	stub.fail(nil)
	got := m.SecretChanged(context.Background(), "cs")

	want := []upstream.ConnectorStatus{{Connector: "c", State: upstream.StateReady}}

	if diff := cmp.Diff(want, got, ignoreSince, ignoreReason); diff != "" {
		t.Errorf("SecretChanged (-want +got):\n%s", diff)
	}
}

func TestCredentialChangedRecoversANeedsAuthConnectorOnceItHasAToken(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20260728)
	stub := &authStub{}
	stub.fail(&needsAuthErr{link: "http://127.0.0.1:7777/auth/c"}) // never authorized
	m, rec := newManager(t, nil, func(o *upstream.Options) { o.AuthTransport = stub.transport })
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})

	if got := statusOf(t, m, "c").State; got != upstream.StateNeedsAuth {
		t.Fatalf("state = %q, want needs_auth before the token is stored", got)
	}

	rec.reset()

	stub.fail(nil) // the auth flow stored a token
	got := m.CredentialChanged(context.Background(), "c")

	want := upstream.ConnectorStatus{Connector: "c", State: upstream.StateReady}

	if diff := cmp.Diff(want, got, ignoreSince, ignoreReason); diff != "" {
		t.Errorf("CredentialChanged (-want +got):\n%s", diff)
	}

	wantStates := []upstream.State{upstream.StateStarting, upstream.StateReady}

	if diff := cmp.Diff(wantStates, rec.of("c")); diff != "" {
		t.Errorf("OnStateChanged (-want +got):\n%s", diff)
	}

	if got := rec.toolsChangedOf("c"); got != 1 {
		t.Errorf("OnToolsChanged calls = %d, want 1: the new session may serve new tools", got)
	}
}

func TestCredentialChangedMovesAReadyConnectorToNeedsAuthOnceItsCredentialIsDeleted(t *testing.T) {
	const link = "http://127.0.0.1:7777/auth/c"
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20260728)
	stub := &authStub{}
	m, _ := newManager(t, nil, func(o *upstream.Options) { o.AuthTransport = stub.transport })
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})

	if got := statusOf(t, m, "c").State; got != upstream.StateReady {
		t.Fatalf("state = %q, want ready before the revoke", got)
	}

	stub.fail(&needsAuthErr{link: link}) // the credential was deleted
	got := m.CredentialChanged(context.Background(), "c")

	// At once, not on the next call.
	want := upstream.ConnectorStatus{Connector: "c", State: upstream.StateNeedsAuth,
		Kind: upstream.KindNeedsAuth, Case: upstream.CaseNoCredential, Link: link}

	if diff := cmp.Diff(want, got, ignoreSince, ignoreReason); diff != "" {
		t.Errorf("CredentialChanged (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff(want, statusOf(t, m, "c"), ignoreSince, ignoreReason); diff != "" {
		t.Errorf("status (-want +got):\n%s", diff)
	}
}

func TestCredentialChangedOfAnUnknownConnectorIsUnknownConnector(t *testing.T) {
	m, _ := newManager(t, nil)

	got := m.CredentialChanged(context.Background(), "nope")

	want := upstream.ConnectorStatus{Connector: "nope", Kind: upstream.KindUnknownConnector}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("CredentialChanged (-want +got):\n%s", diff)
	}
}

// Drop closes a connector's session over its transport, the auth one
// included, and leaves its state alone; the next use connects again. A
// revoke drops the connector before it deletes the credential, so the
// session's DELETE still carries the token (wayfindr-zgp.13).
func TestDropClosesTheSessionAndTheNextUseReconnects(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	proxy := startProxy(t, srv.URL, "Authorization")
	m, rec := newManager(t, nil, func(o *upstream.Options) {
		o.AuthTransport = func(_ config.Connector, next http.RoundTripper) http.RoundTripper {
			return roundTripFunc(func(r *http.Request) (*http.Response, error) {
				r = r.Clone(r.Context())
				r.Header.Set("Authorization", "Bearer tok")

				return next.RoundTrip(r)
			})
		}
	})
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(proxy.URL())}})

	if got := statusOf(t, m, "c").State; got != upstream.StateReady {
		t.Fatalf("state = %q, want ready before the drop", got)
	}

	rec.reset()
	before := rec.toolsChangedOf("c")

	if err := m.Drop(context.Background(), "c"); err != nil {
		t.Fatalf("Drop: %v", err)
	}

	var deletes [][]string

	for _, r := range proxy.requests() {
		if r.Method == http.MethodDelete {
			deletes = append(deletes, r.Header)
		}
	}

	if diff := cmp.Diff([][]string{{"Bearer tok"}}, deletes); diff != "" {
		t.Errorf("session DELETEs (-want +got):\n%s", diff)
	}

	if got := rec.of("c"); len(got) != 0 {
		t.Errorf("OnStateChanged = %v, want none: Drop leaves the state alone", got)
	}

	if got := statusOf(t, m, "c").State; got != upstream.StateReady {
		t.Errorf("state after the drop = %q, want ready", got)
	}

	if _, err := m.ListTools(context.Background(), "c"); err != nil {
		t.Errorf("ListTools after the drop: %v, want a new session", err)
	}

	if got := rec.toolsChangedOf("c") - before; got != 1 {
		t.Errorf("OnToolsChanged calls after the drop = %d, want 1: the next use opened a new session", got)
	}
}

func TestDropOfAnUnknownConnectorDoesNothing(t *testing.T) {
	m, _ := newManager(t, nil)

	if err := m.Drop(context.Background(), "nope"); err != nil {
		t.Errorf("Drop = %v, want nil: there is nothing to drop", err)
	}
}

// A use of a held connector waits for the hold's release before it opens a
// session, so none opens between a revoke's Drop and its credential delete
// (wayfindr-t08.10).
func TestUseOfAHeldConnectorWaitsForTheRelease(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	m, rec := newManager(t, nil)
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
	release, err := m.Hold(context.Background(), "c")

	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	if err := m.Drop(context.Background(), "c"); err != nil {
		t.Fatalf("Drop: %v", err)
	}

	before := rec.toolsChangedOf("c")

	listed := make(chan error, 1)
	go func() {
		_, err := m.ListTools(context.Background(), "c")
		listed <- err
	}()

	select {
	case err := <-listed:
		t.Fatalf("ListTools of a held connector returned %v before the release", err)
	case <-time.After(200 * time.Millisecond):
	}

	if got := rec.toolsChangedOf("c") - before; got != 0 {
		t.Errorf("sessions opened during the hold = %d, want 0", got)
	}

	release()

	if err := <-listed; err != nil {
		t.Errorf("ListTools after the release: %v", err)
	}

	if got := rec.toolsChangedOf("c") - before; got != 1 {
		t.Errorf("sessions opened after the release = %d, want 1", got)
	}

	release() // idempotent
}

// A use waiting on a hold gives up with its context, as an *Error that
// leaves the connector's state alone; a second Hold waits for the first.
func TestUseWaitingOnAHoldEndsWithItsContext(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	m, _ := newManager(t, nil)
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
	release, err := m.Hold(context.Background(), "c")

	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = m.ListTools(ctx, "c")

	if e, ok := upstream.AsError(err); !ok || e.Kind != upstream.KindCanceled || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ListTools waiting on a hold past its deadline = %v, want canceled with %v", err, context.DeadlineExceeded)
	}

	if got := statusOf(t, m, "c").State; got != upstream.StateReady {
		t.Errorf("state = %q, want ready still", got)
	}

	if _, err := m.Hold(ctx, "c"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second Hold while held = %v, want %v", err, context.DeadlineExceeded)
	}
}

func TestHoldOfAnUnknownConnectorHoldsNothing(t *testing.T) {
	m, _ := newManager(t, nil)

	release, err := m.Hold(context.Background(), "nope")

	if err != nil {
		t.Fatalf("Hold = %v, want nil", err)
	}

	release()
}

// By the time CredentialChanged returns, the new session's OnToolsChanged
// has been called, even when another goroutine is delivering the
// connector's callbacks: the daemon then waits for the sync that call
// requested (catalog.Syncer.Await) rather than start a second one
// (wayfindr-t08.9). Here Apply's own delivery is held inside its
// OnToolsChanged while the credential changes.
func TestCredentialChangedReturnsOnceItsOnToolsChangedIsDelivered(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	var (
		mu    sync.Mutex
		calls int
	)
	first := make(chan struct{})
	release := make(chan struct{})
	m, _ := newManager(t, nil, func(o *upstream.Options) {
		o.OnToolsChanged = func(string) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()

			if n == 1 {
				close(first)
				<-release
			}
		}
	})
	applied := make(chan struct{})
	go func() {
		defer close(applied)
		apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
	}()
	<-first // Apply is delivering c's callbacks, and holds there

	type result struct {
		s     upstream.ConnectorStatus
		calls int
	}
	changed := make(chan result, 1)
	go func() {
		s := m.CredentialChanged(context.Background(), "c")
		mu.Lock()
		defer mu.Unlock()
		changed <- result{s, calls}
	}()
	var got result
	released := false

	select {
	case got = <-changed:
		// It returned while its own callback was still queued.
	case <-time.After(500 * time.Millisecond):
		close(release)
		released = true
		got = <-changed
	}

	if !released {
		close(release)
	}

	<-applied

	if got.s.State != upstream.StateReady {
		t.Errorf("CredentialChanged state = %q, want ready", got.s.State)
	}

	if got.calls != 2 {
		t.Errorf("OnToolsChanged calls when CredentialChanged returned = %d, want 2: Apply's and the new session's", got.calls)
	}
}

// SecretChanged, like CredentialChanged, returns only once the retried
// connectors' new sessions have had their OnToolsChanged delivered: the
// daemon's secret set then waits for those syncs, so its rows show the
// tools (wayfindr-t08.9). Here Apply's delivery of the connector's
// auth_error is held while the secret is set.
func TestSecretChangedReturnsOnceItsOnToolsChangedIsDelivered(t *testing.T) {
	s := newSecretStore(t)
	var (
		mu    sync.Mutex
		tools int
	)
	held := make(chan struct{})
	release := make(chan struct{})
	m, _ := newManager(t, s.resolver(), func(o *upstream.Options) {
		o.OnStateChanged = func(_ string, st upstream.State) {
			if st == upstream.StateAuthError {
				close(held)
				<-release
			}
		}
		o.OnToolsChanged = func(string) {
			mu.Lock()
			defer mu.Unlock()
			tools++
		}
	})
	waiting := stdioConnector(t, fakeupstream.V20251125)
	waiting.Env["TOKEN"] = "${secret:token}"
	applied := make(chan struct{})
	go func() {
		defer close(applied)
		apply(context.Background(), m, nil, config.Connectors{"gh": waiting})
	}()
	<-held // Apply is delivering gh's callbacks, and holds there

	s.put(t, s.sealer, "token", "s3cret")
	type result struct {
		statuses []upstream.ConnectorStatus
		tools    int
	}
	changed := make(chan result, 1)
	go func() {
		got := m.SecretChanged(context.Background(), "token")
		mu.Lock()
		defer mu.Unlock()
		changed <- result{got, tools}
	}()
	var got result
	released := false

	select {
	case got = <-changed:
		// It returned while its retry's callback was still queued.
	case <-time.After(500 * time.Millisecond):
		close(release)
		released = true
		got = <-changed
	}

	if !released {
		close(release)
	}

	<-applied

	if len(got.statuses) != 1 || got.statuses[0].State != upstream.StateReady {
		t.Errorf("SecretChanged = %+v, want gh ready", got.statuses)
	}

	if got.tools != 1 {
		t.Errorf("OnToolsChanged calls when SecretChanged returned = %d, want 1: the retry's new session's", got.tools)
	}
}
