package fakeupstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// StartStreamable starts a Streamable HTTP fake pinned to version and closes
// it when the test ends. The MCP endpoint is the server's URL.
//
// Versions before 2026-07-28 are served statefully (with Mcp-Session-Id), so
// the server can send requests and notifications to the client. 2026-07-28
// has no sessions, so it is served statelessly.
func StartStreamable(t testing.TB, version string, opts ...Option) *httptest.Server {
	t.Helper()
	cfg := mustConfig(t, version, opts)

	if cfg.StreamableStatus != 0 {
		return start(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, http.StatusText(cfg.StreamableStatus), cfg.StreamableStatus)
		}))
	}

	server := newServer(t.Context(), cfg)
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: cfg.Version >= V20260728},
	)

	if cfg.HangDelete {
		return start(t, hangDelete(handler))
	}

	return start(t, handler)
}

// StartSSE starts a legacy HTTP+SSE fake pinned to version and closes it when
// the test ends. The SSE endpoint is the server's URL. 2026-07-28 defines no
// HTTP+SSE binding, so asking for it fails the test.
//
// Like real legacy servers, it answers a Streamable HTTP POST to its URL with
// 400, which is what triggers a client's SSE fallback.
func StartSSE(t testing.TB, version string, opts ...Option) *httptest.Server {
	t.Helper()
	cfg := mustConfig(t, version, opts)

	if cfg.Version >= V20260728 {
		t.Fatalf("fakeupstream: protocol version %s has no HTTP+SSE transport", cfg.Version)
	}

	server := newServer(t.Context(), cfg)

	return start(t, mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return server }, nil))
}

// hangDelete holds every DELETE until the client gives up on it or the
// server closes its connection, and only then passes it to next, so the
// server session still ends without the client hearing back. Other
// requests go straight to next.
func hangDelete(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			<-r.Context().Done()
			r = r.WithContext(context.WithoutCancel(r.Context()))
		}

		next.ServeHTTP(w, r)
	})
}

func start(t testing.TB, handler http.Handler) *httptest.Server {
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		// Hanging GET streams would otherwise make Close wait for them.
		srv.CloseClientConnections()
		srv.Close()
	})

	return srv
}

func mustConfig(t testing.TB, version string, opts []Option) config {
	t.Helper()
	cfg, err := newConfig(version, opts)

	if err != nil {
		t.Fatalf("fakeupstream: %v", err)
	}

	return cfg
}
