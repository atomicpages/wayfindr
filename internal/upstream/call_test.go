package upstream_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// resultText is the text of a result's single text block.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()

	if res == nil || len(res.Content) != 1 {
		t.Fatalf("result = %+v, want one content block", res)
	}

	text, ok := res.Content[0].(*mcp.TextContent)

	if !ok {
		t.Fatalf("content is %T, want text", res.Content[0])
	}

	return text.Text
}

func TestCallReturnsUpstreamResult(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			srv := fakeupstream.StartStreamable(t, version)
			m, _ := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})

			res, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, json.RawMessage(`{"text":"hi"}`))

			if err != nil {
				t.Fatalf("Call: %v", err)
			}

			if got, want := resultText(t, res), `{"text":"hi"}`; got != want {
				t.Errorf("result text = %q, want %q", got, want)
			}
		})
	}
}

func TestCallUnknownConnector(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	m, _ := newManager(t, nil)
	set := config.Connectors{"c": {URL: config.Template(srv.URL)}}
	apply(context.Background(), m, nil, set)

	if _, err := m.Call(context.Background(), "nope", fakeupstream.ToolEcho, nil); !isKind(err, upstream.KindUnknownConnector) {
		t.Errorf("Call on a connector never configured: err = %v, want unknown_connector", err)
	}

	if _, err := m.ListTools(context.Background(), "nope"); !isKind(err, upstream.KindUnknownConnector) {
		t.Errorf("ListTools on a connector never configured: err = %v, want unknown_connector", err)
	}

	apply(context.Background(), m, set, config.Connectors{})

	if _, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil); !isKind(err, upstream.KindUnknownConnector) {
		t.Errorf("Call on a removed connector: err = %v, want unknown_connector", err)
	}
}

// startServer serves srv over Streamable HTTP, statelessly under 2026-07-28
// as fakeupstream does. It is for behavior fakeupstream has no switch for.
func startServer(t *testing.T, version string, srv *mcp.Server) string {
	t.Helper()
	h := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: version >= fakeupstream.V20260728},
	)
	ts := httptest.NewServer(h)
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	return ts.URL
}

func newServer(version string) *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{version}})
}

func TestListToolsFollowsCursors(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			srv := fakeupstream.StartStreamable(t, version, fiveTools(), fakeupstream.WithPageSize(2))
			m, _ := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})

			tools, err := m.ListTools(context.Background(), "c")

			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}

			var names []string

			for _, tool := range tools {
				names = append(names, tool.Name)
			}

			if diff := cmp.Diff([]string{"t0", "t1", "t2", "t3", "t4"}, names); diff != "" {
				t.Errorf("tools over three pages of two (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCallsRunConcurrentlyOverOneSession(t *testing.T) {
	const calls = 4

	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			srv := fakeupstream.StartStreamable(t, version, fakeupstream.WithBarrier(calls))
			m, rec := newManager(t, nil)
			// Calls made one at a time would wait for the timeout.
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL), Timeout: config.Duration{Duration: 5 * time.Second}}})
			rec.reset()

			var wg sync.WaitGroup
			errs := make([]error, calls)

			for i := range calls {
				wg.Go(func() {
					res, err := m.Call(context.Background(), "c", fakeupstream.ToolBarrier, nil)

					if err == nil && res.IsError {
						err = fmt.Errorf("tool error: %+v", res.Content)
					}

					errs[i] = err
				})
			}

			wg.Wait()

			for i, err := range errs {
				if err != nil {
					t.Errorf("call %d: %v", i, err)
				}
			}

			if got := rec.of("c"); len(got) != 0 {
				t.Errorf("OnStateChanged during the calls = %v, want none: the calls share the session", got)
			}
		})
	}
}

func TestCallTimesOutAfterConnectorTimeout(t *testing.T) {
	// Stateless 2026-07-28: a stateful fake would hold the manager's
	// closing DELETE until the slow call ends.
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20260728, fakeupstream.WithDelay(10*time.Second))
	m, rec := newManager(t, nil)
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL), Timeout: config.Duration{Duration: 50 * time.Millisecond}}})
	rec.reset()

	start := time.Now()
	_, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil)

	if !isKind(err, upstream.KindTimeout) {
		t.Errorf("err = %v, want timeout", err)
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Call took %v, want about the 50ms timeout", elapsed)
	}

	// A slow tool says nothing about whether the upstream is reachable.
	if got := rec.of("c"); len(got) != 0 {
		t.Errorf("OnStateChanged = %v, want none", got)
	}
}
