package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// fakeApprovals is the daemon's side of the approval queue, in memory: it
// lists rows (or listed, when set, as a listing made before the queue
// changed) and records each answer, as "verb id", then " as shown" when it
// named the entry's created time. An id it doesn't hold is
// admin.ErrNoApproval, and a created time other than the entry's
// admin.ErrApprovalChanged, as the daemon's adapter reports them.
type fakeApprovals struct {
	mu      sync.Mutex
	rows    []admin.ApprovalRow
	listed  []admin.ApprovalRow
	answers []string
}

func (f *fakeApprovals) List(context.Context) ([]admin.ApprovalRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.listed != nil {
		return slices.Clone(f.listed), nil
	}

	return slices.Clone(f.rows), nil
}

func (f *fakeApprovals) Approve(_ context.Context, id string, created time.Time) error {
	return f.answer("approve", id, created)
}

func (f *fakeApprovals) Deny(_ context.Context, id string, created time.Time) error {
	return f.answer("deny", id, created)
}

func (f *fakeApprovals) answer(verb, id string, created time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	i := slices.IndexFunc(f.rows, func(r admin.ApprovalRow) bool { return r.ID == id })

	if i < 0 {
		return fmt.Errorf("%w: %s", admin.ErrNoApproval, id)
	}

	answer := verb + " " + id

	if !created.IsZero() {
		if !created.Equal(f.rows[i].Created) {
			return fmt.Errorf("%w: %s", admin.ErrApprovalChanged, id)
		}

		answer += " as shown"
	}

	f.answers = append(f.answers, answer)

	return nil
}

func (f *fakeApprovals) answered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.answers)
}

// approvalsHost serves the admin API over f and returns a host that reaches
// it.
func approvalsHost(t *testing.T, f *fakeApprovals) Host {
	t.Helper()
	host, root := testHost(t)
	writeStatusHost(t, root, adminDaemonOver(t, &fakeUpstreams{}, &fakeStore{}, admin.ConfigState{}, func(o *admin.Options) { o.Approvals = f }))

	return host
}

// queued is two entries as the daemon lists them, relative to now: the
// owner has not answered the first, and has approved the second. Each time
// sits 30 seconds inside its minute, so a listing made a moment later
// shows the same AGE and EXPIRES.
func queued(now time.Time) []admin.ApprovalRow {
	return []admin.ApprovalRow{
		{
			ID: "a3f2", Client: "grok-bot", Tool: "slack.post_message", Class: "write",
			Arguments: json.RawMessage(`{"channel":"C01","text":"Deploy done, cutting v1.2"}`),
			State:     admin.ApprovalPending, Created: now.Add(-2*time.Minute - 5*time.Second), Expires: now.Add(24*time.Hour - 2*time.Minute + 30*time.Second),
		},
		{
			ID: "0b17", Client: "muse", Tool: "linear.run_query", Class: "destructive",
			Arguments: json.RawMessage(`{}`),
			State:     admin.ApprovalGranted, Created: now.Add(-3*time.Hour - 10*time.Minute), Expires: now.Add(58*time.Minute + 30*time.Second),
		},
	}
}

// TestApprovalsTableMatchesTheLayout pins the listing: the columns, the
// relative times, and each row's arguments on a line of their own, after
// the row's id and the arguments' size.
func TestApprovalsTableMatchesTheLayout(t *testing.T) {
	host := approvalsHost(t, &fakeApprovals{rows: queued(time.Now())})
	stdout, stderr, code := run(t, host, "approvals")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	want := `ID    CLIENT    TOOL                CLASS        STATE    AGE   EXPIRES
a3f2  grok-bot  slack.post_message  write        pending  2m    23h58m
a3f2  arguments (52 B): {"channel":"C01","text":"Deploy done, cutting v1.2"}
0b17  muse      linear.run_query    destructive  granted  3h10m 58m
0b17  arguments (2 B): {}
`
	// tabwriter pads AGE to its widest cell; compare by fields so the test
	// pins the content, and the arguments lines exactly.
	gotLines, wantLines := strings.Split(stdout, "\n"), strings.Split(want, "\n")

	if len(gotLines) != len(wantLines) {
		t.Fatalf("output has %d lines, want %d:\n%s", len(gotLines), len(wantLines), stdout)
	}

	for i, w := range wantLines {
		if strings.Contains(w, " arguments (") {
			if gotLines[i] != w {
				t.Errorf("line %d = %q, want %q", i, gotLines[i], w)
			}

			continue
		}

		if diff := cmp.Diff(strings.Fields(w), strings.Fields(gotLines[i])); diff != "" {
			t.Errorf("line %d (-want +got):\n%s", i, diff)
		}
	}
}

