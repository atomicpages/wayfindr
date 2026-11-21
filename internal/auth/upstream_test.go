package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// newBrokeredManager returns a Manager with a real Broker as its
// AuthTransport, as the daemon wires them.
func newBrokeredManager(t *testing.T, h *brokerHarness) *upstream.Manager {
	t.Helper()
	m := upstream.New(upstream.Options{
		Logger:        zerolog.New(zerolog.NewTestWriter(t)),
		AuthTransport: h.broker.Transport,
	})
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return m
}

// apply applies next to m as the daemon's reload does from an empty config:
// one config.Diff, handed to Apply.
func apply(t *testing.T, m *upstream.Manager, next config.Connectors) {
	t.Helper()
	cfg := &config.Config{Connectors: next}
	m.Apply(t.Context(), cfg.Connectors, config.Diff(nil, cfg))
}

// TestNewOAuthConnectorConnectsAuthorized adds an OAuth connector through
// Apply alone. Its first connect carries the stored token, so the protected
// upstream answers and the connector is ready: the Broker gets the dialed
// config from the connect, and nothing has to tell it about the new
// connector first.
func TestNewOAuthConnectorConnectsAuthorized(t *testing.T) {
	up := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	target, err := url.Parse(up.URL)

	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	h := newBrokerHarness(t, fakeas.WithResourceHandler(proxy))
	h.seed(t, "", time.Hour)
	m := newBrokeredManager(t, h)

	apply(t, m, config.Connectors{"linear": h.conn})

	got := m.Status()

	if len(got) != 1 || got[0].Connector != "linear" || got[0].State != upstream.StateReady {
		t.Fatalf("Status = %+v, want linear ready", got)
	}
}

// TestUpstreamErrorCarriesProviderCode runs a real Broker as upstream's
// AuthTransport. A Slack-style refresh failure (200 {"ok":false,
// "error":"invalid_refresh_token"}) reaches upstream through net/http's
// *url.Error, and upstream's *Error keeps the provider code.
func TestUpstreamErrorCarriesProviderCode(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithSlackStyleErrors())
	h.seed(t, "", 30*time.Second)
	h.as.FailTokenRequests("invalid_refresh_token")
	m := newBrokeredManager(t, h)
	apply(t, m, config.Connectors{"linear": h.conn})

	_, err := m.Call(t.Context(), "linear", "echo", nil)

	e, ok := upstream.AsError(err)

	if !ok {
		t.Fatalf("Call error = %v (%T), want an *upstream.Error", err, err)
	}

	want := []any{upstream.KindNeedsAuth, upstream.CaseTokenRevoked, "invalid_refresh_token"}

	if diff := cmp.Diff(want, []any{e.Kind, e.Case, e.ProviderCode}); diff != "" {
		t.Errorf("Kind, Case, ProviderCode (-want +got):\n%s", diff)
	}
}

// TestDiscoveredConnectorWithoutPKCEIsAuthError adds a connector with no
// client_id whose authorization server does not advertise PKCE S256 (ADR
// 0007). Its first connect puts it in auth_error with the pkce_unsupported
// advice, without the user starting an auth flow that could only fail. A
// manual connector against the same server still needs auth.
func TestDiscoveredConnectorWithoutPKCEIsAuthError(t *testing.T) {
	t.Run("a discovered connector is auth_error", func(t *testing.T) {
		h := newBrokerHarness(t, fakeas.WithoutPKCEAdvertised())
		h.conn.Auth.ClientID = ""
		m := newBrokeredManager(t, h)

		apply(t, m, config.Connectors{"linear": h.conn})

		got := m.Status()

		if len(got) != 1 {
			t.Fatalf("Status = %+v, want one connector", got)
		}

		s := got[0]
		want := []any{upstream.StateAuthError, upstream.KindAuthConfig, upstream.CasePKCEUnsupported, ""}

		if diff := cmp.Diff(want, []any{s.State, s.Kind, s.Case, s.Link}); diff != "" {
			t.Errorf("State, Kind, Case, Link (-want +got):\n%s", diff)
		}

		wantAdvice := upstream.Advice{
			Summary: "The authorization server does not advertise PKCE (S256), which Wayfindr requires " +
				"for a connector without a client_id.",
			Action: "Register an app with the provider and set client_id (and client_secret) in the connector's [auth] table.",
		}

		if diff := cmp.Diff(wantAdvice, s.Advice()); diff != "" {
			t.Errorf("Advice (-want +got):\n%s", diff)
		}
	})
	t.Run("a manual connector needs auth", func(t *testing.T) {
		h := newBrokerHarness(t, fakeas.WithoutPKCEAdvertised())
		m := newBrokeredManager(t, h)

		apply(t, m, config.Connectors{"linear": h.conn})

		got := m.Status()

		if len(got) != 1 || got[0].State != upstream.StateNeedsAuth || got[0].Case != upstream.CaseNoCredential {
			t.Fatalf("Status = %+v, want linear needs_auth (no_credential)", got)
		}
	})
	// A legacy (2025-03-26) server has no metadata to advertise PKCE in,
	// so nothing says it lacks S256; its flow sends S256 anyway
	// (wayfindr-zgp.24).
	t.Run("a discovered connector on a legacy server without metadata needs auth", func(t *testing.T) {
		h := newBrokerHarness(t, fakeas.WithoutMetadata())
		h.conn.Auth.ClientID = ""
		m := newBrokeredManager(t, h)

		apply(t, m, config.Connectors{"linear": h.conn})

		got := m.Status()

		if len(got) != 1 || got[0].State != upstream.StateNeedsAuth || got[0].Case != upstream.CaseNoCredential {
			t.Fatalf("Status = %+v, want linear needs_auth (no_credential)", got)
		}
	})
	t.Run("a discovered connector whose server advertises S256 needs auth", func(t *testing.T) {
		h := newBrokerHarness(t)
		h.conn.Auth.ClientID = ""
		m := newBrokeredManager(t, h)

		apply(t, m, config.Connectors{"linear": h.conn})

		got := m.Status()

		if len(got) != 1 || got[0].State != upstream.StateNeedsAuth || got[0].Case != upstream.CaseNoCredential {
			t.Fatalf("Status = %+v, want linear needs_auth (no_credential)", got)
		}
	})
}

