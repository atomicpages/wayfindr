//go:build !windows

package shim_test

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// An upstream's question reaches a stdio client at every protocol version,
// through the real daemon, whichever way the upstream asks: input_required
// passes through to a client at 2026-07-28, whose SDK answers and calls
// again; an older client is asked directly by the shim.
func TestShimAsksForInputAtEveryClientVersion(t *testing.T) {
	answer := &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"answer": "yes"}}, nil
		},
	}

	for _, upstreamVersion := range []string{fakeupstream.V20251125, fakeupstream.V20260728} {
		t.Run("upstream "+upstreamVersion, func(t *testing.T) {
			m := newMachineAt(t, upstreamVersion, fakeupstream.WithElicitation())

			for _, version := range fakeupstream.Versions() {
				t.Run("client "+version, func(t *testing.T) {
					cs, err := m.connectShimWith(t, "agent-"+version, &mcp.ClientSessionOptions{ProtocolVersion: version}, answer)

					if err != nil {
						t.Fatalf("%v\n%s", err, m.logOf("agent-"+version))
					}

					waitForIssueTool(t, cs)
					got := callText(t, cs, "call_tool", map[string]any{"name": "issues." + fakeupstream.ToolElicit})

					if want := `{"action":"accept","content":{"answer":"yes"}}`; got != want {
						t.Errorf("call_tool through the shim = %s, want %s", got, want)
					}
				})
			}
		})
	}
}

// An older stdio client that can't elicit gets an error saying so, rather
// than a call that hangs.
func TestShimSaysWhenAnOlderClientCannotBeAsked(t *testing.T) {
	m := newMachineAt(t, fakeupstream.V20251125, fakeupstream.WithElicitation())
	cs := m.startShimAt(t, "agent", fakeupstream.V20251125)
	waitForIssueTool(t, cs)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "call_tool", Arguments: map[string]any{"name": "issues." + fakeupstream.ToolElicit},
	})

	if err != nil {
		t.Fatalf("call_tool through the shim: %v", err)
	}

	if got := text(t, res); !res.IsError || !strings.Contains(got, "needs your input") {
		t.Errorf("result (isError %v) %q, want an error saying the client could not be asked", res.IsError, got)
	}
}

// A client at 2026-07-28 that doesn't declare elicitation isn't handed a
// question it can't answer.
func TestShimDoesNotPassQuestionsToAClientWithoutElicitation(t *testing.T) {
	m := newMachineAt(t, fakeupstream.V20251125, fakeupstream.WithElicitation())
	cs := m.startShimAt(t, "agent", fakeupstream.V20260728)
	waitForIssueTool(t, cs)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "call_tool", Arguments: map[string]any{"name": "issues." + fakeupstream.ToolElicit},
	})

	if err != nil {
		t.Fatalf("call_tool through the shim: %v", err)
	}

	if got := text(t, res); !res.IsError || !strings.Contains(got, "does not support elicitation") {
		t.Errorf("result (isError %v) %q, want an error saying the client does not support elicitation", res.IsError, got)
	}
}
