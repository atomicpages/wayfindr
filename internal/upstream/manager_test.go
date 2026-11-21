package upstream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

func TestMain(m *testing.M) {
	lingerIfGrandchild()
	hangIfAsked()
	reportIfChild()
	fakeupstream.ServeStdioIfChild()
	os.Exit(m.Run())
}

// stateChange is one OnStateChanged call.
type stateChange struct {
	Connector string
	State     upstream.State
}

// recorder collects callback calls. The manager may call it from several
// goroutines.
type recorder struct {
	mu           sync.Mutex
	changes      []stateChange
	toolsChanged []string
}

func (r *recorder) onToolsChanged(connector string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolsChanged = append(r.toolsChanged, connector)
}

func (r *recorder) onStateChanged(connector string, s upstream.State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changes = append(r.changes, stateChange{connector, s})
}

// of returns the states connector went through, in order.
func (r *recorder) of(connector string) []upstream.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []upstream.State

	for _, c := range r.changes {
		if c.Connector == connector {
			out = append(out, c.State)
		}
	}

	return out
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changes = nil
}

// toolsChangedOf counts the OnToolsChanged calls for connector.
func (r *recorder) toolsChangedOf(connector string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0

	for _, c := range r.toolsChanged {
		if c == connector {
			n++
		}
	}

	return n
}

// env is a Resolver over a fixed map keyed "kind:name". A missing key is
// ErrNotFound, like the daemon's real resolver.
type env map[string]string

func (e env) Resolve(_ context.Context, ref config.Ref) (string, error) {
	v, ok := e[string(ref.Kind)+":"+ref.Name]

	if !ok {
		return "", config.ErrNotFound
	}

	return v, nil
}

// newManager returns a Manager whose callbacks feed the returned recorder.
// Each with func adjusts the options before New.
func newManager(t *testing.T, r config.Resolver, with ...func(*upstream.Options)) (*upstream.Manager, *recorder) {
	t.Helper()
	rec := &recorder{}

	if r == nil {
		r = env{}
	}

	opts := upstream.Options{
		Logger:         zerolog.New(zerolog.NewTestWriter(t)),
		Resolver:       r,
		OnStateChanged: rec.onStateChanged,
		// Set so that every test connects the way the daemon will: with a
		// tool list_changed handler, which under 2026-07-28 opens a
		// subscriptions/listen stream during connect.
		OnToolsChanged: rec.onToolsChanged,
	}

	for _, f := range with {
		f(&opts)
	}

	m := upstream.New(opts)
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return m, rec
}

// apply moves m from the connectors before (nil for none) to after, as the
// daemon's reload does: one config.Diff, whose Changes Apply carries out.
func apply(ctx context.Context, m *upstream.Manager, before, after config.Connectors) {
	m.Apply(ctx, after, config.Diff(&config.Config{Connectors: before}, &config.Config{Connectors: after}))
}

// statusOf returns the status of one connector, failing the test if the
// manager doesn't list it.
func statusOf(t *testing.T, m *upstream.Manager, connector string) upstream.ConnectorStatus {
	t.Helper()

	for _, s := range m.Status() {
		if s.Connector == connector {
			return s
		}
	}

	t.Fatalf("Status() does not list %q", connector)

	return upstream.ConnectorStatus{}
}

func connectorNames(m *upstream.Manager) []string {
	var names []string

	for _, s := range m.Status() {
		names = append(names, s.Connector)
	}

	return names
}

// stdioConnector is a connector that runs a stdio fake.
func stdioConnector(t *testing.T, version string, opts ...fakeupstream.Option) config.Connector {
	t.Helper()
	cmd := fakeupstream.StdioCommand(t, version, opts...)
	c := config.Connector{Command: cmd.Path, Env: map[string]config.Template{}}

	for _, a := range cmd.Args[1:] {
		c.Args = append(c.Args, config.Template(a))
	}

	for _, kv := range cmd.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "WAYFINDR_FAKEUPSTREAM_CONFIG" {
			c.Env[k] = config.Template(v)
		}
	}

	return c
}

var ignoreSince = cmpopts.IgnoreFields(upstream.ConnectorStatus{}, "Since", "NotReadySince")

