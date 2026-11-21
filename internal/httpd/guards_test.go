package httpd_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// TestForeignOriginForbidden: a browser page from any non-loopback origin
// gets 403 everywhere except the OAuth callback.
func TestForeignOriginForbidden(t *testing.T) {
	h := newStubbedHandler(t)
	origins := []string{
		"https://evil.example",
		"http://evil.example:7777",
		"http://127.0.0.1.nip.io:7777",
		"http://localhost.evil.example:7777",
		"null",
		"file://",
		"not a url",
	}

	for _, rt := range routeTable() {
		for _, origin := range origins {
			t.Run(rt.method+" "+rt.path+" Origin "+origin, func(t *testing.T) {
				req := goodRequest(rt)
				req.Header.Set("Origin", origin)
				code, body := serve(h, req)

				if rt.anyHost {
					if code != http.StatusOK || body != rt.want {
						t.Errorf("got %d %q, want 200 %q", code, body, rt.want)
					}

					return
				}

				if code != http.StatusForbidden {
					t.Errorf("status = %d, want 403 (body %q)", code, body)
				}
			})
		}
	}
}

// TestLoopbackOriginAllowed: a loopback origin on any port passes the Origin
// guard (a local browser-based MCP client, for example). POST /auth needs
// the exact same origin, so it is covered by TestAuthStartSameOrigin.
func TestLoopbackOriginAllowed(t *testing.T) {
	h := newStubbedHandler(t)
	origins := []string{
		"http://127.0.0.1:7777",
		"http://localhost:7777",
		"http://[::1]:7777",
		"http://localhost:6274",
		"http://127.0.0.1",
	}

	for _, rt := range routeTable() {
		if rt.sameOrigin {
			continue
		}

		for _, origin := range origins {
			t.Run(rt.method+" "+rt.path+" Origin "+origin, func(t *testing.T) {
				req := goodRequest(rt)
				req.Header.Set("Origin", origin)

				if code, body := serve(h, req); code != http.StatusOK || body != rt.want {
					t.Errorf("got %d %q, want 200 %q", code, body, rt.want)
				}
			})
		}
	}
}

// TestBearer: bearer routes answer 401 with a Bearer challenge to a missing
// or wrong token; the other routes don't look at Authorization at all.
func TestBearer(t *testing.T) {
	h := newStubbedHandler(t)
	auths := []struct {
		name   string
		header []string // nil means no Authorization header
	}{
		{"missing", nil},
		{"empty", []string{""}},
		{"scheme only", []string{"Bearer"}},
		{"scheme and space", []string{"Bearer "}},
		{"wrong token", []string{"Bearer wf_wrong"}},
		{"token prefix", []string{"Bearer wf_test"}},
		{"token with suffix", []string{"Bearer wf_test-token-extra"}},
		{"token without scheme", []string{testToken}},
		{"basic scheme", []string{"Basic " + testToken}},
		{"two headers", []string{"Bearer " + testToken, "Bearer wf_wrong"}},
	}

	for _, rt := range routeTable() {
		for _, a := range auths {
			t.Run(rt.method+" "+rt.path+" "+a.name, func(t *testing.T) {
				req := goodRequest(rt)
				req.Header.Del("Authorization")

				for _, v := range a.header {
					req.Header.Add("Authorization", v)
				}

				code, body := serve(h, req)

				if !rt.bearer {
					if code != http.StatusOK || body != rt.want {
						t.Errorf("got %d %q, want 200 %q", code, body, rt.want)
					}

					return
				}

				if code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401 (body %q)", code, body)
				}
			})
		}
	}
}

