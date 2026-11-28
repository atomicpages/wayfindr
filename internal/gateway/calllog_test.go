package gateway_test

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// recorder is a gateway.Calls that keeps what it is given.
type recorder struct {
	mu    sync.Mutex
	calls []store.Call
}

func (r *recorder) Record(c store.Call) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls = append(r.calls, c)
}

// take returns the calls recorded so far and forgets them.
func (r *recorder) take() []store.Call {
	r.mu.Lock()
	defer r.mu.Unlock()

	calls := r.calls
	r.calls = nil

	return calls
}

// TestCallToolRecordsEveryCallOnceWithItsOutcome covers each way call_tool
// ends (docs/internal/plans/call-log.md): the upstream was called (ok,
// tool_error, failed, all with a duration), or the gateway refused first
// (rejected, no duration).
func TestCallToolRecordsEveryCallOnceWithItsOutcome(t *testing.T) {
	rec := &recorder{}
	e := newEnvWith(t, envOptions{
		fakes: map[string][]fakeupstream.Option{
			"linear": {fakeupstream.WithTools(createIssue()), fakeupstream.WithFailingTool(), fakeupstream.WithSampling()},
		},
		gateway: gateway.Options{Calls: rec},
	})

	tests := []struct {
		name         string
		args         map[string]any
		want         store.Call
		upstreamCall bool
	}{
		{"ok", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "x"}},
			store.Call{Connector: "linear", Tool: "create_issue", Result: store.CallOK}, true},
		{"tool error", map[string]any{"name": "linear." + fakeupstream.ToolFail},
			store.Call{Connector: "linear", Tool: fakeupstream.ToolFail, Result: store.CallToolError, ErrorText: "tool failed"}, true},
		{"upstream failure", map[string]any{"name": "linear." + fakeupstream.ToolSample},
			store.Call{Connector: "linear", Tool: fakeupstream.ToolSample, Result: store.CallFailed,
				ErrorKind: "interactive_input", ErrorCase: "sampling"}, true},
		{"missing name", map[string]any{},
			store.Call{Result: store.CallRejected, ErrorKind: "invalid_name", ErrorCase: "missing_name"}, false},
		{"unqualified name", map[string]any{"name": "create_issue"},
			store.Call{Tool: "create_issue", Result: store.CallRejected, ErrorKind: "invalid_name", ErrorCase: "unqualified"}, false},
		{"no such connector", map[string]any{"name": "gmail.send"},
			store.Call{Connector: "gmail", Tool: "send", Result: store.CallRejected, ErrorKind: "unknown_tool", ErrorCase: "no_connector"}, false},
		{"no such tool", map[string]any{"name": "linear.nope"},
			store.Call{Connector: "linear", Tool: "nope", Result: store.CallRejected, ErrorKind: "unknown_tool", ErrorCase: "not_in_catalog"}, false},
		{"schema mismatch", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{}},
			store.Call{Connector: "linear", Tool: "create_issue", Result: store.CallRejected,
				ErrorKind: "invalid_arguments", ErrorCase: "schema_mismatch"}, false},
	}

	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			cs := connect(t, e.url, version)
			// The client names itself per request from 2026-07-28 on; earlier,
			// a stateless server never sees its initialize.
			client := ""

			if version >= fakeupstream.V20260728 {
				client = "test-client"
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					rec.take()
					before := time.Now()
					call(t, cs, "call_tool", tt.args)
					got := rec.take()

					if len(got) != 1 {
						t.Fatalf("recorded %d calls, want 1: %+v", len(got), got)
					}

					want := tt.want
					want.Client = client
					want.TokenName = clienttoken.DefaultName
					// The gateway's own messages are pinned by its error tests;
					// here only whether a call that did not end ok kept one.
					opts := cmp.Options{cmpopts.IgnoreFields(store.Call{}, "UserID", "StartedAt", "Duration")}

					if want.Result != store.CallOK && want.Result != store.CallToolError {
						opts = append(opts, cmpopts.IgnoreFields(store.Call{}, "ErrorText"))

						if got[0].ErrorText == "" {
							t.Error("ErrorText is empty, want the message the agent got back")
						}
					}

					if diff := cmp.Diff(want, got[0], opts); diff != "" {
						t.Errorf("recorded call (-want +got):\n%s", diff)
					}

					if got[0].UserID == "" {
						t.Error("UserID is empty")
					}

					if got[0].StartedAt.Before(before.Add(-time.Second)) || got[0].StartedAt.After(time.Now()) {
						t.Errorf("StartedAt %v is not during the call", got[0].StartedAt)
					}

					if (got[0].Duration != nil) != tt.upstreamCall {
						t.Errorf("Duration = %v, want set only when the upstream was called (%v)", got[0].Duration, tt.upstreamCall)
					}
				})
			}
		})
	}
}

