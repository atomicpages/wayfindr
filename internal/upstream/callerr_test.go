package upstream_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

func TestRefusedCallIsUnreachableAndNextCallReconnects(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			p := startProxy(t, fakeupstream.StartStreamable(t, version).URL, "")
			m, rec := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(p.URL())}})
			rec.reset()

			p.stop()
			_, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil)

			if !isCase(err, upstream.KindUnreachable, upstream.CaseConnectRefused) {
				t.Errorf("Call while refused: err = %v, want unreachable (connect_refused)", err)
			}

			if got := statusOf(t, m, "c").State; got != upstream.StateUnreachable {
				t.Errorf("state while refused = %q, want unreachable", got)
			}

			p.restart()

			if _, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil); err != nil {
				t.Errorf("Call after the upstream is back: %v", err)
			}

			wantStates := []upstream.State{upstream.StateUnreachable, upstream.StateReady}

			if diff := cmp.Diff(wantStates, rec.of("c")); diff != "" {
				t.Errorf("OnStateChanged (-want +got):\n%s", diff)
			}
		})
	}
}

// isKind reports whether err is an *upstream.Error of kind k.
func isKind(err error, k upstream.Kind) bool {
	e, ok := upstream.AsError(err)

	return ok && e.Kind == k
}

// isCase reports whether err is an *upstream.Error of kind k and case c.
func isCase(err error, k upstream.Kind, c upstream.Case) bool {
	e, ok := upstream.AsError(err)

	return ok && e.Kind == k && e.Case == c
}

// needsAuthErr is what the auth RoundTripper returns when the user must
// authorize: it carries the link that starts the flow.
type needsAuthErr struct {
	link   string
	stepUp bool
}

func (e *needsAuthErr) Error() string                         { return "no token" }
func (e *needsAuthErr) NeedsAuth() (link string, stepUp bool) { return e.link, e.stepUp }
func (e *needsAuthErr) AuthConfig() bool                      { return false }
func (e *needsAuthErr) AuthCase() string {
	if e.stepUp {
		return "step_up"
	}

	return "no_credential"
}

// authConfigErr is what the auth RoundTripper returns for a config that
// can't work.
type authConfigErr struct{}

func (authConfigErr) Error() string    { return "authorization server does not support PKCE" }
func (authConfigErr) AuthConfig() bool { return true }
func (authConfigErr) AuthCase() string { return "no_registration" }

// authStub is an AuthTransport that fails every request with err once err
// is set, as the auth broker does without a usable credential.
type authStub struct {
	mu  sync.Mutex
	err error
}

func (a *authStub) fail(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

func (a *authStub) transport(_ config.Connector, next http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		a.mu.Lock()
		err := a.err
		a.mu.Unlock()

		if err != nil {
			return nil, err
		}

		return next.RoundTrip(r)
	})
}

func TestAuthFailuresAreTypedAndMoveState(t *testing.T) {
	const link = "http://127.0.0.1:7777/auth/c"
	tests := []struct {
		name      string
		err       error
		wantKind  upstream.Kind
		wantCase  upstream.Case
		wantLink  string
		wantState upstream.State
	}{
		{"needs auth", &needsAuthErr{link: link}, upstream.KindNeedsAuth, upstream.CaseNoCredential, link, upstream.StateNeedsAuth},
		{"step-up", &needsAuthErr{link: link, stepUp: true}, upstream.KindNeedsAuth, upstream.CaseStepUp, link, upstream.StateNeedsAuth},
		{"auth config", authConfigErr{}, upstream.KindAuthConfig, upstream.CaseNoRegistration, "", upstream.StateAuthError},
	}

	for _, version := range []string{fakeupstream.V20251125, fakeupstream.V20260728} {
		for _, tt := range tests {
			for _, when := range []string{"at connect", "mid-session"} {
				t.Run(version+"/"+tt.name+"/"+when, func(t *testing.T) {
					srv := fakeupstream.StartStreamable(t, version)
					stub := &authStub{}
					m, rec := newManager(t, nil, func(o *upstream.Options) { o.AuthTransport = stub.transport })

					if when == "at connect" {
						stub.fail(tt.err)
					}

					apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
					stub.fail(tt.err)

					_, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil)

					e, ok := upstream.AsError(err)

					if !ok {
						t.Fatalf("err = %v (%T), want an *upstream.Error", err, err)
					}

					if e.Kind != tt.wantKind || e.Case != tt.wantCase || e.Link != tt.wantLink {
						t.Errorf("*Error = {Kind %q, Case %q, Link %q}, want {%q, %q, %q}",
							e.Kind, e.Case, e.Link, tt.wantKind, tt.wantCase, tt.wantLink)
					}

					// The stub sits inside a real http.Client, which wraps its
					// error in a *url.Error, as it will the auth broker's.
					var uerr *url.Error

					if !errors.As(err, &uerr) {
						t.Errorf("err = %v, want the http.Client's *url.Error in its chain", err)
					}

					if got := statusOf(t, m, "c").State; got != tt.wantState {
						t.Errorf("state = %q, want %q", got, tt.wantState)
					}

					if !slices.Contains(rec.of("c"), tt.wantState) {
						t.Errorf("OnStateChanged = %v, want %q among them", rec.of("c"), tt.wantState)
					}
				})
			}
		}
	}
}

