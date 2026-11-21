package upstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// olderVersions are the versions at which an upstream asks with
// elicitation/create during the call, which Wayfindr parks.
func olderVersions() []string {
	return []string{fakeupstream.V20250618, fakeupstream.V20251125}
}

// onlyRequest returns the id and params of res's single elicitation.
func onlyRequest(t *testing.T, res *mcp.CallToolResult) (string, *mcp.ElicitParams) {
	t.Helper()

	if res == nil || len(res.InputRequests) != 1 || res.RequestState == "" {
		t.Fatalf("result = %+v, want one input request and a request state", res)
	}

	for id, req := range res.InputRequests {
		p, ok := req.(*mcp.ElicitParams)

		if !ok {
			t.Fatalf("input request %q is %T, want *mcp.ElicitParams", id, req)
		}

		return id, p
	}

	return "", nil
}

// askServer serves "ask", which asks one question and sends the answer it
// got to answers, and "wait", which returns once release is closed. entered
// is closed when wait starts.
func askServer(t *testing.T, version string, answers chan<- *mcp.ElicitResult, entered, release chan struct{}) string {
	t.Helper()
	srv := newServer(version)
	srv.AddTool(&mcp.Tool{Name: "ask", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			resp, ok := req.Params.InputResponses["q"]

			if !ok {
				return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{"q": &mcp.ElicitParams{
					Message:         "Which team?",
					RequestedSchema: map[string]any{"type": "object"},
				}}}, nil
			}

			answer, _ := resp.(*mcp.ElicitResult)
			answers <- answer

			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "answered"}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "wait", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			close(entered)

			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}

			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "waited"}}}, nil
		})

	return startServer(t, version, srv)
}

// receive waits for the upstream's answer.
func receive(t *testing.T, answers <-chan *mcp.ElicitResult) *mcp.ElicitResult {
	t.Helper()

	select {
	case a := <-answers:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream got no answer within 5s")

		return nil
	}
}

func TestCallAsksAndContinueAnswers(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			srv := fakeupstream.StartStreamable(t, version, fakeupstream.WithElicitation())
			m, rec := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})
			rec.reset()

			res, err := m.Call(context.Background(), "c", fakeupstream.ToolElicit, nil)

			if err != nil {
				t.Fatalf("Call: %v", err)
			}

			id, p := onlyRequest(t, res)

			if p.Message != "Answer the question." {
				t.Errorf("message = %q, want the upstream's", p.Message)
			}

			if got := rec.of("c"); len(got) != 0 {
				t.Errorf("OnStateChanged = %v, want none: the upstream answered", got)
			}

			res, err = m.Continue(context.Background(), "c", fakeupstream.ToolElicit, nil, upstream.Input{
				State:     res.RequestState,
				Responses: mcp.InputResponseMap{id: &mcp.ElicitResult{Action: "accept", Content: map[string]any{"answer": "yes"}}},
			})

			if err != nil {
				t.Fatalf("Continue: %v", err)
			}

			if got, want := resultText(t, res), `{"action":"accept","content":{"answer":"yes"}}`; got != want {
				t.Errorf("result = %s, want %s", got, want)
			}
		})
	}
}

func TestContinueRefusesUnknownState(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125, fakeupstream.WithElicitation())
	m, _ := newManager(t, nil)
	apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})

	for _, state := range []string{"", "made-up", "i:NOSUCHINTERACTION"} {
		_, err := m.Continue(context.Background(), "c", fakeupstream.ToolElicit, nil, upstream.Input{State: state})

		if !isKind(err, upstream.KindInputExpired) {
			t.Errorf("Continue(%q) err = %v, want input_expired", state, err)
		}
	}
}

