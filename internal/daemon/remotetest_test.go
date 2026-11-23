package daemon_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// The connection test of a cloud agent's path (ADR 0021, wayfindr-45bf): the
// admin API sends an initialize and a tools/list with a remote token to the
// public URL's /mcp, and the same initialize with none, and maps what comes
// back to a closed outcome. These tests stand a TLS server in for the
// tunnel, shaped as each outcome needs, and then put a real daemon behind a
// reverse proxy that does and does not say the request was forwarded.

// logBuffer is a log sink that is safe to write from the daemon's goroutines
// and read from the test.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.String()
}

// trustingClient trusts srv's certificate, which every httptest TLS server
// shares, and keeps no connection open between requests.
func trustingClient(srv *httptest.Server) *http.Client {
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.DisableKeepAlives = true

	return &http.Client{Transport: tr}
}

// outcome is the part of an admin.RemoteTestResult a test compares; the
// wording is checked separately.
type outcome struct {
	Outcome    admin.RemoteOutcome
	OK         bool
	Status     int
	Connectors int
	Tools      int
}

func outcomeOf(r admin.RemoteTestResult) outcome {
	return outcome{Outcome: r.Outcome, OK: r.OK, Status: r.Status, Connectors: r.Connectors, Tools: r.Tools}
}

// remoteTest sets the public URL to url and tests it with token.
func (r running) remoteTest(t *testing.T, url, token string) admin.RemoteTestResult {
	t.Helper()

	if _, err := r.admin().SetPublicURL(t.Context(), url); err != nil {
		t.Fatalf("SetPublicURL(%q): %v", url, err)
	}

	res, err := r.admin().TestRemote(t.Context(), token)

	if err != nil {
		t.Fatalf("TestRemote: %v", err)
	}

	return res
}

// fakeMCP answers as a Wayfindr behind a correct tunnel does to a request
// with a token (an initialize, then a tools/list at the negotiated protocol
// version), and hands a request without one to bare. It records the
// Authorization header of each request.
type fakeMCP struct {
	bare http.HandlerFunc

	mu   sync.Mutex
	auth []string
}

func (f *fakeMCP) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.auth...)
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()

	if r.URL.Path != "/mcp" || r.Method != http.MethodPost {
		http.NotFound(w, r)

		return
	}

	if r.Header.Get("Authorization") == "" {
		f.bare(w, r)

		return
	}

	if !strings.Contains(r.Header.Get("Accept"), "application/json") || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") ||
		r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "bad headers", http.StatusNotAcceptable)

		return
	}

	body := make([]byte, 512)
	n, _ := r.Body.Read(body)
	w.Header().Set("Content-Type", "application/json")

	switch {
	case strings.Contains(string(body[:n]), `"initialize"`):
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"0"}}}`)
	case strings.Contains(string(body[:n]), `"tools/list"`) && r.Header.Get("MCP-Protocol-Version") == "2025-11-25":
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"search_tools"},{"name":"call_tool"}]}}`)
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
	}
}

// byAuth answers a request with a token with withToken and one without with
// bare.
func byAuth(withToken, bare http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			bare(w, r)

			return
		}

		withToken(w, r)
	}
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "404 page not found", http.StatusNotFound)
}

// ngrokOffline is what ngrok answers for a tunnel that is not running: a 404
// it marks with an ngrok-error-code header, with a plain-text body.
func ngrokOffline(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Ngrok-Error-Code", "ERR_NGROK_3200")
	http.Error(w, "The endpoint your-name.ngrok-free.dev is offline.", http.StatusNotFound)
}

func localChallenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// realmChallenge is a front proxy's own bearer auth: a Bearer challenge that
// names a realm, which Wayfindr's own guard never sends.
func realmChallenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="corp-proxy", error="invalid_token"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// TestRemoteConnectionOutcomes: each way the path to a cloud agent can fail
// or work, as a tunnel in front of the daemon would show it, comes back as
// its own outcome with words for a person, and the token goes to the public
// URL as a bearer on the first probe only.
func TestRemoteConnectionOutcomes(t *testing.T) {
	anchor := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(anchor.Close)

	logs := &logBuffer{}
	h := newHost(t)
	opts := h.options(t)
	opts.Logger = zerolog.New(logs).Level(zerolog.DebugLevel)
	opts.RemoteTestClient = trustingClient(anchor)
	opts.RemoteTestTimeout = 400 * time.Millisecond
	r := serve(t, h, opts, tokenConfig(t))
	token := createTokens(t, h, r)["grok"]

	htmlPage := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, "<html><body>Sign in to continue</body></html>")
		}
	}
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", code) }
	}

	// A target a redirect points at: nothing may reach it.
	var targetHits sync.Mutex
	hits := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetHits.Lock()
		hits++
		targetHits.Unlock()
	}))
	t.Cleanup(target.Close)

	targetURL := target.URL

	tests := []struct {
		name string
		// serve returns the public URL, given the fake tunnel's handler.
		handler http.Handler
		// url overrides the tunnel's URL.
		url  func(tunnel string) string
		want outcome
		// seenAuth is what the tunnel saw: the bearer, twice, then none.
		seenAuth []string
	}{
		{
			name:     "a correct tunnel: the token works and a tokenless request is 404",
			handler:  &fakeMCP{bare: notFound},
			want:     outcome{Outcome: admin.RemoteOK, OK: true, Connectors: 1, Tools: 2},
			seenAuth: []string{"Bearer " + token, "Bearer " + token, ""},
		},
		{
			name:    "a tokenless 401 with a Bearer challenge: the proxy makes the internet look local",
			handler: &fakeMCP{bare: localChallenge},
			want:    outcome{Outcome: admin.RemoteProxyTreatsRemoteAsLocal, Status: http.StatusUnauthorized},
		},
		{
			name: "a tokenless 401 with no Bearer challenge is not the daemon's: unexpected",
			handler: &fakeMCP{bare: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}},
			want: outcome{Outcome: admin.RemoteUnexpected, Status: http.StatusUnauthorized},
		},
		{
			name:    "a tokenless 401 with a Bearer realm is a proxy's own auth, not Wayfindr's: unexpected, not the proxy warning",
			handler: &fakeMCP{bare: realmChallenge},
			want:    outcome{Outcome: admin.RemoteUnexpected, Status: http.StatusUnauthorized},
		},
		{
			name:    "401 for the token and a Bearer realm 401 without: token_refused, not the proxy warning",
			handler: byAuth(status(http.StatusUnauthorized), realmChallenge),
			want:    outcome{Outcome: admin.RemoteTokenRefused, Status: http.StatusUnauthorized},
		},
		{
			name:    "404 for the token and a Bearer realm 401 without: not_found, not the proxy warning",
			handler: byAuth(notFound, realmChallenge),
			want:    outcome{Outcome: admin.RemoteNotFound, Status: http.StatusNotFound},
		},
		{
			name:    "a tokenless 200: unexpected, not safe to call ok",
			handler: &fakeMCP{bare: func(http.ResponseWriter, *http.Request) {}},
			want:    outcome{Outcome: admin.RemoteUnexpected, Status: http.StatusOK},
		},
		{
			name:    "404 for the token",
			handler: http.HandlerFunc(notFound),
			want:    outcome{Outcome: admin.RemoteNotFound, Status: http.StatusNotFound},
		},
		{
			name:    "401 for the token",
			handler: status(http.StatusUnauthorized),
			want:    outcome{Outcome: admin.RemoteTokenRefused, Status: http.StatusUnauthorized},
		},
		{
			name:    "403 for the token",
			handler: status(http.StatusForbidden),
			want:    outcome{Outcome: admin.RemoteTokenRefused, Status: http.StatusForbidden},
		},
		{
			name:    "401 for the token and a Bearer 401 without: the proxy warning outranks token_refused",
			handler: byAuth(status(http.StatusUnauthorized), localChallenge),
			want:    outcome{Outcome: admin.RemoteProxyTreatsRemoteAsLocal, Status: http.StatusUnauthorized},
		},
		{
			name:    "404 for the token and a Bearer 401 without: the proxy warning outranks not_found",
			handler: byAuth(notFound, localChallenge),
			want:    outcome{Outcome: admin.RemoteProxyTreatsRemoteAsLocal, Status: http.StatusUnauthorized},
		},
		{
			name:    "401 for the token and 404 without: token_refused stands",
			handler: byAuth(status(http.StatusUnauthorized), notFound),
			want:    outcome{Outcome: admin.RemoteTokenRefused, Status: http.StatusUnauthorized},
		},
		{
			name: "a token answer that is not MCP, and a Bearer 401 without: the proxy warning wins",
			handler: byAuth(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}, localChallenge),
			want: outcome{Outcome: admin.RemoteProxyTreatsRemoteAsLocal, Status: http.StatusUnauthorized},
		},
		{
			name:    "a tunnel's HTML page for the token is never followed by probe B",
			handler: byAuth(htmlPage(http.StatusOK), localChallenge),
			want:    outcome{Outcome: admin.RemoteInterstitial, Status: http.StatusOK},
		},
		{
			name:    "a tunnel's HTML page with 200",
			handler: htmlPage(http.StatusOK),
			want:    outcome{Outcome: admin.RemoteInterstitial, Status: http.StatusOK},
		},
		{
			name:    "a tunnel's HTML offline page with 404 is a page, not Wayfindr's 404",
			handler: htmlPage(http.StatusNotFound),
			want:    outcome{Outcome: admin.RemoteInterstitial, Status: http.StatusNotFound},
		},
		{
			name:    "ngrok's offline 404 (ERR_NGROK_3200) is the tunnel, not a bad token: unreachable",
			handler: http.HandlerFunc(ngrokOffline),
			want:    outcome{Outcome: admin.RemoteUnreachable, Status: http.StatusNotFound},
		},
		{
			name: "an ngrok-error-code header marks a tunnel 404 even with Wayfindr's body",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Ngrok-Error-Code", "ERR_NGROK_3200")
				notFound(w, r)
			}),
			want: outcome{Outcome: admin.RemoteUnreachable, Status: http.StatusNotFound},
		},
		{
			name: "a 404 with any other plain body is not Wayfindr's: unreachable",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "no such route", http.StatusNotFound)
			}),
			want: outcome{Outcome: admin.RemoteUnreachable, Status: http.StatusNotFound},
		},
		{
			name: "a JSON 404 is not Wayfindr's: unreachable",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprint(w, `{"error":"404 page not found"}`)
			}),
			want: outcome{Outcome: admin.RemoteUnreachable, Status: http.StatusNotFound},
		},
		{
			name:    "Wayfindr's own 404 for the token is not_found",
			handler: http.HandlerFunc(notFound),
			want:    outcome{Outcome: admin.RemoteNotFound, Status: http.StatusNotFound},
		},
		{
			name:    "the token works and the tokenless probe gets ngrok's offline 404: unreachable, never ok",
			handler: &fakeMCP{bare: ngrokOffline},
			want:    outcome{Outcome: admin.RemoteUnreachable, Status: http.StatusNotFound},
		},
		{
			name: "a redirect is reported and not followed",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, targetURL+"/login?next=secret-query", http.StatusFound)
			}),
			want: outcome{Outcome: admin.RemoteRedirect, Status: http.StatusFound},
		},
		{
			name:    "a 5xx",
			handler: status(http.StatusBadGateway),
			want:    outcome{Outcome: admin.RemoteUnexpected, Status: http.StatusBadGateway},
		},
		{
			name: "JSON that is not MCP",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"hello":"world"}`)
			}),
			want: outcome{Outcome: admin.RemoteUnexpected, Status: http.StatusOK},
		},
		{
			name: "an answer over the read cap is cut off, not read whole",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"pad":"`)
				_, _ = fmt.Fprint(w, strings.Repeat("a", 3<<20))
				_, _ = fmt.Fprint(w, `"}}`)
			}),
			want: outcome{Outcome: admin.RemoteUnexpected, Status: http.StatusOK},
		},
		{
			name: "a server that never answers times out",
			handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				// The body first: the server notices a closed connection only
				// once it has read it.
				_, _ = io.Copy(io.Discard, r.Body)
				<-r.Context().Done()
			}),
			want: outcome{Outcome: admin.RemoteTimeout},
		},
		{
			name:    "a certificate for another name is a TLS error",
			handler: &fakeMCP{bare: notFound},
			// The test certificate names 127.0.0.1 and example.com, not localhost.
			url: func(tunnel string) string {
				return strings.Replace(tunnel, "127.0.0.1", "localhost", 1)
			},
			want: outcome{Outcome: admin.RemoteTLSError},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tunnel := httptest.NewTLSServer(tt.handler)
			t.Cleanup(tunnel.Close)

			public := tunnel.URL

			if tt.url != nil {
				public = tt.url(public)
			}

			res := r.remoteTest(t, public, token)

			if diff := cmp.Diff(tt.want, outcomeOf(res)); diff != "" {
				t.Errorf("outcome (-want +got):\n%s\nresult: %+v", diff, res)
			}

			if res.Message == "" {
				t.Error("no message")
			}

			if !res.OK && res.Action == "" {
				t.Error("a failure with no action")
			}

			if strings.Contains(res.Message+res.Action, "secret-query") || strings.Contains(res.Message+res.Action, token) {
				t.Errorf("the result repeats a query or the token: %+v", res)
			}

			if res.Outcome == admin.RemoteOK && res.URL != tunnel.URL+"/mcp" {
				t.Errorf("URL = %q, want %q", res.URL, tunnel.URL+"/mcp")
			}

			if f, ok := tt.handler.(*fakeMCP); ok && tt.seenAuth != nil {
				if diff := cmp.Diff(tt.seenAuth, f.seen()); diff != "" {
					t.Errorf("Authorization headers the tunnel saw (-want +got):\n%s", diff)
				}
			}
		})
	}

	t.Run("a redirect is never followed", func(t *testing.T) {
		targetHits.Lock()
		defer targetHits.Unlock()

		if hits != 0 {
			t.Errorf("the redirect target was reached %d times", hits)
		}
	})

	t.Run("an offline tunnel is worded as a tunnel that is not running", func(t *testing.T) {
		tunnel := httptest.NewTLSServer(http.HandlerFunc(ngrokOffline))
		t.Cleanup(tunnel.Close)

		res := r.remoteTest(t, tunnel.URL, token)

		for _, want := range []string{"tunnel", "not running"} {
			if !strings.Contains(res.Message, want) {
				t.Errorf("message %q lacks %q", res.Message, want)
			}
		}
	})

	t.Run("the proxy warning is in plain words", func(t *testing.T) {
		tunnel := httptest.NewTLSServer(&fakeMCP{bare: localChallenge})
		t.Cleanup(tunnel.Close)

		res := r.remoteTest(t, tunnel.URL, token)

		for _, want := range []string{"Not safe", "default client token", "internet"} {
			if !strings.Contains(res.Message, want) {
				t.Errorf("message %q lacks %q", res.Message, want)
			}
		}
	})

	t.Run("nothing answers at a closed port", func(t *testing.T) {
		tunnel := httptest.NewTLSServer(http.NotFoundHandler())
		public := tunnel.URL
		tunnel.Close()

		res := r.remoteTest(t, public, token)

		if got := outcomeOf(res); got != (outcome{Outcome: admin.RemoteUnreachable}) {
			t.Errorf("outcome = %+v, want unreachable", got)
		}
	})

	t.Run("a plain-HTTP server at an https address is a TLS error", func(t *testing.T) {
		plain := httptest.NewServer(http.NotFoundHandler())
		t.Cleanup(plain.Close)

		res := r.remoteTest(t, "https://"+strings.TrimPrefix(plain.URL, "http://"), token)

		if got := outcomeOf(res); got != (outcome{Outcome: admin.RemoteTLSError}) {
			t.Errorf("outcome = %+v, want tls_error", got)
		}
	})

	t.Run("a name that does not exist is unreachable", func(t *testing.T) {
		res := r.remoteTest(t, "https://wayfindr-test.invalid", token)

		if got := outcomeOf(res); got != (outcome{Outcome: admin.RemoteUnreachable}) {
			t.Errorf("outcome = %+v, want unreachable", got)
		}
	})

	t.Run("no public URL", func(t *testing.T) {
		if _, err := r.admin().SetPublicURL(t.Context(), ""); err != nil {
			t.Fatal(err)
		}

		res, err := r.admin().TestRemote(t.Context(), token)

		if err != nil || res.Outcome != admin.RemoteNotConfigured || res.OK {
			t.Errorf("TestRemote = %+v, %v; want not_configured", res, err)
		}
	})

	t.Run("the daemon's log never holds the token or a bearer", func(t *testing.T) {
		out := logs.String()

		if strings.Contains(out, token) || strings.Contains(out, "Bearer") || strings.Contains(out, "secret-query") {
			t.Errorf("the log holds the token, a bearer, or a redirect's query:\n%s", out)
		}

		if !strings.Contains(out, "remote connection tested") {
			t.Errorf("no log line for the tests:\n%s", out)
		}
	})
}