func TestApprovalsTableSaysWhenNothingWaits(t *testing.T) {
	host := approvalsHost(t, &fakeApprovals{rows: []admin.ApprovalRow{}})
	stdout, stderr, code := run(t, host, "approvals")

	if code != 0 || stdout != "No approvals are waiting.\n" {
		t.Errorf("exit %d, stdout %q, stderr %q; want the empty message", code, stdout, stderr)
	}
}

// TestApprovalsShowTheWholeArgumentsMadeVisible: the owner approves what
// they read, so the arguments are never cut, however long; and nothing the
// agent sent can act on the terminal or hide from the owner: control,
// format, and separator characters, invalid UTF-8, and runs of spaces are
// shown as escapes (redact.Visible). The cells are the agent's text too.
func TestApprovalsShowTheWholeArgumentsMadeVisible(t *testing.T) {
	long := strings.Repeat("0123456789", 2000)
	now := time.Now()
	row := func(id, tool, args string) admin.ApprovalRow {
		return admin.ApprovalRow{ID: id, Client: "grok-bot", Tool: tool, Class: "write", State: admin.ApprovalPending,
			Created: now, Expires: now.Add(time.Hour), Arguments: json.RawMessage(args)}
	}
	rows := []admin.ApprovalRow{
		row("a3f2", "slack.post_message", `{"text":"`+long+`"}`),
		// Characters that are legal inside a JSON string arrive as they are:
		// a bidi override, a zero-width joiner, a byte order mark, a soft
		// hyphen, a line separator, and "rm" in tag characters.
		row("b4a1", "slack.post_message", "{\"text\":\"harmless\u202etxt.exe a\u200db \ufeffkey de\u00adlete x\u2028y \U000E0072\U000E006D\"}"),
		// The agent's tool name goes through the table's cell too: an
		// escape sequence, a bell, and a zero-width space. (Inside the
		// arguments these can only arrive escaped, as JSON requires, and so
		// already visible; redact's tests cover a value that is not JSON.)
		row("d6c3", "slack.\x1b[31mred\x07\u200b", `{"a":"b"}`),
		// Runs of spaces that would push the rest of a line out of view,
		// or wrap it into what looks like a row of its own.
		row("e7d4", "slack.post      message", `{"text":"ok`+strings.Repeat(" ", 200)+`f00d  grok-bot  slack.read"}`),
	}
	host := approvalsHost(t, &fakeApprovals{rows: rows})
	got, stderr, code := run(t, host, "approvals")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if !strings.Contains(got, "a3f2  arguments (20 KiB): {\"text\":\""+long+"\"}\n") {
		t.Errorf("the long arguments were cut or changed; output is %d bytes", len(got))
	}

	for _, bad := range []string{"\u202e", "\u200d", "\ufeff", "\u00ad", "\u2028", "\U000E0072", "\x1b", "\x07", "\u200b"} {
		if strings.Contains(got, bad) {
			t.Errorf("output holds %q, which a terminal would obey or the owner could not see", bad)
		}
	}

	for _, want := range []string{
		`b4a1  arguments (69 B): {"text":"harmless\u202Etxt.exe a\u200Db \uFEFFkey de\u00ADlete x\u2028y \U000E0072\U000E006D"}` + "\n",
		`slack.\u001B[31mred\u0007\u200B`,
		`slack.post  \u0020\u0020\u0020\u0020message`,
		`e7d4  arguments (239 B): {"text":"ok  ` + strings.Repeat(`\u0020`, 198) + `f00d  grok-bot  slack.read"}` + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not hold %q:\n%s", want, got)
		}
	}
}

