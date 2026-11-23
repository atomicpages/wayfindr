package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// seen is what the daemon side of the proxy received.
type seen struct {
	Host, Authorization, Origin string
}

func TestProxyAddsBearerAndMovesOnlyItsOwnPort(t *testing.T) {
	got := make(chan seen, 1)
	daemon := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got <- seen{r.Host, r.Header.Get("Authorization"), r.Header.Get("Origin")}
	}))
	t.Cleanup(daemon.Close)
	target := daemon.Listener.Addr().String()
	_, targetPort, _ := net.SplitHostPort(target)

	h, err := newTargetProxy("127.0.0.1:4100", target, "wf_secret", false)

	if err != nil {
		t.Fatalf("newTargetProxy: %v", err)
	}

	tests := []struct {
		name, host, origin string
		want               seen
	}{
		{"loopback host", "127.0.0.1:4100", "", seen{"127.0.0.1:" + targetPort, "Bearer wf_secret", ""}},
		{"localhost keeps its name", "localhost:4100", "", seen{"localhost:" + targetPort, "Bearer wf_secret", ""}},
		{"foreign host keeps its name", "evil.example:4100", "", seen{"evil.example:" + targetPort, "Bearer wf_secret", ""}},
		{"host on another port passes through", "evil.example", "http://evil.example", seen{"evil.example", "Bearer wf_secret", "http://evil.example"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
			req.Host = tc.host
			req.Header.Set("Authorization", "Bearer client-supplied")

			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}

			h.ServeHTTP(httptest.NewRecorder(), req)

			if diff := cmp.Diff(tc.want, <-got); diff != "" {
				t.Errorf("daemon saw (-want +got):\n%s", diff)
			}
		})
	}
}

// A Host the plain proxy moves to the target's port is written as a client
// writes an http authority: without :80, and with any other port.
func TestPlainProxyDropsTheDefaultPortFromHost(t *testing.T) {
	tests := []struct {
		name, target, host, wantHost string
	}{
		{"default port dropped", "127.0.0.1:80", "127.0.0.1:4100", "127.0.0.1"},
		{"named host, default port dropped", "127.0.0.1:80", "localhost:4100", "localhost"},
		{"ipv6 host, default port dropped", "[::1]:80", "[::1]:4100", "[::1]"},
		{"other port kept", "127.0.0.1:8080", "127.0.0.1:4100", "127.0.0.1:8080"},
		{"another port passes through even when it is 80", "127.0.0.1:8080", "evil.example:80", "evil.example:80"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := newTargetProxy("127.0.0.1:4100", tc.target, "wf_secret", false)

			if err != nil {
				t.Fatalf("newTargetProxy: %v", err)
			}

			in := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
			in.Host = tc.host
			pr := &httputil.ProxyRequest{In: in, Out: in.Clone(t.Context())}
			h.(*httputil.ReverseProxy).Rewrite(pr)

			if pr.Out.Host != tc.wantHost || pr.Out.URL.Scheme != "http" || pr.Out.URL.Host != tc.target {
				t.Errorf("outgoing Host, scheme, URL host = %q, %q, %q; want %q, http, %q",
					pr.Out.Host, pr.Out.URL.Scheme, pr.Out.URL.Host, tc.wantHost, tc.target)
			}
		})
	}
}

func TestTLSProxySendsTheAuthorityTheUserWrote(t *testing.T) {
	tests := []struct {
		name, target, wantHost string
	}{
		{"default port dropped", "x.ngrok-free.app:443", "x.ngrok-free.app"},
		{"other port kept", "x.example:8443", "x.example:8443"},
		{"ipv6 default port dropped", "[::1]:443", "[::1]"},
		{"ipv6 other port kept", "[::1]:8443", "[::1]:8443"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := newTargetProxy("127.0.0.1:4100", tc.target, "wf_secret", true)

			if err != nil {
				t.Fatalf("newTargetProxy: %v", err)
			}

			in := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
			in.Host = "127.0.0.1:4100"
			pr := &httputil.ProxyRequest{In: in, Out: in.Clone(t.Context())}
			h.(*httputil.ReverseProxy).Rewrite(pr)

			got := struct{ Scheme, Host, URLHost, Authorization string }{
				pr.Out.URL.Scheme, pr.Out.Host, pr.Out.URL.Host, pr.Out.Header.Get("Authorization"),
			}
			want := struct{ Scheme, Host, URLHost, Authorization string }{
				"https", tc.wantHost, tc.target, "Bearer wf_secret",
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("outgoing request (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTLSProxyForwardsOverHTTPS(t *testing.T) {
	got := make(chan seen, 1)
	daemon := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got <- seen{r.Host, r.Header.Get("Authorization"), r.Header.Get("Origin")}
	}))
	t.Cleanup(daemon.Close)
	target := daemon.Listener.Addr().String()

	h, err := newTargetProxy("127.0.0.1:4100", target, "wf_secret", true)

	if err != nil {
		t.Fatalf("newTargetProxy: %v", err)
	}

	h.(*httputil.ReverseProxy).Transport = daemon.Client().Transport

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
	req.Host = "127.0.0.1:4100"
	h.ServeHTTP(httptest.NewRecorder(), req)

	if diff := cmp.Diff(seen{target, "Bearer wf_secret", ""}, <-got); diff != "" {
		t.Errorf("daemon saw (-want +got):\n%s", diff)
	}
}
