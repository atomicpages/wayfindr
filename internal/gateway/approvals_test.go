package gateway_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// t0 is when the gated env's clock starts.
var t0 = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

// lockedBuffer is a bytes.Buffer safe for concurrent writes, for the
// gateway's log.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.String()
}

// gatedEnv is a gateway over a linear fake, with a call-log recorder, a
// clock the test moves, and its log in a buffer.
type gatedEnv struct {
	env

	rec  *recorder
	logs *lockedBuffer
	now  atomic.Pointer[time.Time]
}

func newGatedEnv(t *testing.T) *gatedEnv {
	t.Helper()

	return newGatedEnvIDs(t, nil)
}

// newGatedEnvIDs is newGatedEnv with approval ids drawn from ids.
func newGatedEnvIDs(t *testing.T, ids io.Reader) *gatedEnv {
	t.Helper()
	g := &gatedEnv{rec: &recorder{}, logs: &lockedBuffer{}}
	g.now.Store(&t0)
	g.env = newEnvWith(t, envOptions{
		fakes: map[string][]fakeupstream.Option{
			"linear": {fakeupstream.WithTools(createIssue(), searchIssues()), fakeupstream.WithFailingTool(), fakeupstream.WithElicitation()},
		},
		gateway:         gateway.Options{Calls: g.rec, Now: func() time.Time { return *g.now.Load() }, ApprovalIDs: ids},
		upstreamVersion: fakeupstream.V20260728,
		logs:            g.logs,
	})

	return g
}

// searchIssues is a read tool on the linear fake.
func searchIssues() *mcp.Tool {
	return &mcp.Tool{Name: "search_issues", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}
}

// advance moves the clock d on.
func (g *gatedEnv) advance(d time.Duration) {
	next := g.now.Load().Add(d)
	g.now.Store(&next)
}

// remote is a remote client token called grok-bot that may use linear,
// created at t0, with approval entries.
func remote(entries ...string) clienttoken.Client {
	return clienttoken.Client{Name: "grok-bot", Connectors: []string{"linear"}, Approval: entries, Remote: true, Created: t0}
}

// options are the CallOptions a door builds for c, with a client that can
// answer form elicitations at the newest protocol version.
func options(c clienttoken.Client) gateway.CallOptions {
	return gateway.CallOptions{
		Client:     c,
		ClientName: "cloud-agent",
		Narrow:     func() (config.Narrowing, error) { return config.Narrowing{Token: c.Connectors}, nil },
		Asker: gateway.Asker{
			ProtocolVersion: gateway.NewestProtocolVersion,
			Capabilities:    &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}}},
		},
	}
}

// call calls linear.<tool> with args (JSON, "" for none) as c, and returns
// the result and the one call-log row it made.
func (g *gatedEnv) call(t *testing.T, c clienttoken.Client, tool, args string) (*mcp.CallToolResult, store.Call) {
	t.Helper()

	return g.do(t, c, gateway.CallRequest{Name: "linear." + tool, Args: raw(args)})
}

func (g *gatedEnv) do(t *testing.T, c clienttoken.Client, r gateway.CallRequest) (*mcp.CallToolResult, store.Call) {
	t.Helper()
	res := g.gateway.Call(t.Context(), r, options(c)).Result
	rows := g.rec.take()

	if len(rows) != 1 {
		t.Fatalf("call recorded %d call-log rows, want 1", len(rows))
	}

	return res, rows[0]
}

func raw(args string) json.RawMessage {
	if args == "" {
		return nil
	}

	return json.RawMessage(args)
}

// outcome is a call-log row's result, Kind, and Case.
type outcome struct{ Result, Kind, Case string }

func outcomeOf(c store.Call) outcome {
	return outcome{string(c.Result), c.ErrorKind, c.ErrorCase}
}

var (
	ran       = outcome{"ok", "", ""}
	pending   = outcome{"rejected", "approval", "pending"}
	denied    = outcome{"rejected", "approval", "denied"}
	expired   = outcome{"rejected", "approval", "expired"}
	full      = outcome{"rejected", "approval", "queue_full"}
	badState  = outcome{"rejected", "invalid_request_state", ""}
	ambiguous = outcome{"rejected", "invalid_arguments", "ambiguous"}
)

// expect checks the row's outcome.
func expect(t *testing.T, row store.Call, want outcome) {
	t.Helper()

	if got := outcomeOf(row); got != want {
		t.Errorf("call log outcome %+v, want %+v", got, want)
	}
}

// onlyApproval is the one live approval.
func (g *gatedEnv) onlyApproval(t *testing.T) gateway.Approval {
	t.Helper()
	list := g.gateway.Approvals()

	if len(list) != 1 {
		t.Fatalf("Approvals() = %+v, want one", list)
	}

	return list[0]
}

func ids(list []gateway.Approval) []string {
	out := make([]string, len(list))

	for i, a := range list {
		out[i] = a.ID
	}

	return out
}