// NotReadySince is when the connector was last ready, so a move from one
// state that is not ready to another keeps it, while Since follows each
// state; becoming ready clears it.
func TestNotReadySinceSurvivesMovesBetweenStatesThatAreNotReady(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	at := func(t time.Time) {
		mu.Lock()
		defer mu.Unlock()
		now = t
	}
	t0 := now
	m, _ := newManager(t, nil, func(o *upstream.Options) {
		o.Now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()

			return now
		}
	})

	missing := stdioConnector(t, fakeupstream.V20251125)
	missing.Env["TOKEN"] = "${env:MISSING}"
	apply(context.Background(), m, nil, config.Connectors{"gh": missing})

	if got := statusOf(t, m, "gh"); got.State != upstream.StateAuthError || !got.NotReadySince.Equal(t0) {
		t.Fatalf("status = %s, not ready since %v; want auth_error, not ready since %v", got.State, got.NotReadySince, t0)
	}

	t1 := t0.Add(30 * time.Hour)
	at(t1)
	broken := stdioConnector(t, fakeupstream.V20251125)
	broken.Command = filepath.Join(t.TempDir(), "no-such-command")
	apply(context.Background(), m, config.Connectors{"gh": missing}, config.Connectors{"gh": broken})

	if got := statusOf(t, m, "gh"); got.State != upstream.StateUnreachable || !got.Since.Equal(t1) || !got.NotReadySince.Equal(t0) {
		t.Fatalf("status = %s since %v, not ready since %v; want unreachable since %v, not ready since %v",
			got.State, got.Since, got.NotReadySince, t1, t0)
	}

	at(t1.Add(time.Hour))
	working := stdioConnector(t, fakeupstream.V20251125)
	apply(context.Background(), m, config.Connectors{"gh": broken}, config.Connectors{"gh": working})

	if got := statusOf(t, m, "gh"); got.State != upstream.StateReady || !got.NotReadySince.IsZero() {
		t.Errorf("status = %s, not ready since %v; want ready, and no time", got.State, got.NotReadySince)
	}
}

func TestApplyConnectsStreamableConnectorAndReportsReady(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			srv := fakeupstream.StartStreamable(t, version)
			m, rec := newManager(t, nil)

			apply(context.Background(), m, nil, config.Connectors{
				"linear": {Name: "linear", URL: config.Template(srv.URL)},
			})

			want := upstream.ConnectorStatus{Connector: "linear", State: upstream.StateReady}
			got := statusOf(t, m, "linear")

			if diff := cmp.Diff(want, got, ignoreSince); diff != "" {
				t.Errorf("status (-want +got):\n%s", diff)
			}

			if got.Since.IsZero() {
				t.Error("status Since is zero")
			}

			wantStates := []upstream.State{upstream.StateStarting, upstream.StateReady}

			if diff := cmp.Diff(wantStates, rec.of("linear")); diff != "" {
				t.Errorf("OnStateChanged (-want +got):\n%s", diff)
			}
		})
	}
}

func TestApplyConnectsEveryTransport(t *testing.T) {
	sse := fakeupstream.StartSSE(t, fakeupstream.V20251125)
	tests := []struct {
		name      string
		conn      config.Connector
		wantState upstream.State
	}{
		{"stdio 2025-06-18", stdioConnector(t, fakeupstream.V20250618), upstream.StateReady},
		{"stdio 2026-07-28", stdioConnector(t, fakeupstream.V20260728), upstream.StateReady},
		{"sse", config.Connector{URL: config.Template(sse.URL), Transport: config.TransportSSE}, upstream.StateReady},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": tt.conn})

			if got := statusOf(t, m, "c"); got.State != tt.wantState {
				t.Errorf("status = %+v, want state %q", got, tt.wantState)
			}
		})
	}
}

func TestApplyTransportFailureIsUnreachable(t *testing.T) {
	closed := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	closedURL := closed.URL
	closed.Close()
	tests := []struct {
		name     string
		conn     config.Connector
		wantCase upstream.Case
	}{
		{"connection refused", config.Connector{URL: config.Template(closedURL)}, upstream.CaseConnectRefused},
		{"spawn failed", config.Connector{Command: "wayfindr-test-no-such-command"}, upstream.CaseSpawnFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, rec := newManager(t, nil)

			apply(context.Background(), m, nil, config.Connectors{"c": tt.conn})

			if got := statusOf(t, m, "c"); got.State != upstream.StateUnreachable || got.Reason == "" {
				t.Errorf("status = %+v, want unreachable with a reason", got)
			}

			wantStates := []upstream.State{upstream.StateStarting, upstream.StateUnreachable}

			if diff := cmp.Diff(wantStates, rec.of("c")); diff != "" {
				t.Errorf("OnStateChanged (-want +got):\n%s", diff)
			}

			if _, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil); !isCase(err, upstream.KindUnreachable, tt.wantCase) {
				t.Errorf("Call: err = %v, want unreachable (%s)", err, tt.wantCase)
			}
		})
	}
}

