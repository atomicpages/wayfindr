package httpd_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/httpd"
)

const remoteToken = "wf_s_grok-remote-token" //nolint:gosec // G101: a test token, not a credential.

func grokClient() clienttoken.Client {
	return clienttoken.Client{Name: "grok", Connectors: []string{"github"}, Remote: true}
}

// remoteRegistry holds a remote token (grok) and a scoped token that isn't
// remote (nightly).
func remoteRegistry() *clienttoken.Registry {
	return clienttoken.NewRegistry(map[[sha256.Size]byte]clienttoken.Client{
		clienttoken.Digest(remoteToken): grokClient(),
		clienttoken.Digest(scopedToken): nightlyClient(),
	}, nil)
}

// newRemoteHandler is the full route table with stub handlers, the gateway
// replaced by clientStub, and reg as the scoped tokens. Its debug log goes to
// log when log is not nil.
func newRemoteHandler(t *testing.T, reg *clienttoken.Registry, log *bytes.Buffer) http.Handler {
	t.Helper()

	logger := zerolog.Nop()

	if log != nil {
		logger = zerolog.New(log).Level(zerolog.DebugLevel)
	}

	handlers := stubHandlers()
	handlers.MCP = clientStub()
	h, err := httpd.NewHandler(httpd.Config{
		Port:         testPort,
		Token:        testToken,
		Clients:      reg,
		Version:      testVersion,
		Logger:       logger,
		Handlers:     handlers,
		CallbackPath: "/oauth/callback",
	})

	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	return h
}

// remoteWays are the ways a request is remote: each forwarding header on a
// loopback Host, and a Host that isn't loopback with none.
func remoteWays() map[string]func(*http.Request) {
	ways := map[string]func(*http.Request){}

	for name, value := range map[string]string{
		"Forwarded":         "for=203.0.113.7;proto=https",
		"Via":               "1.1 tunnel.example",
		"X-Forwarded-For":   "203.0.113.7",
		"X-Forwarded-Host":  "abc.ngrok.app",
		"X-Forwarded-Proto": "https",
		"X-Real-Ip":         "203.0.113.7",
		"Cf-Connecting-Ip":  "203.0.113.7",
	} {
		ways[name] = func(r *http.Request) { r.Header.Set(name, value) }
	}

	for _, host := range []string{"abc.ngrok.app", "abc.ngrok.app:443", "evil.example:7777", "127.0.0.1:8080"} {
		ways["Host "+host] = func(r *http.Request) { r.Host = host }
	}

	return ways
}

// response is everything a client can see of an answer.
type response struct {
	Code   int
	Header http.Header
	Body   string
}

func record(h http.Handler, req *http.Request) response {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return response{Code: rec.Code, Header: rec.Header(), Body: rec.Body.String()}
}

// foreign404 is what a remote request gets for a path that doesn't exist:
// the answer every refused remote request must be indistinguishable from.
func foreign404(t *testing.T, h http.Handler) response {
	t.Helper()

	req := bearerRequest(http.MethodGet, "/no-such-path")
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	got := record(h, req)

	if got.Code != http.StatusNotFound || got.Body != "404 page not found\n" {
		t.Fatalf("baseline foreign answer = %+v, want 404 page not found", got)
	}

	return got
}