func TestListToolsMapsErrorsLikeCall(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20260728)
	stub := &authStub{}
	m, _ := newManager(t, nil, func(o *upstream.Options) { o.AuthTransport = stub.transport })
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
	stub.fail(&needsAuthErr{link: "http://127.0.0.1:7777/auth/c"})

	if _, err := m.ListTools(context.Background(), "c"); !isCase(err, upstream.KindNeedsAuth, upstream.CaseNoCredential) {
		t.Errorf("err = %v, want needs_auth (no_credential)", err)
	}
}

// The link of a needs_auth error comes from its cause, else from
// Options.AuthURL, and the connector's status and advice carry the same
// link as the error the call returned.
func TestNeedsAuthLinkComesFromTheCauseElseAuthURL(t *testing.T) {
	tests := []struct {
		name, causeLink, want string
	}{
		{"link from the cause", "http://127.0.0.1:7777/auth/c?from=cause", "http://127.0.0.1:7777/auth/c?from=cause"},
		{"link from AuthURL", "", "http://127.0.0.1:7777/auth/c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fakeupstream.StartStreamable(t, fakeupstream.V20260728)
			stub := &authStub{}
			m, _ := newManager(t, nil, func(o *upstream.Options) {
				o.AuthTransport = stub.transport
				o.AuthURL = func(c string) string { return "http://127.0.0.1:7777/auth/" + c }
			})
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
			stub.fail(&needsAuthErr{link: tt.causeLink})

			_, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil)

			if e, ok := upstream.AsError(err); !ok || e.Link != tt.want {
				t.Errorf("Call err = %v, want a needs_auth *Error with Link %q", err, tt.want)
			}

			s := statusOf(t, m, "c")

			if s.State != upstream.StateNeedsAuth || s.Link != tt.want {
				t.Errorf("status = %+v, want needs_auth with Link %q", s, tt.want)
			}

			if got := s.Advice().Link; got != tt.want {
				t.Errorf("Advice().Link = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConnectTimesOutAndLeavesConnectorUnreachable(t *testing.T) {
	// The server accepts connections but never answers. A handler that
	// hasn't read its request body doesn't see the client go away, so the
	// handlers are released explicitly at cleanup.
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
	m, _ := newManager(t, nil, func(o *upstream.Options) { o.ConnectTimeout = 100 * time.Millisecond })

	start := time.Now()
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Apply took %v, want about the 100ms connect timeout", elapsed)
	}

	got := statusOf(t, m, "c")

	if got.State != upstream.StateUnreachable || !strings.Contains(got.Reason, "connect_timeout") {
		t.Errorf("status = %+v, want unreachable with a reason saying the connect timed out", got)
	}

	if _, err := m.Call(context.Background(), "c", fakeupstream.ToolEcho, nil); !isCase(err, upstream.KindUnreachable, upstream.CaseConnectTimeout) {
		t.Errorf("Call: err = %v, want unreachable (connect_timeout)", err)
	}
}

func TestCallDeclinesSampling(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		// Under 2026-07-28 the upstream answers input_required; before it,
		// the upstream asks the client (or refuses the call because the
		// client can't answer).
		t.Run(version, func(t *testing.T) {
			srv := fakeupstream.StartStreamable(t, version, fakeupstream.WithSampling())
			m, rec := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
			rec.reset()

			res, err := m.Call(context.Background(), "c", fakeupstream.ToolSample, nil)

			if !isCase(err, upstream.KindInteractiveInput, upstream.CaseSampling) || res != nil {
				t.Fatalf("Call = (%+v, %v), want interactive_input (sampling)", res, err)
			}

			if got := rec.of("c"); len(got) != 0 {
				t.Errorf("OnStateChanged = %v, want none: the upstream answered", got)
			}
		})
	}
}

func TestListChangedFiresToolsChanged(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			srv := fakeupstream.StartStreamable(t, version, fakeupstream.WithListChanged())
			m, rec := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
			before := rec.toolsChangedOf("c")

			if _, err := m.Call(context.Background(), "c", fakeupstream.ToolChangeTools, nil); err != nil {
				t.Fatalf("Call: %v", err)
			}

			deadline := time.Now().Add(5 * time.Second)

			for rec.toolsChangedOf("c") == before {
				if time.Now().After(deadline) {
					t.Fatal("OnToolsChanged not called within 5s of the upstream's list_changed")
				}

				time.Sleep(10 * time.Millisecond)
			}

			tools, err := m.ListTools(context.Background(), "c")

			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}

			if !slices.ContainsFunc(tools, func(tool *mcp.Tool) bool { return tool.Name == "added_1" }) {
				t.Errorf("ListTools after list_changed lacks added_1")
			}
		})
	}
}

