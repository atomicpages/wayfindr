package upstream_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// seenRequest is one request a recordingProxy forwarded.
type seenRequest struct {
	Method string
	// Header is the request's values of the header the proxy watches.
	Header []string
}

// recordingProxy is a reverse proxy in front of a fake upstream that logs
// every request it forwards. It can be stopped, so that connecting to it is
// refused, and restarted on the same address.
type recordingProxy struct {
	t      *testing.T
	target *url.URL
	watch  string
	addr   string

	mu   sync.Mutex
	reqs []seenRequest
	srv  *httptest.Server
}

// startProxy starts a recordingProxy to target that records the values of
// header watch.
func startProxy(t *testing.T, target, watch string) *recordingProxy {
	t.Helper()
	u, err := url.Parse(target)

	if err != nil {
		t.Fatal(err)
	}

	p := &recordingProxy{t: t, target: u, watch: watch, addr: "127.0.0.1:0"}
	p.start()
	t.Cleanup(p.stop)

	return p
}

// URL is the proxy's MCP endpoint.
func (p *recordingProxy) URL() string { return "http://" + p.addr }

func (p *recordingProxy) start() {
	p.t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", p.addr)

	if err != nil {
		p.t.Fatalf("listen on %s: %v", p.addr, err)
	}

	p.addr = ln.Addr().String()
	rp := httputil.NewSingleHostReverseProxy(p.target)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.reqs = append(p.reqs, seenRequest{Method: r.Method, Header: r.Header.Values(p.watch)})
		p.mu.Unlock()
		rp.ServeHTTP(w, r)
	}))
	srv.Listener = ln
	srv.Start()
	p.mu.Lock()
	p.srv = srv
	p.mu.Unlock()
}

// stop closes the proxy and every connection through it. Connecting to it
// is refused until restart.
func (p *recordingProxy) stop() {
	p.mu.Lock()
	srv := p.srv
	p.srv = nil
	p.mu.Unlock()

	if srv != nil {
		srv.CloseClientConnections()
		srv.Close()
	}
}

// restart starts the proxy again on the address it had.
func (p *recordingProxy) restart() { p.start() }

// requests returns the requests forwarded since the last reset.
func (p *recordingProxy) requests() []seenRequest {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]seenRequest(nil), p.reqs...)
}

// methods returns the methods of the requests forwarded since the last
// reset, in order.
func (p *recordingProxy) methods() []string {
	var out []string

	for _, r := range p.requests() {
		out = append(out, r.Method)
	}

	return out
}

func (p *recordingProxy) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = nil
}

// sseVersions are the protocol versions that have an HTTP+SSE binding.
func sseVersions() []string { return []string{fakeupstream.V20250618, fakeupstream.V20251125} }

func TestStreamable4xxFallsBackToSSE(t *testing.T) {
	for _, version := range sseVersions() {
		t.Run(version, func(t *testing.T) {
			// A legacy SSE server answers the Streamable POST with 400.
			p := startProxy(t, fakeupstream.StartSSE(t, version).URL, "")
			m, rec := newManager(t, nil)

			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(p.URL())}})

			want := upstream.ConnectorStatus{Connector: "c", State: upstream.StateReady}

			if diff := cmp.Diff(want, statusOf(t, m, "c"), ignoreSince); diff != "" {
				t.Errorf("status (-want +got):\n%s", diff)
			}

			wantStates := []upstream.State{upstream.StateStarting, upstream.StateReady}

			if diff := cmp.Diff(wantStates, rec.of("c")); diff != "" {
				t.Errorf("OnStateChanged (-want +got):\n%s", diff)
			}

			if got := p.methods(); len(got) < 2 || got[0] != http.MethodPost || !contains(got, http.MethodGet) {
				t.Errorf("requests = %v, want a Streamable POST, then the SSE GET", got)
			}
		})
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}

	return false
}

