package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// upstreamVersions are the two ways an upstream asks: elicitation/create
// during the call (2025-x), and input_required (2026-07-28).
func upstreamVersions() []string {
	return []string{fakeupstream.V20251125, fakeupstream.V20260728}
}

// answering returns client options that declare elicitation and answer
// every question with accept and answer "yes". manual turns off the SDK's
// multi round-trip handling, so CallTool returns input_required as is.
func answering(manual bool) *mcp.ClientOptions {
	return &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"answer": "yes"}}, nil
		},
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: manual},
	}
}

// connectWith is connect with client options.
func connectWith(t *testing.T, url, version string, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, opts)
	cs, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url, MaxRetries: -1},
		&mcp.ClientSessionOptions{ProtocolVersion: version})

	if err != nil {
		t.Fatalf("Connect at %s: %v", version, err)
	}

	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

// elicitCall is call_tool of the linear fake's elicit tool.
func elicitCall() *mcp.CallToolParams {
	return &mcp.CallToolParams{Name: "call_tool", Arguments: map[string]any{"name": "linear." + fakeupstream.ToolElicit}}
}

const wantAnswer = `{"action":"accept","content":{"answer":"yes"}}`

func TestCallToolAsksANewClientAndCompletesWithItsAnswer(t *testing.T) {
	for _, uv := range upstreamVersions() {
		t.Run("upstream "+uv, func(t *testing.T) {
			rec := &recorder{}
			e := newEnvWith(t, envOptions{
				fakes:           map[string][]fakeupstream.Option{"linear": {fakeupstream.WithElicitation()}},
				upstreamVersion: uv,
				gateway:         gateway.Options{Calls: rec},
			})
			cs := connectWith(t, e.url, fakeupstream.V20260728, answering(true))

			res, err := cs.CallTool(t.Context(), elicitCall())

			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}

			if !res.NeedsInput() || len(res.InputRequests) != 1 || res.RequestState == "" {
				t.Fatalf("result = %+v, want input_required with one request and a request state", res)
			}

			var id string

			for k, r := range res.InputRequests {
				p, ok := r.(*mcp.ElicitParams)

				if !ok || p.Message != "linear asks: Answer the question." {
					t.Errorf("input request %q = %+v, want the upstream's question, prefixed with its connector", k, r)
				}

				id = k
			}

			if !strings.HasPrefix(id, "linear-") {
				t.Errorf("input request id %q, want it prefixed with the connector", id)
			}

			p := elicitCall()
			p.RequestState = res.RequestState
			p.InputResponses = mcp.InputResponseMap{id: &mcp.ElicitResult{Action: "accept", Content: map[string]any{"answer": "yes"}}}
			res, err = cs.CallTool(t.Context(), p)

			if err != nil {
				t.Fatalf("CallTool with the answer: %v", err)
			}

			if got := text(t, res); res.IsError || got != wantAnswer {
				t.Errorf("result (isError %v) %q, want %s", res.IsError, got, wantAnswer)
			}

			var results []store.CallResult

			logged := rec.take()

			for _, c := range logged {
				results = append(results, c.Result)
			}

			if diff := cmp.Diff([]store.CallResult{store.CallInputRequired, store.CallOK}, results); diff != "" {
				t.Errorf("call log results (-want +got):\n%s", diff)
			}

			// Only the result that ended the call is measured, not the
			// question that came before it.
			if len(logged) == 2 && (logged[0].ResultBytes != nil || logged[1].ResultBytes == nil || *logged[1].ResultBytes <= 0) {
				t.Errorf("ResultBytes = %v then %v, want none for input_required, then the size of the answer",
					logged[0].ResultBytes, logged[1].ResultBytes)
			}
		})
	}
}

// TestCallToolRoundTripsThroughTheSDK lets the client's SDK run the round
// trip, as a real client at 2026-07-28 does.
func TestCallToolRoundTripsThroughTheSDK(t *testing.T) {
	for _, uv := range upstreamVersions() {
		t.Run("upstream "+uv, func(t *testing.T) {
			e := newEnvWith(t, envOptions{
				fakes:           map[string][]fakeupstream.Option{"linear": {fakeupstream.WithElicitation()}},
				upstreamVersion: uv,
			})
			cs := connectWith(t, e.url, fakeupstream.V20260728, answering(false))

			res, err := cs.CallTool(t.Context(), elicitCall())

			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}

			if got := text(t, res); res.IsError || got != wantAnswer {
				t.Errorf("result (isError %v) %q, want %s", res.IsError, got, wantAnswer)
			}
		})
	}
}

