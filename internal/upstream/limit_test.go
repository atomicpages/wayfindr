package upstream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// testLimit is the message limit the tests here set: big enough for every
// ordinary message, small enough to pass cheaply.
const testLimit = 16 << 10

func withLimit(o *upstream.Options) { o.MaxMessageBytes = testLimit }

// toolBig is the tool of startJSONUpstream that answers over the limit.
const toolBig = "big"

// startJSONUpstream serves a Streamable HTTP upstream that answers every
// POST with a JSON body, not an event stream, so the body limit applies to
// it. Its tool "big" answers with a text twice the limit, in a 200 JSON
// answer, or with errorBody, in a 400 JSON-RPC error body (which a client
// that read it whole would report as a refusal). Its tool "echo" answers
// as usual.
func startJSONUpstream(t *testing.T, version string, errorBody bool) string {
	t.Helper()
	srv := newServer(version)
	text := strings.Repeat("x", 2*testLimit)
	srv.AddTool(&mcp.Tool{Name: toolBig, InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: fakeupstream.ToolEcho, InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	var h http.Handler = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: version >= fakeupstream.V20260728, JSONResponse: true},
	)

	if errorBody {
		h = errorBodyFor(toolBig, text, h)
	}

	ts := httptest.NewServer(h)
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	return ts.URL
}

// errorBodyFor answers a tools/call of tool with 400 and a JSON-RPC error
// whose message is text, and passes every other request to next.
func errorBodyFor(tool, text string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)

			return
		}

		body, err := io.ReadAll(r.Body)

		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}

		if json.Unmarshal(body, &msg) == nil && msg.Method == "tools/call" && msg.Params.Name == tool {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":%q}}`, msg.ID, text)

			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

// bodyMeter wraps a connector's transport, inside Wayfindr's own layers,
// and keeps the most bytes read from any one response body.
type bodyMeter struct{ most atomic.Int64 }

func (b *bodyMeter) transport(_ config.Connector, next http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := next.RoundTrip(req)

		if err != nil {
			return nil, err
		}

		resp.Body = &meteredBody{ReadCloser: resp.Body, meter: b}

		return resp, nil
	})
}

type meteredBody struct {
	io.ReadCloser
	meter *bodyMeter
	read  int64
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)

	for {
		most := b.meter.most.Load()

		if b.read <= most || b.meter.most.CompareAndSwap(most, b.read) {
			break
		}
	}

	return n, err
}

// checkClosedAndReconnects checks that the failed use closed connector's
// session and left its state alone: the next call succeeds over a new
// session, which fires OnToolsChanged.
func checkClosedAndReconnects(t *testing.T, m *upstream.Manager, rec *recorder, connector string, sessions int) {
	t.Helper()

	if got := rec.of(connector); len(got) != 0 {
		t.Errorf("OnStateChanged after the oversized answer = %v, want none", got)
	}

	if _, err := m.Call(context.Background(), connector, fakeupstream.ToolEcho, nil); err != nil {
		t.Fatalf("Call after the oversized answer: %v", err)
	}

	if got := rec.toolsChangedOf(connector); got != sessions+1 {
		t.Errorf("sessions opened = %d, want %d: the oversized answer closes the session, so the next call reconnects", got, sessions+1)
	}

	if got := rec.of(connector); len(got) != 0 {
		t.Errorf("OnStateChanged after the next call = %v, want none", got)
	}
}

func TestHTTPBodyOverTheLimitIsResponseTooLarge(t *testing.T) {
	for _, errorBody := range []bool{false, true} {
		for _, version := range fakeupstream.Versions() {
			t.Run(fmt.Sprintf("errorBody=%v/%s", errorBody, version), func(t *testing.T) {
				t.Parallel()
				url := startJSONUpstream(t, version, errorBody)
				meter := &bodyMeter{}
				m, rec := newManager(t, nil, withLimit, func(o *upstream.Options) { o.AuthTransport = meter.transport })
				apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(url)}})
				rec.reset()

				_, err := m.Call(context.Background(), "c", toolBig, nil)

				if !isCase(err, upstream.KindResponseTooLarge, upstream.CaseBody) {
					t.Fatalf("Call answered over the limit: err = %v, want response_too_large (body)", err)
				}

				// The limit reads one byte past itself to tell the body is
				// over, and no more.
				if most := meter.most.Load(); most > testLimit+1 {
					t.Errorf("most bytes read from one body = %d, want at most %d", most, testLimit+1)
				}

				checkClosedAndReconnects(t, m, rec, "c", 1)
			})
		}
	}
}