func TestGatedCallWaitsForTheOwnerAndRunsOnce(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("@write")
	args := `{"title":"Deploy done"}`

	res, row := g.call(t, c, "create_issue", args)
	expect(t, row, pending)
	a := g.onlyApproval(t)

	want := gateway.Approval{
		ID: a.ID, Client: "grok-bot", Connector: "linear", Tool: "create_issue", Class: config.ClassWrite,
		Args: json.RawMessage(args), State: gateway.ApprovalPending, Created: t0, Expires: t0.Add(24 * time.Hour),
	}

	if diff := cmp.Diff(want, a); diff != "" {
		t.Errorf("approval (-want +got):\n%s", diff)
	}

	if len(a.ID) < 4 || strings.Trim(a.ID, "0123456789abcdef") != "" {
		t.Errorf("approval id %q, want 4 or more hex characters", a.ID)
	}

	wantText := "linear.create_issue needs the owner's approval before it runs (approval " + a.ID + ").\n" +
		"To fix: Wait for the owner to approve it in Wayfindr, then call it again with the same arguments. " +
		"Changed arguments need a new approval."

	if got := text(t, res); !res.IsError || got != wantText {
		t.Errorf("result (isError %v):\n%s\nwant:\n%s", res.IsError, got, wantText)
	}

	if row.Duration != nil {
		t.Errorf("call log duration %v, want none: the upstream was not called", *row.Duration)
	}

	// A repeat waits on the same approval.
	_, row = g.call(t, c, "create_issue", args)
	expect(t, row, pending)

	if got := g.onlyApproval(t).ID; got != a.ID {
		t.Errorf("repeat call's approval %s, want %s", got, a.ID)
	}

	g.advance(time.Hour)

	if err := g.gateway.Approve(a.ID, time.Time{}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if got := g.onlyApproval(t); got.State != gateway.ApprovalGranted || !got.Expires.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("approved: state %s, expires %v, want granted, %v", got.State, got.Expires, t0.Add(2*time.Hour))
	}

	res, row = g.call(t, c, "create_issue", args)
	expect(t, row, ran)

	if got := text(t, res); got != args {
		t.Errorf("result %q, want the upstream's echo %q", got, args)
	}

	// Spent: the same call again asks again.
	_, row = g.call(t, c, "create_issue", args)
	expect(t, row, pending)

	if got := g.onlyApproval(t); got.ID == a.ID || got.State != gateway.ApprovalPending {
		t.Errorf("after spending: approval %+v, want a new pending one", got)
	}
}