// TestRemoteTokenReachesMCP: a remote request with a remote scoped token
// reaches the gateway on every /mcp method, which sees that token's Client
// and the request's own Authorization header (ADR 0021).
func TestRemoteTokenReachesMCP(t *testing.T) {
	h := newRemoteHandler(t, remoteRegistry(), nil)

	for way, apply := range remoteWays() {
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
			t.Run(way+" "+method, func(t *testing.T) {
				req := bearerRequest(method, "/mcp?x=1", "Bearer "+remoteToken)
				apply(req)
				got := serveSeen(t, h, req)
				want := seen{Found: true, Client: grokClient(), Authorization: []string{"Bearer " + remoteToken}}

				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("handler saw (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// TestRemoteRefusalLooksForeign: every remote request to /mcp without a
// remote token gets exactly the answer a remote request for a path that
// doesn't exist gets: same status, headers, and body, never 401 or
// WWW-Authenticate. A foreign Origin doesn't change that: the token is
// checked first.
func TestRemoteRefusalLooksForeign(t *testing.T) {
	h := newRemoteHandler(t, remoteRegistry(), nil)
	want := foreign404(t, h)
	auths := []struct {
		name  string
		auths []string
	}{
		{"missing", nil},
		{"empty", []string{""}},
		{"scheme only", []string{"Bearer"}},
		{"scheme and space", []string{"Bearer "}},
		{"without scheme", []string{remoteToken}},
		{"basic scheme", []string{"Basic " + remoteToken}},
		{"default token", []string{"Bearer " + testToken}},
		{"scoped token that isn't remote", []string{"Bearer " + scopedToken}},
		{"wrong token", []string{"Bearer wf_s_wrong"}},
		{"remote token prefix", []string{"Bearer " + remoteToken[:len(remoteToken)-1]}},
		{"remote token twice", []string{"Bearer " + remoteToken, "Bearer " + remoteToken}},
		{"remote and default", []string{"Bearer " + remoteToken, "Bearer " + testToken}},
	}
	methods := []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodPut, http.MethodOptions}

	for way, apply := range remoteWays() {
		for _, a := range auths {
			for _, method := range methods {
				for _, origin := range []string{"", "https://evil.example"} {
					t.Run(way+" "+a.name+" "+method+" Origin "+origin, func(t *testing.T) {
						req := bearerRequest(method, "/mcp", a.auths...)
						apply(req)

						if origin != "" {
							req.Header.Set("Origin", origin)
						}

						if diff := cmp.Diff(want, record(h, req)); diff != "" {
							t.Errorf("answer differs from the foreign 404 (-want +got):\n%s", diff)
						}
					})
				}
			}
		}
	}
}

// TestRemoteTokenOnlyReachesMCP: a remote token gets a remote request
// nowhere but /mcp: the admin API, the console, auth links, /healthz, and
// paths that only clean to /mcp all answer the foreign 404.
func TestRemoteTokenOnlyReachesMCP(t *testing.T) {
	h := newRemoteHandler(t, remoteRegistry(), nil)
	want := foreign404(t, h)
	paths := []struct{ method, path string }{
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/admin/status"},
		{http.MethodPost, "/admin/tokens"},
		{http.MethodGet, "/ui/"},
		{http.MethodPost, "/ui/tokens"},
		{http.MethodGet, "/auth/slack"},
		{http.MethodPost, "/auth/slack"},
		{http.MethodPost, "/mcp/"},
		{http.MethodPost, "//mcp"},
		{http.MethodPost, "/./mcp"},
		{http.MethodPost, "/MCP"},
		{http.MethodGet, "/"},
	}

	for way, apply := range remoteWays() {
		for _, p := range paths {
			t.Run(way+" "+p.method+" "+p.path, func(t *testing.T) {
				req := bearerRequest(p.method, "/", "Bearer "+remoteToken)
				req.URL.Path = p.path
				apply(req)

				if diff := cmp.Diff(want, record(h, req)); diff != "" {
					t.Errorf("answer differs from the foreign 404 (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// TestRemoteTokenStillChecksOrigin: once a remote token lets a remote
// request in, it gets the Origin check a local request gets.
func TestRemoteTokenStillChecksOrigin(t *testing.T) {
	h := newRemoteHandler(t, remoteRegistry(), nil)
	tests := []struct {
		name    string
		origins []string
		want    int
	}{
		{"none", nil, http.StatusOK},
		{"loopback", []string{"http://127.0.0.1:6274"}, http.StatusOK},
		{"foreign", []string{"https://evil.example"}, http.StatusForbidden},
		{"null", []string{"null"}, http.StatusForbidden},
		{"two", []string{"http://127.0.0.1:6274", "http://127.0.0.1:6274"}, http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := bearerRequest(http.MethodPost, "/mcp", "Bearer "+remoteToken)
			req.Header.Set("X-Forwarded-For", "203.0.113.7")

			for _, o := range tt.origins {
				req.Header.Add("Origin", o)
			}

			if code, body := serve(h, req); code != tt.want {
				t.Errorf("status = %d, want %d (body %q)", code, tt.want, body)
			}
		})
	}
}

// TestRemoteTokenWorksLocally: a remote token is a scoped token to a local
// request, and a local request without a token still gets the 401
// challenge.
func TestRemoteTokenWorksLocally(t *testing.T) {
	h := newRemoteHandler(t, remoteRegistry(), nil)
	got := serveSeen(t, h, bearerRequest(http.MethodPost, "/mcp", "Bearer "+remoteToken))

	if diff := cmp.Diff(grokClient(), got.Client); diff != "" {
		t.Errorf("client (-want +got):\n%s", diff)
	}

	if code, body := serve(h, bearerRequest(http.MethodGet, "/admin/status", "Bearer "+remoteToken)); code != http.StatusForbidden {
		t.Errorf("local /admin/ with a remote token: status = %d, want 403 (body %q)", code, body)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(http.MethodPost, "/mcp"))

	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("local, no token: got %d with WWW-Authenticate %q, want 401 Bearer",
			rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

// TestRemoteCallbackUnchanged: the OAuth callback still answers any remote
// request, with or without a token.
func TestRemoteCallbackUnchanged(t *testing.T) {
	h := newRemoteHandler(t, remoteRegistry(), nil)

	for way, apply := range remoteWays() {
		for _, auths := range [][]string{nil, {"Bearer " + remoteToken}} {
			t.Run(way+" "+strings.Join(auths, ","), func(t *testing.T) {
				req := bearerRequest(http.MethodGet, "/oauth/callback?code=c&state=s", auths...)
				apply(req)

				if code, body := serve(h, req); code != http.StatusOK || body != "oauth-callback connector= id= name=" {
					t.Errorf("got %d %q, want the callback", code, body)
				}
			})
		}
	}
}

// TestRemoteRevokeAppliesToNextRequest: once a remote token is removed from
// the registry, the next remote request with it gets the foreign 404.
func TestRemoteRevokeAppliesToNextRequest(t *testing.T) {
	reg := remoteRegistry()
	h := newRemoteHandler(t, reg, nil)
	want := foreign404(t, h)
	forwardedMCP := func() *http.Request {
		req := bearerRequest(http.MethodPost, "/mcp", "Bearer "+remoteToken)
		req.Header.Set("Cf-Connecting-Ip", "203.0.113.7")

		return req
	}

	serveSeen(t, h, forwardedMCP())

	if !reg.Remove("grok") {
		t.Fatal("Remove(grok) = false")
	}

	if diff := cmp.Diff(want, record(h, forwardedMCP())); diff != "" {
		t.Errorf("after Remove, answer differs from the foreign 404 (-want +got):\n%s", diff)
	}
}

// TestRemoteRefusalLogged: a refused remote request is logged at debug with
// the check that refused it, never a header value or the query string.
func TestRemoteRefusalLogged(t *testing.T) {
	tests := []struct {
		name, method, path string
		auths              []string
		want               string
	}{
		{"no token on /mcp", http.MethodPost, "/mcp?secret=q", nil, "remote_token"},
		{"default token on /mcp", http.MethodPost, "/mcp?secret=q", []string{"Bearer " + testToken}, "remote_token"},
		{"scoped token on /mcp", http.MethodPost, "/mcp?secret=q", []string{"Bearer " + scopedToken}, "remote_token"},
		{"remote token on /healthz", http.MethodGet, "/healthz?secret=q", []string{"Bearer " + remoteToken}, "host"},
		{"remote token on /admin/", http.MethodGet, "/admin/status?secret=q", []string{"Bearer " + remoteToken}, "host"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log bytes.Buffer

			h := newRemoteHandler(t, remoteRegistry(), &log)
			req := bearerRequest(tt.method, tt.path, tt.auths...)
			req.Header.Set("X-Forwarded-Host", "abc.ngrok.app")
			serve(h, req)

			var entry map[string]any

			if err := json.Unmarshal(log.Bytes(), &entry); err != nil {
				t.Fatalf("decode log %q: %v", log.String(), err)
			}

			want := map[string]any{
				"level":   "debug",
				"method":  tt.method,
				"path":    req.URL.Path,
				"guard":   tt.want,
				"message": "request rejected",
			}

			if diff := cmp.Diff(want, entry); diff != "" {
				t.Errorf("log entry (-want +got):\n%s", diff)
			}

			for _, leak := range []string{testToken, scopedToken, remoteToken, "abc.ngrok.app", "secret"} {
				if strings.Contains(log.String(), leak) {
					t.Errorf("log %q contains %q", log.String(), leak)
				}
			}
		})
	}
}