func TestCallToolRefusesClientsThatCannotAsk(t *testing.T) {
	e := newEnv(t, map[string][]fakeupstream.Option{"linear": {fakeupstream.WithElicitation()}})

	tests := []struct {
		name    string
		version string
		opts    *mcp.ClientOptions
		want    string
	}{
		{"2025-11-25 client", fakeupstream.V20251125, answering(true), "MCP version (2025-11-25) can't ask you"},
		{"2025-06-18 client", fakeupstream.V20250618, answering(true), "MCP version (2025-06-18) can't ask you"},
		{"no elicitation", fakeupstream.V20260728, &mcp.ClientOptions{MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}},
			"does not support elicitation"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := connectWith(t, e.url, tt.version, tt.opts)
			res := call(t, cs, "call_tool", map[string]any{"name": "linear." + fakeupstream.ToolElicit})

			if got := text(t, res); !res.IsError || !strings.Contains(got, tt.want) || !strings.Contains(got, "wayfindr stdio") {
				t.Errorf("result (isError %v) %q, want the error saying %q and pointing at wayfindr stdio", res.IsError, got, tt.want)
			}
		})
	}
}

func TestCallToolRefusesAStateThatIsNotThisCalls(t *testing.T) {
	var now atomic.Pointer[time.Time]
	start := time.Now()
	now.Store(&start)
	// A 2026-07-28 upstream parks nothing, so each refused retry leaves no
	// call in flight to make the next question ambiguous.
	e := newEnvWith(t, envOptions{
		fakes:           map[string][]fakeupstream.Option{"linear": {fakeupstream.WithElicitation(), fakeupstream.WithTools(createIssue())}},
		gateway:         gateway.Options{Now: func() time.Time { return *now.Load() }},
		upstreamVersion: fakeupstream.V20260728,
	})
	cs := connectWith(t, e.url, fakeupstream.V20260728, answering(true))

	// ask returns a fresh request state and its one request id.
	ask := func(t *testing.T) (string, string) {
		t.Helper()
		res, err := cs.CallTool(t.Context(), elicitCall())

		if err != nil || !res.NeedsInput() {
			t.Fatalf("CallTool = (%+v, %v), want input_required", res, err)
		}

		for id := range res.InputRequests {
			return res.RequestState, id
		}

		return res.RequestState, ""
	}

	tests := []struct {
		name  string
		retry func(t *testing.T, state, id string) *mcp.CallToolParams
	}{
		{"tampered", func(_ *testing.T, state, id string) *mcp.CallToolParams {
			p := elicitCall()
			p.RequestState = flip(state)
			p.InputResponses = mcp.InputResponseMap{id: &mcp.ElicitResult{Action: "accept"}}

			return p
		}},
		{"another tool", func(_ *testing.T, state, id string) *mcp.CallToolParams {
			return &mcp.CallToolParams{
				Name:           "call_tool",
				Arguments:      map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "x"}},
				RequestState:   state,
				InputResponses: mcp.InputResponseMap{id: &mcp.ElicitResult{Action: "accept"}},
			}
		}},
		{"expired", func(_ *testing.T, state, id string) *mcp.CallToolParams {
			later := start.Add(11 * time.Minute)
			now.Store(&later)
			t.Cleanup(func() { now.Store(&start) })
			p := elicitCall()
			p.RequestState = state
			p.InputResponses = mcp.InputResponseMap{id: &mcp.ElicitResult{Action: "accept"}}

			return p
		}},
		{"answers without a state", func(_ *testing.T, _, id string) *mcp.CallToolParams {
			p := elicitCall()
			p.InputResponses = mcp.InputResponseMap{id: &mcp.ElicitResult{Action: "accept"}}

			return p
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, id := ask(t)
			res, err := cs.CallTool(t.Context(), tt.retry(t, state, id))

			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}

			if got := text(t, res); !res.IsError || !strings.Contains(got, "does not belong to this call") {
				t.Errorf("result (isError %v) %q, want the invalid request state error", res.IsError, got)
			}
		})
	}
}

// flip changes one character in the middle of s.
func flip(s string) string {
	b := []byte(s)
	i := len(b) / 2

	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}

	return string(b)
}

