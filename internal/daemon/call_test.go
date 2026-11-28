package daemon_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// POST /admin/tools/call (`wayfindr call`) runs a call through the gateway
// as the token it names: none and the default token's value are the owner,
// narrowed by nothing; a scoped token is narrowed to its connectors; an
// unknown one is refused before any call. The call log names each.
func TestAdminCallRunsAsTheTokenItNames(t *testing.T) {
	h := newHost(t)
	r := serve(t, h, h.options(t), map[string]any{
		"server": map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{
			"files": stdioConnector(t, fakeupstream.V20251125),
			"notes": stdioConnector(t, fakeupstream.V20251125),
		},
	})
	r.waitStatus(t, "files and notes synced", synced("files", "notes"))
	c := r.admin()
	owner, err := clienttoken.Read(t.Context(), h.env, h.paths)

	if err != nil {
		t.Fatalf("clienttoken.Read: %v", err)
	}

	ci, err := c.CreateToken(t.Context(), admin.TokenSpec{Name: "ci", Connectors: []string{"files"}})

	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	tests := []struct {
		name, token, tool string
		wantText          string
		wantError         bool
		wantToken         string
	}{
		{"owner without a token", "", "notes.echo", `{"text":"hi"}`, false, "default"},
		{"owner by the default token", owner, "notes.echo", `{"text":"hi"}`, false, "default"},
		{"scoped token, its connector", ci.Token, "files.echo", `{"text":"hi"}`, false, "ci"},
		{"scoped token, another connector", ci.Token, "notes.echo", "no connector is called notes", true, "ci"},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := c.Call(t.Context(), admin.CallRequest{
				Name: tt.tool, Arguments: json.RawMessage(`{"text":"hi"}`), Client: admin.CallClientCLI, Token: tt.token,
			})

			if err != nil {
				t.Fatalf("Call: %v", err)
			}

			got, err := res.Decode()

			if err != nil {
				t.Fatalf("Decode: %v", err)
			}

			if len(got.Content) != 1 || got.IsError != tt.wantError || !strings.Contains(got.Content[0].Text, tt.wantText) {
				t.Errorf("result %+v, want isError %v and text containing %q", got, tt.wantError, tt.wantText)
			}

			// echo has no annotations, so it is destructive.
			if res.Class != "destructive" {
				t.Errorf("class = %q, want destructive", res.Class)
			}

			calls := r.waitCalls(t, i+1)
			last := calls.Calls[0]

			if last.Client != admin.CallClientCLI || last.TokenName != tt.wantToken {
				t.Errorf("call log row client %q, token %q, want %s, %s", last.Client, last.TokenName, admin.CallClientCLI, tt.wantToken)
			}
		})
	}

	_, err = c.Call(t.Context(), admin.CallRequest{Name: "files.echo", Client: admin.CallClientCLI, Token: "wf_s_nobody"})

	if statusOf(err) != http.StatusBadRequest {
		t.Errorf("an unknown token: %v, want 400", err)
	}

	if calls := r.waitCalls(t, len(tests)); len(calls.Calls) != len(tests) {
		t.Errorf("the call log has %d calls, want %d: an unknown token's call never runs", len(calls.Calls), len(tests))
	}
}

// An upstream's question comes back from POST /admin/tools/call as an
// input_required result in MCP's wire form, and the answer, sent with its
// request state, completes the call.
func TestAdminCallRoundTripsAQuestion(t *testing.T) {
	h := newHost(t)
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"files": stdioConnector(t, fakeupstream.V20251125, fakeupstream.WithElicitation())},
	})
	r.waitStatus(t, "files synced", synced("files"))
	c := r.admin()
	call := admin.CallRequest{Name: "files." + fakeupstream.ToolElicit, Client: admin.CallClientConsole}

	res, err := c.Call(t.Context(), call)

	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	asked, err := res.Decode()

	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if asked.ResultType != "input_required" || asked.RequestState == "" || len(asked.InputRequests) != 1 {
		t.Fatalf("result %+v, want input_required with one request and a state", asked)
	}

	ids := slices.Collect(func(yield func(string) bool) {
		for id := range asked.InputRequests {
			yield(id)
		}
	})

	if q := asked.InputRequests[ids[0]]; q.Method != "elicitation/create" || !strings.HasPrefix(q.Params.Message, "files asks: ") {
		t.Errorf("request %+v, want an elicitation the gateway prefixed with its connector", q)
	}

	call.RequestState = asked.RequestState
	call.InputResponses = map[string]admin.InputResponse{ids[0]: {Action: "accept", Content: map[string]any{"answer": "yes"}}}
	res, err = c.Call(t.Context(), call)

	if err != nil {
		t.Fatalf("Call with the answer: %v", err)
	}

	done, err := res.Decode()

	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if done.IsError || done.InputRequired() || len(done.Content) != 1 {
		t.Fatalf("result %+v, want the completed call", done)
	}

	var answer struct {
		Action  string         `json:"action"`
		Content map[string]any `json:"content"`
	}

	if err := json.Unmarshal([]byte(done.Content[0].Text), &answer); err != nil {
		t.Fatalf("answer %q: %v", done.Content[0].Text, err)
	}

	if diff := cmp.Diff(map[string]any{"answer": "yes"}, answer.Content); diff != "" || answer.Action != "accept" {
		t.Errorf("the upstream got %+v, want accept with the content\n%s", answer, diff)
	}
}