func TestBearerChallengeAndScheme(t *testing.T) {
	h := newStubbedHandler(t)
	req := goodRequest(route{method: "GET", path: "/admin/status"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
	}

	// The auth scheme is case-insensitive (RFC 9110 §11.1).
	req = goodRequest(route{method: "GET", path: "/admin/status"})
	req.Header.Set("Authorization", "bearer "+testToken)

	if code, _ := serve(h, req); code != http.StatusOK {
		t.Errorf("lowercase scheme: status = %d, want 200", code)
	}
}

func TestLoopbackHostsAccepted(t *testing.T) {
	h := newStubbedHandler(t)
	hosts := []string{"127.0.0.1:7777", "localhost:7777", "LocalHost:7777", "[::1]:7777"}

	for _, rt := range routeTable() {
		for _, host := range hosts {
			t.Run(rt.method+" "+rt.path+" Host "+host, func(t *testing.T) {
				req := goodRequest(rt)
				req.Host = host

				if rt.sameOrigin {
					req.Header.Set("Origin", "http://"+host)
				}

				if code, body := serve(h, req); code != http.StatusOK || body != rt.want {
					t.Errorf("got %d %q, want 200 %q", code, body, rt.want)
				}
			})
		}
	}
}

// TestForeignHostRejected covers DNS rebinding: a page on an attacker's name
// that resolves to 127.0.0.1 sends its own name as Host. Only the OAuth
// callback, which the authorization server redirects to, accepts it.
func TestForeignHostRejected(t *testing.T) {
	h := newStubbedHandler(t)
	hosts := []string{
		"evil.example:7777",     // rebinding name
		"127.0.0.1.nip.io:7777", // wildcard DNS to loopback
		"localhost.evil.example:7777",
		"evil.example",
		"127.0.0.1:8080", // loopback, wrong port
		"localhost",      // no port
		"127.0.0.1",      // no port
		"[::2]:7777",
		"0.0.0.0:7777",
		"127.0.0.2:7777",
		"[::ffff:127.0.0.1]:7777",
		"",
	}

	for _, rt := range routeTable() {
		for _, host := range hosts {
			t.Run(rt.method+" "+rt.path+" Host "+host, func(t *testing.T) {
				req := goodRequest(rt)
				req.Host = host
				code, body := serve(h, req)

				if rt.anyHost {
					if code != http.StatusOK || body != rt.want {
						t.Errorf("got %d %q, want 200 %q", code, body, rt.want)
					}

					return
				}

				if code != http.StatusNotFound {
					t.Errorf("status = %d, want 404 (body %q)", code, body)
				}
			})
		}
	}
}

// TestForwardedRequestRejected covers a tunnel or proxy that rewrites Host to
// the loopback address (ngrok --host-header=rewrite, cloudflared
// httpHostHeader): it still adds a forwarding header, and such a request is
// treated as foreign. Only the OAuth callback accepts it (wayfindr-rrvq).
func TestForwardedRequestRejected(t *testing.T) {
	h := newStubbedHandler(t)
	headers := map[string]string{
		"Forwarded":         "for=203.0.113.7",
		"X-Forwarded-For":   "203.0.113.7",
		"X-Forwarded-Host":  "abc.ngrok.app",
		"X-Forwarded-Proto": "https",
		"X-Real-Ip":         "203.0.113.7",
		"Cf-Connecting-Ip":  "203.0.113.7",
	}

	for _, rt := range routeTable() {
		for name, value := range headers {
			t.Run(rt.method+" "+rt.path+" "+name, func(t *testing.T) {
				req := goodRequest(rt)
				req.Header.Set(name, value)
				code, body := serve(h, req)

				if rt.anyHost {
					if code != http.StatusOK || body != rt.want {
						t.Errorf("got %d %q, want 200 %q", code, body, rt.want)
					}

					return
				}

				if code != http.StatusNotFound {
					t.Errorf("status = %d, want 404 (body %q)", code, body)
				}
			})
		}
	}
}

// TestBoundAddressAccepted: a daemon bound to another loopback address,
// such as server.listen = "127.0.0.2:7777", is reachable by that IP literal
// (wayfindr-cqet), as Host and as Origin, besides
// the three loopback names. An IP literal is never resolved, so it can't be
// rebound.
func TestBoundAddressAccepted(t *testing.T) {
	h := newStubbedHandlerOn(t, netip.MustParseAddr("127.0.0.2"))

	for _, rt := range routeTable() {
		for _, host := range []string{"127.0.0.2:7777", "127.0.0.1:7777", "localhost:7777"} {
			t.Run(rt.method+" "+rt.path+" Host "+host, func(t *testing.T) {
				req := goodRequest(rt)
				req.Host = host

				if rt.sameOrigin {
					req.Header.Set("Origin", "http://"+host)
				} else {
					req.Header.Set("Origin", "http://127.0.0.2:6274")
				}

				if code, body := serve(h, req); code != http.StatusOK || body != rt.want {
					t.Errorf("got %d %q, want 200 %q", code, body, rt.want)
				}
			})
		}
	}
}

// TestBoundAddressAddsOnlyItself: the bound address adds that one IP
// literal on the daemon's port, and nothing for an address that isn't
// loopback.
func TestBoundAddressAddsOnlyItself(t *testing.T) {
	tests := []struct {
		name, ip, host, origin string
	}{
		{"another loopback address", "127.0.0.2", "127.0.0.3:7777", ""},
		{"wrong port", "127.0.0.2", "127.0.0.2:8080", ""},
		{"no port", "127.0.0.2", "127.0.0.2", ""},
		{"ipv4-mapped form", "127.0.0.2", "[::ffff:127.0.0.2]:7777", ""},
		{"leading zeros", "127.0.0.2", "127.000.000.002:7777", ""},
		{"not loopback", "192.0.2.1", "192.0.2.1:7777", ""},
		{"origin on another loopback address", "127.0.0.2", "127.0.0.2:7777", "http://127.0.0.3:7777"},
		{"origin on an address that isn't loopback", "192.0.2.1", "127.0.0.1:7777", "http://192.0.2.1:7777"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newStubbedHandlerOn(t, netip.MustParseAddr(tt.ip))
			req := goodRequest(route{method: "GET", path: "/healthz"})
			req.Host = tt.host
			want := http.StatusNotFound

			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
				want = http.StatusForbidden
			}

			if code, body := serve(h, req); code != want {
				t.Errorf("status = %d, want %d (body %q)", code, want, body)
			}
		})
	}
}