// A refused answer discards its question, so the upstream's call doesn't
// sit parked, making the connector's next question ambiguous.
func TestCallToolRefusedAnswerReleasesTheQuestion(t *testing.T) {
	e := newEnv(t, map[string][]fakeupstream.Option{"linear": {fakeupstream.WithElicitation(), fakeupstream.WithTools(createIssue())}})
	cs := connectWith(t, e.url, fakeupstream.V20260728, answering(true))

	res, err := cs.CallTool(t.Context(), elicitCall())

	if err != nil || !res.NeedsInput() {
		t.Fatalf("CallTool = (%+v, %v), want input_required", res, err)
	}

	// The answer is sent for another tool, so it is refused.
	refused, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:         "call_tool",
		Arguments:    map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "x"}},
		RequestState: res.RequestState,
	})

	if err != nil || !refused.IsError {
		t.Fatalf("CallTool with a mismatched state = (%+v, %v), want an error result", refused, err)
	}

	// The upstream's call ends once told cancel; until then, a new question
	// would be declined as ambiguous.
	deadline := time.Now().Add(5 * time.Second)

	for {
		res, err = cs.CallTool(t.Context(), elicitCall())

		if err == nil && res.NeedsInput() {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("CallTool after the refusal = (%+v, %v), want input_required", res, err)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// TestCallToolRefusesAStateFromAnotherToken: a request state is bound to
// the client token whose call was asked, so an answer sent with another
// token is refused, even one allowed the same connector. That refusal
// leaves the question alone: the client it was put to can still answer it.
func TestCallToolRefusesAStateFromAnotherToken(t *testing.T) {
	for _, uv := range upstreamVersions() {
		t.Run("upstream "+uv, func(t *testing.T) {
			rec := &recorder{}
			e := newEnvWith(t, envOptions{
				fakes:           map[string][]fakeupstream.Option{"linear": {fakeupstream.WithElicitation()}},
				upstreamVersion: uv,
				gateway:         gateway.Options{Calls: rec},
			})
			asker := connectWith(t, e.url, fakeupstream.V20260728, answering(true))
			other := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, answering(true))
			stranger, err := other.Connect(t.Context(), &mcp.StreamableClientTransport{
				Endpoint:   e.url,
				MaxRetries: -1,
				HTTPClient: &http.Client{Transport: withHeader(scopedToken("linear"))},
			}, &mcp.ClientSessionOptions{ProtocolVersion: fakeupstream.V20260728})

			if err != nil {
				t.Fatalf("Connect with the scoped token: %v", err)
			}

			t.Cleanup(func() { _ = stranger.Close() })

			res, err := asker.CallTool(t.Context(), elicitCall())

			if err != nil || !res.NeedsInput() {
				t.Fatalf("CallTool = (%+v, %v), want input_required", res, err)
			}

			p := elicitCall()
			p.RequestState = res.RequestState

			for id := range res.InputRequests {
				p.InputResponses = mcp.InputResponseMap{id: &mcp.ElicitResult{Action: "accept", Content: map[string]any{"answer": "yes"}}}
			}

			rec.take()
			refused, err := stranger.CallTool(t.Context(), p)

			if err != nil {
				t.Fatalf("CallTool with the other token: %v", err)
			}

			if got := text(t, refused); !refused.IsError || !strings.Contains(got, "does not belong to this call") {
				t.Errorf("result (isError %v) %q, want the invalid request state error", refused.IsError, got)
			}

			if calls := rec.take(); len(calls) != 1 || calls[0].ErrorKind != "invalid_request_state" || calls[0].TokenName != "nightly" {
				t.Errorf("call log %+v, want one invalid_request_state call by nightly", calls)
			}

			answered, err := asker.CallTool(t.Context(), p)

			if err != nil {
				t.Fatalf("CallTool with the asker's answer: %v", err)
			}

			if got := text(t, answered); answered.IsError || got != wantAnswer {
				t.Errorf("asker's answer after the refusal: result (isError %v) %q, want %s", answered.IsError, got, wantAnswer)
			}
		})
	}
}

// TestCallBindsTheStateToTheTokenInstance: a request state is bound to
// the instance of the client token whose call was asked, not only its
// name. Revoking a token frees its name, so a token re-created under it
// within the state's lifetime is refused, and its refused answer leaves
// the question for the token it was put to. The asked token's own refused
// answer does discard it. Driven through Call, the other doors' entry
// point: no MCP request can carry two instances of one name.
func TestCallBindsTheStateToTheTokenInstance(t *testing.T) {
	// A 2025-11-25 upstream parks its question, so a discard is observable:
	// until it, a new question is declined as ambiguous.
	e := newEnv(t, map[string][]fakeupstream.Option{"linear": {fakeupstream.WithElicitation(), fakeupstream.WithTools(createIssue())}})
	created := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	asked := clienttoken.Client{Name: "nightly", Connectors: []string{"linear"}, Created: created}
	recreated := asked
	recreated.Created = created.Add(time.Minute)

	optsFor := func(c clienttoken.Client) gateway.CallOptions {
		return gateway.CallOptions{
			Client:     c,
			ClientName: "test-client",
			Narrow:     func() (config.Narrowing, error) { return config.Narrowing{Token: c.Connectors}, nil },
			Asker: gateway.Asker{
				ProtocolVersion: fakeupstream.V20260728,
				Capabilities:    &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{}},
			},
		}
	}
	elicit := gateway.CallRequest{Name: "linear." + fakeupstream.ToolElicit}

	// ask puts a new question to c and returns its state and answer.
	ask := func(t *testing.T, c clienttoken.Client) *gateway.Resume {
		t.Helper()
		res := e.gateway.Call(t.Context(), elicit, optsFor(c)).Result

		// NeedsInput reads the resultType the SDK sets on the wire, which a
		// result straight from callCore doesn't have yet.
		if len(res.InputRequests) == 0 || res.RequestState == "" {
			t.Fatalf("Call = %+v, want input_required", res)
		}

		r := &gateway.Resume{State: res.RequestState, Responses: mcp.InputResponseMap{}}

		for id := range res.InputRequests {
			r.Responses[id] = &mcp.ElicitResult{Action: "accept", Content: map[string]any{"answer": "yes"}}
		}

		return r
	}

	answer := func(t *testing.T, c clienttoken.Client, r *gateway.Resume) *mcp.CallToolResult {
		t.Helper()
		req := elicit
		req.Resume = r

		return e.gateway.Call(t.Context(), req, optsFor(c)).Result
	}

	wantRefusedState := func(t *testing.T, res *mcp.CallToolResult) {
		t.Helper()

		if got := text(t, res); !res.IsError || !strings.Contains(got, "does not belong to this call") {
			t.Errorf("result (isError %v) %q, want the invalid request state error", res.IsError, got)
		}
	}

	wantAnswered := func(t *testing.T, res *mcp.CallToolResult) {
		t.Helper()

		if got := text(t, res); res.IsError || got != wantAnswer {
			t.Errorf("result (isError %v) %q, want %s", res.IsError, got, wantAnswer)
		}
	}

	t.Run("its own token answers", func(t *testing.T) {
		wantAnswered(t, answer(t, asked, ask(t, asked)))
	})

	t.Run("a re-created token is refused and leaves the question", func(t *testing.T) {
		r := ask(t, asked)
		wantRefusedState(t, answer(t, recreated, r))
		wantAnswered(t, answer(t, asked, r))
	})

	t.Run("its own token's refused answer discards the question", func(t *testing.T) {
		r := ask(t, asked)
		other := gateway.CallRequest{Name: "linear.create_issue", Args: json.RawMessage(`{"title":"x"}`), Resume: r}
		wantRefusedState(t, e.gateway.Call(t.Context(), other, optsFor(asked)).Result)

		// The upstream's call ends once told cancel; until then, a new
		// question would be declined as ambiguous.
		deadline := time.Now().Add(5 * time.Second)

		for {
			res := e.gateway.Call(t.Context(), elicit, optsFor(asked)).Result

			if len(res.InputRequests) > 0 {
				wantAnswered(t, answer(t, asked, &gateway.Resume{State: res.RequestState, Responses: r.Responses}))

				return
			}

			if time.Now().After(deadline) {
				t.Fatalf("Call after the refusal = %+v, want input_required", res)
			}

			time.Sleep(20 * time.Millisecond)
		}
	})

	// Last: the question it leaves can't be answered or discarded, so it
	// stays parked until the test ends.
	t.Run("a client with no token instance can't answer", func(t *testing.T) {
		nobody := clienttoken.Client{Connectors: []string{"linear"}}
		wantRefusedState(t, answer(t, nobody, ask(t, nobody)))
	})
}