func TestContinueAnswersAnInteractionOnce(t *testing.T) {
	for _, version := range olderVersions() {
		t.Run(version, func(t *testing.T) {
			srv := fakeupstream.StartStreamable(t, version, fakeupstream.WithElicitation())
			m, _ := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(srv.URL)}})

			res, err := m.Call(context.Background(), "c", fakeupstream.ToolElicit, nil)

			if err != nil {
				t.Fatalf("Call: %v", err)
			}

			id, _ := onlyRequest(t, res)
			in := upstream.Input{State: res.RequestState, Responses: mcp.InputResponseMap{id: &mcp.ElicitResult{Action: "decline"}}}

			if _, err := m.Continue(context.Background(), "c", fakeupstream.ToolElicit, nil, in); err != nil {
				t.Fatalf("first Continue: %v", err)
			}

			if _, err := m.Continue(context.Background(), "c", fakeupstream.ToolElicit, nil, in); !isKind(err, upstream.KindInputExpired) {
				t.Errorf("second Continue err = %v, want input_expired", err)
			}
		})
	}
}

func TestParkedInteractionExpires(t *testing.T) {
	for _, version := range olderVersions() {
		t.Run(version, func(t *testing.T) {
			answers := make(chan *mcp.ElicitResult, 1)
			url := askServer(t, version, answers, make(chan struct{}), make(chan struct{}))
			m, _ := newManager(t, nil, func(o *upstream.Options) { o.InteractionTTL = 50 * time.Millisecond })
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(url)}})

			res, err := m.Call(context.Background(), "c", "ask", nil)

			if err != nil {
				t.Fatalf("Call: %v", err)
			}

			id, _ := onlyRequest(t, res)

			if got := receive(t, answers); got == nil || got.Action != "cancel" {
				t.Errorf("upstream's answer = %+v, want cancel once the interaction expired", got)
			}

			in := upstream.Input{State: res.RequestState, Responses: mcp.InputResponseMap{id: &mcp.ElicitResult{Action: "accept"}}}

			if _, err := m.Continue(context.Background(), "c", "ask", nil, in); !isKind(err, upstream.KindInputExpired) {
				t.Errorf("Continue after expiry err = %v, want input_expired", err)
			}
		})
	}
}

func TestDiscardCancelsTheQuestion(t *testing.T) {
	for _, version := range olderVersions() {
		t.Run(version, func(t *testing.T) {
			answers := make(chan *mcp.ElicitResult, 1)
			url := askServer(t, version, answers, make(chan struct{}), make(chan struct{}))
			m, _ := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(url)}})

			res, err := m.Call(context.Background(), "c", "ask", nil)

			if err != nil {
				t.Fatalf("Call: %v", err)
			}

			onlyRequest(t, res)
			m.Discard(res.RequestState)

			if got := receive(t, answers); got == nil || got.Action != "cancel" {
				t.Errorf("upstream's answer = %+v, want cancel after Discard", got)
			}
		})
	}
}

func TestConnectorTimeoutPausedWhileTheUserAnswers(t *testing.T) {
	for _, version := range olderVersions() {
		t.Run(version, func(t *testing.T) {
			answers := make(chan *mcp.ElicitResult, 1)
			url := askServer(t, version, answers, make(chan struct{}), make(chan struct{}))
			m, _ := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {
				URL: config.Template(url), Timeout: config.Duration{Duration: 300 * time.Millisecond},
			}})

			res, err := m.Call(context.Background(), "c", "ask", nil)

			if err != nil {
				t.Fatalf("Call: %v", err)
			}

			id, _ := onlyRequest(t, res)
			// The user takes longer than the connector's timeout.
			time.Sleep(600 * time.Millisecond)

			res, err = m.Continue(context.Background(), "c", "ask", nil, upstream.Input{
				State: res.RequestState, Responses: mcp.InputResponseMap{id: &mcp.ElicitResult{Action: "accept"}},
			})

			if err != nil {
				t.Fatalf("Continue: %v, want the call to complete", err)
			}

			if got := resultText(t, res); got != "answered" {
				t.Errorf("result = %q, want answered", got)
			}

			if got := receive(t, answers); got == nil || got.Action != "accept" {
				t.Errorf("upstream's answer = %+v, want accept", got)
			}
		})
	}
}