// probes counts the requests the resource saw without a bearer token: the
// broker's probes for the challenge.
func (h *brokerHarness) probes() int {
	n := 0

	for _, r := range h.as.ResourceRequests() {
		if !r.Bearer {
			n++
		}
	}

	return n
}

// TestDiscoveredConnectorWithoutCredentialProbesOncePerDiscovery connects
// a connector with no client_id and no credential again and again. Telling
// needs_auth from auth_error needs the server's challenge, but one probe
// serves every connect until discovery is fetched again, which an auth flow
// does (wayfindr-zgp.20).
func TestDiscoveredConnectorWithoutCredentialProbesOncePerDiscovery(t *testing.T) {
	h := newBrokerHarness(t)
	h.conn.Auth.ClientID = ""
	m := newBrokeredManager(t, h)
	apply(t, m, config.Connectors{"linear": h.conn})

	for range 2 {
		if err := m.Retry(t.Context(), "linear"); err == nil {
			t.Fatal("Retry = nil, want needs_auth")
		}
	}

	if got := h.probes(); got != 1 {
		t.Errorf("probes after three connects = %d, want 1", got)
	}

	h.start(t)

	for range 2 {
		if err := m.Retry(t.Context(), "linear"); err == nil {
			t.Fatal("Retry = nil, want needs_auth")
		}
	}

	if got := h.probes(); got != 2 {
		t.Errorf("probes after a flow refetched discovery and two more connects = %d, want 2", got)
	}

	if s := m.Status(); len(s) != 1 || s[0].State != upstream.StateNeedsAuth || s[0].Case != upstream.CaseNoCredential {
		t.Errorf("Status = %+v, want linear needs_auth (no_credential)", s)
	}
}

// TestURLChangeToAServerWithoutPKCE moves a discovered connector, authorized
// at its old url, to a new one. Its token is bound to the old url, so it
// is never sent to the new one. If the new url's authorization server
// metadata lacks PKCE S256, no auth flow could succeed, so the connector is
// auth_error (pkce_unsupported) rather than needs_auth (url_changed) with a
// link that always fails. Without such metadata (a server with no OAuth,
// or one that advertises S256) it needs auth (wayfindr-zgp.21).
func TestURLChangeToAServerWithoutPKCE(t *testing.T) {
	plain := fakeupstream.StartStreamable(t, fakeupstream.V20251125)

	for _, tt := range []struct {
		name   string
		newURL func(t *testing.T) (string, func() []fakeas.ResourceRequest)
		want   []any
	}{
		{
			"metadata without S256 is auth_error",
			func(t *testing.T) (string, func() []fakeas.ResourceRequest) {
				s := fakeas.Start(t, fakeas.WithoutPKCEAdvertised())

				return s.ResourceURL, s.ResourceRequests
			},
			[]any{upstream.StateAuthError, upstream.KindAuthConfig, upstream.CasePKCEUnsupported},
		},
		{
			"metadata with S256 needs auth",
			func(t *testing.T) (string, func() []fakeas.ResourceRequest) {
				s := fakeas.Start(t)

				return s.ResourceURL, s.ResourceRequests
			},
			[]any{upstream.StateNeedsAuth, upstream.KindNeedsAuth, upstream.CaseURLChanged},
		},
		{
			"no metadata needs auth",
			func(*testing.T) (string, func() []fakeas.ResourceRequest) { return plain.URL, nil },
			[]any{upstream.StateNeedsAuth, upstream.KindNeedsAuth, upstream.CaseURLChanged},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newBrokerHarness(t)
			h.seed(t, "", time.Hour)
			u, seen := tt.newURL(t)
			h.conn.Auth.ClientID = ""
			h.conn.URL = config.Template(u)
			m := newBrokeredManager(t, h)

			apply(t, m, config.Connectors{"linear": h.conn})

			got := m.Status()

			if len(got) != 1 {
				t.Fatalf("Status = %+v, want one connector", got)
			}

			if diff := cmp.Diff(tt.want, []any{got[0].State, got[0].Kind, got[0].Case}); diff != "" {
				t.Errorf("State, Kind, Case (-want +got):\n%s", diff)
			}

			if seen == nil {
				return
			}

			for _, r := range seen() {
				if r.Bearer {
					t.Errorf("the new url got a %s with a bearer token, want none", r.Method)
				}
			}
		})
	}
}

