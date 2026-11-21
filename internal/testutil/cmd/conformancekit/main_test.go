package main

import (
	"net"
	"net/http"
	"net/http/httptest"
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

	h, err := newProxy("127.0.0.1:4100", target, "wf_secret")

	if err != nil {
		t.Fatalf("newProxy: %v", err)
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
