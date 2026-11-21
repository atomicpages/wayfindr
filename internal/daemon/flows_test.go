package daemon_test

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// The tests in this file cover the one flow entry both doors go through
// (wayfindr-zgp.23): the browser's /auth/{connector} pages and the admin
// API that `wayfindr auth login` calls.

// garbledAS is an authorization server whose metadata is good but whose
// registration endpoint answers with something that is neither a
// registration nor an RFC 7591 error.
func garbledAS(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                           srv.URL,
			"authorization_endpoint":           srv.URL + "/authorize",
			"token_endpoint":                   srv.URL + "/token",
			"registration_endpoint":            srv.URL + "/register",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>maintenance</html>")
	})
	srv.Config.Handler = mux
	t.Cleanup(srv.Close)

	return srv
}

// startAtAdmin is the admin door: POST /admin/auth/{connector} as `wayfindr auth login`
// sends it. It returns the status and the body.
func startAtAdmin(t *testing.T, r running, connector string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://"+r.addr+"/admin/auth/"+url.PathEscape(connector), nil)

	if err != nil {
		t.Fatal(err)
	}

	return send(t, r.client, req)
}

// startInBrowser is the browser door: GET the confirmation page for its
// nonce, then submit its form from the daemon's own origin. A page that
// has no form (it already refused) is submitted with a made-up nonce. It
// returns the status and the page of the POST.
func startInBrowser(t *testing.T, r running, connector string) (int, string) {
	t.Helper()
	page := "http://" + r.addr + "/auth/" + url.PathEscape(connector)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, page, nil)

	if err != nil {
		t.Fatal(err)
	}

	_, body := send(t, browser(), req)
	nonce := "made-up"

	if m := nonceInput.FindStringSubmatch(body); m != nil {
		nonce = m[1]
	}

	req, err = http.NewRequestWithContext(t.Context(), http.MethodPost, page,
		strings.NewReader(url.Values{"nonce": {nonce}}.Encode()))

	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+r.addr)

	return send(t, browser(), req)
}

func send(t *testing.T, c *http.Client, req *http.Request) (int, string) {
	t.Helper()
	resp, err := c.Do(req)

	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}

	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)

	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(b)
}

// Both doors answer a flow that cannot start with the same status and the
// same advice: auth decides both, and neither door words its own.
func TestFlowThatCannotStartReadsTheSameThroughBothDoors(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	nopkce := fakeas.Start(t, fakeas.WithoutPKCEAdvertised())
	garbled := garbledAS(t)
	down := "http://" + freeAddr(t)
	h := newHost(t)
	r := serve(t, h, h.options(t), map[string]any{
		"server": map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{
			"plain":  map[string]any{"url": down + "/mcp"},
			"nopkce": map[string]any{"url": nopkce.ResourceURL, "auth": map[string]any{"type": "oauth"}},
			"down": map[string]any{"url": down + "/mcp", "auth": map[string]any{
				"type": "oauth", "authorization_server": down,
			}},
			"garbled": map[string]any{"url": garbled.URL + "/mcp", "auth": map[string]any{
				"type": "oauth", "authorization_server": garbled.URL,
			}},
		},
	})

	tests := []struct {
		connector       string
		status          int
		summary, action string
	}{
		{"nope", http.StatusNotFound, `No connector named "nope" is configured.`,
			"Check the connector's name with `wayfindr status`."},
		{"plain", http.StatusNotFound, `plain does not use OAuth: it has no [auth] table of type "oauth".`,
			"Add one with `wayfindr connector edit plain --oauth` to authorize it through Wayfindr, or put its API key in headers."},
		{"nopkce", http.StatusConflict,
			"The authorization server does not advertise PKCE (S256), which Wayfindr requires for a connector without a client_id.",
			"Register an app with the provider and set client_id (and client_secret) in the connector's [auth] table."},
		{"down", http.StatusBadGateway, "The authorization server cannot be reached.",
			"Check the network. Wayfindr tries again on the next call."},
		{"garbled", http.StatusBadGateway, "The authorization server sent a response Wayfindr cannot read.",
			"Wayfindr tries again on the next call. If it persists, report it with the daemon log."},
	}

	for _, tt := range tests {
		t.Run(tt.connector, func(t *testing.T) {
			status, body := startAtAdmin(t, r, tt.connector)

			if want := tt.summary + "\n" + tt.action + "\n"; status != tt.status || body != want {
				t.Errorf("admin: %d %q, want %d %q", status, body, tt.status, want)
			}

			status, page := startInBrowser(t, r, tt.connector)

			if status != tt.status || !strings.Contains(page, html.EscapeString(tt.summary)) ||
				!strings.Contains(page, html.EscapeString(tt.action)) {
				t.Errorf("browser: %d %q, want %d with %q and %q", status, page, tt.status, tt.summary, tt.action)
			}
		})
	}

	r.stop()
}

