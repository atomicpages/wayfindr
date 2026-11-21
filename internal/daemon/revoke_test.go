package daemon_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/goleak"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// The tests in this file cover the end of a credential (wayfindr-141):
// `wayfindr auth logout`'s call to the admin API, a connector whose url
// changes under its token, and what the status shows of credentials.

// linearAS is a fake authorization server in front of a fake MCP upstream
// pinned to version, whose authorized requests are recorded in seen.
func linearAS(t *testing.T, version string, seen *headerLog) *fakeas.Server {
	t.Helper()
	proxy := reverseProxy(t, fakeupstream.StartStreamable(t, version).URL)

	return fakeas.Start(t, fakeas.WithResourceHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		proxy.ServeHTTP(w, r)
	})))
}

// reverseProxy forwards to target, streaming responses as they come.
func reverseProxy(t *testing.T, target string) *httputil.ReverseProxy {
	t.Helper()
	u, err := url.Parse(target)

	if err != nil {
		t.Fatal(err)
	}

	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.FlushInterval = -1

	return proxy
}

// oauthConfig is a config with one OAuth connector, linear, at u.
func oauthConfig(listen, u string) map[string]any {
	return map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"linear": map[string]any{"url": u, "auth": map[string]any{"type": "oauth"}}},
	}
}

// authorizeLinear runs linear's auth flow as `wayfindr auth login` and the
// browser would, and waits until linear is ready.
func authorizeLinear(t *testing.T, r running) {
	t.Helper()
	c := admin.NewClient("http://"+r.addr, "", r.client)
	f, err := c.StartAuth(t.Context(), "linear", admin.AuthStart{})

	if err != nil {
		t.Fatalf("StartAuth: %v", err)
	}

	_, back := visit(t, f.AuthorizeURL)

	if status, _ := visit(t, back); status != http.StatusOK {
		t.Fatalf("callback = %d, want 200", status)
	}

	got := waitFlow(t, c, f.FlowID)

	if got.State != admin.FlowDone || got.Result == nil || got.Result.State != upstream.StateReady {
		t.Fatalf("WaitAuth = %+v, want done with linear ready", got)
	}
}

// waitFlow long-polls the flow until it is no longer pending.
func waitFlow(t *testing.T, c *admin.Client, id string) admin.AuthFlowStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)

	for {
		got, err := c.WaitAuth(t.Context(), id, 5*time.Second)

		if err != nil {
			t.Fatalf("WaitAuth: %v", err)
		}

		if got.State != admin.FlowPending {
			return got
		}

		if time.Now().After(deadline) {
			t.Fatalf("the flow never ended; last: %+v", got)
		}
	}
}

// rawStatus is the body of GET /admin/status as it is on the wire.
func rawStatus(t *testing.T, r running) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+r.addr+"/admin/status", nil)

	if err != nil {
		t.Fatal(err)
	}

	resp, err := r.client.Do(req)

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)

	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

// The acceptance test of revoke: the status shows the token's expiry and
// never the token; a revoke settles linear in needs_auth through the one
// recovery path, its answer shows it, and a second revoke finds nothing.
// The upstream is stateful (2025-11-25): the revoke closes linear's session
// while the token still exists, so the session's DELETE carries it and the
// upstream ends the session, whose reader goroutine goleak would otherwise
// find (wayfindr-zgp.13).
func TestRevokeMovesTheConnectorToNeedsAuth(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	var seen headerLog
	as := linearAS(t, fakeupstream.V20251125, &seen)
	h := newHost(t)
	r := serve(t, h, h.options(t), oauthConfig(freeAddr(t), as.ResourceURL))
	r.waitStatus(t, "linear needs_auth", func(s admin.Status) bool {
		c, ok := connector(s, "linear")

		return ok && c.State == upstream.StateNeedsAuth
	})
	authorizedAt := time.Now()
	authorizeLinear(t, r)

	// fakeas tokens live an hour, from the broker's clock.
	s := getStatus(t, r.client, "http://"+r.addr)
	c, _ := connector(s, "linear")

	if exp := c.TokenExpiresAt; exp == nil || exp.Before(authorizedAt.Add(59*time.Minute)) || exp.After(time.Now().Add(61*time.Minute)) {
		t.Errorf("token_expires_at = %v, want about an hour from now", exp)
	}

	if seen.len() == 0 {
		t.Fatal("the upstream saw no authorized request")
	}

	token := strings.TrimPrefix(seen.since(0)[0], "Bearer ")

	if raw := rawStatus(t, r); token == "" || strings.Contains(raw, token) {
		t.Errorf("the status holds the access token: %s", raw)
	}

	ac := admin.NewClient("http://"+r.addr, "", r.client)
	got, err := ac.Revoke(t.Context(), "linear")

	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if got.Result == nil || got.Result.State != upstream.StateNeedsAuth || got.Result.TokenExpiresAt != nil ||
		got.Result.Advice == nil || got.Result.Advice.Summary != "The connector has not been authorized yet." {
		t.Errorf("Revoke = %+v (result %+v), want linear in needs_auth with no token", got, got.Result)
	}

	// The resource handler sees only authorized requests: fakeas answers
	// an unauthorized one 401 itself.
	if diff := cmp.Diff([]string{"Bearer " + token}, seen.deleted()); diff != "" {
		t.Errorf("session DELETEs the upstream saw (-want +got):\n%s", diff)
	}

	s = getStatus(t, r.client, "http://"+r.addr)

	if c, _ := connector(s, "linear"); c.State != upstream.StateNeedsAuth || c.TokenExpiresAt != nil {
		t.Errorf("status of linear after the revoke = %+v, want needs_auth with no token expiry", c)
	}

	if _, err := ac.Revoke(t.Context(), "linear"); statusOf(err) != http.StatusNotFound {
		t.Errorf("second Revoke = %v, want 404: there is no credential left", err)
	}

	r.stop()
}