func TestApprovalsCommandListsTheQueue(t *testing.T) {
	for _, args := range [][]string{{"approvals"}, {"approvals", "list"}, {"approvals", "ls"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			host := approvalsHost(t, &fakeApprovals{rows: queued(time.Now())})
			stdout, stderr, code := run(t, host, args...)

			if code != 0 {
				t.Fatalf("exit code = %d; stderr: %s", code, stderr)
			}

			lines := strings.Split(stdout, "\n")

			if got := strings.Fields(lines[0]); !slices.Equal(got, []string{"ID", "CLIENT", "TOOL", "CLASS", "STATE", "AGE", "EXPIRES"}) {
				t.Errorf("header %v, want the columns", got)
			}

			if got := strings.Fields(lines[1]); !slices.Equal(got, []string{"a3f2", "grok-bot", "slack.post_message", "write", "pending", "2m", "23h58m"}) {
				t.Errorf("first row %v, want a3f2's cells", got)
			}

			if lines[2] != `a3f2  arguments (52 B): {"channel":"C01","text":"Deploy done, cutting v1.2"}` {
				t.Errorf("arguments line %q", lines[2])
			}
		})
	}
}

func TestApprovalsCommandJSON(t *testing.T) {
	now := time.Now()
	host := approvalsHost(t, &fakeApprovals{rows: queued(now)})
	stdout, stderr, code := run(t, host, "approvals", "--json")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	var got admin.ApprovalList

	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not an ApprovalList: %v\n%s", err, stdout)
	}

	if len(got.Approvals) != 2 || got.Approvals[0].ID != "a3f2" {
		t.Fatalf("approvals %+v, want the queue as the daemon sent it", got.Approvals)
	}

	// MarshalIndent lays the arguments out; they are the same JSON.
	var args bytes.Buffer

	if err := json.Compact(&args, got.Approvals[0].Arguments); err != nil || args.String() != `{"channel":"C01","text":"Deploy done, cutting v1.2"}` {
		t.Errorf("arguments %s (%v), want the call's as sent", got.Approvals[0].Arguments, err)
	}
}

// a3f2Call is how approve shows queued's first entry before it approves
// it.
const a3f2Call = `Client:  grok-bot
Tool:    slack.post_message
Class:   write
State:   pending
Arguments (52 B): {"channel":"C01","text":"Deploy done, cutting v1.2"}
`

// approved is approve's line once a3f2 is approved.
const approved = "Approved a3f2: the client's next call with the same tool and arguments runs, once.\n"

// TestApprovalsApproveShowsTheCallFirst: approve prints the call the id
// names (an agent can ask the owner to run the command, so the owner must
// see what it approves), and with --yes approves it without asking.
func TestApprovalsApproveShowsTheCallFirst(t *testing.T) {
	f := &fakeApprovals{rows: queued(time.Now())}
	host := approvalsHost(t, f)
	stdout, stderr, code := run(t, host, "approvals", "approve", "a3f2", "--yes")

	if code != 0 || stdout != a3f2Call+approved || strings.Contains(stderr, "[y/N]") {
		t.Errorf("exit %d, stdout %q, stderr %q; want the call, then the approval, and no question", code, stdout, stderr)
	}

	if diff := cmp.Diff([]string{"approve a3f2 as shown"}, f.answered()); diff != "" {
		t.Errorf("answers (-want +got):\n%s", diff)
	}
}

// TestApprovalsApproveOffATerminalNeedsYes: without a terminal to ask on,
// approve shows the call and approves nothing without --yes.
func TestApprovalsApproveOffATerminalNeedsYes(t *testing.T) {
	f := &fakeApprovals{rows: queued(time.Now())}
	host := approvalsHost(t, f)
	host.Stdin = strings.NewReader("y\n")
	stdout, stderr, code := run(t, host, "approvals", "approve", "a3f2")

	if want := "Error: not a terminal, so nothing was asked or changed: pass --yes to approve it once you have reviewed it with `wayfindr approvals`\n"; code == 0 || stdout != a3f2Call || stderr != want {
		t.Errorf("exit %d, stdout %q, stderr %q; want the call, and a failure saying %q", code, stdout, stderr, want)
	}

	if len(f.answered()) != 0 {
		t.Errorf("answers %v, want none", f.answered())
	}
}

// a3f2Question is approve's question for queued's first entry: it names
// the tool and the client again, since long arguments can scroll the call
// out of view.
const a3f2Question = "Approve slack.post_message for grok-bot? [y/N] "

