package upstream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// answer is how scriptedUpstream answers one JSON-RPC method.
type answer struct {
	status      int
	contentType string
	body        string
	// challenge is the WWW-Authenticate header, if any.
	challenge string
}

// scriptedUpstream is an MCP endpoint that answers each POST by its
// JSON-RPC method from script, and every other request by its HTTP method
// from script, else with 405. It records the methods it saw, with GET for
// the SSE probe.
type scriptedUpstream struct {
	mu   sync.Mutex
	seen []string
}

func startScripted(t *testing.T, script map[string]answer) (*httptest.Server, *scriptedUpstream) {
	t.Helper()
	u := &scriptedUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a answer
		var ok bool

		if r.Method != http.MethodPost {
			u.saw(r.Method)

			if a, ok = script[r.Method]; !ok {
				a = answer{status: http.StatusMethodNotAllowed}
			}
		} else {
			var msg struct {
				Method string `json:"method"`
			}

			_ = json.NewDecoder(r.Body).Decode(&msg)
			u.saw(msg.Method)

			if a, ok = script[msg.Method]; !ok {
				a = answer{status: http.StatusInternalServerError}
			}
		}

		if a.challenge != "" {
			w.Header().Set("WWW-Authenticate", a.challenge)
		}

		if a.contentType != "" {
			w.Header().Set("Content-Type", a.contentType)
		}

		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	}))
	t.Cleanup(srv.Close)

	return srv, u
}

func (u *scriptedUpstream) saw(m string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.seen = append(u.seen, m)
}

func (u *scriptedUpstream) methods() []string {
	u.mu.Lock()
	defer u.mu.Unlock()

	return append([]string(nil), u.seen...)
}

func TestFailedInitializeBodyReachesReason(t *testing.T) {
	tests := []struct {
		name       string
		initialize answer
		want       []string
	}{
		{
			// Slack's shape: not JSON-RPC, so the SDK says only "Bad Request".
			name:       "non-JSON-RPC body",
			initialize: answer{status: http.StatusBadRequest, contentType: "application/json", body: `{"ok":false,"error":"invalid_arguments"}`},
			want:       []string{"upstream answered 400", "invalid_arguments"},
		},
		{
			name:       "plain text body",
			initialize: answer{status: http.StatusBadRequest, contentType: "text/plain", body: "unsupported protocol version\n2025-11-25"},
			want:       []string{"upstream answered 400", "unsupported protocol version 2025-11-25"},
		},
		{
			// The SDK decodes a JSON-RPC error itself: the probe must leave
			// the whole body for it to read.
			name: "JSON-RPC error body",
			initialize: answer{status: http.StatusBadRequest, contentType: "application/json",
				body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"bad params from upstream"}}`},
			want: []string{"bad params from upstream", "upstream answered 400"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := startScripted(t, map[string]answer{
				"server/discover": {status: http.StatusBadRequest, contentType: "text/plain", body: "unknown method"},
				"initialize":      tt.initialize,
			})
			m, _ := newManager(t, nil)

			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
			st := statusOf(t, m, "c")

			if st.State != upstream.StateUnreachable {
				t.Errorf("state = %q, want unreachable", st.State)
			}

			for _, w := range tt.want {
				if !strings.Contains(st.Reason, w) {
					t.Errorf("reason = %q, want it to contain %q", st.Reason, w)
				}
			}
		})
	}
}

func TestFailedInitializeBodyIsBounded(t *testing.T) {
	srv, _ := startScripted(t, map[string]answer{
		"initialize": {status: http.StatusBadRequest, contentType: "text/plain", body: strings.Repeat("x", 4096)},
	})
	m, _ := newManager(t, nil)

	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
	reason := statusOf(t, m, "c").Reason

	if got := strings.Count(reason, "x"); got > 600 {
		t.Errorf("reason holds %d bytes of the body, want at most about 512", got)
	}

	if !strings.Contains(reason, "…") {
		t.Errorf("reason = %q, want the cut body marked with …", reason)
	}
}

// A 4xx to server/discover is how every pre-2026-07-28 server answers; only
// the initialize answer says whether the server is a legacy SSE one.
func TestFallbackFollowsInitializeNotDiscover(t *testing.T) {
	tests := []struct {
		name         string
		initialize   int
		wantFallback bool
	}{
		{"initialize 500", http.StatusInternalServerError, false},
		{"initialize 401", http.StatusUnauthorized, false},
		{"initialize 404", http.StatusNotFound, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, u := startScripted(t, map[string]answer{
				"server/discover": {status: http.StatusBadRequest, contentType: "text/plain", body: "unknown method"},
				"initialize":      {status: tt.initialize},
			})
			m, _ := newManager(t, nil)

			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})

			if got := contains(u.methods(), http.MethodGet); got != tt.wantFallback {
				t.Errorf("SSE tried = %v, want %v (requests %v)", got, tt.wantFallback, u.methods())
			}
		})
	}
}