func TestUnreachableReasonOmitsURLQuery(t *testing.T) {
	closed := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	closedURL := closed.URL
	closed.Close()
	m, _ := newManager(t, env{"secret:k": "hunter2"})

	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(closedURL + "/mcp?key=${secret:k}")}})

	got := statusOf(t, m, "c")

	if got.State != upstream.StateUnreachable {
		t.Fatalf("state = %q, want unreachable", got.State)
	}

	if strings.Contains(got.Reason, "hunter2") {
		t.Errorf("reason %q contains the resolved secret", got.Reason)
	}
}

func TestApplyMissingReferenceIsAuthErrorNamingIt(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	tests := []struct {
		name string
		conn config.Connector
		ref  string
	}{
		{"env in url", config.Connector{URL: config.Template(srv.URL + "?team=${env:LINEAR_TEAM}")}, "${env:LINEAR_TEAM}"},
		{"env in header", config.Connector{
			URL:     config.Template(srv.URL),
			Headers: map[string]config.Template{"Authorization": "Bearer ${env:LINEAR_API_KEY}"},
		}, "${env:LINEAR_API_KEY}"},
		{"secret in header", config.Connector{
			URL:     config.Template(srv.URL),
			Headers: map[string]config.Template{"X-Api-Key": "${secret:linear_key}"},
		}, "${secret:linear_key}"},
		{"env in stdio args", config.Connector{Command: "true", Args: []config.Template{"--token=${env:GH_TOKEN}"}}, "${env:GH_TOKEN}"},
		{"env in stdio env", config.Connector{Command: "true", Env: map[string]config.Template{"GH": "${env:GH_TOKEN}"}}, "${env:GH_TOKEN}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, rec := newManager(t, env{"env:PRESENT": "x"})

			apply(context.Background(), m, nil, config.Connectors{"c": tt.conn})

			got := statusOf(t, m, "c")

			if got.State != upstream.StateAuthError {
				t.Errorf("state = %q, want auth_error", got.State)
			}

			if !strings.Contains(got.Reason, tt.ref) {
				t.Errorf("reason = %q, want it to name %s", got.Reason, tt.ref)
			}

			_, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil)

			if e, ok := upstream.AsError(err); !ok || e.Kind != upstream.KindMissingValue || "${"+e.Ref+"}" != tt.ref {
				t.Errorf("Call: err = %v, want missing_value naming %s", err, tt.ref)
			}

			wantStates := []upstream.State{upstream.StateStarting, upstream.StateAuthError}

			if diff := cmp.Diff(wantStates, rec.of("c")); diff != "" {
				t.Errorf("OnStateChanged (-want +got):\n%s", diff)
			}
		})
	}
}

func TestApplyResolvesTemplatesOnEveryConnect(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	var mu sync.Mutex
	vals := env{}
	resolver := config.ResolverFunc(func(ctx context.Context, ref config.Ref) (string, error) {
		mu.Lock()
		defer mu.Unlock()

		return vals.Resolve(ctx, ref)
	})
	m, rec := newManager(t, resolver)
	conn := config.Connector{URL: config.Template(srv.URL), Headers: map[string]config.Template{"X-Key": "${secret:k}"}}
	before := conn

	apply(context.Background(), m, nil, config.Connectors{"c": conn})

	if got := statusOf(t, m, "c").State; got != upstream.StateAuthError {
		t.Fatalf("state with the secret missing = %q, want auth_error", got)
	}

	mu.Lock()
	vals["secret:k"] = "v"
	mu.Unlock()
	conn.Timeout = config.Duration{Duration: time.Minute} // any config change reconnects
	apply(context.Background(), m, config.Connectors{"c": before}, config.Connectors{"c": conn})

	want := upstream.ConnectorStatus{Connector: "c", State: upstream.StateReady}

	if diff := cmp.Diff(want, statusOf(t, m, "c"), ignoreSince); diff != "" {
		t.Errorf("status after the secret is set and config changes (-want +got):\n%s", diff)
	}

	wantStates := []upstream.State{
		upstream.StateStarting, upstream.StateAuthError,
		upstream.StateStarting, upstream.StateReady,
	}

	if diff := cmp.Diff(wantStates, rec.of("c")); diff != "" {
		t.Errorf("OnStateChanged (-want +got):\n%s", diff)
	}
}