func TestForcedSSESkipsStreamableProbe(t *testing.T) {
	for _, version := range sseVersions() {
		t.Run(version, func(t *testing.T) {
			p := startProxy(t, fakeupstream.StartSSE(t, version).URL, "")
			m, _ := newManager(t, nil)

			apply(context.Background(), m, nil, config.Connectors{
				"c": {URL: config.Template(p.URL()), Transport: config.TransportSSE},
			})

			if got := statusOf(t, m, "c").State; got != upstream.StateReady {
				t.Errorf("state = %q, want ready", got)
			}

			if got := p.methods(); len(got) == 0 || got[0] != http.MethodGet {
				t.Errorf("requests = %v, want the SSE GET first and no Streamable POST before it", got)
			}
		})
	}
}

func TestFallbackOnlyOn4xxOtherThanAuth(t *testing.T) {
	tests := []struct {
		status       int
		wantFallback bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusNotFound, true},
		{http.StatusMethodNotAllowed, true},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusInternalServerError, false},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125, fakeupstream.WithStreamableStatus(tt.status))
			p := startProxy(t, srv.URL, "")
			m, _ := newManager(t, nil)

			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(p.URL())}})

			// The fake answers everything with the status, SSE GET included,
			// so the connector is unreachable either way; the GET shows
			// whether SSE was tried.
			if got := statusOf(t, m, "c").State; got != upstream.StateUnreachable {
				t.Errorf("state = %q, want unreachable", got)
			}

			if got := contains(p.methods(), http.MethodGet); got != tt.wantFallback {
				t.Errorf("SSE tried = %v, want %v (requests %v)", got, tt.wantFallback, p.methods())
			}
		})
	}
}

func TestConfiguredHeadersReachUpstream(t *testing.T) {
	type upstreamCase struct {
		name      string
		url       string
		transport string
	}
	var cases []upstreamCase

	for _, v := range fakeupstream.Versions() {
		cases = append(cases, upstreamCase{"streamable " + v, fakeupstream.StartStreamable(t, v).URL, ""})
	}

	for _, v := range sseVersions() {
		cases = append(cases,
			upstreamCase{"sse fallback " + v, fakeupstream.StartSSE(t, v).URL, ""},
			upstreamCase{"forced sse " + v, fakeupstream.StartSSE(t, v).URL, config.TransportSSE},
		)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := startProxy(t, tc.url, "X-Api-Key")
			m, _ := newManager(t, env{"secret:key": "k-123"})

			apply(context.Background(), m, nil, config.Connectors{"c": {
				URL:       config.Template(p.URL()),
				Transport: tc.transport,
				Headers:   map[string]config.Template{"X-Api-Key": "${secret:key}"},
			}})

			if got := statusOf(t, m, "c").State; got != upstream.StateReady {
				t.Fatalf("state = %q, want ready", got)
			}

			reqs := p.requests()

			if len(reqs) == 0 {
				t.Fatal("no requests reached the upstream")
			}

			for i, r := range reqs {
				if diff := cmp.Diff([]string{"k-123"}, r.Header); diff != "" {
					t.Errorf("request %d (%s) X-Api-Key (-want +got):\n%s", i, r.Method, diff)
				}
			}
		})
	}
}

func TestAuthTransportWrapsHTTPConnectors(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		transport string
	}{
		{"streamable", fakeupstream.StartStreamable(t, fakeupstream.V20260728).URL, ""},
		{"sse fallback", fakeupstream.StartSSE(t, fakeupstream.V20251125).URL, ""},
		{"forced sse", fakeupstream.StartSSE(t, fakeupstream.V20251125).URL, config.TransportSSE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := startProxy(t, tt.url, "Authorization")
			var mu sync.Mutex
			var wrapped []config.Connector
			m, _ := newManager(t, nil, func(o *upstream.Options) {
				o.AuthTransport = func(c config.Connector, next http.RoundTripper) http.RoundTripper {
					mu.Lock()
					wrapped = append(wrapped, c)
					mu.Unlock()

					return roundTripFunc(func(r *http.Request) (*http.Response, error) {
						r = r.Clone(r.Context())
						r.Header.Set("Authorization", "Bearer from-hook")

						return next.RoundTrip(r)
					})
				}
			})

			conn := config.Connector{URL: config.Template(p.URL()), Transport: tt.transport}
			apply(context.Background(), m, nil, config.Connectors{"c": conn})

			if got := statusOf(t, m, "c").State; got != upstream.StateReady {
				t.Fatalf("state = %q, want ready", got)
			}

			mu.Lock()
			// The hook gets the config the connector is dialed with, named.
			dialed := conn
			dialed.Name = "c"

			if diff := cmp.Diff([]config.Connector{dialed}, wrapped); diff != "" {
				t.Errorf("AuthTransport calls (-want +got):\n%s", diff)
			}

			mu.Unlock()
			reqs := p.requests()

			if len(reqs) == 0 {
				t.Fatal("no requests reached the upstream")
			}

			for i, r := range reqs {
				if diff := cmp.Diff([]string{"Bearer from-hook"}, r.Header); diff != "" {
					t.Errorf("request %d (%s) Authorization (-want +got):\n%s", i, r.Method, diff)
				}
			}
		})
	}
}

