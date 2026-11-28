package fakeupstream_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// TestMain is wired exactly as a consuming package must wire its own, so that
// the stdio fake can re-exec this test binary.
func TestMain(m *testing.M) {
	fakeupstream.ServeStdioIfChild()
	os.Exit(m.Run())
}

// transport starts a fake upstream and returns an SDK client transport for it.
type transport struct {
	name  string
	start func(t *testing.T, version string, opts ...fakeupstream.Option) mcp.Transport
}

// transports are the three ways to reach a fake upstream. Each HTTP one
// uses its own server's client: the subtests run in parallel, and a server's
// Close calls CloseIdleConnections on http.DefaultTransport, which would break
// another subtest's request in flight on the shared transport.
func transports() []transport {
	return []transport{
		{"stdio", func(t *testing.T, version string, opts ...fakeupstream.Option) mcp.Transport {
			return &mcp.CommandTransport{Command: fakeupstream.StdioCommand(t, version, opts...)}
		}},
		{"streamable", func(t *testing.T, version string, opts ...fakeupstream.Option) mcp.Transport {
			srv := fakeupstream.StartStreamable(t, version, opts...)

			return &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: srv.Client()}
		}},
		{"sse", func(t *testing.T, version string, opts ...fakeupstream.Option) mcp.Transport {
			srv := fakeupstream.StartSSE(t, version, opts...)

			return &mcp.SSEClientTransport{Endpoint: srv.URL, HTTPClient: srv.Client()}
		}},
	}
}

// combination is one transport at one protocol version.
type combination struct {
	transport
	version string
}

// combinations lists every valid transport × version pair. 2026-07-28
// defines no HTTP+SSE binding, so that pair is excluded.
func combinations() []combination {
	var out []combination

	for _, tr := range transports() {
		for _, v := range fakeupstream.Versions() {
			if tr.name == "sse" && v == fakeupstream.V20260728 {
				continue
			}

			out = append(out, combination{tr, v})
		}
	}

	return out
}

func (c combination) String() string { return c.name + "/" + c.version }

func connect(t *testing.T, tr mcp.Transport, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, opts)
	cs, err := client.Connect(t.Context(), tr, nil)

	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

func callText(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})

	if err != nil {
		t.Fatalf("call %s: %v", tool, err)
	}

	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("call %s: unexpected result %+v", tool, res)
	}

	text, ok := res.Content[0].(*mcp.TextContent)

	if !ok {
		t.Fatalf("call %s: content is %T, want text", tool, res.Content[0])
	}

	return text.Text
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(t.Context(), nil)

	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	var names []string

	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}

	return names
}

func TestEveryTransportAndVersionServesDefaultTool(t *testing.T) {
	for _, c := range combinations() {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			cs := connect(t, c.start(t, c.version), nil)

			if got := cs.InitializeResult().ProtocolVersion; got != c.version {
				t.Errorf("negotiated protocol version = %q, want %q", got, c.version)
			}

			if diff := cmp.Diff([]string{"echo"}, toolNames(t, cs)); diff != "" {
				t.Errorf("tools (-want +got):\n%s", diff)
			}

			if got, want := callText(t, cs, "echo", map[string]any{"text": "hi"}), `{"text":"hi"}`; got != want {
				t.Errorf("echo returned %q, want %q", got, want)
			}
		})
	}
}

func TestWithToolsReplacesDefaultToolSet(t *testing.T) {
	for _, c := range combinations() {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			tools := fakeupstream.WithTools(
				&mcp.Tool{Name: "create_issue", Description: "Create an issue."},
				&mcp.Tool{Name: "list_issues", Description: "List issues."},
			)
			cs := connect(t, c.start(t, c.version, tools), nil)

			if diff := cmp.Diff([]string{"create_issue", "list_issues"}, toolNames(t, cs)); diff != "" {
				t.Errorf("tools (-want +got):\n%s", diff)
			}

			if got, want := callText(t, cs, "create_issue", map[string]any{"title": "x"}), `{"title":"x"}`; got != want {
				t.Errorf("create_issue returned %q, want %q", got, want)
			}
		})
	}
}