// TestClientAdvertisesOnlyElicitation checks that the client advertises
// elicitation (form and URL), which it passes on to the user, and nothing
// else: not sampling, and not the roots the SDK would by default.
func TestClientAdvertisesOnlyElicitation(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			srv := fakeupstream.StartStreamable(t, version)
			spy := &capabilitiesSpy{}
			proxy := httptest.NewServer(spy.handler(t, srv.URL))
			t.Cleanup(proxy.Close)
			m, _ := newManager(t, nil)

			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(proxy.URL)}})

			if got := statusOf(t, m, "c").State; got != upstream.StateReady {
				t.Fatalf("state = %q, want ready", got)
			}

			seen := spy.seen()

			if len(seen) == 0 {
				t.Fatal("no request carried client capabilities")
			}

			for _, raw := range seen {
				var got map[string]any

				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatalf("decode capabilities %s: %v", raw, err)
				}

				if diff := cmp.Diff(map[string]any{"elicitation": map[string]any{"form": map[string]any{}, "url": map[string]any{}}}, got); diff != "" {
					t.Errorf("advertised capabilities (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// capabilitiesSpy is a reverse proxy that records the client capabilities
// each JSON-RPC request carries: in initialize params, or in _meta under
// 2026-07-28.
type capabilitiesSpy struct {
	mu   sync.Mutex
	caps []json.RawMessage
}

func (s *capabilitiesSpy) seen() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.caps
}

func (s *capabilitiesSpy) handler(t *testing.T, target string) http.Handler {
	t.Helper()
	u, err := url.Parse(target)

	if err != nil {
		t.Fatal(err)
	}

	proxy := httputil.NewSingleHostReverseProxy(u)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)

			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)

				return
			}

			r.Body = io.NopCloser(bytes.NewReader(body))
			var msg struct {
				Params struct {
					Capabilities json.RawMessage            `json:"capabilities"`
					Meta         map[string]json.RawMessage `json:"_meta"`
				} `json:"params"`
			}

			if json.Unmarshal(body, &msg) == nil {
				s.mu.Lock()

				if c := msg.Params.Capabilities; c != nil {
					s.caps = append(s.caps, c)
				}

				if c, ok := msg.Params.Meta["io.modelcontextprotocol/clientCapabilities"]; ok {
					s.caps = append(s.caps, c)
				}

				s.mu.Unlock()
			}
		}

		proxy.ServeHTTP(w, r)
	})
}

func TestApplyRemovesConnector(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	m, rec := newManager(t, nil)
	before := config.Connectors{
		"a": stdioConnector(t, fakeupstream.V20251125),
		"b": {URL: config.Template(srv.URL)},
	}
	apply(context.Background(), m, nil, before)
	rec.reset()

	apply(context.Background(), m, before, config.Connectors{"b": {URL: config.Template(srv.URL)}})

	if diff := cmp.Diff([]string{"b"}, connectorNames(m)); diff != "" {
		t.Errorf("connectors after removing a (-want +got):\n%s", diff)
	}

	if got := rec.of("a"); len(got) != 0 {
		t.Errorf("OnStateChanged for removed connector: %v, want none", got)
	}
}

// TestApplyAfterCloseDoesNothing applies a connector to a closed manager:
// with no reaper left to stop it, the connector must not be added or
// connected (Manager.Apply).
func TestApplyAfterCloseDoesNothing(t *testing.T) {
	m, rec := newManager(t, nil)

	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	apply(context.Background(), m, nil, config.Connectors{"c": stdioConnector(t, fakeupstream.V20251125)})

	if got := m.Status(); len(got) != 0 {
		t.Errorf("Status after Apply after Close = %v, want no connectors", got)
	}

	if got := rec.of("c"); len(got) != 0 {
		t.Errorf("OnStateChanged = %v, want none", got)
	}

	if got := rec.toolsChangedOf("c"); got != 0 {
		t.Errorf("OnToolsChanged calls = %d, want 0: no child was spawned", got)
	}
}