func TestStdioLineOverTheLimitIsResponseTooLargeAndStopsTheChild(t *testing.T) {
	conn, report := reportingConnector(t)
	m, rec := newManager(t, nil, withLimit)
	apply(context.Background(), m, nil, config.Connectors{"gh": conn})
	first := readReport(t, report).PID
	rec.reset()

	// The fake echoes its arguments back, so the answer is over the limit.
	args, err := json.Marshal(map[string]string{"text": strings.Repeat("x", 2*testLimit)})

	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Call(context.Background(), "gh", fakeupstream.ToolEcho, args)

	if !isCase(err, upstream.KindResponseTooLarge, upstream.CaseMessage) {
		t.Fatalf("Call answered over the limit: err = %v, want response_too_large (message)", err)
	}

	if alive(first) {
		t.Error("child still running after its oversized answer; want it stopped with the session")
	}

	checkClosedAndReconnects(t, m, rec, "gh", 1)

	if second := readReport(t, report).PID; second == first || !alive(second) {
		t.Errorf("call after the oversized answer: child pid %d (first was %d), alive %v; want a new running child", second, first, alive(second))
	}
}

func TestSSEEventOverTheLimitFailsAndClosesTheSession(t *testing.T) {
	// The SDK bounds each SSE event itself (MaxEventSize, which the Manager
	// sets to the limit) and reports an overflow as a broken connection.
	for _, tc := range []struct {
		name string
		url  func(t *testing.T) string
	}{
		{"streamable", func(t *testing.T) string { return fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL }},
		{"sse", func(t *testing.T) string { return fakeupstream.StartSSE(t, fakeupstream.V20251125).URL }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Started first, so it closes after the manager's sessions.
			c := config.Connector{URL: config.Template(tc.url(t))}
			m, rec := newManager(t, nil, withLimit)

			if tc.name == "sse" {
				c.Transport = config.TransportSSE
			}

			apply(context.Background(), m, nil, config.Connectors{"c": c})
			sessions := rec.toolsChangedOf("c")
			args, err := json.Marshal(map[string]string{"text": strings.Repeat("x", 2*testLimit)})

			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if _, err := m.Call(ctx, "c", fakeupstream.ToolEcho, args); !isKind(err, upstream.KindUnreachable) {
				t.Fatalf("Call answered over the limit: err = %v, want unreachable", err)
			}

			if _, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil); err != nil {
				t.Fatalf("Call after the oversized answer: %v", err)
			}

			if got := rec.toolsChangedOf("c"); got != sessions+1 {
				t.Errorf("sessions opened = %d, want %d: the oversized event closes the session", got, sessions+1)
			}
		})
	}
}

// listMode is how startListUpstream answers tools/list.
type listMode int

const (
	// listOK: one page, one tool, a cache hint of a minute.
	listOK listMode = iota
	// listEndless: a new cursor on every page.
	listEndless
	// listRepeat: the same cursor on every page.
	listRepeat
	// listTooMany: one page of one tool more than Wayfindr keeps.
	listTooMany
	// listTooBig: a new cursor on every page, and one tool of 7 KiB.
	listTooBig
)

// listUpstream is a Streamable HTTP upstream whose tools/list answers as
// its mode says, counting the pages it serves.
type listUpstream struct {
	mode  atomic.Int64
	pages atomic.Int64
	url   string
}

func startListUpstream(t *testing.T) *listUpstream {
	t.Helper()
	u := &listUpstream{}
	srv := newServer(fakeupstream.V20251125)
	srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/list" {
				return next(ctx, method, req)
			}

			n := u.pages.Add(1)
			tool := &mcp.Tool{Name: fmt.Sprintf("t%d", n), InputSchema: map[string]any{"type": "object"}}

			switch listMode(u.mode.Load()) {
			case listOK:
				res := &mcp.ListToolsResult{Tools: []*mcp.Tool{tool}}
				res.TTLMs = 60_000

				return res, nil
			case listEndless:
				return &mcp.ListToolsResult{Tools: []*mcp.Tool{tool}, NextCursor: fmt.Sprint(n)}, nil
			case listRepeat:
				return &mcp.ListToolsResult{Tools: []*mcp.Tool{tool}, NextCursor: "again"}, nil
			case listTooMany:
				tools := make([]*mcp.Tool, 2001)

				for i := range tools {
					tools[i] = &mcp.Tool{Name: fmt.Sprintf("t%d", i), InputSchema: map[string]any{"type": "object"}}
				}

				return &mcp.ListToolsResult{Tools: tools}, nil
			case listTooBig:
				tool.Description = strings.Repeat("x", 7<<10)

				return &mcp.ListToolsResult{Tools: []*mcp.Tool{tool}, NextCursor: fmt.Sprint(n)}, nil
			}

			return nil, fmt.Errorf("unknown list mode %d", u.mode.Load())
		}
	})
	u.url = startServer(t, fakeupstream.V20251125, srv)

	return u
}

