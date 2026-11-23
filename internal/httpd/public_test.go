package httpd_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/httpd"
)

// Wayfindr's authorization server's routes and the 401 challenge, as httpd
// guards them (ADR 0022, design §7.11): what a remote request reaches while
// [server.oauth] is on, and that turning it off restores the 404.

// metadataURL is what the daemon's ResourceMetadataURL returns for
// https://w.example while OAuth is on.
const metadataURL = "https://w.example/.well-known/oauth-protected-resource/mcp"

// onOff is a ResourceMetadataURL a test turns on and off, as a reload does.
type onOff struct{ url atomic.Pointer[string] }

func (s *onOff) set(url string) { s.url.Store(&url) }

func (s *onOff) get() string {
	if p := s.url.Load(); p != nil {
		return *p
	}

	return ""
}

// newOAuthHandler is newRemoteHandler with ResourceMetadataURL read from
// the returned switch, on with metadataURL.
func newOAuthHandler(t *testing.T) (http.Handler, *onOff) {
	t.Helper()
	sw := &onOff{}
	sw.set(metadataURL)
	handlers := stubHandlers()
	handlers.MCP = clientStub()
	h, err := httpd.NewHandler(httpd.Config{
		Port:                testPort,
		Token:               testToken,
		Clients:             remoteRegistry(),
		Version:             testVersion,
		Logger:              zerolog.Nop(),
		Handlers:            handlers,
		CallbackPath:        "/oauth/callback",
		ResourceMetadataURL: sw.get,
	})

	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	return h, sw
}

// tunnelWays are the ways a remote request comes through the tunnel, which
// alone get the public admission and the /mcp challenge: each forwarding
// header on a loopback Host (a tunnel that rewrites Host), and the issuer's
// own Host (w.example), with or without a forwarding header, in any case and
// with or without https's default port.
func tunnelWays() map[string]func(*http.Request) {
	ways := map[string]func(*http.Request){}

	for name, apply := range remoteWays() {
		if !strings.HasPrefix(name, "Host ") {
			ways[name] = apply
		}
	}

	for _, host := range []string{"w.example", "W.Example", "w.example:443"} {
		ways["Host "+host] = func(r *http.Request) { r.Host = host }
	}

	ways["Host w.example with X-Forwarded-For"] = func(r *http.Request) {
		r.Host = "w.example"
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
	}

	return ways
}

// foreignWays are remote requests that did not come through the tunnel: a
// Host that is neither the issuer's nor loopback, as a DNS-rebinding page
// sends, or a loopback Host on another port, with no forwarding header; and a
// forwarding header beside a Host that is neither.
func foreignWays() map[string]func(*http.Request) {
	ways := map[string]func(*http.Request){}

	for _, host := range []string{"abc.ngrok.app", "rebind.evil.example:7777", "127.0.0.1:8080", "w.example:8443", "w.example.", "[::1]:7778"} {
		ways["Host "+host] = func(r *http.Request) { r.Host = host }
	}

	for _, host := range []string{"abc.ngrok.app", "rebind.evil.example:7777", "127.0.0.1:8080"} {
		ways["Host "+host+" with X-Forwarded-For"] = func(r *http.Request) {
			r.Host = host
			r.Header.Set("X-Forwarded-For", "203.0.113.7")
		}
	}

	return ways
}

// publicRoutes are the authorization server's routes with every method a
// remote request may use on them: HEAD too on the three metadata paths, and
// GET only on authorize.
func publicRoutes() []struct{ method, path, want string } {
	return []struct{ method, path, want string }{
		{"GET", "/.well-known/oauth-protected-resource", "oauth-resource GET /.well-known/oauth-protected-resource"},
		{"HEAD", "/.well-known/oauth-protected-resource", "oauth-resource HEAD /.well-known/oauth-protected-resource"},
		{"GET", "/.well-known/oauth-protected-resource/mcp", "oauth-resource GET /.well-known/oauth-protected-resource/mcp"},
		{"HEAD", "/.well-known/oauth-protected-resource/mcp", "oauth-resource HEAD /.well-known/oauth-protected-resource/mcp"},
		{"GET", "/.well-known/oauth-authorization-server", "oauth-server GET /.well-known/oauth-authorization-server"},
		{"HEAD", "/.well-known/oauth-authorization-server", "oauth-server HEAD /.well-known/oauth-authorization-server"},
		{"GET", "/oauth/authorize?client_id=c&state=s", "oauth-authorize GET /oauth/authorize"},
		{"POST", "/oauth/token", "oauth-token POST /oauth/token"},
	}
}

