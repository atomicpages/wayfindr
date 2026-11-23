package upstream_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// challengeAuth is an AuthTransport that decides on the upstream's
// refusals as the auth broker does for a connector holding a token: a 403
// insufficient_scope is a step-up and a 401 needs auth, each failing the
// request with a needs_auth error.
func challengeAuth(link string) func(config.Connector, http.RoundTripper) http.RoundTripper {
	return func(_ config.Connector, next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp, err := next.RoundTrip(r)

			if err != nil {
				return nil, err
			}

			challenge := resp.Header.Get("WWW-Authenticate")

			switch {
			case resp.StatusCode == http.StatusForbidden && strings.Contains(challenge, `error="insufficient_scope"`):
				_ = resp.Body.Close()

				return nil, &needsAuthErr{link: link, stepUp: true}
			case resp.StatusCode == http.StatusUnauthorized:
				_ = resp.Body.Close()

				return nil, &needsAuthErr{link: link}
			}

			return resp, nil
		})
	}
}

// An auth decision on any request of a connect ends the connect there: the
// transport fallback (server/discover, then initialize, then HTTP+SSE) is
// for a server that speaks another transport, and a refused token is
// refused on each of them (wayfindr-n6nc). The requests after it would
// only hide what the connector needs behind unreachable.
func TestAuthDecisionStopsTheTransportFallback(t *testing.T) {
	const link = "http://127.0.0.1:7777/auth/c"
	stepUp := answer{
		status: http.StatusForbidden, contentType: "application/json", body: `{"error":"insufficient_scope"}`,
		challenge: `Bearer error="insufficient_scope", scope="mcp:basic mcp:write"`,
	}
	unauthorized := answer{status: http.StatusUnauthorized, challenge: `Bearer realm="mcp"`}
	// How a 2026-07-28 server answers an initialize without the
	// MCP-Protocol-Version header (the conformance suite's test server).
	noVersion := answer{status: http.StatusBadRequest, contentType: "text/plain", body: "Missing MCP-Protocol-Version"}
	unknownMethod := answer{status: http.StatusBadRequest, contentType: "text/plain", body: "unknown method"}

	tests := []struct {
		name     string
		script   map[string]answer
		wantCase upstream.Case
		wantSent []string
	}{
		{
			name:     "step-up on server/discover",
			script:   map[string]answer{"server/discover": stepUp, "initialize": noVersion},
			wantCase: upstream.CaseStepUp,
			wantSent: []string{"server/discover"},
		},
		{
			name:     "401 on server/discover",
			script:   map[string]answer{"server/discover": unauthorized, "initialize": noVersion},
			wantCase: upstream.CaseNoCredential,
			wantSent: []string{"server/discover"},
		},
		{
			name:     "step-up on initialize",
			script:   map[string]answer{"server/discover": unknownMethod, "initialize": stepUp},
			wantCase: upstream.CaseStepUp,
			wantSent: []string{"server/discover", "initialize"},
		},
		{
			name:     "401 on initialize",
			script:   map[string]answer{"server/discover": unknownMethod, "initialize": unauthorized},
			wantCase: upstream.CaseNoCredential,
			wantSent: []string{"server/discover", "initialize"},
		},
		{
			// A legacy SSE server: the 404 to initialize starts the
			// fallback, and its GET is the request refused.
			name: "step-up on the HTTP+SSE fallback",
			script: map[string]answer{
				"server/discover": unknownMethod, "initialize": {status: http.StatusNotFound}, http.MethodGet: stepUp,
			},
			wantCase: upstream.CaseStepUp,
			wantSent: []string{"server/discover", "initialize", http.MethodGet},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, u := startScripted(t, tt.script)
			m, _ := newManager(t, nil, func(o *upstream.Options) { o.AuthTransport = challengeAuth(link) })

			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
			st := statusOf(t, m, "c")

			if st.State != upstream.StateNeedsAuth || st.Case != tt.wantCase || st.Link != link {
				t.Errorf("status = {State %q, Case %q, Link %q, Reason %q}, want {%q, %q, %q}",
					st.State, st.Case, st.Link, st.Reason, upstream.StateNeedsAuth, tt.wantCase, link)
			}

			if diff := cmp.Diff(tt.wantSent, u.methods()); diff != "" {
				t.Errorf("requests the upstream got (-want +got):\n%s", diff)
			}
		})
	}
}

// A 401 or 403 to server/discover with no auth hook to decide on it is an
// auth refusal too, not the sign of a legacy SSE server: the fallback stops
// short of HTTP+SSE even when initialize then gets another 4xx.
func TestRefusedDiscoverDoesNotFallBackToSSE(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv, u := startScripted(t, map[string]answer{
				"server/discover": {status: status},
				"initialize":      {status: http.StatusBadRequest, contentType: "text/plain", body: "Missing MCP-Protocol-Version"},
			})
			m, _ := newManager(t, nil)

			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})

			if slices.Contains(u.methods(), http.MethodGet) {
				t.Errorf("SSE tried after a %d to server/discover (requests %v)", status, u.methods())
			}
		})
	}
}
