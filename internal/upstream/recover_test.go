package upstream_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/resolve"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// callSeen wraps next so that each tools/call request that reaches it is
// reported on seen as it is sent on.
func callSeen(next http.RoundTripper, seen chan<- struct{}) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body []byte

		if r.Body != nil {
			var err error

			if body, err = io.ReadAll(r.Body); err != nil {
				return nil, err
			}

			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		if bytes.Contains(body, []byte(`"tools/call"`)) {
			select {
			case seen <- struct{}{}:
			default:
			}
		}

		return next.RoundTrip(r)
	})
}

// releaseBarrier makes the second call to the fake's barrier tool over a
// client of its own, which releases the first.
func releaseBarrier(t *testing.T, url string) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "release", Version: "v0"}, nil)
	s, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url}, nil)

	if err != nil {
		t.Fatalf("connect to release the barrier: %v", err)
	}

	defer func() { _ = s.Close() }()
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: fakeupstream.ToolBarrier})

	if err != nil || res.IsError {
		t.Fatalf("release the barrier: %+v, %v", res, err)
	}
}

func TestSuccessOnSessionOpenedBeforeRevokeDoesNotClearNeedsAuth(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125, fakeupstream.WithBarrier(2))
	stub := &authStub{}
	seen := make(chan struct{}, 1)
	m, _ := newManager(t, nil, func(o *upstream.Options) {
		o.AuthTransport = func(c config.Connector, next http.RoundTripper) http.RoundTripper {
			return stub.transport(c, callSeen(next, seen))
		}
	})
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL), Timeout: config.Duration{Duration: 10 * time.Second}}})
	// probe is the session opened before the revoke, which the slow call
	// below runs over.
	probe, release, err := m.Acquire(context.Background(), "c")

	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	defer release()

	// A slow call is sent over the session before the credential is
	// revoked; the upstream answers it only after.
	slow := make(chan error, 1)
	go func() {
		_, err := m.Call(context.Background(), "c", fakeupstream.ToolBarrier, nil)
		slow <- err
	}()
	<-seen

	// The revoke: the next call needs auth, and its verdict closes the
	// session, which waits for the slow call to finish.
	stub.fail(&needsAuthErr{link: "http://127.0.0.1:7777/auth/c"})
	revoked := make(chan error, 1)
	go func() {
		_, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil)
		revoked <- err
	}()

	// Once the session is closing, it refuses new requests without sending
	// them.
	for {
		err := probe.Ping(t.Context(), nil)

		if errors.Is(err, mcp.ErrConnectionClosed) {
			break
		}

		time.Sleep(time.Millisecond)
	}

	releaseBarrier(t, srv.URL)

	if err := <-slow; err != nil {
		t.Fatalf("slow call: %v, want it to succeed", err)
	}

	if err := <-revoked; !isKind(err, upstream.KindNeedsAuth) {
		t.Fatalf("revoked call: %v, want needs_auth", err)
	}

	if got := statusOf(t, m, "c").State; got != upstream.StateNeedsAuth {
		t.Errorf("state = %q, want needs_auth: the success came over a session opened before the revoke", got)
	}
}

func TestRetryAfterAuthorizingMakesConnectorReady(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20260728)
	stub := &authStub{}
	m, _ := newManager(t, nil, func(o *upstream.Options) { o.AuthTransport = stub.transport })
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
	stub.fail(&needsAuthErr{link: "http://127.0.0.1:7777/auth/c"})

	if _, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil); !isKind(err, upstream.KindNeedsAuth) {
		t.Fatalf("Call after revoke: %v, want needs_auth", err)
	}

	stub.fail(nil) // the user authorized

	if err := m.Retry(context.Background(), "c"); err != nil {
		t.Fatalf("Retry: %v, want nil", err)
	}

	if got := statusOf(t, m, "c"); got.State != upstream.StateReady || got.Kind != "" {
		t.Errorf("status = %+v, want ready with no error", got)
	}
}

func TestCallOnNewSessionRecoversNeedsAuth(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20260728)
	stub := &authStub{}
	m, rec := newManager(t, nil, func(o *upstream.Options) { o.AuthTransport = stub.transport })
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
	stub.fail(&needsAuthErr{link: "http://127.0.0.1:7777/auth/c"})

	if _, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil); !isKind(err, upstream.KindNeedsAuth) {
		t.Fatalf("Call after revoke: %v, want needs_auth", err)
	}

	if got := statusOf(t, m, "c"); got.Kind != upstream.KindNeedsAuth || got.Case != upstream.CaseNoCredential {
		t.Errorf("status = %+v, want Kind needs_auth, Case no_credential", got)
	}

	rec.reset()

	stub.fail(nil) // the user authorized

	if _, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil); err != nil {
		t.Fatalf("Call after authorizing: %v", err)
	}

	if got := statusOf(t, m, "c"); got.State != upstream.StateReady || got.Kind != "" {
		t.Errorf("status = %+v, want ready with no error", got)
	}

	if got := rec.of("c"); len(got) == 0 || got[len(got)-1] != upstream.StateReady {
		t.Errorf("OnStateChanged = %v, want it to end in ready", got)
	}
}