// A config edit to a connector's url or [auth] table while its flow waits
// for the browser cancels the flow, and the waiting long-poll reports it;
// the late callback stores nothing. An edit to anything else, such as its
// timeout, leaves the flow pending.
func TestConfigEditDuringAFlowCancelsItOnlyForTheURLOrAuthTable(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	up := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	target, err := url.Parse(up.URL)

	if err != nil {
		t.Fatal(err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	as := fakeas.Start(t, fakeas.WithResourceHandler(proxy))
	listen := freeAddr(t)
	linear := func(extra, auth map[string]any) map[string]any {
		a := map[string]any{"type": "oauth"}

		for k, v := range auth {
			a[k] = v
		}

		c := map[string]any{"url": as.ResourceURL, "auth": a}

		for k, v := range extra {
			c[k] = v
		}

		return c
	}
	h := newHost(t)
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"linear": linear(nil, nil)},
	})
	r.waitStatus(t, "linear needs_auth", stateIs("linear", upstream.StateNeedsAuth))
	c := admin.NewClient("http://"+r.addr, "", r.client)
	f, err := c.StartAuth(t.Context(), "linear", admin.AuthStart{})

	if err != nil {
		t.Fatalf("StartAuth: %v", err)
	}

	_, late := visit(t, f.AuthorizeURL)

	// The timeout, and a new connector to see the reload by.
	other := "http://" + freeAddr(t) + "/mcp"
	h.writeConfig(t, map[string]any{
		"server": map[string]any{"listen": listen},
		"connectors": map[string]any{
			"linear": linear(map[string]any{"timeout": "45s"}, nil),
			"other":  map[string]any{"url": other},
		},
	})
	r.waitStatus(t, "other configured", func(s admin.Status) bool { _, ok := connector(s, "other"); return ok })

	if got, err := c.WaitAuth(t.Context(), f.FlowID, 0); err != nil || got.State != admin.FlowPending {
		t.Fatalf("WaitAuth after a timeout edit = %+v, %v; want pending", got, err)
	}

	waited := make(chan admin.AuthFlowStatus, 1)
	go func() {
		got, err := c.WaitAuth(t.Context(), f.FlowID, 20*time.Second)

		if err != nil {
			t.Errorf("WaitAuth: %v", err)
		}

		waited <- got
	}()
	h.writeConfig(t, map[string]any{
		"server": map[string]any{"listen": listen},
		"connectors": map[string]any{
			"linear": linear(map[string]any{"timeout": "45s"}, map[string]any{"scopes": []string{"read", "write"}}),
			"other":  map[string]any{"url": other},
		},
	})

	if got := <-waited; got.State != admin.FlowFailed || got.Advice == nil ||
		!strings.HasPrefix(got.Advice.Summary, "The authorization was cancelled") {
		t.Errorf("long-poll during the [auth] edit = %+v, want failed as cancelled", got)
	}

	if status, _ := visit(t, late); status != http.StatusBadRequest {
		t.Errorf("late callback = %d, want 400", status)
	}

	if n := len(as.TokenRequests()); n != 0 {
		t.Errorf("token requests = %d, want none: the late callback exchanges no code", n)
	}

	r.stop()
}
