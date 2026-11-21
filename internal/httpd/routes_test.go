package httpd_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/httpd"
)

// stub answers 200 with its name and any path values, so a test can see
// which injected handler a request reached.
func stub(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		//nolint:gosec // G705: test stub; the plain-text echo is what the test asserts on.
		_, _ = fmt.Fprintf(w, "%s connector=%s id=%s name=%s",
			name, r.PathValue("connector"), r.PathValue("id"), r.PathValue("name"))
	})
}

// adminStub stands in for admin.New, which routes /admin/* itself: it
// answers 200 with the method and path it received.
func adminStub() http.Handler { return treeStub("admin") }

// treeStub stands in for a handler that routes a whole tree itself
// (admin.New, webui.New): it answers 200 with name, the method, and the
// path it received.
func treeStub(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		//nolint:gosec // G705: test stub; the plain-text echo is what the test asserts on.
		_, _ = fmt.Fprintf(w, "%s %s %s", name, r.Method, r.URL.Path)
	})
}

func stubHandlers() httpd.Handlers {
	return httpd.Handlers{
		MCP:           stub("mcp"),
		AuthConfirm:   stub("auth-confirm"),
		AuthStart:     stub("auth-start"),
		OAuthCallback: stub("oauth-callback"),
		Admin:         adminStub(),
		UI:            treeStub("ui"),
	}
}

func newStubbedHandler(t *testing.T) http.Handler {
	t.Helper()

	return newStubbedHandlerOn(t, netip.Addr{})
}

// newStubbedHandlerOn is newStubbedHandler for a daemon bound to ip, as
// NewServer passes it.
func newStubbedHandlerOn(t *testing.T, ip netip.Addr) http.Handler {
	t.Helper()
	h, err := httpd.NewHandler(httpd.Config{
		Port:     testPort,
		IP:       ip,
		Token:    testToken,
		Version:  testVersion,
		Logger:   zerolog.Nop(),
		Handlers: stubHandlers(),
		// auth.CallbackPath, which the daemon passes.
		CallbackPath: "/oauth/callback",
	})

	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	return h
}

// route is one row of the design §7.7 route table, with what a request that
// passes every guard should reach.
type route struct {
	method, path string
	// want is the body of the stub the request reaches.
	want string
	// bearer, origin, sameOrigin, anyHost mirror the table's guard column.
	bearer     bool
	sameOrigin bool
	anyHost    bool
}

// routeTable is design §7.7, row for row (/mcp expanded per method, and
// /admin/ by the admin API's routes).
func routeTable() []route {
	return []route{
		{method: "GET", path: "/healthz", want: `{"version":"v1.2.3"}` + "\n"},
		{method: "POST", path: "/mcp", want: "mcp connector= id= name=", bearer: true},
		{method: "GET", path: "/mcp", want: "mcp connector= id= name=", bearer: true},
		{method: "DELETE", path: "/mcp", want: "mcp connector= id= name=", bearer: true},
		{method: "GET", path: "/auth/slack", want: "auth-confirm connector=slack id= name="},
		{method: "POST", path: "/auth/slack", want: "auth-start connector=slack id= name=", sameOrigin: true},
		{method: "GET", path: "/oauth/callback?code=c&state=s", want: "oauth-callback connector= id= name=", anyHost: true},
		// Every /admin/ path reaches the admin API, which routes it.
		{method: "POST", path: "/admin/auth/slack", want: "admin POST /admin/auth/slack", bearer: true},
		{method: "GET", path: "/admin/auth/flows/f-1?wait=30s", want: "admin GET /admin/auth/flows/f-1", bearer: true},
		{method: "DELETE", path: "/admin/credentials/slack", want: "admin DELETE /admin/credentials/slack", bearer: true},
		{method: "PUT", path: "/admin/secrets/LINEAR_KEY", want: "admin PUT /admin/secrets/LINEAR_KEY", bearer: true},
		{method: "GET", path: "/admin/status", want: "admin GET /admin/status", bearer: true},
		// Every /ui/ path reaches the console, which routes it; a POST only
		// from the console's own origin.
		{method: "GET", path: "/ui/", want: "ui GET /ui/"},
		{method: "GET", path: "/ui/calls?errors=1", want: "ui GET /ui/calls"},
		{method: "POST", path: "/ui/tokens", want: "ui POST /ui/tokens", sameOrigin: true},
	}
}

// goodRequest builds a request for rt that satisfies every guard.
func goodRequest(rt route) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), rt.method, "http://127.0.0.1:7777"+rt.path, nil)

	if rt.bearer {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}

	if rt.sameOrigin {
		req.Header.Set("Origin", "http://127.0.0.1:7777")
	}

	return req
}

func serve(h http.Handler, req *http.Request) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)

	return rec.Code, string(body)
}

func TestRoutesReachTheirHandlers(t *testing.T) {
	h := newStubbedHandler(t)

	for _, rt := range routeTable() {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			code, body := serve(h, goodRequest(rt))

			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", code, body)
			}

			if body != rt.want {
				t.Errorf("body = %q, want %q", body, rt.want)
			}
		})
	}
}