// prompted is stderr for a command that asks on a terminal: it records what
// is written, and closes asked once the question's [y/N] is written, so a
// test types its answer only after the question, as the owner does.
type prompted struct {
	mu    sync.Mutex
	b     bytes.Buffer
	asked chan struct{}
	once  sync.Once
}

func newPrompted() *prompted { return &prompted{asked: make(chan struct{})} }

func (p *prompted) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, err := p.b.Write(b)

	if strings.Contains(p.b.String(), "[y/N] ") {
		p.once.Do(func() { close(p.asked) })
	}

	return n, err
}

func (p *prompted) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.b.String()
}

// approveOnTerminal runs `approvals approve a3f2` on a pseudo-terminal:
// ahead is typed before the command starts, answer once it asks.
func approveOnTerminal(t *testing.T, f *fakeApprovals, ahead, answer string) (stdout, stderr string, code int) {
	t.Helper()
	keys, tty := openTerminal(t)
	host := approvalsHost(t, f)
	host.Stdin = tty

	if _, err := keys.WriteString(ahead); err != nil {
		t.Fatalf("type ahead %q: %v", ahead, err)
	}

	var out bytes.Buffer
	errOut := newPrompted()
	done := make(chan int)

	go func() { done <- RunWith([]string{"approvals", "approve", "a3f2"}, &out, errOut, host) }()

	select {
	case <-errOut.asked:
	case code := <-done:
		t.Fatalf("exit %d before asking; stdout %q, stderr %q", code, out.String(), errOut.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("no question after 10s; stderr %q", errOut.String())
	}

	if _, err := keys.WriteString(answer); err != nil {
		t.Fatalf("type %q: %v", answer, err)
	}

	select {
	case code = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("no exit 10s after the answer %q; stderr %q", answer, errOut.String())
	}

	return out.String(), errOut.String(), code
}

// TestApprovalsApproveAsksOnATerminal: on a terminal, approve shows the
// call and asks, naming the tool and client again; only a yes approves it,
// and the default is no.
func TestApprovalsApproveAsksOnATerminal(t *testing.T) {
	tests := []struct {
		name, typed string
		yes         bool
	}{
		{"yes", "y\n", true},
		{"yes in full", "Yes\n", true},
		{"no", "n\n", false},
		{"the default", "\n", false},
		{"anything else", "approve\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeApprovals{rows: queued(time.Now())}
			stdout, stderr, code := approveOnTerminal(t, f, "", tt.typed)
			wantOut, wantErr, wantAnswers := a3f2Call, a3f2Question+"Nothing changed.\n", []string(nil)

			if tt.yes {
				wantOut, wantErr, wantAnswers = a3f2Call+approved, a3f2Question, []string{"approve a3f2 as shown"}
			}

			if code != 0 || stdout != wantOut || stderr != wantErr {
				t.Errorf("exit %d, stdout %q, stderr %q; want stdout %q, stderr %q", code, stdout, stderr, wantOut, wantErr)
			}

			if diff := cmp.Diff(wantAnswers, f.answered()); diff != "" {
				t.Errorf("answers (-want +got):\n%s", diff)
			}
		})
	}
}

// TestApprovalsApproveDiscardsTypedAhead: what was typed or pasted before
// approve asks, such as a "y" an agent put after the command for the owner
// to paste, is discarded; only what is typed after the question answers it.
func TestApprovalsApproveDiscardsTypedAhead(t *testing.T) {
	t.Run("a yes typed ahead approves nothing", func(t *testing.T) {
		f := &fakeApprovals{rows: queued(time.Now())}
		stdout, stderr, code := approveOnTerminal(t, f, "y\ny\n", "\n")

		if code != 0 || stdout != a3f2Call || stderr != a3f2Question+"Nothing changed.\n" {
			t.Errorf("exit %d, stdout %q, stderr %q; want the call, the question, and nothing changed", code, stdout, stderr)
		}

		if len(f.answered()) != 0 {
			t.Errorf("answers %v, want none", f.answered())
		}
	})

	t.Run("a no typed ahead doesn't stop a yes after the question", func(t *testing.T) {
		f := &fakeApprovals{rows: queued(time.Now())}
		_, _, code := approveOnTerminal(t, f, "n\n", "y\n")

		if diff := cmp.Diff([]string{"approve a3f2 as shown"}, f.answered()); code != 0 || diff != "" {
			t.Errorf("exit %d, answers (-want +got):\n%s", code, diff)
		}
	})
}