// tunnelTo is a TLS reverse proxy to the daemon at daemonAddr. forwarding
// says whether it behaves as ngrok or Tailscale Funnel do, keeping the public
// Host and adding X-Forwarded-*, or as a bare proxy does, which sends the
// loopback address as Host and adds nothing (wayfindr-gz1y).
func tunnelTo(t *testing.T, daemonAddr string, forwarding bool) *httptest.Server {
	t.Helper()
	target := &url.URL{Scheme: "http", Host: daemonAddr}
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)

		if forwarding {
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		}
	}}
	srv := httptest.NewTLSServer(proxy)
	t.Cleanup(srv.Close)

	return srv
}

// TestRemoteConnectionThroughAProxyToTheRealDaemon: the test against a real
// daemon, behind a tunnel that says the request was forwarded and behind one
// that does not. Only the first is a path a cloud agent should use; the
// second would let the default token in from the internet.
func TestRemoteConnectionThroughAProxyToTheRealDaemon(t *testing.T) {
	cfg := tokenConfig(t)
	daemonAddr := cfg["server"].(map[string]any)["listen"].(string)
	good := tunnelTo(t, daemonAddr, true)
	bare := tunnelTo(t, daemonAddr, false)

	h := newHost(t)
	opts := h.options(t)
	opts.RemoteTestClient = trustingClient(good)
	r := serve(t, h, opts, cfg)
	tokens := createTokens(t, h, r)

	tests := []struct {
		name   string
		tunnel *httptest.Server
		token  string
		want   outcome
	}{
		{"forwarding tunnel, remote token", good, tokens["grok"], outcome{Outcome: admin.RemoteOK, OK: true, Connectors: 1, Tools: 2}},
		{"forwarding tunnel, scoped token that is not remote", good, tokens["nightly"], outcome{Outcome: admin.RemoteNotFound, Status: http.StatusNotFound}},
		{"forwarding tunnel, a token the daemon never issued", good, "wf_s_nope", outcome{Outcome: admin.RemoteNotFound, Status: http.StatusNotFound}},
		{"bare proxy, remote token: works, but the internet looks local", bare, tokens["grok"], outcome{Outcome: admin.RemoteProxyTreatsRemoteAsLocal, Status: http.StatusUnauthorized}},
		{"bare proxy, scoped token that is not remote: still the proxy warning", bare, tokens["nightly"], outcome{Outcome: admin.RemoteProxyTreatsRemoteAsLocal, Status: http.StatusUnauthorized}},
		{"bare proxy, a wrong token: still the proxy warning", bare, "wf_s_nope", outcome{Outcome: admin.RemoteProxyTreatsRemoteAsLocal, Status: http.StatusUnauthorized}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := r.remoteTest(t, tt.tunnel.URL, tt.token)

			if diff := cmp.Diff(tt.want, outcomeOf(res)); diff != "" {
				t.Errorf("outcome (-want +got):\n%s\nresult: %+v", diff, res)
			}
		})
	}
}