// TestAuthStartSameOrigin: POST /auth/{connector} is the browser form behind
// the confirmation page, so it needs Origin to be exactly the daemon's own
// origin. A missing Origin is refused here, unlike everywhere else.
func TestAuthStartSameOrigin(t *testing.T) {
	h := newStubbedHandler(t)
	tests := []struct {
		name   string
		host   string
		origin []string
		want   int
	}{
		{"same origin", "127.0.0.1:7777", []string{"http://127.0.0.1:7777"}, http.StatusOK},
		{"same origin localhost", "localhost:7777", []string{"http://localhost:7777"}, http.StatusOK},
		{"missing", "127.0.0.1:7777", nil, http.StatusForbidden},
		{"other loopback port", "127.0.0.1:7777", []string{"http://127.0.0.1:6274"}, http.StatusForbidden},
		{"other loopback name", "127.0.0.1:7777", []string{"http://localhost:7777"}, http.StatusForbidden},
		{"https", "127.0.0.1:7777", []string{"https://127.0.0.1:7777"}, http.StatusForbidden},
		{"no port", "127.0.0.1:7777", []string{"http://127.0.0.1"}, http.StatusForbidden},
		{"foreign", "127.0.0.1:7777", []string{"https://evil.example"}, http.StatusForbidden},
		{"null", "127.0.0.1:7777", []string{"null"}, http.StatusForbidden},
		{"two headers", "127.0.0.1:7777", []string{"http://127.0.0.1:7777", "http://127.0.0.1:7777"}, http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+tt.host+"/auth/slack", nil)

			for _, o := range tt.origin {
				req.Header.Add("Origin", o)
			}

			if code, body := serve(h, req); code != tt.want {
				t.Errorf("status = %d, want %d (body %q)", code, tt.want, body)
			}
		})
	}
}
