package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// syncBuffer is a log sink the handler and the test share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

// callAPI serves the admin API with call as Options.Call and its log in
// the returned buffer.
func callAPI(t *testing.T, call func(context.Context, admin.CallRequest) (admin.CallOutcome, error)) (*admin.Client, *syncBuffer) {
	t.Helper()
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
	log := &syncBuffer{}
	opts.Logger = zerolog.New(log)
	opts.Call = call
	c, _ := serve(t, opts)

	return c, log
}

const secretToken = "wf_s_never-in-the-log" //nolint:gosec // G101: a test fixture

func TestCallPassesTheRequestAndAnswersTheResult(t *testing.T) {
	var got admin.CallRequest

	result := json.RawMessage(`{"content":[{"type":"text","text":"Issue ENG-1 created."}]}`)
	c, log := callAPI(t, func(_ context.Context, req admin.CallRequest) (admin.CallOutcome, error) {
		got = req

		return admin.CallOutcome{Result: result, Class: "write"}, nil
	})

	req := admin.CallRequest{
		Name:           "linear.create_issue",
		Arguments:      json.RawMessage(`{"title":"x"}`),
		RequestState:   "sealed",
		InputResponses: map[string]admin.InputResponse{"linear-1": {Action: "accept", Content: map[string]any{"team": "core"}}},
		Client:         admin.CallClientCLI,
		Token:          secretToken,
	}
	res, err := c.Call(t.Context(), req)

	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if diff := cmp.Diff(req, got); diff != "" {
		t.Errorf("request (-sent +received):\n%s", diff)
	}

	if diff := cmp.Diff(string(result), string(res.Result)); diff != "" || res.Class != "write" || res.ElapsedMS < 0 {
		t.Errorf("response %+v, want the result, class write, and an elapsed time\n%s", res, diff)
	}

	decoded, err := res.Decode()

	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	want := admin.ToolResult{Content: []admin.Content{{Type: "text", Text: "Issue ENG-1 created."}}}

	if diff := cmp.Diff(want, decoded); diff != "" {
		t.Errorf("decoded (-want +got):\n%s", diff)
	}

	if strings.Contains(log.String(), secretToken) {
		t.Errorf("the log holds the token:\n%s", log.String())
	}
}

// TestCallRefusesBadRequests: a bad client, a bad action, an unknown
// token, and a body over 4 MiB are refused, and the token appears in no
// log line, not even one for a failure.
func TestCallRefusesBadRequests(t *testing.T) {
	calls := 0
	c, log := callAPI(t, func(_ context.Context, req admin.CallRequest) (admin.CallOutcome, error) {
		calls++

		switch req.Token {
		case "unknown":
			return admin.CallOutcome{}, &admin.CallError{Case: admin.CallUnknownToken}
		case secretToken:
			return admin.CallOutcome{}, errors.New("the daemon broke")
		}

		return admin.CallOutcome{Result: json.RawMessage(`{}`)}, nil
	})

	tests := []struct {
		name      string
		req       admin.CallRequest
		status    int
		wantCalls int
	}{
		{"no client", admin.CallRequest{Name: "a.b"}, http.StatusBadRequest, 0},
		{"another client", admin.CallRequest{Name: "a.b", Client: "claude-code"}, http.StatusBadRequest, 0},
		{"bad action", admin.CallRequest{Name: "a.b", Client: admin.CallClientConsole,
			InputResponses: map[string]admin.InputResponse{"a-1": {Action: "maybe"}}}, http.StatusBadRequest, 0},
		{"unknown token", admin.CallRequest{Name: "a.b", Client: admin.CallClientCLI, Token: "unknown"}, http.StatusBadRequest, 1},
		{"failure", admin.CallRequest{Name: "a.b", Client: admin.CallClientCLI, Token: secretToken}, http.StatusInternalServerError, 1},
		{"too large", admin.CallRequest{Name: "a.b", Client: admin.CallClientCLI, Token: secretToken,
			Arguments: json.RawMessage(`{"x":"` + strings.Repeat("a", admin.MaxCallBody) + `"}`)}, http.StatusRequestEntityTooLarge, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls = 0
			_, err := c.Call(t.Context(), tt.req)

			if got := statusOf(err); got != tt.status {
				t.Errorf("status = %d (%v), want %d", got, err, tt.status)
			}

			if calls != tt.wantCalls {
				t.Errorf("Options.Call ran %d times, want %d", calls, tt.wantCalls)
			}
		})
	}

	if strings.Contains(log.String(), secretToken) {
		t.Errorf("the log holds the token:\n%s", log.String())
	}

	if !strings.Contains(log.String(), "admin call") {
		t.Errorf("the failure was not logged:\n%s", log.String())
	}
}

func TestCallWithoutTheSeamIs501(t *testing.T) {
	c, _ := callAPI(t, nil)
	_, err := c.Call(t.Context(), admin.CallRequest{Name: "a.b", Client: admin.CallClientCLI})

	if got := statusOf(err); got != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", got)
	}
}

// TestDecodeReadsTheWireForm: an input_required result's elicitations, and
// an image's bytes, come out of MCP's wire form.
func TestDecodeReadsTheWireForm(t *testing.T) {
	res := admin.CallResponse{Result: json.RawMessage(`{"content":[{"type":"image","data":"AAEC","mimeType":"image/png"}],` +
		`"resultType":"input_required","requestState":"s","inputRequests":{"linear-1":{"method":"elicitation/create",` +
		`"params":{"mode":"form","message":"linear asks: Which team?","requestedSchema":{"type":"object"}}}}}`)}
	got, err := res.Decode()

	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	want := admin.ToolResult{
		Content:      []admin.Content{{Type: "image", Data: []byte{0, 1, 2}, MIMEType: "image/png"}},
		ResultType:   "input_required",
		RequestState: "s",
		InputRequests: map[string]admin.InputRequest{"linear-1": {Method: "elicitation/create", Params: admin.ElicitRequest{
			Mode: "form", Message: "linear asks: Which team?", RequestedSchema: json.RawMessage(`{"type":"object"}`),
		}}},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("decoded (-want +got):\n%s", diff)
	}

	if !got.InputRequired() {
		t.Error("InputRequired = false, want true")
	}
}

// TestCallErrorAdvice: each Case gets its own words, which the 400 carries.
func TestCallErrorAdvice(t *testing.T) {
	a := (&admin.CallError{Case: admin.CallUnknownToken}).Advice()

	if !strings.Contains(a.Summary, "accepts no client token") || !strings.Contains(a.Action, "wayfindr token list") {
		t.Errorf("advice = %+v, want the unknown token's words", a)
	}
}