func TestElicitationDuringAnotherCallIsDeclined(t *testing.T) {
	for _, version := range olderVersions() {
		t.Run(version, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			url := askServer(t, version, make(chan *mcp.ElicitResult, 1), entered, release)
			m, _ := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {
				URL: config.Template(url), Timeout: config.Duration{Duration: 10 * time.Second},
			}})

			waited := make(chan error, 1)
			go func() {
				_, err := m.Call(context.Background(), "c", "wait", nil)
				waited <- err
			}()
			<-entered

			res, err := m.Call(context.Background(), "c", "ask", nil)

			if !isCase(err, upstream.KindInteractiveInput, upstream.CaseConcurrentCalls) {
				t.Errorf("Call = (%+v, %v), want interactive_input (concurrent_calls)", res, err)
			}

			close(release)

			if err := <-waited; err != nil {
				t.Errorf("the other call: %v, want its own result", err)
			}
		})
	}
}

// SEP-2322: an answer that leaves the question out gets it asked again.
func TestContinueWithoutAnAnswerAsksAgain(t *testing.T) {
	for _, version := range olderVersions() {
		t.Run(version, func(t *testing.T) {
			answers := make(chan *mcp.ElicitResult, 1)
			url := askServer(t, version, answers, make(chan struct{}), make(chan struct{}))
			m, _ := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"c": {URL: config.Template(url)}})

			res, err := m.Call(context.Background(), "c", "ask", nil)

			if err != nil {
				t.Fatalf("Call: %v", err)
			}

			res, err = m.Continue(context.Background(), "c", "ask", nil, upstream.Input{State: res.RequestState})

			if err != nil {
				t.Fatalf("Continue without an answer: %v", err)
			}

			id, p := onlyRequest(t, res)

			if p.Message != "Which team?" {
				t.Errorf("message = %q, want the same question again", p.Message)
			}

			res, err = m.Continue(context.Background(), "c", "ask", nil, upstream.Input{
				State: res.RequestState, Responses: mcp.InputResponseMap{id: &mcp.ElicitResult{Action: "accept"}},
			})

			if err != nil || resultText(t, res) != "answered" {
				t.Fatalf("Continue with the answer = (%+v, %v), want answered", res, err)
			}
		})
	}
}

// Closing a session waits for its calls in flight, so a parked one must
// not hold a config change (which closes the session under the
// connector's lock) until the user answers.
func TestConfigChangeDoesNotWaitOnAParkedQuestion(t *testing.T) {
	for _, version := range olderVersions() {
		t.Run(version, func(t *testing.T) {
			answers := make(chan *mcp.ElicitResult, 1)
			url := askServer(t, version, answers, make(chan struct{}), make(chan struct{}))
			m, _ := newManager(t, nil)
			before := config.Connectors{"c": {URL: config.Template(url)}}
			apply(context.Background(), m, nil, before)

			res, err := m.Call(context.Background(), "c", "ask", nil)

			if err != nil {
				t.Fatalf("Call: %v", err)
			}

			id, _ := onlyRequest(t, res)
			after := config.Connectors{"c": {URL: config.Template(url), Timeout: config.Duration{Duration: time.Minute}}}
			done := make(chan struct{})
			go func() {
				apply(context.Background(), m, before, after)
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Apply still waiting on the parked call after 5s")
			}

			_, err = m.Continue(context.Background(), "c", "ask", nil, upstream.Input{
				State: res.RequestState, Responses: mcp.InputResponseMap{id: &mcp.ElicitResult{Action: "accept"}},
			})

			if !isKind(err, upstream.KindInputExpired) {
				t.Errorf("Continue after the session closed: %v, want input_expired", err)
			}
		})
	}
}