// TestPublicRoutesReachedRemotelyOnlyWhileOn: while on, a remote request
// through the tunnel reaches each authorization server route with its own
// method, with no token or with any, and whatever its Origin; while off it
// gets the foreign 404, byte for byte.
func TestPublicRoutesReachedRemotelyOnlyWhileOn(t *testing.T) {
	h, sw := newOAuthHandler(t)
	auths := [][]string{nil, {"Bearer " + remoteToken}, {"Bearer " + testToken}, {"Bearer wf_s_wrong"}}

	for way, apply := range tunnelWays() {
		for _, p := range publicRoutes() {
			for _, a := range auths {
				for _, origin := range []string{"", "https://chatgpt.com"} {
					t.Run(way+" "+p.method+" "+p.path+" "+strings.Join(a, ",")+" Origin "+origin, func(t *testing.T) {
						req := bearerRequest(p.method, p.path, a...)
						apply(req)

						if origin != "" {
							req.Header.Set("Origin", origin)
						}

						sw.set(metadataURL)

						if code, body := serve(h, req); code != http.StatusOK || body != p.want {
							t.Errorf("on: got %d %q, want 200 %q", code, body, p.want)
						}

						sw.set("")
						want := foreign404(t, h)

						if diff := cmp.Diff(want, record(h, req)); diff != "" {
							t.Errorf("off: answer differs from the foreign 404 (-want +got):\n%s", diff)
						}
					})
				}
			}
		}
	}
}

