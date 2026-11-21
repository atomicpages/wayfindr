package gateway_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

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

			for _, c := range rec.take() {
				results = append(results, c.Result)
			}

			if diff := cmp.Diff([]store.CallResult{store.CallInputRequired, store.CallOK}, results); diff != "" {
				t.Errorf("call log results (-want +got):\n%s", diff)
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