// TestCallReturnsThePendingApprovalsID: the door that isn't /mcp gets the id
// of the approval a call waits on beside the result, for the owner's CLI to
// offer. Any other outcome returns none: a denied call's id is in its text,
// and an ungated, ran, or failed call has no approval to wait on.
func TestCallReturnsThePendingApprovalsID(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("@write")
	args := `{"title":"Deploy done"}`
	call := func(tool, args string) gateway.CallResponse {
		t.Helper()
		res := g.gateway.Call(t.Context(), gateway.CallRequest{Name: "linear." + tool, Args: raw(args)}, options(c))
		g.rec.take()

		return res
	}

	got := call("create_issue", args)
	a := g.onlyApproval(t)

	if got.Approval != a.ID || got.Approval == "" {
		t.Errorf("pending: approval %q, want the queue's id %q", got.Approval, a.ID)
	}

	if !strings.Contains(text(t, got.Result), "(approval "+a.ID+")") {
		t.Errorf("pending: result text %q does not name the approval", text(t, got.Result))
	}

	// A repeat waits on the same approval.
	if again := call("create_issue", args); again.Approval != a.ID {
		t.Errorf("repeat: approval %q, want %q", again.Approval, a.ID)
	}

	// An ungated read tool runs.
	if got := call("search_issues", `{}`); got.Approval != "" || got.Result.IsError {
		t.Errorf("ungated call: approval %q, isError %v, want none and a result", got.Approval, got.Result.IsError)
	}

	// Denied: refused with the id in its text, but not returned.
	if err := g.gateway.Deny(a.ID, time.Time{}); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	denied := call("create_issue", args)

	if !denied.Result.IsError || denied.Approval != "" {
		t.Errorf("denied: isError %v, approval %q, want a refusal with no approval", denied.Result.IsError, denied.Approval)
	}

	// Granted, the call runs and returns none.
	if err := g.gateway.Approve(a.ID, time.Time{}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if ran := call("create_issue", args); ran.Result.IsError || ran.Approval != "" {
		t.Errorf("granted: isError %v, approval %q, want a result with no approval", ran.Result.IsError, ran.Approval)
	}

	// A call that fails before the gate returns none.
	if bad := call("no_such_tool", `{}`); !bad.Result.IsError || bad.Approval != "" {
		t.Errorf("unknown tool: isError %v, approval %q, want a refusal with no approval", bad.Result.IsError, bad.Approval)
	}
}

func TestApprovalMatchesTheSameArgumentsWrittenDifferently(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("linear.create_*")

	_, row := g.call(t, c, "create_issue", `{"title":"x","labels":["a","b"]}`)
	expect(t, row, pending)

	if err := g.gateway.Approve(g.onlyApproval(t).ID, time.Time{}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	_, row = g.call(t, c, "create_issue", "{ \"labels\": [\"a\", \"b\"],\n  \"title\": \"x\" }")
	expect(t, row, ran)
}

func TestChangedArgumentsNeedANewApproval(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("@write")

	_, row := g.call(t, c, "create_issue", `{"title":"a"}`)
	expect(t, row, pending)
	first := g.onlyApproval(t)

	if err := g.gateway.Approve(first.ID, time.Time{}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	_, row = g.call(t, c, "create_issue", `{"title":"b"}`)
	expect(t, row, pending)

	list := g.gateway.Approvals()

	if len(list) != 2 || list[0].ID != first.ID || list[1].State != gateway.ApprovalPending {
		t.Fatalf("Approvals() = %+v, want the granted one, then a new pending one", list)
	}

	// The granted approval still covers its own arguments.
	_, row = g.call(t, c, "create_issue", `{"title":"a"}`)
	expect(t, row, ran)
}

func TestDenialStandsUntilItExpires(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("@write")
	args := `{"title":"x"}`

	_, row := g.call(t, c, "create_issue", args)
	expect(t, row, pending)
	id := g.onlyApproval(t).ID

	if err := g.gateway.Deny(id, time.Time{}); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	for range 2 {
		res, row := g.call(t, c, "create_issue", args)
		expect(t, row, denied)

		if got := text(t, res); !strings.Contains(got, "denied linear.create_issue") || !strings.Contains(got, "(approval "+id+")") ||
			!strings.Contains(got, "Don't retry it") {
			t.Errorf("result %q, want the denial naming approval %s", got, id)
		}
	}

	if got := g.onlyApproval(t); got.State != gateway.ApprovalDenied || !got.Expires.Equal(t0.Add(24*time.Hour)) {
		t.Errorf("denied: state %s, expires %v, want denied, %v", got.State, got.Expires, t0.Add(24*time.Hour))
	}

	g.advance(24 * time.Hour)

	_, row = g.call(t, c, "create_issue", args)
	expect(t, row, expired)

	_, row = g.call(t, c, "create_issue", args)
	expect(t, row, pending)

	if got := g.onlyApproval(t).ID; got == id {
		t.Errorf("after expiry the call waits on %s again, want a new approval", got)
	}
}

func TestApprovalsExpire(t *testing.T) {
	tests := []struct {
		name string
		// answer is what the owner does, an hour after the call, or nil.
		answer func(g *gatedEnv, id string) error
		// lives is how long after the call the approval expires.
		lives time.Duration
	}{
		{"pending", nil, 24 * time.Hour},
		{"granted", func(g *gatedEnv, id string) error { return g.gateway.Approve(id, time.Time{}) }, 2 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGatedEnv(t)
			c := remote("@write")
			args := `{"title":"x"}`

			_, row := g.call(t, c, "create_issue", args)
			expect(t, row, pending)
			id := g.onlyApproval(t).ID
			g.advance(time.Hour)

			if tt.answer != nil {
				if err := tt.answer(g, id); err != nil {
					t.Fatalf("answer: %v", err)
				}
			}

			g.advance(tt.lives - time.Hour - time.Second)

			if got := g.gateway.Approvals(); len(got) != 1 {
				t.Fatalf("a second before expiry, Approvals() = %+v, want it listed", got)
			}

			g.advance(time.Second)

			if got := g.gateway.Approvals(); len(got) != 0 {
				t.Errorf("at expiry, Approvals() = %+v, want none", got)
			}

			res, row := g.call(t, c, "create_issue", args)
			expect(t, row, expired)

			if got := text(t, res); !strings.Contains(got, "(approval "+id+") has expired") {
				t.Errorf("result %q, want the expiry of approval %s", got, id)
			}

			_, row = g.call(t, c, "create_issue", args)
			expect(t, row, pending)
		})
	}
}

func TestFailedUpstreamCallStillSpendsTheApproval(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("@destructive")
	args := `{"message":"boom"}`

	_, row := g.call(t, c, fakeupstream.ToolFail, args)
	expect(t, row, pending)

	if err := g.gateway.Approve(g.onlyApproval(t).ID, time.Time{}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	_, row = g.call(t, c, fakeupstream.ToolFail, args)

	if row.Result != store.CallToolError {
		t.Errorf("approved call's result %s, want tool_error", row.Result)
	}

	_, row = g.call(t, c, fakeupstream.ToolFail, args)
	expect(t, row, pending)
}

func TestFullQueueRefusesWithoutEvicting(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("@write")

	for i := range 8 {
		_, row := g.call(t, c, "create_issue", fmt.Sprintf(`{"title":"%d"}`, i))
		expect(t, row, pending)
	}

	before := ids(g.gateway.Approvals())

	res, row := g.call(t, c, "create_issue", `{"title":"ninth"}`)
	expect(t, row, full)

	if got := text(t, res); !strings.Contains(got, "already has 8 open approvals (waiting, approved, or denied)") || strings.Contains(got, "(approval") ||
		!strings.Contains(got, "A slot frees when an approved call is retried with the same arguments, or when an entry expires") {
		t.Errorf("result %q, want the full queue and how a slot frees, with no approval id", got)
	}

	if diff := cmp.Diff(before, ids(g.gateway.Approvals())); diff != "" {
		t.Errorf("approvals after a refused ninth (-before +after):\n%s", diff)
	}

	// The first still waits on its own approval.
	_, row = g.call(t, c, "create_issue", `{"title":"0"}`)
	expect(t, row, pending)

	// The bound is per client token instance.
	other := c
	other.Created = t0.Add(time.Minute)
	_, row = g.call(t, other, "create_issue", `{"title":"ninth"}`)
	expect(t, row, pending)

	// Expired approvals don't count.
	g.advance(24 * time.Hour)
	_, row = g.call(t, c, "create_issue", `{"title":"tenth"}`)
	expect(t, row, pending)

	if got := g.gateway.Approvals(); len(got) != 1 {
		t.Errorf("Approvals() = %+v, want only the new one", got)
	}
}

func TestOwnerCanChangeTheirAnswer(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("@write")

	t.Run("approve a denied call", func(t *testing.T) {
		_, row := g.call(t, c, "create_issue", `{"title":"a"}`)
		expect(t, row, pending)
		id := g.onlyApproval(t).ID

		if err := g.gateway.Deny(id, time.Time{}); err != nil {
			t.Fatalf("Deny: %v", err)
		}

		if err := g.gateway.Approve(id, time.Time{}); err != nil {
			t.Fatalf("Approve: %v", err)
		}

		_, row = g.call(t, c, "create_issue", `{"title":"a"}`)
		expect(t, row, ran)
	})

	t.Run("deny a granted call", func(t *testing.T) {
		_, row := g.call(t, c, "create_issue", `{"title":"b"}`)
		expect(t, row, pending)
		id := g.onlyApproval(t).ID

		if err := g.gateway.Approve(id, time.Time{}); err != nil {
			t.Fatalf("Approve: %v", err)
		}

		if err := g.gateway.Deny(id, time.Time{}); err != nil {
			t.Fatalf("Deny: %v", err)
		}

		_, row = g.call(t, c, "create_issue", `{"title":"b"}`)
		expect(t, row, denied)
	})

	t.Run("unknown or expired ids", func(t *testing.T) {
		if err := g.gateway.Approve("zzzz", time.Time{}); !errors.Is(err, gateway.ErrNoApproval) {
			t.Errorf("Approve(unknown) = %v, want ErrNoApproval", err)
		}

		if err := g.gateway.Deny("zzzz", time.Time{}); !errors.Is(err, gateway.ErrNoApproval) {
			t.Errorf("Deny(unknown) = %v, want ErrNoApproval", err)
		}

		id := g.onlyApproval(t).ID
		g.advance(24 * time.Hour)

		if err := g.gateway.Approve(id, time.Time{}); !errors.Is(err, gateway.ErrNoApproval) {
			t.Errorf("Approve(expired) = %v, want ErrNoApproval", err)
		}
	})
}

// An answer names the entry the owner was shown by its id and its creation
// time. A restart empties the queue, so an id from a listing made before
// it can name a new call; the creation time tells them apart, and the
// answer is refused, changing nothing. A zero time answers whatever entry
// the id names.
func TestAnswerIsBoundToTheEntryShown(t *testing.T) {
	before := newGatedEnvIDs(t, zeros{})
	c := remote("@write")

	_, row := before.call(t, c, "create_issue", `{"title":"harmless"}`)
	expect(t, row, pending)
	shown := before.onlyApproval(t)

	// The daemon restarts a little over an hour later; the agent's next
	// call gets the same id.
	g := newGatedEnvIDs(t, zeros{})
	g.advance(time.Hour + 123456789*time.Nanosecond)
	_, row = g.call(t, c, "create_issue", `{"title":"harmful"}`)
	expect(t, row, pending)
	live := g.onlyApproval(t)

	if live.ID != shown.ID {
		t.Fatalf("approval id after the restart %s, want %s again", live.ID, shown.ID)
	}

	for name, answer := range map[string]func(string, time.Time) error{"Approve": g.gateway.Approve, "Deny": g.gateway.Deny} {
		if err := answer(shown.ID, shown.Created); !errors.Is(err, gateway.ErrApprovalChanged) {
			t.Errorf("%s with the old listing's creation time = %v, want ErrApprovalChanged", name, err)
		}

		if err := answer(live.ID, live.Created.Add(time.Nanosecond)); !errors.Is(err, gateway.ErrApprovalChanged) {
			t.Errorf("%s a nanosecond off = %v, want ErrApprovalChanged", name, err)
		}
	}

	if got := g.onlyApproval(t).State; got != gateway.ApprovalPending {
		t.Errorf("after the refused answers: state %s, want pending", got)
	}

	// The creation time as the admin API carries it: RFC 3339 with
	// nanoseconds, here in another zone, is the same instant.
	created, err := time.Parse(time.RFC3339Nano, live.Created.In(time.FixedZone("UTC+2", 2*60*60)).Format(time.RFC3339Nano))

	if err != nil {
		t.Fatal(err)
	}

	if err := g.gateway.Approve(live.ID, created); err != nil {
		t.Fatalf("Approve with the live entry's creation time: %v", err)
	}

	_, row = g.call(t, c, "create_issue", `{"title":"harmful"}`)
	expect(t, row, ran)

	t.Run("a zero time answers the entry the id names", func(t *testing.T) {
		_, row := g.call(t, c, "create_issue", `{"title":"other"}`)
		expect(t, row, pending)

		if err := g.gateway.Deny(g.onlyApproval(t).ID, time.Time{}); err != nil {
			t.Errorf("Deny with no creation time: %v", err)
		}
	})
}

func TestApprovalIsBoundToTheTokenInstance(t *testing.T) {
	g := newGatedEnv(t)
	old := remote("@write")
	args := `{"title":"x"}`

	_, row := g.call(t, old, "create_issue", args)
	expect(t, row, pending)
	id := g.onlyApproval(t).ID

	if err := g.gateway.Approve(id, time.Time{}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// The token is revoked and created again under its name.
	again := old
	again.Created = t0.Add(time.Hour)

	_, row = g.call(t, again, "create_issue", args)
	expect(t, row, pending)

	list := g.gateway.Approvals()

	if len(list) != 2 || list[0].ID != id || list[0].State != gateway.ApprovalGranted || list[1].ID == id {
		t.Errorf("Approvals() = %+v, want %s still granted, and a new one for the new token", list, id)
	}
}

func TestOnlyRemoteClientsWithAGatingRuleAreGated(t *testing.T) {
	local := remote("@write")
	local.Remote = false

	tests := []struct {
		name   string
		client clienttoken.Client
	}{
		{"local token with entries", local},
		{"remote token without entries", remote()},
		{"remote token whose rule doesn't gate the call", remote("@destructive", "linear.delete_*")},
		{"remote token whose call is exempt", remote("@write", "!linear.create_*")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGatedEnv(t)
			_, row := g.call(t, tt.client, "create_issue", `{"title":"x"}`)
			expect(t, row, ran)

			if got := g.gateway.Approvals(); len(got) != 0 {
				t.Errorf("Approvals() = %+v, want none", got)
			}
		})
	}
}

// Entries that don't parse (a later release can tighten the grammar) gate
// every call from the client, a read included, and never let one through
// unapproved (ADR 0024).
func TestUnparseableApprovalEntriesGateEveryCall(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("@write", "@everything")

	_, row := g.call(t, c, "search_issues", "")
	expect(t, row, pending)

	if !strings.Contains(g.logs.String(), "approval entries do not parse") {
		t.Errorf("log %q, want the parse failure", g.logs.String())
	}

	if err := g.gateway.Approve(g.onlyApproval(t).ID, time.Time{}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	_, row = g.call(t, c, "search_issues", "")
	expect(t, row, ran)
}

// askState calls c's linear.elicit with args, which must ask for input,
// and returns the result's request state and its one request id.
func (g *gatedEnv) askState(t *testing.T, c clienttoken.Client, args string) (string, string) {
	t.Helper()
	res, row := g.call(t, c, fakeupstream.ToolElicit, args)

	if row.Result != store.CallInputRequired {
		t.Fatalf("elicit call: %+v, want input_required", outcomeOf(row))
	}

	for id := range res.InputRequests {
		return res.RequestState, id
	}

	t.Fatal("input_required result with no requests")

	return "", ""
}

// approvedState is askState for a gated c, once the owner approved the
// call.
func (g *gatedEnv) approvedState(t *testing.T, c clienttoken.Client, args string) (string, string) {
	t.Helper()
	_, row := g.call(t, c, fakeupstream.ToolElicit, args)
	expect(t, row, pending)

	var id string

	for _, a := range g.gateway.Approvals() {
		if a.State == gateway.ApprovalPending && string(a.Args) == args {
			id = a.ID
		}
	}

	if err := g.gateway.Approve(id, time.Time{}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	return g.askState(t, c, args)
}

// resume answers the question id of request state st, with args. A nil
// answer answers nothing, so the upstream asks again.
func resume(st, id, args string, answer *mcp.ElicitResult) gateway.CallRequest {
	r := gateway.CallRequest{Name: "linear." + fakeupstream.ToolElicit, Args: raw(args), Resume: &gateway.Resume{State: st}}

	if answer != nil {
		r.Resume.Responses = mcp.InputResponseMap{id: answer}
	}

	return r
}

// yes is the answer the fake's elicit tool echoes as wantAnswer.
func yes() *mcp.ElicitResult {
	return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"answer": "yes"}}
}

// A call that answers an input_required with a valid request state skips
// the gate: the call that got the state passed it. One whose state was
// sealed for another token instance is refused for its state, and queues
// nothing.
func TestResumedCallSkipsTheGate(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("linear.*")

	t.Run("valid state", func(t *testing.T) {
		st, id := g.approvedState(t, c, "")
		res, row := g.do(t, c, resume(st, id, "", yes()))
		expect(t, row, ran)

		if got := text(t, res); got != wantAnswer {
			t.Errorf("resumed result %q, want %q", got, wantAnswer)
		}

		if got := g.gateway.Approvals(); len(got) != 0 {
			t.Errorf("Approvals() = %+v, want none", got)
		}
	})

	t.Run("state sealed for another token instance", func(t *testing.T) {
		st, id := g.approvedState(t, c, "")
		other := c
		other.Created = t0.Add(time.Minute)

		_, row := g.do(t, other, resume(st, id, "", yes()))
		expect(t, row, badState)

		if got := g.gateway.Approvals(); len(got) != 0 {
			t.Errorf("Approvals() = %+v, want none", got)
		}
	})
}

// One approval runs a gated tool once: a request state it led to is taken
// once, so sending it back again, in the same text or another encoding of
// the same bytes, is refused before the upstream is called.
func TestGatedRequestStateIsTakenOnce(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("linear.*")
	st, id := g.approvedState(t, c, "")

	_, row := g.do(t, c, resume(st, id, "", yes()))
	expect(t, row, ran)

	// Base64 decoding skips line breaks, so this is the same sealed state.
	for _, again := range []string{st, st[:4] + "\n" + st[4:]} {
		_, row = g.do(t, c, resume(again, id, "", yes()))
		expect(t, row, badState)

		if row.Duration != nil {
			t.Errorf("replayed state: call log duration %v, want none: the upstream was not called", *row.Duration)
		}
	}
}

// Each round of a gated call's elicitation mints a new state, so a chain
// of several rounds runs; a state of an earlier round is spent.
func TestGatedElicitationOfSeveralRounds(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("linear.*")
	first, id := g.approvedState(t, c, "")

	// No answer: the upstream asks again, under a new state.
	res, row := g.do(t, c, resume(first, id, "", nil))

	if row.Result != store.CallInputRequired || res.RequestState == "" || res.RequestState == first {
		t.Fatalf("unanswered round: %+v, state %q, want input_required with a new state", outcomeOf(row), res.RequestState)
	}

	second := res.RequestState

	_, row = g.do(t, c, resume(first, id, "", yes()))
	expect(t, row, badState)

	res, row = g.do(t, c, resume(second, id, "", yes()))
	expect(t, row, ran)

	if got := text(t, res); got != wantAnswer {
		t.Errorf("second round's result %q, want %q", got, wantAnswer)
	}
}

// A client the approval rule doesn't gate resumes as before: its request
// states are not taken.
func TestUngatedRequestStateIsNotTaken(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("@destructive", "!linear.elicit")
	st, id := g.askState(t, c, "")

	for range 2 {
		_, row := g.do(t, c, resume(st, id, "", yes()))
		expect(t, row, ran)
	}
}

// restart replaces the gateway with a new one over the same upstreams,
// catalog, clock, and master key, as a daemon restart does: its approval
// queue, and its record of spent request states, start empty.
func (g *gatedEnv) restart() {
	g.gateway = gateway.New(g.options)
}

// The record of spent request states is in memory, so a gated call's state
// is taken only by the gateway that sealed it: after a restart, a state
// that may have been spent before it is refused, and the agent's next call
// pends again.
func TestGatedRequestStateDoesNotSurviveARestart(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("linear.*")
	st, id := g.approvedState(t, c, "")

	_, row := g.do(t, c, resume(st, id, "", yes()))
	expect(t, row, ran)

	g.restart()

	_, row = g.do(t, c, resume(st, id, "", yes()))
	expect(t, row, badState)

	if row.Duration != nil {
		t.Errorf("replayed state: call log duration %v, want none: the upstream was not called", *row.Duration)
	}

	t.Run("never used before the restart", func(t *testing.T) {
		st, id := g.approvedState(t, c, `{"n":1}`)
		g.restart()

		_, row := g.do(t, c, resume(st, id, `{"n":1}`, yes()))
		expect(t, row, badState)

		_, row = g.call(t, c, fakeupstream.ToolElicit, `{"n":1}`)
		expect(t, row, pending)
	})
}

// An ungated call's state is not taken, so it still resumes after a
// restart, as often as the client sends it.
func TestUngatedRequestStateSurvivesARestart(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("@destructive", "!linear.elicit")
	st, id := g.askState(t, c, "")

	g.restart()

	for range 2 {
		_, row := g.do(t, c, resume(st, id, "", yes()))
		expect(t, row, ran)
	}
}

// A state minted by a gated call is taken once even when the call is no
// longer gated as it resumes: here the same client token instance resumes
// with entries that exempt the tool, as when a tool's class changes under
// a class entry.
func TestGatedRequestStateIsTakenOnceAfterTheGateLetsGo(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("linear.*")
	st, id := g.approvedState(t, c, "")
	exempt := remote("@destructive", "!linear.elicit")

	if exempt.Instance() != c.Instance() {
		t.Fatalf("instances %q and %q differ, want the same token", exempt.Instance(), c.Instance())
	}

	_, row := g.do(t, exempt, resume(st, id, "", yes()))
	expect(t, row, ran)

	_, row = g.do(t, exempt, resume(st, id, "", yes()))
	expect(t, row, badState)

	t.Run("its next round is taken once too", func(t *testing.T) {
		st, id := g.approvedState(t, c, `{"n":2}`)
		res, row := g.do(t, exempt, resume(st, id, `{"n":2}`, nil))

		if row.Result != store.CallInputRequired {
			t.Fatalf("unanswered round: %+v, want input_required", outcomeOf(row))
		}

		next := res.RequestState

		_, row = g.do(t, exempt, resume(next, id, `{"n":2}`, yes()))
		expect(t, row, ran)

		_, row = g.do(t, exempt, resume(next, id, `{"n":2}`, yes()))
		expect(t, row, badState)
	})
}

// A gated call's arguments must read the same to the approval's digest as
// to the upstream: no repeated key, no invalid UTF-8. Otherwise the owner
// could approve {"title":"a"} and the upstream run {"title":"b","title":"a"}
// read another way. The call is refused before the gate, queuing nothing;
// an ungated call is not checked.
func TestGatedCallWithAmbiguousArgumentsIsRefused(t *testing.T) {
	tests := []struct{ name, args string }{
		{"repeated key", `{"title":"b","title":"a"}`},
		{"repeated key, escaped", `{"title":"b","\u0074itle":"a"}`},
		{"repeated key, nested", `{"title":"a","meta":[{"to":"x","to":"y"}]}`},
		{"invalid UTF-8", "{\"title\":\"a\xff\"}"},
		{"lone high surrogate", `{"title":"a\ud800"}`},
		{"lone low surrogate", `{"title":"\udc00a"}`},
		{"high surrogate before another high", `{"title":"\ud800\ud800"}`},
		{"lone surrogate in a key", `{"title":"a","\ud800":1}`},
		// Keys that look alike: equal ignoring case, or once normalized
		// (NFKC). An upstream that matches keys ignoring case, or
		// normalizes them, would merge them.
		{"keys that differ in case", `{"title":"a","to":"friend","TO":"attacker"}`},
		{"keys that differ in case, nested", `{"title":"a","meta":{"Cc":"x","cC":"y"}}`},
		{"kelvin sign and k", "{\"title\":\"a\",\"k\":1,\"\u212a\":2}"},
		{"kelvin sign escaped and K", `{"title":"a","\u212a":1,"K":2}`},
		{"long s and S", "{\"title\":\"a\",\"\u017f\":1,\"S\":2}"},
		{"dotless i", "{\"title\":\"a\",\"file\":1,\"f\u0131le\":2}"},
		{"dotted capital I", "{\"title\":\"a\",\"file\":1,\"f\u0130le\":2}"},
		{"fi ligature", "{\"title\":\"a\",\"file\":1,\"\ufb01le\":2}"},
		{"sharp s and ss", "{\"title\":\"a\",\"ss\":1,\"\u00df\":2}"},
		{"capital sharp s and SS", "{\"title\":\"a\",\"\u1e9e\":1,\"SS\":2}"},
		{"full-width f", "{\"title\":\"a\",\"file\":1,\"\uff46ile\":2}"},
		{"composed and decomposed accent", "{\"title\":\"a\",\"caf\u00e9\":1,\"cafe\u0301\":2}"},
		{"final sigma and sigma", "{\"title\":\"a\",\"\u03c3\":1,\"\u03c2\":2}"},
		{"look-alike keys, nested", "{\"title\":\"a\",\"meta\":[{\"Stra\u00dfe\":1,\"STRASSE\":2}]}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGatedEnv(t)
			res, row := g.call(t, remote("@write"), "create_issue", tt.args)
			expect(t, row, ambiguous)

			if got := text(t, res); !strings.Contains(got, "repeat a key in an object or hold text that isn't valid UTF-8") {
				t.Errorf("result %q, want the ambiguity named", got)
			}

			if got := g.gateway.Approvals(); len(got) != 0 {
				t.Errorf("Approvals() = %+v, want none", got)
			}

			local := remote("@write")
			local.Remote = false
			_, row = g.call(t, local, "create_issue", tt.args)
			expect(t, row, ran)
		})
	}

	t.Run("escapes that read one way are fine", func(t *testing.T) {
		g := newGatedEnv(t)
		_, row := g.call(t, remote("@write"), "create_issue", `{"title":"\ud83d\ude00 \"\\u\" \ufffd"}`)
		expect(t, row, pending)
	})

	t.Run("the same key in different objects is fine", func(t *testing.T) {
		g := newGatedEnv(t)
		_, row := g.call(t, remote("@write"), "create_issue", `{"title":"a","meta":[{"to":"x"},{"TO":"y"}],"TITLES":1}`)
		expect(t, row, pending)
	})

	// One value, then more, which a lenient reader could take as arguments
	// too, is refused before the gate, queuing nothing. The input schema's
	// check refuses it first, as not an object; gatedArguments' own check
	// (TestGatedArgumentsTable) stands behind it.
	t.Run("data after the arguments", func(t *testing.T) {
		for _, args := range []string{`{"title":"a"} {"title":"b"}`, `{"title":"a"}1`, `{"title":"a"}]`} {
			g := newGatedEnv(t)
			_, row := g.call(t, remote("@write"), "create_issue", args)

			if got := outcomeOf(row); got.Result != "rejected" || got.Kind != "invalid_arguments" {
				t.Errorf("%s: call log outcome %+v, want rejected as invalid_arguments", args, got)
			}

			if got := g.gateway.Approvals(); len(got) != 0 {
				t.Errorf("%s: Approvals() = %+v, want none", args, got)
			}
		}
	})

	t.Run("distinct keys are fine", func(t *testing.T) {
		for _, args := range []string{
			"{\"title\":\"a\",\"\u041d\u0430\u0437\u0432\u0430\u043d\u0438\u0435\":1,\"\u0421\u0442\u0430\u0442\u0443\u0441\":2}",
			"{\"title\":\"a\",\"cafe\":1,\"caf\u00e9\":2,\"s\":3,\"ss\":4}",
			"{\"title\":\"a\",\"\u5b9b\u5148\":1,\"\u4ef6\u540d\":2}",
			`{"title":"a"}` + " \n\t",
		} {
			g := newGatedEnv(t)
			_, row := g.call(t, remote("@write"), "create_issue", args)
			expect(t, row, pending)
		}
	})

	t.Run("resumed with the approved arguments, repeated", func(t *testing.T) {
		g := newGatedEnv(t)
		c := remote("linear.*")
		st, id := g.approvedState(t, c, `{"to":"friend"}`)

		_, row := g.do(t, c, resume(st, id, `{"to":"attacker","to":"friend"}`, yes()))
		expect(t, row, ambiguous)
	})
}

// deep is arguments nested depth levels deep, the arguments object being
// the first: a title, and objects inside "meta".
func deep(depth int) string {
	return `{"title":"a","meta":` + strings.Repeat(`{"a":`, depth-2) + "{}" + strings.Repeat("}", depth-2) + "}"
}

// A gated call's arguments are read in full by the owner and held in the
// queue, so arguments over 64 KiB, or nested deeper than 32 levels, are
// refused before the gate, queuing nothing, and without echoing them. An
// ungated call is not checked.
func TestGatedCallWithTooLargeArgumentsIsRefused(t *testing.T) {
	tooLarge := outcome{"rejected", "invalid_arguments", "too_large"}
	// 64 KiB and one byte: the title fills what the rest leaves.
	big := `{"title":"` + strings.Repeat("x", 64<<10-len(`{"title":""}`)+1) + `"}`

	tests := []struct{ name, args string }{
		{"over 64 KiB", big},
		{"33 levels", deep(33)},
		{"an indent bomb", deep(995)},
		{"33 levels of arrays", `{"title":"a","meta":` + strings.Repeat("[", 32) + strings.Repeat("]", 32) + "}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGatedEnv(t)
			res, row := g.call(t, remote("@write"), "create_issue", tt.args)
			expect(t, row, tooLarge)

			if got := text(t, res); !strings.Contains(got, "larger than 64 KiB or nest deeper than 32 levels") || strings.Contains(got, "xxxx") {
				t.Errorf("result %q, want the bounds named and the arguments not echoed", got)
			}

			if got := g.gateway.Approvals(); len(got) != 0 {
				t.Errorf("Approvals() = %+v, want none", got)
			}

			local := remote("@write")
			local.Remote = false
			_, row = g.call(t, local, "create_issue", tt.args)
			expect(t, row, ran)
		})
	}

	t.Run("64 KiB and 32 levels are held", func(t *testing.T) {
		g := newGatedEnv(t)

		for _, args := range []string{big[:len(big)-3] + `"}`, deep(32)} {
			_, row := g.call(t, remote("@write"), "create_issue", args)
			expect(t, row, pending)
		}
	})
}

// zeros is an approval id source that draws zero bytes, so every new id
// would be 0000.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)

	return len(p), nil
}