// sessionServer is a Streamable HTTP server that needs no token: it answers
// GET with 405 (no SSE stream) and an initialize POST with 200 and a new
// Mcp-Session-Id, and records every DELETE. Anything else is 404.
type sessionServer struct {
	mu      sync.Mutex
	deletes []http.Header
}

const probeSession = "probe-session-1"

func (s *sessionServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)

		return
	}

	switch r.Method {
	case http.MethodGet:
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	case http.MethodPost:
		w.Header().Set("Mcp-Session-Id", probeSession)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25",` +
			`"capabilities":{},"serverInfo":{"name":"plain","version":"1"}}}` + "\n"))
	case http.MethodDelete:
		s.mu.Lock()
		s.deletes = append(s.deletes, r.Header.Clone())
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	}
}

func (s *sessionServer) deleted() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.deletes
}

// TestProbeEndsTheSessionItOpened connects a discovered connector with no
// credential to a server whose GET is 405 and whose initialize POST is
// answered 200 with a session, as a server that needs no token may. The
// probe for the challenge sent that initialize, so it ends the session it
// opened with an unauthenticated DELETE (wayfindr-zgp.24).
func TestProbeEndsTheSessionItOpened(t *testing.T) {
	srv := &sessionServer{}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	h := newBrokerHarness(t)
	h.conn.Auth.ClientID = ""
	h.conn.URL = config.Template(ts.URL + "/mcp")
	m := newBrokeredManager(t, h)

	apply(t, m, config.Connectors{"linear": h.conn})

	got := srv.deleted()

	if len(got) != 1 {
		t.Fatalf("DELETEs = %d, want 1", len(got))
	}

	if id := got[0].Get("Mcp-Session-Id"); id != probeSession {
		t.Errorf("DELETE Mcp-Session-Id = %q, want %q", id, probeSession)
	}

	if a := got[0].Get("Authorization"); a != "" {
		t.Errorf("DELETE Authorization = %q, want none", a)
	}
}

// TestNoCredentialAndURLChangeAgreeWithoutMetadata connects a discovered
// connector to a server that publishes no authorization server metadata,
// once with no credential and once with a token bound to an earlier url.
// Both answers come from the one PKCE rule, so they agree (wayfindr-zgp.24):
// a legacy server, which has nowhere to advertise PKCE, needs auth, and its
// flow sends S256 anyway; endpoints from config, with no client_id to
// authorize as, are pkce_unsupported either way.
func TestNoCredentialAndURLChangeAgreeWithoutMetadata(t *testing.T) {
	for _, server := range []struct {
		name string
		// auth sets the connector's endpoints, if config gives them.
		auth  func(a *config.Auth, s *fakeas.Server)
		state upstream.State
		// authError is the Case when the state is not needs_auth.
		authError upstream.Case
	}{
		{
			name:  "legacy server",
			auth:  func(*config.Auth, *fakeas.Server) {},
			state: upstream.StateNeedsAuth,
		},
		{
			name: "endpoints from config",
			auth: func(a *config.Auth, s *fakeas.Server) {
				a.AuthorizationEndpoint, a.TokenEndpoint = s.AuthorizationEndpoint, s.TokenEndpoint
			},
			state:     upstream.StateAuthError,
			authError: upstream.CasePKCEUnsupported,
		},
	} {
		for _, tt := range []struct {
			name     string
			seed     bool
			needAuth upstream.Case
		}{
			{"no credential", false, upstream.CaseNoCredential},
			{"url changed", true, upstream.CaseURLChanged},
		} {
			t.Run(server.name+"/"+tt.name, func(t *testing.T) {
				h := newBrokerHarness(t)

				if tt.seed {
					h.seed(t, "", time.Hour)
				}

				s := fakeas.Start(t, fakeas.WithoutMetadata())
				h.conn.Auth.ClientID = ""
				server.auth(h.conn.Auth, s)
				h.conn.URL = config.Template(s.ResourceURL)
				m := newBrokeredManager(t, h)

				apply(t, m, config.Connectors{"linear": h.conn})

				got := m.Status()

				if len(got) != 1 {
					t.Fatalf("Status = %+v, want one connector", got)
				}

				want := []any{server.state, server.authError}

				if server.state == upstream.StateNeedsAuth {
					want[1] = tt.needAuth
				}

				if diff := cmp.Diff(want, []any{got[0].State, got[0].Case}); diff != "" {
					t.Errorf("State, Case (-want +got):\n%s", diff)
				}
			})
		}
	}
}