func TestWithPageSizePaginatesToolsList(t *testing.T) {
	for _, c := range combinations() {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			tools := fakeupstream.WithTools(
				&mcp.Tool{Name: "t0"}, &mcp.Tool{Name: "t1"}, &mcp.Tool{Name: "t2"},
				&mcp.Tool{Name: "t3"}, &mcp.Tool{Name: "t4"},
			)
			cs := connect(t, c.start(t, c.version, tools, fakeupstream.WithPageSize(2)), nil)

			first, err := cs.ListTools(t.Context(), nil)

			if err != nil {
				t.Fatalf("list tools: %v", err)
			}

			if len(first.Tools) != 2 || first.NextCursor == "" {
				t.Errorf("first page has %d tools and cursor %q, want 2 tools and a cursor", len(first.Tools), first.NextCursor)
			}

			var all []string

			for tool, err := range cs.Tools(t.Context(), nil) {
				if err != nil {
					t.Fatalf("list tools: %v", err)
				}

				all = append(all, tool.Name)
			}

			if diff := cmp.Diff([]string{"t0", "t1", "t2", "t3", "t4"}, all); diff != "" {
				t.Errorf("tools over every page (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWithBarrierAnswersOnceAllCallersArrive(t *testing.T) {
	const callers = 3

	for _, c := range combinations() {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			cs := connect(t, c.start(t, c.version, fakeupstream.WithBarrier(callers)), nil)

			var wg sync.WaitGroup
			got := make([]string, callers)

			for i := range callers {
				wg.Go(func() {
					res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: fakeupstream.ToolBarrier, Arguments: map[string]any{"i": i}})

					if err != nil {
						got[i] = "error: " + err.Error()

						return
					}

					if text, ok := res.Content[0].(*mcp.TextContent); ok && len(res.Content) == 1 && !res.IsError {
						got[i] = text.Text
					}
				})
			}

			wg.Wait()
			want := []string{`{"i":0}`, `{"i":1}`, `{"i":2}`}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("barrier results (-want +got):\n%s", diff)
			}
		})
	}
}

// Fewer callers than the barrier waits for never get an answer.
func TestWithBarrierHoldsTooFewCallers(t *testing.T) {
	for _, c := range combinations() {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			cs := connect(t, c.start(t, c.version, fakeupstream.WithBarrier(2)), nil)

			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()

			if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: fakeupstream.ToolBarrier}); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("lone call: err = %v, want context.DeadlineExceeded", err)
			}
		})
	}
}

func TestWithListChangedNotifiesClient(t *testing.T) {
	for _, c := range combinations() {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			changed := make(chan struct{}, 1)
			cs := connect(t, c.start(t, c.version, fakeupstream.WithListChanged()), &mcp.ClientOptions{
				ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
					select {
					case changed <- struct{}{}:
					default:
					}
				},
			})

			callText(t, cs, fakeupstream.ToolChangeTools, nil)

			select {
			case <-changed:
			case <-time.After(5 * time.Second):
				t.Fatal("no tools/list_changed notification")
			}

			want := []string{"added_1", fakeupstream.ToolChangeTools, "echo"}

			if diff := cmp.Diff(want, toolNames(t, cs)); diff != "" {
				t.Errorf("tools after change (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWithElicitationAsksClient(t *testing.T) {
	for _, c := range combinations() {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			cs := connect(t, c.start(t, c.version, fakeupstream.WithElicitation()), &mcp.ClientOptions{
				ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
					return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"answer": "yes"}}, nil
				},
			})

			got := callText(t, cs, fakeupstream.ToolElicit, nil)

			if want := `{"action":"accept","content":{"answer":"yes"}}`; got != want {
				t.Errorf("elicit returned %q, want %q", got, want)
			}
		})
	}
}

// Sampling is deprecated as of 2026-07-28 (SEP-2577), but upstreams still use it.
//
//nolint:staticcheck // see above.
func TestWithSamplingAsksClient(t *testing.T) {
	for _, c := range combinations() {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			cs := connect(t, c.start(t, c.version, fakeupstream.WithSampling()), &mcp.ClientOptions{
				CreateMessageHandler: func(context.Context, *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
					return &mcp.CreateMessageResult{Role: "assistant", Model: "fake", Content: &mcp.TextContent{Text: "sampled"}}, nil
				},
			})

			if got, want := callText(t, cs, fakeupstream.ToolSample, nil), "sampled"; got != want {
				t.Errorf("sample returned %q, want %q", got, want)
			}
		})
	}
}