// A revoke right behind an auth flow's callback, while the recovery the
// stored token started is still connecting (its initialize is slowed),
// settles linear in needs_auth, and every session opened with the token
// is closed with it: the revoke drops the session only once that recovery
// has finished, and deletes the credential before its own recovery
// reconnects (wayfindr-t08.9). goleak finds any session the stateful
// upstream kept.
func TestRevokeDuringACredentialRecoveryClosesItsSessionWithTheToken(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	proxy := reverseProxy(t, fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL)
	var opened, seen headerLog
	as := fakeas.Start(t, fakeas.WithResourceHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		body, err := io.ReadAll(r.Body)

		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		if bytes.Contains(body, []byte(`"method":"initialize"`)) {
			opened.add(r.Header.Get("Authorization"))

			select {
			case <-time.After(300 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		proxy.ServeHTTP(w, r)
	})))
	h := newHost(t)
	r := serve(t, h, h.options(t), oauthConfig(freeAddr(t), as.ResourceURL))
	r.waitStatus(t, "linear needs_auth", func(s admin.Status) bool {
		c, ok := connector(s, "linear")

		return ok && c.State == upstream.StateNeedsAuth
	})
	ac := admin.NewClient("http://"+r.addr, "", r.client)
	f, err := ac.StartAuth(t.Context(), "linear", admin.AuthStart{})

	if err != nil {
		t.Fatalf("StartAuth: %v", err)
	}

	_, back := visit(t, f.AuthorizeURL)

	if status, _ := visit(t, back); status != http.StatusOK {
		t.Fatalf("callback = %d, want 200", status)
	}

	got, err := ac.Revoke(t.Context(), "linear")

	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if got.Result == nil || got.Result.State != upstream.StateNeedsAuth {
		t.Errorf("Revoke = %+v (result %+v), want linear in needs_auth", got, got.Result)
	}

	if diff := cmp.Diff(opened.since(0), seen.deleted()); diff != "" {
		t.Errorf("sessions closed (-opened +closed, by Authorization):\n%s", diff)
	}

	r.stop()
}

// Calls that race a revoke, arriving while it closes linear's session,
// wait until the credential is deleted and only then connect, without
// the token, so every session opened with the token is closed with it
// (wayfindr-t08.10). The revoke's session DELETE is held until the calls
// are under way. goleak finds any session the stateful upstream kept.
func TestRevokeRacingCallsClosesEverySessionWithTheToken(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	proxy := reverseProxy(t, fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL)
	var opened, seen headerLog
	var holding atomic.Bool
	deleting := make(chan struct{}, 1)
	release := make(chan struct{})
	as := fakeas.Start(t, fakeas.WithResourceHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		body, err := io.ReadAll(r.Body)

		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		if bytes.Contains(body, []byte(`"method":"initialize"`)) {
			opened.add(r.Header.Get("Authorization"))
		}

		if r.Method == http.MethodDelete && holding.CompareAndSwap(true, false) {
			deleting <- struct{}{}

			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		proxy.ServeHTTP(w, r)
	})))
	h := newHost(t)
	r := serve(t, h, h.options(t), oauthConfig(freeAddr(t), as.ResourceURL))
	r.waitStatus(t, "linear needs_auth", stateIs("linear", upstream.StateNeedsAuth))
	authorizeLinear(t, r)

	holding.Store(true)
	type revoked struct {
		got admin.Revoked
		err error
	}
	done := make(chan revoked, 1)
	go func() {
		got, err := admin.NewClient("http://"+r.addr, "", r.client).Revoke(t.Context(), "linear")
		done <- revoked{got, err}
	}()

	select {
	case <-deleting:
	case <-time.After(15 * time.Second):
		t.Fatal("the revoke never closed linear's session")
	}

	var calls sync.WaitGroup

	for range 4 {
		calls.Go(func() {
			// Whether a call fails (needs_auth) or not is not the point.
			_, _ = r.mcp.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      "call_tool",
				Arguments: map[string]any{"name": "linear.echo", "arguments": map[string]any{"text": "hi"}},
			})
		})
	}

	// The calls reach linear while its session is closing.
	time.Sleep(100 * time.Millisecond)
	close(release)

	res := <-done
	calls.Wait()

	if res.err != nil {
		t.Fatalf("Revoke: %v", res.err)
	}

	if res.got.Result == nil || res.got.Result.State != upstream.StateNeedsAuth {
		t.Errorf("Revoke = %+v (result %+v), want linear in needs_auth", res.got, res.got.Result)
	}

	if diff := cmp.Diff(opened.since(0), seen.deleted()); diff != "" {
		t.Errorf("sessions closed (-opened +closed, by Authorization):\n%s", diff)
	}

	r.stop()
}

