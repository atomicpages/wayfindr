package gateway_test

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// waitLimit bounds how long blockingCatalog waits for its ctx to end, and
// how long a test waits for anything else. A cancellation that arrives
// reaches the handler at once, so only a missing one takes this long.
const waitLimit = 5 * time.Second

// blockingCatalog's GetTool blocks until its ctx ends. It tells entered when
// a read starts, and ended whether the ctx ended before waitLimit.
type blockingCatalog struct {
	stubCatalog

	entered chan struct{}
	ended   chan bool
}

func newBlockingCatalog() *blockingCatalog {
	return &blockingCatalog{entered: make(chan struct{}, 1), ended: make(chan bool, 1)}
}

func (c *blockingCatalog) GetTool(ctx context.Context, _, _, _ string) (store.Tool, error) {
	c.entered <- struct{}{}

	select {
	case <-ctx.Done():
		c.ended <- true

		return store.Tool{}, ctx.Err()
	case <-time.After(waitLimit):
		c.ended <- false

		return store.Tool{}, errors.New("the request's context never ended")
	}
}

// callChan is a gateway.Calls that hands each call it is given to the test.
type callChan chan store.Call

func (c callChan) Record(call store.Call) { c <- call }

// A tool handler's context ends with the HTTP request that carries it, at
// every protocol version: when the client gives up on the request, and when
// the daemon's hard stop cancels the base context every request's context
// derives from (httpd.Server.Shutdown, design §7.7). A catalog read cut
// short that way is the caller giving up, so call_tool logs it as failed:
// canceled, not as Wayfindr failing to read its catalog (wayfindr-rs5r).
func TestToolHandlersEndWithTheirHTTPRequest(t *testing.T) {
	tools := []struct {
		name string
		args map[string]any
	}{
		{"call_tool", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "x"}}},
		{"search_tools", map[string]any{"names": []string{"linear.create_issue"}}},
	}
	// stop ends the request: the client's call context, or the server's
	// base context.
	ends := []struct {
		name string
		stop func(client, server context.CancelFunc)
	}{
		{"client gives up", func(client, _ context.CancelFunc) { client() }},
		{"hard stop", func(_, server context.CancelFunc) { server() }},
	}

	for _, version := range fakeupstream.Versions() {
		for _, tool := range tools {
			for _, end := range ends {
				t.Run(version+"/"+tool.name+"/"+end.name, func(t *testing.T) {
					cat := newBlockingCatalog()
					calls := make(callChan, 1)
					g := gateway.New(gateway.Options{
						Upstreams: &stubUpstreams{statuses: []upstream.ConnectorStatus{
							{Connector: "linear", State: upstream.StateReady},
						}},
						Catalog: cat,
						Calls:   calls,
						Logger:  zerolog.Nop(),
					})
					base, stopServer := context.WithCancel(context.Background())
					defer stopServer()
					srv := httptest.NewUnstartedServer(authed(g))
					srv.Config.BaseContext = func(net.Listener) context.Context { return base }
					srv.Start()
					t.Cleanup(srv.Close)

					cs := connect(t, srv.URL, version)
					ctx, stopClient := context.WithCancel(t.Context())
					defer stopClient()
					returned := make(chan struct{})

					go func() {
						defer close(returned)
						// Its result doesn't matter: the request is gone.
						_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: tool.name, Arguments: tool.args})
					}()

					select {
					case <-cat.entered:
					case <-time.After(waitLimit):
						t.Fatal("the handler never read the catalog")
					}

					end.stop(stopClient, stopServer)

					if !<-cat.ended {
						t.Errorf("the handler's context did not end with its HTTP request")
					}

					if tool.name == "call_tool" {
						select {
						case got := <-calls:
							want := store.Call{
								TokenName: clienttoken.DefaultName,
								Connector: "linear",
								Tool:      "create_issue",
								Result:    store.CallFailed,
								ErrorKind: "canceled",
							}
							opts := cmpopts.IgnoreFields(store.Call{}, "Client", "StartedAt", "ErrorText")

							if diff := cmp.Diff(want, got, opts); diff != "" {
								t.Errorf("recorded call (-want +got):\n%s", diff)
							}
						case <-time.After(waitLimit):
							t.Error("call_tool recorded no call")
						}
					}

					stopClient()
					<-returned
				})
			}
		}
	}
}