// TestCredentialsNeverLeaveTheConnectorOrigin: a redirect to another origin
// and an HTTP+SSE endpoint event naming one are refused before anything is
// sent there, so the configured headers and the auth hook's token never
// reach it (wayfindr-twbw).
func TestCredentialsNeverLeaveTheConnectorOrigin(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Method+" "+r.URL.Path+" key="+r.Header.Get("X-Api-Key")+" auth="+r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(foreign.Close)

	tests := []struct {
		name      string
		transport string
		handler   http.HandlerFunc
	}{
		{"redirect", "", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, foreign.URL+"/mcp", http.StatusTemporaryRedirect)
		}},
		{"redirect from sse", config.TransportSSE, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, foreign.URL+"/sse", http.StatusFound)
		}},
		{"sse endpoint on another origin", config.TransportSSE, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: endpoint\ndata: " + foreign.URL + "/message\n\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mu.Lock()
			leaked = nil
			mu.Unlock()
			srv := httptest.NewServer(tt.handler)
			t.Cleanup(srv.Close)
			m, _ := newManager(t, env{"secret:key": "k-123"}, func(o *upstream.Options) {
				o.AuthTransport = func(_ config.Connector, next http.RoundTripper) http.RoundTripper {
					return roundTripFunc(func(r *http.Request) (*http.Response, error) {
						r = r.Clone(r.Context())
						r.Header.Set("Authorization", "Bearer from-hook")

						return next.RoundTrip(r)
					})
				}
			})

			apply(context.Background(), m, nil, config.Connectors{"c": {
				URL:       config.Template(srv.URL + "/mcp"),
				Transport: tt.transport,
				Headers:   map[string]config.Template{"X-Api-Key": "${secret:key}"},
			}})

			if got := statusOf(t, m, "c").State; got == upstream.StateReady {
				t.Errorf("state = %q, want the connect to fail", got)
			}

			mu.Lock()
			defer mu.Unlock()

			if len(leaked) != 0 {
				t.Errorf("the other origin got %q, want no request", leaked)
			}
		})
	}
}

// TestSecretInURLPathIsMasked: a reference in the url's path resolves to a
// value that the connect error quotes; the connector's reason shows the
// reference instead (wayfindr-wt2v).
func TestSecretInURLPathIsMasked(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close() // the connect is refused, and the error quotes the url

	const value = "zap-k-123456"
	m, _ := newManager(t, env{"secret:zap": value})

	apply(context.Background(), m, nil, config.Connectors{"c": {
		URL: config.Template(down.URL + "/api/mcp/s/${secret:zap}/mcp"),
	}})

	st := statusOf(t, m, "c")

	if st.State != upstream.StateUnreachable {
		t.Fatalf("state = %q, want unreachable", st.State)
	}

	if strings.Contains(st.Reason, value) || !strings.Contains(st.Reason, "/s/${secret:zap}/mcp") {
		t.Errorf("reason = %q, want the reference in place of the secret's value", st.Reason)
	}

	_, err := m.ListTools(context.Background(), "c")

	if err == nil || strings.Contains(err.Error(), value) {
		t.Errorf("ListTools error = %v, want one that hides the secret's value", err)
	}
}