// TestApprovalsApproveShowsTheCallMadeVisible: the call approve shows is
// the agent's text, so it goes through redact.Visible as the listing's
// does, and the arguments are shown as they decode, which is what the
// approval binds.
func TestApprovalsApproveShowsTheCallMadeVisible(t *testing.T) {
	now := time.Now()
	f := &fakeApprovals{rows: []admin.ApprovalRow{{
		ID: "c5b2", Client: "grok\u202ebot", Tool: "shell.\x1b[31mrun\u200b", Class: "destructive", State: admin.ApprovalPending,
		Created: now, Expires: now.Add(time.Hour), Arguments: json.RawMessage(`{"cmd":"\u0072\u006d -rf ~","note":"a\u200bb"}`),
	}}}
	host := approvalsHost(t, f)
	stdout, stderr, code := run(t, host, "approvals", "approve", "c5b2", "--yes")

	want := `Client:  grok\u202Ebot
Tool:    shell.\u001B[31mrun\u200B
Class:   destructive
State:   pending
Arguments (46 B): {"cmd":"rm -rf ~","note":"a\u200Bb"}
Approved c5b2: the client's next call with the same tool and arguments runs, once.
`

	if code != 0 || stdout != want {
		t.Errorf("exit %d, stderr %q, stdout\n%s\nwant\n%s", code, stderr, stdout, want)
	}
}

// TestApprovalsApproveRefusesAnotherCallUnderTheID: approve names the
// created time of the entry it showed, so when the id names another call
// by the time it approves (the daemon restarted, emptying the queue and
// its ids), the daemon's 409 is the error and nothing is approved.
func TestApprovalsApproveRefusesAnotherCallUnderTheID(t *testing.T) {
	now := time.Now()
	f := &fakeApprovals{listed: queued(now), rows: queued(now)}
	f.rows[0].Created = f.rows[0].Created.Add(time.Hour)
	host := approvalsHost(t, f)
	stdout, stderr, code := run(t, host, "approvals", "approve", "a3f2", "--yes")

	if want := `Error: approval "a3f2" is not the call you were shown; list approvals again` + "\n"; code == 0 || stdout != a3f2Call || stderr != want {
		t.Errorf("exit %d, stdout %q, stderr %q; want the call shown, then %q", code, stdout, stderr, want)
	}

	if len(f.answered()) != 0 {
		t.Errorf("answers %v, want none", f.answered())
	}
}

// TestApprovalsDenyAsksNothing: a denial lets nothing run, so deny neither
// shows the call nor asks, and names no created time.
func TestApprovalsDenyAsksNothing(t *testing.T) {
	f := &fakeApprovals{rows: queued(time.Now())}
	host := approvalsHost(t, f)
	stdout, stderr, code := run(t, host, "approvals", "deny", "a3f2")

	if want := "Denied a3f2: matching calls are refused until it expires.\n"; code != 0 || stdout != want {
		t.Errorf("exit %d, stdout %q, stderr %q; want one confirming line %q", code, stdout, stderr, want)
	}

	if diff := cmp.Diff([]string{"deny a3f2"}, f.answered()); diff != "" {
		t.Errorf("answers (-want +got):\n%s", diff)
	}
}

// TestApprovalsAnswerOfAnUnknownIDFails: an id nothing waits on is the
// daemon's 404 message, a non-zero exit, and nothing printed to stdout.
func TestApprovalsAnswerOfAnUnknownIDFails(t *testing.T) {
	for _, verb := range []string{"approve", "deny"} {
		t.Run(verb, func(t *testing.T) {
			f := &fakeApprovals{rows: queued(time.Now())}
			host := approvalsHost(t, f)
			stdout, stderr, code := run(t, host, "approvals", verb, "zzzz")

			if code == 0 || stdout != "" || !strings.Contains(stderr, `no approval "zzzz" is waiting: it expired, was spent, or never existed`) {
				t.Errorf("exit %d, stdout %q, stderr %q; want the daemon's 404 message and a failure", code, stdout, stderr)
			}

			if len(f.answered()) != 0 {
				t.Errorf("answers %v, want none", f.answered())
			}
		})
	}
}