// A client that, like Wayfindr, disables multi round-trip sees the
// input_required result itself under 2026-07-28.
func TestElicitationReturnsInputRequiredUnder20260728(t *testing.T) {
	for _, c := range combinations() {
		if c.version != fakeupstream.V20260728 {
			continue
		}

		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			cs := connect(t, c.start(t, c.version, fakeupstream.WithElicitation()), &mcp.ClientOptions{
				Capabilities:   &mcp.ClientCapabilities{},
				MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
			})

			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: fakeupstream.ToolElicit})

			if err != nil {
				t.Fatalf("call: %v", err)
			}

			if !res.NeedsInput() {
				t.Fatalf("NeedsInput() = false, want true; result %+v", res)
			}

			if len(res.InputRequests) != 1 {
				t.Fatalf("got %d input requests, want 1", len(res.InputRequests))
			}

			for _, ir := range res.InputRequests {
				if _, ok := ir.(*mcp.ElicitParams); !ok {
					t.Errorf("input request is %T, want *mcp.ElicitParams", ir)
				}
			}
		})
	}
}

// Before 2026-07-28 the server must ask the client directly, so a client
// that advertises no elicitation capability gets a failed call.
func TestElicitationFailsForIncapableClientBefore20260728(t *testing.T) {
	for _, c := range combinations() {
		if c.version == fakeupstream.V20260728 {
			continue
		}

		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			cs := connect(t, c.start(t, c.version, fakeupstream.WithElicitation()), &mcp.ClientOptions{
				Capabilities:   &mcp.ClientCapabilities{},
				MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
			})

			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: fakeupstream.ToolElicit})

			if err == nil && !res.IsError {
				t.Fatalf("call succeeded with %+v, want an error", res)
			}
		})
	}
}

func TestWithDelaySlowsToolCalls(t *testing.T) {
	const delay = 50 * time.Millisecond

	for _, c := range combinations() {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			cs := connect(t, c.start(t, c.version, fakeupstream.WithDelay(delay)), nil)

			start := time.Now()
			callText(t, cs, "echo", nil)

			if elapsed := time.Since(start); elapsed < delay {
				t.Errorf("call took %v, want at least %v", elapsed, delay)
			}

			ctx, cancel := context.WithTimeout(t.Context(), delay/5)
			defer cancel()

			if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo"}); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("call with short deadline: err = %v, want context.DeadlineExceeded", err)
			}
		})
	}
}

func TestStreamableRejectsWithConfiguredStatus(t *testing.T) {
	for _, v := range fakeupstream.Versions() {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			srv := fakeupstream.StartStreamable(t, v, fakeupstream.WithStreamableStatus(http.StatusNotFound))

			if got := postInitialize(t, srv); got != http.StatusNotFound {
				t.Errorf("POST status = %d, want %d", got, http.StatusNotFound)
			}

			client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)

			if cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: srv.Client()}, nil); err == nil {
				_ = cs.Close()
				t.Fatal("connect succeeded, want an error")
			}
		})
	}
}

// A legacy SSE server answers a Streamable HTTP initialize POST with a 4xx,
// which is what makes a client fall back to SSE.
func TestSSERejectsStreamablePost(t *testing.T) {
	for _, v := range []string{fakeupstream.V20250618, fakeupstream.V20251125} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			srv := fakeupstream.StartSSE(t, v)

			if got := postInitialize(t, srv); got < 400 || got > 499 {
				t.Errorf("POST status = %d, want 4xx", got)
			}
		})
	}
}

// postInitialize sends a Streamable HTTP initialize POST and returns the
// response status. It uses srv's own client, not the shared default one.
func postInitialize(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"v0"}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(body))

	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := srv.Client().Do(req)

	if err != nil {
		t.Fatalf("post: %v", err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}

func TestServeRunsFakeOnGivenTransport(t *testing.T) {
	serverSide, clientSide := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- fakeupstream.Serve(t.Context(), fakeupstream.V20250618, serverSide) }()

	cs := connect(t, clientSide, nil)

	if got, want := cs.InitializeResult().ProtocolVersion, fakeupstream.V20250618; got != want {
		t.Errorf("negotiated protocol version = %q, want %q", got, want)
	}

	if got, want := callText(t, cs, "echo", map[string]any{"text": "hi"}), `{"text":"hi"}`; got != want {
		t.Errorf("echo returned %q, want %q", got, want)
	}

	_ = cs.Close()

	if err := <-done; err != nil {
		t.Errorf("Serve returned %v after the client closed, want nil", err)
	}
}

func TestServeRejectsUnknownVersion(t *testing.T) {
	serverSide, _ := mcp.NewInMemoryTransports()

	if err := fakeupstream.Serve(t.Context(), "1999-01-01", serverSide); err == nil {
		t.Error("Serve with an unknown version returned nil, want an error")
	}
}