// TestPublicPathWithAnotherMethodLooksForeign: while on, a remote request
// with any other method on an authorization server path, or for a path that
// only cleans or redirects to one, gets the foreign 404, never the mux's 405
// or a redirect. A remote HEAD of authorize never reaches its handler.
func TestPublicPathWithAnotherMethodLooksForeign(t *testing.T) {
	h, _ := newOAuthHandler(t)
	want := foreign404(t, h)
	methods := []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"}
	allowed := map[string]bool{}

	for _, p := range publicRoutes() {
		allowed[p.method+" "+strings.Split(p.path, "?")[0]] = true
	}

	var reqs []struct{ method, path string }

	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-authorization-server",
		"/oauth/authorize",
		"/oauth/token",
	} {
		for _, m := range methods {
			if !allowed[m+" "+path] {
				reqs = append(reqs, struct{ method, path string }{m, path})
			}
		}
	}

	for _, path := range []string{
		"/oauth/token/", "/oauth//token", "/./oauth/token", "/oauth/x/../token", "/OAUTH/TOKEN",
		"/oauth/authorize/", "/.well-known/oauth-protected-resource/", "/.well-known/oauth-protected-resource/mcp/",
		"/.well-known/oauth-protected-resource/other", "/.well-known/oauth-authorization-server/mcp",
		"/.well-known/openid-configuration", "/.well-known/", "/oauth/", "/mcp/", "//mcp",
	} {
		for _, m := range []string{"GET", "POST"} {
			reqs = append(reqs, struct{ method, path string }{m, path})
		}
	}

	for way, apply := range remoteWays() {
		for _, r := range reqs {
			t.Run(way+" "+r.method+" "+r.path, func(t *testing.T) {
				req := bearerRequest(r.method, "/")
				req.URL.Path = r.path
				apply(req)

				if diff := cmp.Diff(want, record(h, req)); diff != "" {
					t.Errorf("answer differs from the foreign 404 (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// TestPublicAdmissionReachesNothingElse: while on, a remote request with
// no remote token still gets the foreign 404 on every route that isn't the
// authorization server's (or /mcp, which challenges), the console's approve
// page included, whatever token it carries.
func TestPublicAdmissionReachesNothingElse(t *testing.T) {
	h, _ := newOAuthHandler(t)
	want := foreign404(t, h)
	paths := []struct{ method, path string }{
		{"GET", "/healthz"},
		{"GET", "/admin/status"},
		{"POST", "/admin/tokens"},
		{"POST", "/admin/oauth/arm"},
		{"GET", "/admin/oauth/requests/r1"},
		{"POST", "/admin/oauth/requests/r1/approve"},
		{"GET", "/ui/"},
		{"GET", "/ui/oauth/approve?req=r1"},
		{"POST", "/ui/oauth/approve"},
		{"POST", "/ui/oauth/deny"},
		{"POST", "/ui/cloud-agents/arm"},
		{"GET", "/auth/slack"},
		{"POST", "/auth/slack"},
		{"GET", "/"},
	}
	auths := [][]string{nil, {"Bearer " + testToken}, {"Bearer " + remoteToken}, {"Bearer " + scopedToken}}

	for way, apply := range remoteWays() {
		for _, p := range paths {
			for _, a := range auths {
				t.Run(way+" "+p.method+" "+p.path+" "+strings.Join(a, ","), func(t *testing.T) {
					req := bearerRequest(p.method, p.path, a...)
					apply(req)

					if diff := cmp.Diff(want, record(h, req)); diff != "" {
						t.Errorf("answer differs from the foreign 404 (-want +got):\n%s", diff)
					}
				})
			}
		}
	}
}

// TestRemoteMCPChallengeWhileOn: while on, a remote request to /mcp through
// the tunnel without a remote token gets 401 naming the protected resource
// metadata, with error="invalid_token" only when it sent an Authorization
// header, on every method and whatever its Origin; a remote token still
// reaches the gateway, whichever way it is remote.
func TestRemoteMCPChallengeWhileOn(t *testing.T) {
	h, _ := newOAuthHandler(t)
	auths := []struct {
		name  string
		auths []string
		sent  bool
	}{
		{"missing", nil, false},
		{"empty", []string{""}, true},
		{"scheme only", []string{"Bearer"}, true},
		{"basic scheme", []string{"Basic " + remoteToken}, true},
		{"default token", []string{"Bearer " + testToken}, true},
		{"scoped token that isn't remote", []string{"Bearer " + scopedToken}, true},
		{"wrong token", []string{"Bearer wf_s_wrong"}, true},
		{"remote token twice", []string{"Bearer " + remoteToken, "Bearer " + remoteToken}, true},
	}
	methods := []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodPut, http.MethodOptions}

	for way, apply := range tunnelWays() {
		for _, a := range auths {
			for _, method := range methods {
				for _, origin := range []string{"", "https://evil.example"} {
					t.Run(way+" "+a.name+" "+method+" Origin "+origin, func(t *testing.T) {
						req := bearerRequest(method, "/mcp?x=1", a.auths...)
						apply(req)

						if origin != "" {
							req.Header.Set("Origin", origin)
						}

						challenge := `Bearer resource_metadata="` + metadataURL + `"`

						if a.sent {
							challenge += `, error="invalid_token"`
						}

						want := response{
							Code: http.StatusUnauthorized,
							Header: http.Header{
								"Content-Type":           {"text/plain; charset=utf-8"},
								"X-Content-Type-Options": {"nosniff"},
								"Www-Authenticate":       {challenge},
							},
							Body: "unauthorized\n",
						}

						if diff := cmp.Diff(want, record(h, req)); diff != "" {
							t.Errorf("answer (-want +got):\n%s", diff)
						}
					})
				}
			}
		}
	}

	ways := remoteWays()
	maps.Copy(ways, tunnelWays())
	maps.Copy(ways, foreignWays())

	for way, apply := range ways {
		t.Run(way+" remote token", func(t *testing.T) {
			req := bearerRequest(http.MethodPost, "/mcp", "Bearer "+remoteToken)
			apply(req)
			got := serveSeen(t, h, req)

			if diff := cmp.Diff(grokClient(), got.Client); diff != "" {
				t.Errorf("client (-want +got):\n%s", diff)
			}
		})
	}
}

// TestForeignHostLearnsNothingWhileOn: while on, a remote request that did
// not come through the tunnel, such as a DNS-rebinding page's with a foreign
// Host, gets the foreign 404 on every authorization server route and on
// /mcp without a remote token: it can't read public_url from the metadata
// or the challenge.
func TestForeignHostLearnsNothingWhileOn(t *testing.T) {
	h, _ := newOAuthHandler(t)
	want := foreign404(t, h)
	auths := [][]string{nil, {"Bearer " + testToken}, {"Bearer wf_s_wrong"}}

	for way, apply := range foreignWays() {
		for _, a := range auths {
			reqs := []*http.Request{bearerRequest(http.MethodPost, "/mcp", a...), bearerRequest(http.MethodGet, "/mcp", a...)}

			for _, p := range publicRoutes() {
				reqs = append(reqs, bearerRequest(p.method, p.path, a...))
			}

			for _, req := range reqs {
				t.Run(way+" "+req.Method+" "+req.URL.Path+" "+strings.Join(a, ","), func(t *testing.T) {
					apply(req)

					if diff := cmp.Diff(want, record(h, req)); diff != "" {
						t.Errorf("answer differs from the foreign 404 (-want +got):\n%s", diff)
					}
				})
			}
		}
	}
}

// TestLocalRequestsUnchangedWhileOn: while on, a local /mcp request without
// a token still gets exactly "Bearer", which the connection test relies on,
// and a local request reaches every authorization server route, with any
// Origin.
func TestLocalRequestsUnchangedWhileOn(t *testing.T) {
	h, _ := newOAuthHandler(t)
	got := record(h, bearerRequest(http.MethodPost, "/mcp"))

	if got.Code != http.StatusUnauthorized || got.Header.Values("WWW-Authenticate")[0] != "Bearer" || len(got.Header.Values("WWW-Authenticate")) != 1 {
		t.Errorf("local /mcp without a token: got %+v, want 401 with exactly Bearer", got)
	}

	got = record(h, bearerRequest(http.MethodPost, "/mcp", "Bearer wf_s_wrong"))

	if got.Code != http.StatusUnauthorized || got.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("local /mcp with a wrong token: got %+v, want 401 with exactly Bearer", got)
	}

	for _, p := range publicRoutes() {
		for _, origin := range []string{"", "https://evil.example"} {
			t.Run(p.method+" "+p.path+" Origin "+origin, func(t *testing.T) {
				req := bearerRequest(p.method, p.path)

				if origin != "" {
					req.Header.Set("Origin", origin)
				}

				if code, body := serve(h, req); code != http.StatusOK || body != p.want {
					t.Errorf("got %d %q, want 200 %q", code, body, p.want)
				}
			})
		}
	}
}

// TestTurningOAuthOffRestoresThe404: a reload that turns OAuth off makes
// the next remote request's answer, on every path the switch touches, the
// same byte for byte as a daemon that never had it (ResourceMetadataURL
// nil); turning it on again restores the challenge.
func TestTurningOAuthOffRestoresThe404(t *testing.T) {
	h, sw := newOAuthHandler(t)
	never := newRemoteHandler(t, remoteRegistry(), nil)
	reqs := func() []*http.Request {
		var out []*http.Request

		for _, p := range publicRoutes() {
			out = append(out, bearerRequest(p.method, p.path))
		}

		for _, a := range [][]string{nil, {"Bearer " + testToken}} {
			for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
				out = append(out, bearerRequest(m, "/mcp", a...))
			}
		}

		out = append(out, bearerRequest(http.MethodGet, "/oauth/token"), bearerRequest(http.MethodGet, "/ui/"))

		for _, r := range out {
			r.Header.Set("X-Forwarded-For", "203.0.113.7")
		}

		return out
	}

	if got := record(h, reqs()[len(publicRoutes())]); got.Code != http.StatusUnauthorized {
		t.Fatalf("while on, a remote /mcp without a token got %d, want 401", got.Code)
	}

	sw.set("")
	wants := reqs()

	for i, req := range reqs() {
		t.Run(req.Method+" "+req.URL.Path+" "+req.Header.Get("Authorization"), func(t *testing.T) {
			if diff := cmp.Diff(record(never, wants[i]), record(h, req)); diff != "" {
				t.Errorf("off: answer differs from a daemon without OAuth (-want +got):\n%s", diff)
			}
		})
	}

	sw.set(metadataURL)

	if got := record(h, reqs()[len(publicRoutes())]); got.Code != http.StatusUnauthorized {
		t.Errorf("on again, a remote /mcp without a token got %d, want 401", got.Code)
	}
}

// TestOAuthRequestsLogNoQueryOrLocation: httpd logs a refused remote
// request with its method and path only, and an admitted one not at all,
// so neither an authorize request's code and state nor a redirect's
// Location reach the log, while on or off.
func TestOAuthRequestsLogNoQueryOrLocation(t *testing.T) {
	const secret = "s3cr3t"
	query := "?code=" + secret + "-code&state=" + secret + "-state&redirect_uri=https%3A%2F%2F" + secret + ".example"
	tests := []struct {
		name, method, path string
		on                 bool
		want               []map[string]any
	}{
		{"authorize while on", http.MethodGet, "/oauth/authorize" + query, true, nil},
		{"token while on", http.MethodPost, "/oauth/token" + query, true, nil},
		{"authorize while off", http.MethodGet, "/oauth/authorize" + query, false, []map[string]any{{"guard": "host", "method": "GET", "path": "/oauth/authorize"}}},
		{"authorize with POST", http.MethodPost, "/oauth/authorize" + query, true, []map[string]any{{"guard": "host", "method": "POST", "path": "/oauth/authorize"}}},
		{"mcp challenge", http.MethodPost, "/mcp" + query, true, []map[string]any{{"guard": "remote_token", "method": "POST", "path": "/mcp"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log bytes.Buffer

			h, sw := newOAuthHandlerWith(t, &log, redirectingAuthorize(secret))

			if !tt.on {
				sw.set("")
			}

			req := bearerRequest(tt.method, tt.path, "Bearer wf_s_"+secret)
			req.Header.Set("X-Forwarded-For", "203.0.113.7")
			serve(h, req)
			got := []map[string]any{}

			for line := range strings.SplitSeq(strings.TrimSpace(log.String()), "\n") {
				if line == "" {
					continue
				}

				var entry map[string]any

				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatalf("decode log line %q: %v", line, err)
				}

				delete(entry, "level")
				delete(entry, "message")
				got = append(got, entry)
			}

			want := tt.want

			if want == nil {
				want = []map[string]any{}
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("log (-want +got):\n%s", diff)
			}

			if strings.Contains(log.String(), secret) {
				t.Errorf("log %q contains a query, header, or Location value", log.String())
			}
		})
	}
}

// redirectingAuthorize stands in for the authorize handler: it redirects
// with a code and state in the Location, as Approve's redirect would.
func redirectingAuthorize(secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+secret+".example/cb?code="+secret+"&state="+secret, http.StatusFound)
	})
}

