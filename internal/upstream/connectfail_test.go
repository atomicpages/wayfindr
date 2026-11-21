package upstream_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// TestReconnectOutlastingCallTimeoutLeavesConnectorUnreachable respawns an
// idle stdio child that never answers, under a connector timeout shorter
// than the connect timeout: the call gives up on the connect, and the
// connector must say the upstream didn't answer. A connect given up on is
// unreachable, not a timeout of the call (design §7.2, rule order).
func TestReconnectOutlastingCallTimeoutLeavesConnectorUnreachable(t *testing.T) {
	clock := newFakeClock()
	conn := stdioConnector(t, fakeupstream.V20251125)
	hang := filepath.Join(t.TempDir(), "hang")
	conn.Env[hangEnv] = config.Template(hang)
	conn.Timeout = config.Duration{Duration: 200 * time.Millisecond}
	m, rec := newManager(t, nil, withClock(clock))
	apply(context.Background(), m, nil, config.Connectors{"gh": conn})

	if got := statusOf(t, m, "gh").State; got != upstream.StateReady {
		t.Fatalf("state after Apply = %q, want ready", got)
	}

	clock.Advance(10*time.Minute + time.Second)
	m.Reap()

	if err := os.WriteFile(hang, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	rec.reset()

	_, err := m.Call(context.Background(), "gh", fakeupstream.ToolEcho, nil)

	if !isCase(err, upstream.KindUnreachable, upstream.CaseConnectTimeout) {
		t.Errorf("Call: err = %v, want unreachable (connect_timeout)", err)
	}

	if diff := cmp.Diff([]upstream.State{upstream.StateUnreachable}, rec.of("gh")); diff != "" {
		t.Errorf("OnStateChanged (-want +got):\n%s", diff)
	}
}

// TestCancelledApplyLeavesConnectorUnreachable gives up on Apply while an
// upstream that never answers is connecting: the connector must not stay
// starting.
func TestCancelledApplyLeavesConnectorUnreachable(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.CloseClientConnections()
		srv.Close()
	})
	m, _ := newManager(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	apply(ctx, m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})

	deadline := time.Now().Add(5 * time.Second)

	for {
		got := statusOf(t, m, "c").State

		if got == upstream.StateUnreachable {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("state 5s after a cancelled Apply = %q, want unreachable", got)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// TestCallEndingAfterRemovalIsUnknownConnector removes a connector while a
// call on it is in progress, and then the call ends: with the upstream's
// answer, or failing because the upstream went away. Either way the call
// must not report on, or move the state of, a connector that is gone.
func TestCallEndingAfterRemovalIsUnknownConnector(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		for _, ending := range []string{"answered", "upstream gone"} {
			t.Run(version+"/"+ending, func(t *testing.T) {
				entered := make(chan struct{})
				release := make(chan struct{})
				srv := newServer(version)
				srv.AddTool(&mcp.Tool{Name: "wait", InputSchema: map[string]any{"type": "object"}},
					func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						close(entered)

						select {
						case <-release:
						case <-ctx.Done():
						}

						return &mcp.CallToolResult{}, nil
					})
				var once sync.Once
				releaseCall := func() { once.Do(func() { close(release) }) }
				p := startProxy(t, startServer(t, version, srv), "")
				t.Cleanup(releaseCall)
				m, rec := newManager(t, nil)
				set := config.Connectors{"c": {URL: config.Template(p.URL()), Timeout: config.Duration{Duration: 10 * time.Second}}}
				apply(context.Background(), m, nil, set)
				rec.reset()

				done := make(chan error, 1)
				go func() {
					_, err := m.Call(context.Background(), "c", "wait", nil)
					done <- err
				}()
				<-entered
				go func() {
					// Ended from aside: a stateful session's closing DELETE
					// may wait for the call.
					time.Sleep(50 * time.Millisecond)

					if ending == "answered" {
						releaseCall()
					} else {
						p.stop()
					}
				}()
				apply(context.Background(), m, set, config.Connectors{})
				err := <-done

				if !isKind(err, upstream.KindUnknownConnector) {
					t.Errorf("Call: err = %v, want unknown_connector", err)
				}

				if got := rec.of("c"); len(got) != 0 {
					t.Errorf("OnStateChanged = %v, want none: the connector is gone", got)
				}
			})
		}
	}
}