// sampleTool asks the client to sample a message, the way fakeupstream's
// sample tool does: by returning an input request, which the SDK sends as
// sampling/createMessage before 2026-07-28 and as input_required from it.
//
//nolint:staticcheck // sampling is deprecated as of 2026-07-28 but upstreams still use it.
func sampleTool(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if _, ok := req.Params.InputResponses["input"]; ok {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sampled"}}}, nil
	}

	return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{"input": &mcp.CreateMessageParams{
		MaxTokens: 16,
		Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "Say something."}}},
	}}}, nil
}

func TestDeclinedInputDoesNotRelabelAnotherCallsFailure(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			srv := newServer(version)
			srv.AddTool(&mcp.Tool{Name: "sample", InputSchema: map[string]any{"type": "object"}}, sampleTool)
			// fail is a tool that fails on its own, once the test lets it.
			srv.AddTool(&mcp.Tool{Name: "fail", InputSchema: map[string]any{"type": "object"}},
				func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					close(entered)

					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}

					res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "disk full"}}}
					res.IsError = true

					return res, nil
				})
			url := startServer(t, version, srv)
			m, _ := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(url), Timeout: config.Duration{Duration: 10 * time.Second}}})

			// B starts a call that will fail on its own; while it runs, A
			// calls a tool that asks for sampling, which Wayfindr declines.
			type outcome struct {
				res *mcp.CallToolResult
				err error
			}
			b := make(chan outcome, 1)
			go func() {
				res, err := m.Call(context.Background(), "c", "fail", nil)
				b <- outcome{res, err}
			}()
			<-entered

			if _, err := m.Call(context.Background(), "c", "sample", nil); !isKind(err, upstream.KindInteractiveInput) {
				t.Errorf("A: err = %v, want interactive_input", err)
			}

			close(release)
			got := <-b

			if got.err != nil {
				t.Fatalf("B: err = %v, want its own isError result", got.err)
			}

			if !got.res.IsError || resultText(t, got.res) != "disk full" {
				t.Errorf("B: result = %+v, want the upstream's isError result saying disk full", got.res)
			}
		})
	}
}

// TestCallDeclinesSamplingAskedByTheTool covers a tool that sends the
// sampling request itself, rather than returning an input request, and
// turns the decline into an isError result, as the SDK's typed handlers
// do with a wrapped error. 2026-07-28 forbids sending it during a call.
func TestCallDeclinesSamplingAskedByTheTool(t *testing.T) {
	for _, version := range []string{fakeupstream.V20250618, fakeupstream.V20251125} {
		t.Run(version, func(t *testing.T) {
			srv := newServer(version)
			mcp.AddTool(srv, &mcp.Tool{Name: "ask"},
				func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
					//nolint:staticcheck // sampling is deprecated as of 2026-07-28 but upstreams still use it.
					_, err := req.Session.CreateMessage(ctx, &mcp.CreateMessageParams{
						MaxTokens: 16,
						Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "Say something."}}},
					})

					if err != nil {
						return nil, nil, fmt.Errorf("sample: %w", err)
					}

					return &mcp.CallToolResult{}, nil, nil
				})
			url := startServer(t, version, srv)
			m, _ := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(url)}})

			res, err := m.Call(context.Background(), "c", "ask", nil)

			if !isKind(err, upstream.KindInteractiveInput) {
				t.Errorf("Call = (%+v, %v), want interactive_input", res, err)
			}
		})
	}
}

// TestSDKServerRefusesElicitationWithKnownText pins the text a Go SDK
// server fails a call with when the client can't elicit. The server
// refuses before sending, with no error code, so Call can only match this
// text; if an SDK upgrade changes it, this test says so.
func TestSDKServerRefusesElicitationWithKnownText(t *testing.T) {
	for _, version := range []string{fakeupstream.V20250618, fakeupstream.V20251125} {
		t.Run(version, func(t *testing.T) {
			srv := fakeupstream.StartStreamable(t, version, fakeupstream.WithElicitation())
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, &mcp.ClientOptions{
				Capabilities:   &mcp.ClientCapabilities{},
				MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
			})
			s, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)

			if err != nil {
				t.Fatalf("Connect: %v", err)
			}

			t.Cleanup(func() { _ = s.Close() })

			_, err = s.CallTool(context.Background(), &mcp.CallToolParams{Name: fakeupstream.ToolElicit})

			var werr *jsonrpc.Error

			if !errors.As(err, &werr) || !strings.Contains(werr.Message, "client does not support elicitation") {
				t.Errorf("CallTool err = %v, want a JSON-RPC error saying the client does not support elicitation", err)
			}
		})
	}
}