// newOAuthHandlerWith is newOAuthHandler with authorize served by h.
func newOAuthHandlerWith(t *testing.T, log *bytes.Buffer, authorize http.Handler) (http.Handler, *onOff) {
	t.Helper()
	sw := &onOff{}
	sw.set(metadataURL)
	handlers := stubHandlers()
	handlers.OAuthAuthorize = authorize
	h, err := httpd.NewHandler(httpd.Config{
		Port:                testPort,
		Token:               testToken,
		Clients:             clienttoken.NewRegistry(nil, nil),
		Logger:              zerolog.New(log).Level(zerolog.DebugLevel),
		Handlers:            handlers,
		CallbackPath:        "/oauth/callback",
		ResourceMetadataURL: sw.get,
	})

	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	return h, sw
}

// TestCallbackPathMayNotTakeARoute: the OAuth callback, served to any
// remote request with no guard, can't equal or sit under a path another route
// serves: the authorization server's, /mcp, /healthz, /auth/, /admin/, /ui/.
func TestCallbackPathMayNotTakeARoute(t *testing.T) {
	for _, path := range []string{
		httpd.AuthorizePath, httpd.TokenPath, httpd.ResourceMetadataPath, httpd.ServerMetadataPath,
		httpd.ResourceMetadataPath + "/mcp", httpd.ResourceMetadataPath + "/other",
		"/mcp", "/mcp/x", "/healthz", "/healthz/x",
		"/admin", "/admin/", "/admin/status", "/ui", "/ui/", "/ui/oauth/approve",
		"/auth", "/auth/slack", "/ui/../mcp",
	} {
		_, err := httpd.NewHandler(httpd.Config{Port: testPort, Token: testToken, Logger: zerolog.Nop(), CallbackPath: path})

		if err == nil {
			t.Errorf("NewHandler with CallbackPath %q succeeded, want an error", path)
		}
	}

	for _, path := range []string{"/oauth/callback", "/callback", "/oauth/cb"} {
		if _, err := httpd.NewHandler(httpd.Config{Port: testPort, Token: testToken, Logger: zerolog.Nop(), CallbackPath: path}); err != nil {
			t.Errorf("NewHandler with CallbackPath %q: %v", path, err)
		}
	}
}