// TestRemoteConnectionClosesTheSessionItOpened: when the far side answers the
// first probe's initialize with an MCP session id, the test ends that session
// with a DELETE, so a server that keeps sessions is not left with one per
// test. Wayfindr itself keeps none, so its /mcp has nothing to close.
func TestRemoteConnectionClosesTheSessionItOpened(t *testing.T) {
	anchor := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(anchor.Close)

	h := newHost(t)
	opts := h.options(t)
	opts.RemoteTestClient = trustingClient(anchor)
	r := serve(t, h, opts, tokenConfig(t))
	token := createTokens(t, h, r)["grok"]
	mcp := &fakeMCP{bare: notFound}

	var (
		mu         sync.Mutex
		closed     []string
		bearerKept = true
	)

	tunnel := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			mu.Lock()
			closed = append(closed, req.Header.Get("Mcp-Session-Id"))
			bearerKept = bearerKept && req.Header.Get("Authorization") == "Bearer "+token
			mu.Unlock()
			w.WriteHeader(http.StatusMethodNotAllowed)

			return
		}

		w.Header().Set("Mcp-Session-Id", "session-1")
		mcp.ServeHTTP(w, req)
	}))
	t.Cleanup(tunnel.Close)

	res := r.remoteTest(t, tunnel.URL, token)

	if got, want := outcomeOf(res), (outcome{Outcome: admin.RemoteOK, OK: true, Connectors: 1, Tools: 2}); got != want {
		t.Fatalf("outcome = %+v, want %+v", got, want)
	}

	mu.Lock()
	defer mu.Unlock()

	if diff := cmp.Diff([]string{"session-1"}, closed); diff != "" {
		t.Errorf("sessions closed (-want +got):\n%s", diff)
	}

	if !bearerKept {
		t.Error("the DELETE did not carry the token as its bearer")
	}
}