// TestSameOriginRedirectIsFollowed: the origin guard leaves a redirect
// within the connector's origin alone.
func TestSameOriginRedirectIsFollowed(t *testing.T) {
	up, err := url.Parse(fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL)

	if err != nil {
		t.Fatal(err)
	}

	rp := httputil.NewSingleHostReverseProxy(up)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/", http.StatusPermanentRedirect)

			return
		}

		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	m, _ := newManager(t, nil)

	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL + "/old")}})

	if got := statusOf(t, m, "c").State; got != upstream.StateReady {
		t.Errorf("state = %q, want ready", got)
	}
}

func TestRefusedConnectorIsUnreachableAndRecoversOnNextCall(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			p := startProxy(t, fakeupstream.StartStreamable(t, version).URL, "")
			p.stop()
			m, rec := newManager(t, nil)

			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(p.URL())}})

			if got := statusOf(t, m, "c").State; got != upstream.StateUnreachable {
				t.Fatalf("state while refused = %q, want unreachable", got)
			}

			p.restart()
			use(t, m, "c")

			wantStates := []upstream.State{upstream.StateStarting, upstream.StateUnreachable, upstream.StateReady}

			if diff := cmp.Diff(wantStates, rec.of("c")); diff != "" {
				t.Errorf("OnStateChanged (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDroppedSessionIsRecreatedOverRememberedSSE(t *testing.T) {
	for _, version := range sseVersions() {
		t.Run(version, func(t *testing.T) {
			p := startProxy(t, fakeupstream.StartSSE(t, version).URL, "")
			m, rec := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(p.URL())}})
			s, release, err := m.Acquire(context.Background(), "c")

			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}

			release()

			// The upstream goes away: the SSE stream breaks and new
			// connections are refused.
			p.stop()
			_ = s.Wait()
			waitForState(t, m, "c", upstream.StateUnreachable)

			p.restart()
			p.reset()
			use(t, m, "c")

			wantStates := []upstream.State{
				upstream.StateStarting, upstream.StateReady,
				upstream.StateUnreachable, upstream.StateReady,
			}

			if diff := cmp.Diff(wantStates, rec.of("c")); diff != "" {
				t.Errorf("OnStateChanged (-want +got):\n%s", diff)
			}

			if got := p.methods(); len(got) == 0 || got[0] != http.MethodGet {
				t.Errorf("requests on reconnect = %v, want the SSE GET first: the fallback is remembered", got)
			}
		})
	}
}

func TestEveryNewHTTPSessionFiresToolsChanged(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			p := startProxy(t, fakeupstream.StartStreamable(t, version).URL, "")
			m, rec := newManager(t, nil)
			// refuse makes the upstream go away and lets a call find out.
			refuse := func() {
				t.Helper()
				p.stop()

				if _, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil); err == nil {
					t.Fatal("Call while the upstream refuses connections succeeded")
				}

				p.restart()
			}

			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(p.URL())}})

			if got := rec.toolsChangedOf("c"); got != 1 {
				t.Fatalf("OnToolsChanged calls after Apply connected = %d, want 1", got)
			}

			use(t, m, "c")

			if got := rec.toolsChangedOf("c"); got != 1 {
				t.Errorf("OnToolsChanged calls after a use of the open session = %d, want 1", got)
			}

			refuse()

			if err := m.Retry(context.Background(), "c"); err != nil {
				t.Fatalf("Retry: %v", err)
			}

			if got := rec.toolsChangedOf("c"); got != 2 {
				t.Errorf("OnToolsChanged calls after Retry connected = %d, want 2", got)
			}

			refuse()
			use(t, m, "c")

			if got := rec.toolsChangedOf("c"); got != 3 {
				t.Errorf("OnToolsChanged calls after a use reconnected = %d, want 3", got)
			}
		})
	}
}

// waitForState uses connector, as successive calls would, until it is in
// state want or a deadline passes.
func waitForState(t *testing.T, m *upstream.Manager, connector string, want upstream.State) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)

	for {
		_, _ = m.ListTools(context.Background(), connector)
		got := statusOf(t, m, connector).State

		if got == want {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("state = %q after 5s, want %q", got, want)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