func TestCallToolClipsLongNamesInTheCallLog(t *testing.T) {
	rec := &recorder{}
	e := newEnvWith(t, envOptions{
		fakes:   map[string][]fakeupstream.Option{"linear": {fakeupstream.WithTools(createIssue())}},
		gateway: gateway.Options{Calls: rec},
	})
	cs := connect(t, e.url, fakeupstream.V20251125)
	// 300 two-byte runes: a cut at byte 256 lands on a rune boundary only
	// by luck, so the clipped name must still be valid UTF-8.
	long := "x" + strings.Repeat("é", 300)
	call(t, cs, "call_tool", map[string]any{"name": "linear." + long})
	got := rec.take()

	if len(got) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(got))
	}

	if tool := got[0].Tool; len(tool) > 256 || !utf8.ValidString(tool) || !strings.HasPrefix(long, tool) {
		t.Errorf("Tool is %d bytes (valid UTF-8 %v), want a valid prefix of the name of at most 256 bytes",
			len(tool), utf8.ValidString(tool))
	}

	// NUL, which Postgres refuses, and an escape sequence and a bidi
	// override, which a terminal printing `wayfindr calls` would obey, are
	// replaced.
	call(t, cs, "call_tool", map[string]any{"name": "linear.a\x00b\x1b[2Jc\td\u202ee"})

	if got := rec.take(); len(got) != 1 || got[0].Tool != "a\uFFFDb\uFFFD[2Jc\uFFFDd\uFFFDe" {
		t.Errorf("recorded %+v, want one call whose Tool has its control characters replaced", got)
	}
}

// TestCallToolKeepsCleanErrorTextInTheCallLog: a tool error's text is kept
// on one line, printable, with URL query strings masked, and clipped to 500
// bytes on a rune boundary.
func TestCallToolKeepsCleanErrorTextInTheCallLog(t *testing.T) {
	rec := &recorder{}
	e := newEnvWith(t, envOptions{
		fakes:   map[string][]fakeupstream.Option{"linear": {fakeupstream.WithFailingTool()}},
		gateway: gateway.Options{Calls: rec},
	})
	cs := connect(t, e.url, fakeupstream.V20251125)

	fail := func(message string) string {
		t.Helper()
		call(t, cs, "call_tool", map[string]any{"name": "linear." + fakeupstream.ToolFail,
			"arguments": map[string]any{"message": message}})
		got := rec.take()

		if len(got) != 1 {
			t.Fatalf("recorded %d calls, want 1", len(got))
		}

		return got[0].ErrorText
	}

	got := fail("Bad filter.\n\n  See https://api.example.com/docs?token=s3cret for \x1b[2Jhelp.")
	want := "Bad filter. See https://api.example.com/docs?REDACTED for \uFFFD[2Jhelp."

	if got != want {
		t.Errorf("ErrorText = %q, want %q", got, want)
	}

	long := strings.Repeat("é", 300)

	if got := fail(long); len(got) > 500 || !utf8.ValidString(got) || !strings.HasPrefix(long, got) || len(got) < 498 {
		t.Errorf("ErrorText is %d bytes (valid UTF-8 %v), want a valid prefix of the text of at most 500 bytes",
			len(got), utf8.ValidString(got))
	}
}

// TestOnSearchFiresOncePerSearchTools: the daemon counts search_tools calls
// for the usage report (ADR 0020) through OnSearch. Every search_tools call
// counts, one the gateway refuses included; call_tool never does.
func TestOnSearchFiresOncePerSearchTools(t *testing.T) {
	var searches atomic.Int64
	e := newEnvWith(t, envOptions{
		fakes:   map[string][]fakeupstream.Option{"linear": {fakeupstream.WithTools(createIssue())}},
		gateway: gateway.Options{OnSearch: func() { searches.Add(1) }},
	})
	cs := connect(t, e.url, fakeupstream.Versions()[0])

	search(t, cs, map[string]any{"query": "issue"})
	call(t, cs, "search_tools", map[string]any{"size": -1})
	call(t, cs, "call_tool", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "x"}})

	if got := searches.Load(); got != 2 {
		t.Errorf("OnSearch fired %d times, want 2", got)
	}
}