// A credential whose connector was removed is listed as orphaned, the way
// startup and reload keep it, and revoke removes it like any other.
func TestRevokeRemovesAnOrphanedCredential(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	h := newHost(t)
	files := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	initHost(t, h, map[string]any{
		"server":     map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"files": map[string]any{"url": files.URL}},
	})
	putCredential(t, h, "old")
	r := start(t, h, h.options(t))
	r.waitStatus(t, "files synced, old orphaned", func(s admin.Status) bool {
		return synced("files")(s) && cmp.Equal([]string{"old"}, s.OrphanedCredentials)
	})

	got, err := admin.NewClient("http://"+r.addr, "", r.client).Revoke(t.Context(), "old")

	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if diff := cmp.Diff(admin.Revoked{Connector: "old"}, got); diff != "" {
		t.Errorf("Revoke (-want +got):\n%s", diff)
	}

	if s := getStatus(t, r.client, "http://"+r.addr); len(s.OrphanedCredentials) != 0 {
		t.Errorf("orphaned credentials after the revoke = %v, want none", s.OrphanedCredentials)
	}

	r.stop()
}

// A url change puts the connector in needs_auth (url_changed) on reload
// and on the next startup, and the token bound to the old url is never
// sent to the new one. A flow started for the old url is cancelled, so its
// late callback stores nothing.
func TestURLChangeNeedsAuthAndNeverSendsTheTokenToTheNewURL(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	var seenOld, seenNew headerLog
	as := linearAS(t, fakeupstream.V20251125, &seenOld)
	moved := recordingProxy(t, fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL, &seenNew)
	h := newHost(t)
	listen := freeAddr(t)
	r := serve(t, h, h.options(t), oauthConfig(listen, as.ResourceURL))
	r.waitStatus(t, "linear needs_auth", func(s admin.Status) bool {
		c, ok := connector(s, "linear")

		return ok && c.State == upstream.StateNeedsAuth
	})
	authorizeLinear(t, r)
	// A second flow for the old url, whose browser comes back only after
	// the url changed.
	ac := admin.NewClient("http://"+r.addr, "", r.client)
	f, err := ac.StartAuth(t.Context(), "linear", admin.AuthStart{})

	if err != nil {
		t.Fatalf("StartAuth: %v", err)
	}

	_, late := visit(t, f.AuthorizeURL)

	urlChanged := func(s admin.Status) bool {
		c, ok := connector(s, "linear")

		return ok && c.State == upstream.StateNeedsAuth && c.Advice != nil && strings.Contains(c.Advice.Summary, "URL changed")
	}
	h.writeConfig(t, oauthConfig(listen, moved.URL))
	r.waitStatus(t, "linear needs_auth because its url changed", urlChanged)

	if status, _ := visit(t, late); status != http.StatusBadRequest {
		t.Errorf("late callback = %d, want 400", status)
	}

	if got := waitFlow(t, ac, f.FlowID); got.State != admin.FlowFailed || got.Advice == nil ||
		!strings.HasPrefix(got.Advice.Summary, "The authorization was cancelled") {
		t.Errorf("WaitAuth of the flow for the old url = %+v, want failed as cancelled", got)
	}

	// On load, as on reload.
	r.stop()
	r = start(t, h, h.options(t))
	r.waitStatus(t, "linear needs_auth because its url changed, after a restart", urlChanged)

	for _, a := range seenNew.since(0) {
		if a != "" {
			t.Errorf("the new url got Authorization %q, want none", a)
		}
	}

	r.stop()
}
