package upstream_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// secret is a value that rides in a URL query and must never be logged or
// returned (design §9).
const secret = "s3cret-query-value"

// TestCloseErrorsHideURLQueries closes sessions whose upstream has gone
// away. A stateful Streamable session ends with a DELETE, and its failure
// quotes the url, query and all.
func TestCloseErrorsHideURLQueries(t *testing.T) {
	for _, version := range []string{fakeupstream.V20250618, fakeupstream.V20251125} {
		for _, how := range []string{"Close", "removed by Apply"} {
			t.Run(version+"/"+how, func(t *testing.T) {
				p := startProxy(t, fakeupstream.StartStreamable(t, version).URL, "")
				var buf syncBuffer
				m, _ := newManager(t, nil, func(o *upstream.Options) {
					o.Logger = zerolog.New(&buf).Level(zerolog.DebugLevel)
				})
				set := config.Connectors{"c": {URL: config.Template(p.URL() + "/?key=" + secret)}}
				apply(context.Background(), m, nil, set)

				if got := statusOf(t, m, "c").State; got != upstream.StateReady {
					t.Fatalf("state = %q, want ready", got)
				}

				p.stop()

				var err error

				if how == "Close" {
					err = m.Close(context.Background())

					if err == nil {
						t.Fatal("Close = nil, want the failed DELETE's error")
					}
				} else {
					apply(context.Background(), m, set, config.Connectors{})
				}

				if err != nil && strings.Contains(err.Error(), secret) {
					t.Errorf("Close error quotes the url query: %v", err)
				}

				if strings.Contains(buf.String(), secret) {
					t.Errorf("log quotes the url query:\n%s", buf.String())
				}

				if how != "Close" && !strings.Contains(buf.String(), "?"+"REDACTED") {
					t.Errorf("log does not carry the redacted close error:\n%s", buf.String())
				}
			})
		}
	}
}

func TestProtocolErrorHidesURLQueries(t *testing.T) {
	srv := newServer(fakeupstream.V20251125)
	srv.AddTool(&mcp.Tool{Name: "fetch", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "cannot fetch https://api.example.com/x?token=" + secret}
		})
	url := startServer(t, fakeupstream.V20251125, srv)
	m, _ := newManager(t, nil)
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(url)}})

	_, err := m.Call(context.Background(), "c", "fetch", nil)

	var werr *jsonrpc.Error

	if !errors.As(err, &werr) || werr.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("err = %v, want the upstream's invalid-params error", err)
	}

	if strings.Contains(err.Error(), secret) {
		t.Errorf("err quotes the url query: %v", err)
	}

	if !strings.Contains(err.Error(), "https://api.example.com/x?REDACTED") {
		t.Errorf("err = %q, want the url with its query redacted", err)
	}
}

func TestStdioStderrHidesURLQueries(t *testing.T) {
	var buf syncBuffer
	conn := stdioConnector(t, fakeupstream.V20251125)
	conn.Env[stderrEnv] = "GET https://api.example.com/x?token=" + secret + " failed"
	m, _ := newManager(t, nil, func(o *upstream.Options) {
		o.Logger = zerolog.New(&buf).Level(zerolog.DebugLevel)
	})
	apply(context.Background(), m, nil, config.Connectors{"gh": conn})

	// Close waits for the child, and for its stderr to be copied.
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if strings.Contains(buf.String(), secret) {
		t.Errorf("log quotes the url query:\n%s", buf.String())
	}

	if !strings.Contains(buf.String(), "GET https://api.example.com/x?REDACTED failed") {
		t.Errorf("log lacks the redacted stderr line:\n%s", buf.String())
	}
}