// TestCallCoreServesAnyDoorAsCallToolDoes: callCore, given the options
// call_tool would build, returns what call_tool returns and records the
// same call-log row, but for the client and token names it was given.
// It drives callCore through export_test.go's CallCore, a deliberate seam
// exception the call-core plan asked for, until `wayfindr call` is the
// second door.
func TestCallCoreServesAnyDoorAsCallToolDoes(t *testing.T) {
	rec := &recorder{}
	e := newEnvWith(t, envOptions{
		fakes: map[string][]fakeupstream.Option{
			"linear": {fakeupstream.WithTools(createIssue()), fakeupstream.WithFailingTool()},
		},
		gateway: gateway.Options{Calls: rec},
	})
	cs := connect(t, e.url, fakeupstream.V20260728)
	opts := gateway.CallOptions{
		Client:     clienttoken.Client{Name: "admin"},
		ClientName: "wayfindr-cli",
		Narrow:     func() (config.Narrowing, error) { return config.Narrowing{}, nil },
		Asker:      gateway.Asker{ProtocolVersion: fakeupstream.V20260728},
	}

	tests := []struct {
		name string
		args map[string]any
	}{
		{"ok", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "x"}}},
		{"tool error", map[string]any{"name": "linear." + fakeupstream.ToolFail}},
		{"rejected", map[string]any{"name": "linear.nope"}},
		{"invalid arguments", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec.take()
			viaMCP := call(t, cs, "call_tool", tt.args)
			fromMCP := rec.take()

			name, _ := tt.args["name"].(string)
			var args json.RawMessage

			if a, ok := tt.args["arguments"]; ok {
				args, _ = json.Marshal(a)
			}

			viaCore := e.gateway.CallCore(t.Context(), gateway.CallRequest{Name: name, Args: args}, opts)
			fromCore := rec.take()

			if diff := cmp.Diff(asJSON(t, viaMCP), asJSON(t, viaCore)); diff != "" {
				t.Errorf("result (-call_tool +callCore):\n%s", diff)
			}

			if len(fromMCP) != 1 || len(fromCore) != 1 {
				t.Fatalf("recorded %d calls through call_tool and %d through callCore, want 1 each", len(fromMCP), len(fromCore))
			}

			if got := fromCore[0]; got.Client != "wayfindr-cli" || got.TokenName != "admin" {
				t.Errorf("callCore recorded client %q, token %q, want wayfindr-cli, admin", got.Client, got.TokenName)
			}

			if (fromMCP[0].Duration != nil) != (fromCore[0].Duration != nil) {
				t.Errorf("Duration set through call_tool %v, through callCore %v, want the same", fromMCP[0].Duration != nil, fromCore[0].Duration != nil)
			}

			ignore := cmpopts.IgnoreFields(store.Call{}, "Client", "TokenName", "StartedAt", "Duration")

			if diff := cmp.Diff(fromMCP[0], fromCore[0], ignore); diff != "" {
				t.Errorf("call log row (-call_tool +callCore):\n%s", diff)
			}
		})
	}
}

// asJSON is what res says, as JSON, so a result that crossed the wire
// compares with one that didn't. The _meta and resultType the SDK adds on
// the wire are left out.
func asJSON(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	b, err := json.Marshal(struct {
		Content           []mcp.Content
		StructuredContent any
		IsError           bool
	}{res.Content, res.StructuredContent, res.IsError})

	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}

	return string(b)
}

// TestCallCoreRefusesACallWithoutNarrow: Narrow is required, and a door
// that leaves it out is refused as a request with no Client is, never
// served unnarrowed.
func TestCallCoreRefusesACallWithoutNarrow(t *testing.T) {
	rec := &recorder{}
	e := newEnvWith(t, envOptions{
		fakes:   map[string][]fakeupstream.Option{"linear": {fakeupstream.WithTools(createIssue())}},
		gateway: gateway.Options{Calls: rec},
	})
	res := e.gateway.CallCore(t.Context(),
		gateway.CallRequest{Name: "linear.create_issue", Args: json.RawMessage(`{"title":"x"}`)},
		gateway.CallOptions{Client: clienttoken.Default()})

	if !res.IsError {
		t.Errorf("result %+v, want an error", res)
	}

	if got := rec.take(); len(got) != 1 || got[0].ErrorKind != "internal" || got[0].ErrorCase != "no_client" {
		t.Errorf("call log %+v, want one internal/no_client call", got)
	}
}