func TestRetry(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20260728)

	t.Run("ready is a no-op", func(t *testing.T) {
		m, rec := newManager(t, nil)
		apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
		rec.reset()

		if err := m.Retry(context.Background(), "c"); err != nil {
			t.Fatalf("Retry: %v", err)
		}

		if got := rec.of("c"); len(got) != 0 {
			t.Errorf("OnStateChanged = %v, want none", got)
		}
	})
	t.Run("unknown connector", func(t *testing.T) {
		m, _ := newManager(t, nil)

		if err := m.Retry(context.Background(), "nope"); !isKind(err, upstream.KindUnknownConnector) {
			t.Errorf("Retry: %v, want unknown_connector", err)
		}
	})
	t.Run("still needs auth", func(t *testing.T) {
		stub := &authStub{}
		m, _ := newManager(t, nil, func(o *upstream.Options) { o.AuthTransport = stub.transport })
		stub.fail(&needsAuthErr{link: "http://127.0.0.1:7777/auth/c"})
		apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})

		if err := m.Retry(context.Background(), "c"); !isCase(err, upstream.KindNeedsAuth, upstream.CaseNoCredential) {
			t.Errorf("Retry: %v, want needs_auth (no_credential)", err)
		}

		if got := statusOf(t, m, "c").State; got != upstream.StateNeedsAuth {
			t.Errorf("state = %q, want needs_auth", got)
		}
	})
	t.Run("unreachable upstream is back", func(t *testing.T) {
		p := startProxy(t, srv.URL, "")
		p.stop()
		m, _ := newManager(t, nil)
		apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(p.URL())}})

		if got := statusOf(t, m, "c"); got.State != upstream.StateUnreachable || got.Kind != upstream.KindUnreachable {
			t.Fatalf("status = %+v, want unreachable with Kind unreachable", got)
		}

		p.restart()

		if err := m.Retry(context.Background(), "c"); err != nil {
			t.Fatalf("Retry: %v", err)
		}

		if got := statusOf(t, m, "c").State; got != upstream.StateReady {
			t.Errorf("state = %q, want ready", got)
		}
	})
}

var ignoreReason = cmpopts.IgnoreFields(upstream.ConnectorStatus{}, "Reason")

func TestSecretSealedUnderAnotherKeyIsAuthErrorNamingIt(t *testing.T) {
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })
	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}

	oldKey, err := secrets.NewSealer([]byte("0123456789abcdef0123456789abcdef"))

	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}

	newKey, err := secrets.NewSealer([]byte("fedcba9876543210fedcba9876543210"))

	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}

	// Stored before the master key changed.
	if err := resolve.New(user, os.LookupEnv, st, oldKey, zerolog.Nop()).PutSecret(t.Context(), "token", "s3cret"); err != nil {
		t.Fatalf("put secret: %v", err)
	}

	m, rec := newManager(t, resolve.New(user, os.LookupEnv, st, newKey, zerolog.Nop()))
	conn := stdioConnector(t, fakeupstream.V20251125)
	conn.Env["TOKEN"] = "${secret:token}"

	apply(context.Background(), m, nil, config.Connectors{"gh": conn})

	want := upstream.ConnectorStatus{Connector: "gh", State: upstream.StateAuthError,
		Kind: upstream.KindAuthConfig, Case: upstream.CaseSecretUnreadable, Ref: "secret:token"}

	if diff := cmp.Diff(want, statusOf(t, m, "gh"), ignoreSince, ignoreReason); diff != "" {
		t.Errorf("status (-want +got):\n%s", diff)
	}

	_, err = m.Call(context.Background(), "gh", fakeupstream.ToolEcho, nil)

	if e, ok := upstream.AsError(err); !ok || e.Kind != upstream.KindAuthConfig ||
		e.Case != upstream.CaseSecretUnreadable || e.Ref != "secret:token" {
		t.Errorf("Call: err = %v, want auth_config (secret_unreadable) naming secret:token", err)
	}

	// The Call's failed reconnect leaves it in auth_error, never
	// unreachable, which would be retried with backoff.
	wantStates := []upstream.State{upstream.StateStarting, upstream.StateAuthError}

	if diff := cmp.Diff(wantStates, rec.of("gh")); diff != "" {
		t.Errorf("OnStateChanged (-want +got):\n%s", diff)
	}
}

func TestRetryAfterSettingMissingSecretMakesConnectorReady(t *testing.T) {
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

	r := resolve.New(user, os.LookupEnv, st, sealer, zerolog.Nop())
	m, rec := newManager(t, r)
	conn := stdioConnector(t, fakeupstream.V20251125)
	conn.Env["TOKEN"] = "${secret:token}"
	apply(context.Background(), m, nil, config.Connectors{"gh": conn})
	want := upstream.ConnectorStatus{Connector: "gh", State: upstream.StateAuthError,
		Kind: upstream.KindMissingValue, Case: upstream.CaseSecret, Ref: "secret:token"}

	if diff := cmp.Diff(want, statusOf(t, m, "gh"), ignoreSince, ignoreReason); diff != "" {
		t.Fatalf("status before the secret is set (-want +got):\n%s", diff)
	}

	// What `wayfindr secret set token` does.
	if err := r.PutSecret(t.Context(), "token", "s3cret"); err != nil {
		t.Fatalf("put secret: %v", err)
	}

	if err := m.Retry(context.Background(), "gh"); err != nil {
		t.Fatalf("Retry: %v, want nil", err)
	}

	want = upstream.ConnectorStatus{Connector: "gh", State: upstream.StateReady}

	if diff := cmp.Diff(want, statusOf(t, m, "gh"), ignoreSince, ignoreReason); diff != "" {
		t.Errorf("status after Retry (-want +got):\n%s", diff)
	}

	// Sync-on-session: the child Retry started may serve new tools.
	if got := rec.toolsChangedOf("gh"); got != 1 {
		t.Errorf("OnToolsChanged calls = %d, want 1", got)
	}
}