func TestToolListOverTheSyncLimitsIsResponseTooLarge(t *testing.T) {
	tests := []struct {
		name  string
		mode  listMode
		kase  upstream.Case
		pages int64
	}{
		{"endless cursors", listEndless, upstream.CaseSyncPages, 50},
		{"repeated cursor", listRepeat, upstream.CaseSyncPages, 2},
		{"too many tools", listTooMany, upstream.CaseSyncTools, 1},
		// Four pages of 7 KiB are under the test's 32 KiB budget; the fifth
		// is over.
		{"too many bytes", listTooBig, upstream.CaseSyncBytes, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u := startListUpstream(t)
			m, rec := newManager(t, nil, func(o *upstream.Options) { o.MaxSyncBytes = 32 << 10 })
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(u.url)}})

			if _, err := m.ListTools(context.Background(), "c"); err != nil {
				t.Fatalf("first ListTools: %v", err)
			}

			rec.reset()
			sessions := rec.toolsChangedOf("c")
			u.mode.Store(int64(tt.mode))
			u.pages.Store(0)

			tools, err := m.ListTools(context.Background(), "c")

			if !isCase(err, upstream.KindResponseTooLarge, tt.kase) || tools != nil {
				t.Fatalf("ListTools = %d tools, err %v; want none, response_too_large (%s)", len(tools), err, tt.kase)
			}

			if got := u.pages.Load(); got != tt.pages {
				t.Errorf("pages read = %d, want %d", got, tt.pages)
			}

			// A failed sync records nothing: the last good list's cache
			// hint stays, as the catalog keeps its tools.
			if got := statusOf(t, m, "c").ToolsTTL; got != time.Minute {
				t.Errorf("ToolsTTL after the failed sync = %v, want the last good list's 1m", got)
			}

			if got := rec.of("c"); len(got) != 0 {
				t.Errorf("OnStateChanged after the failed sync = %v, want none", got)
			}

			// The upstream answered every page within the protocol, so its
			// session stays open.
			if _, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil); !isKind(err, upstream.KindRejected) {
				t.Errorf("Call after the failed sync: err = %v, want rejected (the fake has no tools)", err)
			}

			if got := rec.toolsChangedOf("c"); got != sessions {
				t.Errorf("sessions opened after the failed sync = %d, want %d", got-sessions, 0)
			}
		})
	}
}

// toolFail is the tool of startFallbackUpstream whose call the upstream
// refuses below the protocol.
const toolFail = "fail"

// startFallbackUpstream serves a legacy HTTP+SSE upstream that turns
// Streamable HTTP away with a 400 whose body is over the limit, so the
// connect records an overflow and then falls back to SSE. A call of
// toolFail is answered 500, which fails it below the protocol.
func startFallbackUpstream(t *testing.T) string {
	t.Helper()
	srv := newServer(fakeupstream.V20251125)
	srv.AddTool(&mcp.Tool{Name: fakeupstream.ToolEcho, InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	sse := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	big := strings.Repeat("x", 2*testLimit)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sse.ServeHTTP(w, r)

			return
		}

		if r.URL.Query().Get("sessionid") == "" {
			// A Streamable POST: no SSE session names it.
			http.Error(w, big, http.StatusBadRequest)

			return
		}

		body, err := io.ReadAll(r.Body)

		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		var msg struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}

		if json.Unmarshal(body, &msg) == nil && msg.Method == "tools/call" && msg.Params.Name == toolFail {
			http.Error(w, "refused", http.StatusInternalServerError)

			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		sse.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	return ts.URL
}

// An oversized answer to a request of the connect, here the Streamable
// initialize before the SSE fallback, did not come over the session the
// connect opened, so it doesn't relabel that session's later failures.
func TestOverflowDuringAConnectThatFellBackIsForgotten(t *testing.T) {
	url := startFallbackUpstream(t)
	m, _ := newManager(t, nil, withLimit)
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(url)}})

	if got := statusOf(t, m, "c").State; got != upstream.StateReady {
		t.Fatalf("state after the fallback = %q, want ready", got)
	}

	if _, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil); err != nil {
		t.Fatalf("Call over the SSE fallback: %v", err)
	}

	if _, err := m.Call(context.Background(), "c", toolFail, nil); !isKind(err, upstream.KindUnreachable) {
		t.Errorf("Call refused below the protocol: err = %v, want unreachable", err)
	}
}