// An id that left the queue, spent or expired, is not handed to another
// call for 24 hours, so an owner reading an old listing can't answer a
// call it never showed. A restart starts the ids over; an answer's created
// time covers that (TestAnswerIsBoundToTheEntryShown).
func TestApprovalIDsAreNotReusedForADay(t *testing.T) {
	g := newGatedEnvIDs(t, zeros{})
	c := remote("@write")

	_, row := g.call(t, c, "create_issue", `{"title":"a"}`)
	expect(t, row, pending)

	if got := g.onlyApproval(t).ID; got != "0000" {
		t.Fatalf("first approval id %s, want 0000", got)
	}

	if err := g.gateway.Approve("0000", time.Time{}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	_, row = g.call(t, c, "create_issue", `{"title":"a"}`)
	expect(t, row, ran)

	// 0000 was spent: the next approval gets a longer id.
	_, row = g.call(t, c, "create_issue", `{"title":"b"}`)
	expect(t, row, pending)

	if got := g.onlyApproval(t).ID; got != "000000" {
		t.Errorf("approval id after 0000 was spent: %s, want 000000", got)
	}

	// A day on, 000000 has expired and is retired in turn; 0000 is free.
	g.advance(24 * time.Hour)

	for _, args := range []string{`{"title":"c"}`, `{"title":"d"}`} {
		_, row = g.call(t, c, "create_issue", args)
		expect(t, row, pending)
	}

	if diff := cmp.Diff([]string{"0000", "00000000"}, ids(g.gateway.Approvals())); diff != "" {
		t.Errorf("approval ids a day on (-want +got):\n%s", diff)
	}
}

// A denial stands for at least an hour, even of a grant made just before
// the entry's 24 hours ran out.
func TestLateDenialStandsForAnHour(t *testing.T) {
	g := newGatedEnv(t)
	c := remote("@write")
	args := `{"title":"x"}`

	_, row := g.call(t, c, "create_issue", args)
	expect(t, row, pending)
	id := g.onlyApproval(t).ID
	g.advance(23*time.Hour + 30*time.Minute)

	if err := g.gateway.Approve(id, time.Time{}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	g.advance(10 * time.Minute)

	if err := g.gateway.Deny(id, time.Time{}); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	want := t0.Add(24*time.Hour + 40*time.Minute)

	if got := g.onlyApproval(t); got.State != gateway.ApprovalDenied || !got.Expires.Equal(want) {
		t.Errorf("denied late: state %s, expires %v, want denied, %v", got.State, got.Expires, want)
	}

	g.advance(30 * time.Minute)

	_, row = g.call(t, c, "create_issue", args)
	expect(t, row, denied)
}

// The arguments live only in the queue: not in the call log, whatever the
// call's outcome, and not in the gateway's log.
func TestApprovalArgumentsReachNeitherTheCallLogNorTheLog(t *testing.T) {
	const secret = "s3cret-7f1c"

	g := newGatedEnv(t)
	c := remote("@write", "linear.search_*")
	args := `{"title":"` + secret + `"}`
	var rows []store.Call

	record := func(row store.Call) { rows = append(rows, row) }

	_, row := g.call(t, c, "create_issue", args)
	record(row)
	id := g.onlyApproval(t).ID

	if err := g.gateway.Deny(id, time.Time{}); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	_, row = g.call(t, c, "create_issue", args)
	record(row)

	if err := g.gateway.Approve(id, time.Time{}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	_, row = g.call(t, c, "create_issue", args)
	record(row)

	for i := range 9 {
		_, row = g.call(t, c, "search_issues", fmt.Sprintf(`{"q":"%s-%d"}`, secret, i))
		record(row)
	}

	g.advance(25 * time.Hour)

	_, row = g.call(t, c, "search_issues", `{"q":"`+secret+`-0"}`)
	record(row)

	got := make([]outcome, len(rows))

	for i, r := range rows {
		got[i] = outcomeOf(r)

		if b, _ := json.Marshal(r); bytes.Contains(b, []byte(secret)) {
			t.Errorf("call log row %s holds the arguments", b)
		}
	}

	want := []outcome{pending, denied, ran, pending, pending, pending, pending, pending, pending, pending, pending, full, expired}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("outcomes (-want +got):\n%s", diff)
	}

	logs := g.logs.String()

	if !strings.Contains(logs, `"approval_id":"`+id+`"`) {
		t.Errorf("log %q, want the pending approval's line", logs)
	}

	if strings.Contains(logs, secret) {
		t.Errorf("log holds the arguments:\n%s", logs)
	}
}

// Two identical calls at once spend one approval: one runs, the other
// waits on a new approval.
func TestConcurrentIdenticalCallsSpendAnApprovalOnce(t *testing.T) {
	g := newGatedEnv(t)

	for i := range 20 {
		// A token instance per round, so the leftover approvals of earlier
		// rounds don't fill its queue.
		c := remote("@write")
		c.Created = t0.Add(time.Duration(i) * time.Minute)
		args := fmt.Sprintf(`{"title":"%d"}`, i)

		g.call(t, c, "create_issue", args)

		var id string

		for _, a := range g.gateway.Approvals() {
			if string(a.Args) == args {
				id = a.ID
			}
		}

		if err := g.gateway.Approve(id, time.Time{}); err != nil {
			t.Fatalf("round %d: Approve: %v", i, err)
		}

		var wg sync.WaitGroup

		for range 2 {
			wg.Go(func() {
				g.gateway.Call(t.Context(), gateway.CallRequest{Name: "linear.create_issue", Args: raw(args)}, options(c))
			})
		}

		wg.Wait()

		got := g.rec.take()
		outcomes := make([]outcome, len(got))

		for j, r := range got {
			outcomes[j] = outcomeOf(r)
		}

		less := func(a, b outcome) bool { return a.Kind < b.Kind }

		if diff := cmp.Diff([]outcome{ran, pending}, outcomes, cmpopts.SortSlices(less)); diff != "" {
			t.Fatalf("round %d outcomes (-want +got):\n%s", i, diff)
		}
	}
}
