package daemon_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// oauthDaemon serves a daemon with one OAuth connector, linear: a fake MCP
// upstream behind a fake authorization server with DCR, so the connector
// is discovered and registers its own client. It waits until linear needs
// auth.
func oauthDaemon(t *testing.T) (running, *fakeas.Server) {
	t.Helper()
	up := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	target, err := url.Parse(up.URL)

	if err != nil {
		t.Fatal(err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	as := fakeas.Start(t, fakeas.WithResourceHandler(proxy))
	h := newHost(t)
	r := serve(t, h, h.options(t), map[string]any{
		"server": map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{
			"linear": map[string]any{"url": as.ResourceURL, "auth": map[string]any{"type": "oauth"}},
		},
	})
	r.waitStatus(t, "linear needs_auth", func(s admin.Status) bool {
		c, ok := connector(s, "linear")

		return ok && c.State == upstream.StateNeedsAuth
	})

	return r, as
}

// browser is the user's browser: no client token, and it does not follow
// redirects, so the test sees each hop.
func browser() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// visit GETs target in the browser and returns the status and Location.
func visit(t *testing.T, target string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)

	if err != nil {
		t.Fatal(err)
	}

	resp, err := browser().Do(req)

	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode, resp.Header.Get("Location")
}

// The acceptance test of the auth flow (wayfindr-421): `wayfindr auth login`'s
// calls to the admin API, with the browser's part scripted. Once the
// callback has stored the token, the long-poll reports linear ready with
// its tools, and search finds them.
func TestAuthFlowMakesTheConnectorReadyAndItsToolsSearchable(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	r, as := oauthDaemon(t)
	c := admin.NewClient("http://"+r.addr, "", r.client)

	f, err := c.StartAuth(t.Context(), "linear", admin.AuthStart{})

	if err != nil {
		t.Fatalf("StartAuth: %v", err)
	}

	if !strings.HasPrefix(f.AuthorizeURL, as.AuthorizationEndpoint+"?") {
		t.Fatalf("authorize URL = %s, want the fake's endpoint", f.AuthorizeURL)
	}

	// The authorization server consents at once and sends the browser back.
	_, back := visit(t, f.AuthorizeURL)

	if want := "http://" + r.addr + "/oauth/callback?"; !strings.HasPrefix(back, want) {
		t.Fatalf("authorize redirected to %q, want the daemon's callback", back)
	}

	if status, _ := visit(t, back); status != http.StatusOK {
		t.Fatalf("callback = %d, want 200", status)
	}

	var got admin.AuthFlowStatus
	deadline := time.Now().Add(15 * time.Second)

	for got.State == "" || got.State == admin.FlowPending {
		if time.Now().After(deadline) {
			t.Fatalf("the flow never ended; last: %+v", got)
		}

		if got, err = c.WaitAuth(t.Context(), f.FlowID, 5*time.Second); err != nil {
			t.Fatalf("WaitAuth: %v", err)
		}
	}

	if got.State != admin.FlowDone || got.Result == nil || got.Result.State != upstream.StateReady || got.Result.Tools == 0 {
		t.Fatalf("WaitAuth = %+v (result %+v), want done with linear ready and its tools", got, got.Result)
	}

	searchUntilFound(t, r.mcp, "echo", "linear.echo")

	if status, _ := visit(t, back); status != http.StatusBadRequest {
		t.Errorf("replayed callback = %d, want 400", status)
	}

	r.stop()
}

// A poll without wait on a finished flow reports it done every time, not
// pending at random (wayfindr-zgp.18).
func TestAuthFlowPollWithoutWaitSeesAFinishedFlow(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	r, _ := oauthDaemon(t)
	c := admin.NewClient("http://"+r.addr, "", r.client)
	f, err := c.StartAuth(t.Context(), "linear", admin.AuthStart{})

	if err != nil {
		t.Fatalf("StartAuth: %v", err)
	}

	_, back := visit(t, f.AuthorizeURL)

	if status, _ := visit(t, back); status != http.StatusOK {
		t.Fatalf("callback = %d, want 200", status)
	}

	if got := waitFlow(t, c, f.FlowID); got.State != admin.FlowDone {
		t.Fatalf("WaitAuth = %+v, want done", got)
	}

	for i := range 100 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			"http://"+r.addr+"/admin/auth/flows/"+url.PathEscape(f.FlowID), nil)

		if err != nil {
			t.Fatal(err)
		}

		resp, err := r.client.Do(req)

		if err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}

		var got admin.AuthFlowStatus
		err = json.NewDecoder(resp.Body).Decode(&got)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK || err != nil {
			t.Fatalf("poll %d = %d, %v; want 200 with a flow status", i, resp.StatusCode, err)
		}

		if got.State != admin.FlowDone || got.Result == nil || got.Result.State != upstream.StateReady {
			t.Fatalf("poll %d = %+v, want done with linear ready", i, got)
		}
	}

	r.stop()
}

var nonceInput = regexp.MustCompile(`name="nonce" value="([^"]+)"`)

// The confirmation page's form starts the flow only from the daemon's own
// origin.
func TestAuthPageStartsTheFlowOnlyFromItsOwnOrigin(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	r, as := oauthDaemon(t)
	page := "http://" + r.addr + "/auth/linear"

	confirm := func() string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, page, nil)

		if err != nil {
			t.Fatal(err)
		}

		resp, err := browser().Do(req)

		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = resp.Body.Close() }()
		var body strings.Builder

		if _, err := io.Copy(&body, resp.Body); err != nil {
			t.Fatal(err)
		}

		m := nonceInput.FindStringSubmatch(body.String())

		if resp.StatusCode != http.StatusOK || m == nil {
			t.Fatalf("GET /auth/linear = %d %q, want the confirmation form", resp.StatusCode, body.String())
		}

		return m[1]
	}
	submit := func(origin, nonce string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, page,
			strings.NewReader(url.Values{"nonce": {nonce}}.Encode()))

		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		if origin != "" {
			req.Header.Set("Origin", origin)
		}

		resp, err := browser().Do(req)

		if err != nil {
			t.Fatal(err)
		}

		_ = resp.Body.Close()

		return resp.StatusCode, resp.Header.Get("Location")
	}

	nonce := confirm()

	for _, origin := range []string{"https://evil.example", "http://localhost:1", ""} {
		if status, _ := submit(origin, nonce); status != http.StatusForbidden {
			t.Errorf("POST /auth/linear from Origin %q = %d, want 403", origin, status)
		}
	}

	if n := len(as.Authorizations()) + len(as.Clients()); n != 0 {
		t.Errorf("the authorization server saw %d requests, want none", n)
	}

	status, loc := submit("http://"+r.addr, nonce)

	if status != http.StatusFound || !strings.HasPrefix(loc, as.AuthorizationEndpoint+"?") {
		t.Errorf("same-origin POST = %d to %q, want 302 to the authorize endpoint", status, loc)
	}

	r.stop()
}