func TestApplyReconfigures(t *testing.T) {
	oldSrv := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	newSrv := fakeupstream.StartStreamable(t, fakeupstream.V20260728)
	oauth := &config.Auth{Type: "oauth"}
	tests := []struct {
		name       string
		before     config.Connector
		after      config.Connector
		wantStates []upstream.State
	}{
		{
			name:       "unchanged connector is left alone",
			before:     config.Connector{URL: config.Template(oldSrv.URL)},
			after:      config.Connector{URL: config.Template(oldSrv.URL)},
			wantStates: nil,
		},
		{
			name:       "changed connector reconnects",
			before:     config.Connector{URL: config.Template(oldSrv.URL)},
			after:      config.Connector{URL: config.Template(oldSrv.URL), Headers: map[string]config.Template{"X-A": "b"}},
			wantStates: []upstream.State{upstream.StateStarting, upstream.StateReady},
		},
		{
			name:       "url change without oauth reconnects",
			before:     config.Connector{URL: config.Template(oldSrv.URL)},
			after:      config.Connector{URL: config.Template(newSrv.URL)},
			wantStates: []upstream.State{upstream.StateStarting, upstream.StateReady},
		},
		{
			name:       "url change with oauth needs auth",
			before:     config.Connector{URL: config.Template(oldSrv.URL), Auth: oauth},
			after:      config.Connector{URL: config.Template(newSrv.URL), Auth: oauth},
			wantStates: []upstream.State{upstream.StateNeedsAuth},
		},
		{
			name:       "switch from url to command reconnects",
			before:     config.Connector{URL: config.Template(oldSrv.URL)},
			after:      stdioConnector(t, fakeupstream.V20251125),
			wantStates: []upstream.State{upstream.StateStarting, upstream.StateReady},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, rec := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": tt.before})

			if got := statusOf(t, m, "c").State; got != upstream.StateReady {
				t.Fatalf("state before = %q, want ready", got)
			}

			rec.reset()

			apply(context.Background(), m, config.Connectors{"c": tt.before}, config.Connectors{"c": tt.after})

			if diff := cmp.Diff(tt.wantStates, rec.of("c")); diff != "" {
				t.Errorf("OnStateChanged (-want +got):\n%s", diff)
			}
		})
	}
}

func TestApplyStartsConnectorsConcurrently(t *testing.T) {
	set := config.Connectors{}
	versions := fakeupstream.Versions()

	for i := range 8 {
		srv := fakeupstream.StartStreamable(t, versions[i%len(versions)])
		set[fmt.Sprintf("c%d", i)] = config.Connector{URL: config.Template(srv.URL)}
	}

	m, rec := newManager(t, nil)

	// Read Status while Apply connects, for the race detector.
	stop := make(chan struct{})
	polled := make(chan struct{})
	go func() {
		defer close(polled)

		for {
			select {
			case <-stop:
				return
			default:
				_ = m.Status()
			}
		}
	}()
	apply(context.Background(), m, nil, set)
	close(stop)
	<-polled

	for name := range set {
		wantStates := []upstream.State{upstream.StateStarting, upstream.StateReady}

		if diff := cmp.Diff(wantStates, rec.of(name)); diff != "" {
			t.Errorf("%s: OnStateChanged (-want +got):\n%s", name, diff)
		}
	}
}

func TestStatusIsSortedByConnector(t *testing.T) {
	m, _ := newManager(t, nil)
	apply(context.Background(), m, nil, config.Connectors{
		"zeta":  {Command: "wayfindr-test-no-such-command"},
		"alpha": {Command: "wayfindr-test-no-such-command"},
		"mid":   {URL: "http://127.0.0.1:1/${env:MISSING}"},
	})

	if diff := cmp.Diff([]string{"alpha", "mid", "zeta"}, connectorNames(m)); diff != "" {
		t.Errorf("Status order (-want +got):\n%s", diff)
	}
}

func TestCloseRemovesEveryConnector(t *testing.T) {
	m, _ := newManager(t, nil)
	apply(context.Background(), m, nil, config.Connectors{"c": stdioConnector(t, fakeupstream.V20251125)})

	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := m.Status(); len(got) != 0 {
		t.Errorf("Status after Close = %+v, want empty", got)
	}
}