func TestApprovalsAnswerNeedsOneID(t *testing.T) {
	host := approvalsHost(t, &fakeApprovals{})

	for _, args := range [][]string{{"approvals", "approve"}, {"approvals", "deny", "a", "b"}, {"approvals", "nope"}} {
		if _, _, code := run(t, host, args...); code == 0 {
			t.Errorf("%v: exit 0, want a usage failure", args)
		}
	}
}

// approvalSpan is the AGE and EXPIRES wording, a decision table tested
// directly (CODING_STANDARDS.md, Tests); TestApprovalsTableMatchesTheLayout
// covers it through the command.
func TestApprovalSpan(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-time.Minute, "<1m"},
		{59 * time.Second, "<1m"},
		{time.Minute, "1m"},
		{59*time.Minute + 59*time.Second, "59m"},
		{time.Hour, "1h"},
		{time.Hour + 5*time.Minute, "1h5m"},
		{24 * time.Hour, "24h"},
	}

	for _, tt := range tests {
		if got := approvalSpan(tt.d); got != tt.want {
			t.Errorf("approvalSpan(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// TestStatusCountsPendingApprovals: "N approvals waiting" appears beside the
// warnings when entries are pending, counts only the pending ones, and is
// absent for none, for granted and denied entries alone, and from --json.
func TestStatusCountsPendingApprovals(t *testing.T) {
	now := time.Now()
	row := func(id string, state admin.ApprovalState) admin.ApprovalRow {
		return admin.ApprovalRow{ID: id, Client: "grok-bot", Tool: "slack.post_message", Class: "write", State: state, Created: now, Expires: now.Add(time.Hour)}
	}

	tests := []struct {
		name string
		rows []admin.ApprovalRow
		want string
	}{
		{"none", nil, ""},
		{"only answered ones", []admin.ApprovalRow{row("a", "granted"), row("b", "denied")}, ""},
		{"one", []admin.ApprovalRow{row("a", "pending"), row("b", "granted")}, "\n1 approval waiting. Review it with `wayfindr approvals`.\n"},
		{"several", []admin.ApprovalRow{row("a", "pending"), row("b", "pending"), row("c", "denied"), row("d", "pending")},
			"\n3 approvals waiting. Review them with `wayfindr approvals`.\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, root := testHost(t)
			writeStatusHost(t, root, adminDaemonOver(t, &fakeUpstreams{}, &fakeStore{}, admin.ConfigState{},
				func(o *admin.Options) { o.Approvals = &fakeApprovals{rows: tt.rows} }))

			stdout, stderr, code := run(t, host, "status")

			if code != 0 {
				t.Fatalf("exit code = %d; stderr: %s", code, stderr)
			}

			if want := "No connectors are configured.\n" + tt.want; stdout != want {
				t.Errorf("stdout %q, want %q", stdout, want)
			}

			stdout, _, _ = run(t, host, "status", "--json")

			if strings.Contains(stdout, "waiting") {
				t.Errorf("--json output holds the count: %s", stdout)
			}
		})
	}
}

// TestStatusWithoutApprovalsStillPrints: a daemon that can't list approvals
// (the route answers 501) doesn't fail `status`; the failure is logged at
// debug, and only there.
func TestStatusWithoutApprovalsStillPrints(t *testing.T) {
	host, root := testHost(t)
	writeStatusHost(t, root, adminDaemonOver(t, &fakeUpstreams{}, &fakeStore{}, admin.ConfigState{}))

	stdout, stderr, code := run(t, host, "status")

	if code != 0 || stdout != "No connectors are configured.\n" || strings.Contains(stderr, "count waiting approvals") {
		t.Errorf("exit %d, stdout %q, stderr %q; want the plain status and no log", code, stdout, stderr)
	}

	stdout, stderr, code = run(t, host, "--log-level", "debug", "status")

	if code != 0 || stdout != "No connectors are configured.\n" || !strings.Contains(stderr, `"message":"count waiting approvals"`) {
		t.Errorf("exit %d, stdout %q, stderr %q; want the plain status and the failure at debug", code, stdout, stderr)
	}
}
