package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// linearTools is the listing the call tests' daemon serves for linear.
func linearTools() admin.ToolList {
	return admin.ToolList{Connector: "linear", Tools: []admin.ToolEntry{
		{Name: "create_issue", Class: "write", Exposed: true, RequiredArgs: 2, Definition: json.RawMessage(`{"name":"create_issue",` +
			`"inputSchema":{"type":"object","properties":{"team":{"type":"string","description":"The team's key."},` +
			`"title":{"type":"string"},"labels":{"type":"array","description":"Label names."}},"required":["team","title"]}}`)},
		{Name: "delete_issue", Class: "destructive", Exposed: false},
		{Name: "search_issues", Class: "read", Exposed: true, RequiredArgs: 1},
	}}
}

// callDaemon records the call requests it gets and answers each with the
// next of answers (the last one repeats).
type callDaemon struct {
	mu       sync.Mutex
	requests []admin.CallRequest
	answers  []string
	// toolsErr, when set, is how the tool listing fails.
	toolsErr error
}

func (d *callDaemon) call(_ context.Context, req admin.CallRequest) (admin.CallOutcome, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.requests = append(d.requests, req)
	i := min(len(d.requests), len(d.answers)) - 1

	return admin.CallOutcome{Result: json.RawMessage(d.answers[i]), Class: "write"}, nil
}

func (d *callDaemon) got() []admin.CallRequest {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.requests
}

// newCallDaemon serves the admin API with linearTools and a callDaemon
// that answers with answers.
func newCallDaemon(t *testing.T, answers ...string) (Host, *callDaemon) {
	t.Helper()
	d := &callDaemon{answers: answers}
	host, root := testHost(t)
	writeStatusHost(t, root, serveAdminAPI(t, admin.Options{
		Upstreams: &fakeUpstreams{},
		Store:     &fakeStore{},
		UserID:    "local",
		Config:    func() admin.ConfigState { return admin.ConfigState{} },
		Tools: func(context.Context, string) (admin.ToolList, error) {
			d.mu.Lock()
			defer d.mu.Unlock()

			if d.toolsErr != nil {
				return admin.ToolList{}, d.toolsErr
			}

			return linearTools(), nil
		},
		Call:   d.call,
		Logger: zerolog.New(zerolog.NewTestWriter(t)),
	}))
	host.Stdin = strings.NewReader("")

	return host, d
}

const okResult = `{"content":[{"type":"text","text":"Issue ENG-481 created."},{"type":"image","data":"AAECAw==","mimeType":"image/png"}]}`

// TestCallScaffoldPrintsOnlyJSON: --scaffold's stdout is the JSON object of
// the required arguments and nothing else, and stderr stays empty, so the
// two never interleave on a terminal.
func TestCallScaffoldPrintsOnlyJSON(t *testing.T) {
	host, d := newCallDaemon(t, okResult)
	stdout, stderr, code := run(t, host, "call", "linear.create_issue", "--scaffold")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if want := "{\"team\": \"\", \"title\": \"\"}\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}

	if len(d.got()) != 0 {
		t.Errorf("--scaffold called the tool: %+v", d.got())
	}
}

// TestScaffoldOverSchemaShapes: the scaffold's decision table, a pure
// function of the schema (each type's zero value, the first enum value,
// only required properties, in the schema's order).
func TestScaffoldOverSchemaShapes(t *testing.T) {
	tests := []struct {
		name, schema, want string
	}{
		{"none", ``, `{}`},
		{"no properties", `{"type":"object"}`, `{}`},
		{"flat types", `{"properties":{"s":{"type":"string"},"n":{"type":"number"},"i":{"type":"integer"},"b":{"type":"boolean"}},` +
			`"required":["s","n","i","b"]}`, `{"s": "", "n": 0, "i": 0, "b": false}`},
		{"nested", `{"properties":{"a":{"type":"array"},"o":{"type":"object","properties":{"x":{"type":"string"}}}},"required":["o","a"]}`,
			`{"a": [], "o": {}}`},
		{"enum", `{"properties":{"p":{"type":"string","enum":["low","high"]}},"required":["p"]}`, `{"p": "low"}`},
		{"optional left out", `{"properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["b"]}`, `{"b": ""}`},
		{"nullable type", `{"properties":{"a":{"type":["null","integer"]}},"required":["a"]}`, `{"a": 0}`},
		{"no type", `{"properties":{"a":{}},"required":["a"]}`, `{"a": null}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields, err := admin.SchemaFields(json.RawMessage(tt.schema))

			if err != nil {
				t.Fatalf("schemaFields: %v", err)
			}

			got := scaffold(fields)

			if got != tt.want {
				t.Errorf("scaffold = %s, want %s", got, tt.want)
			}

			if !json.Valid([]byte(got)) {
				t.Errorf("scaffold %s is not JSON", got)
			}
		})
	}
}

func TestCallRunsAReadToolAtOnceAndPrintsThePreview(t *testing.T) {
	host, d := newCallDaemon(t, okResult)
	stdout, stderr, code := run(t, host, "call", "linear.search_issues", "--args", `{"query":"login"}`)

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if !strings.HasPrefix(stdout, "ok · ") || !strings.Contains(stdout, "Issue ENG-481 created.\nimage, 4 B\n") {
		t.Errorf("stdout = %q, want the status line, the text, and the image as one line", stdout)
	}

	want := []admin.CallRequest{{Name: "linear.search_issues", Arguments: json.RawMessage(`{"query":"login"}`), Client: admin.CallClientCLI}}

	if diff := cmp.Diff(want, d.got()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

// TestCallConfirmsWriteTools: a write tool off a terminal needs --yes, and
// fails before the call without it; with --yes it runs. A hidden tool is
// not asked about: the daemon refuses it.
func TestCallConfirmsWriteTools(t *testing.T) {
	refused := `{"content":[{"type":"text","text":"linear.delete_issue is hidden by the tools list."}],"isError":true}`

	t.Run("without --yes", func(t *testing.T) {
		host, d := newCallDaemon(t, okResult)
		_, stderr, code := run(t, host, "call", "linear.create_issue", "--args", `{"team":"core","title":"x"}`)

		if code == 0 || !strings.Contains(stderr, "pass --yes to run it") {
			t.Errorf("exit %d, stderr %q; want a failure asking for --yes", code, stderr)
		}

		if len(d.got()) != 0 {
			t.Errorf("the tool was called: %+v", d.got())
		}
	})

	t.Run("with --yes", func(t *testing.T) {
		host, d := newCallDaemon(t, okResult)
		_, stderr, code := run(t, host, "call", "linear.create_issue", "--args", `{"team":"core","title":"x"}`, "--yes")

		if code != 0 || len(d.got()) != 1 {
			t.Errorf("exit %d, %d calls; want the call made. stderr: %s", code, len(d.got()), stderr)
		}
	})

	t.Run("hidden tool", func(t *testing.T) {
		host, d := newCallDaemon(t, refused)
		stdout, _, code := run(t, host, "call", "linear.delete_issue")

		if code != 1 || len(d.got()) != 1 || !strings.Contains(stdout, "hidden by the tools list") {
			t.Errorf("exit %d, %d calls, stdout %q; want the daemon's refusal and exit 1", code, len(d.got()), stdout)
		}
	})
}

func TestCallRefusesWhatItCannotSend(t *testing.T) {
	host, d := newCallDaemon(t, okResult)

	tests := []struct {
		args []string
		want string
	}{
		{[]string{"call", "create_issue"}, "<connector>.<tool>"},
		{[]string{"call", "linear.nope"}, "linear serves no tool called nope"},
		{[]string{"call", "linear.search_issues", "--args", `[1]`}, "not a JSON object"},
		{[]string{"call", "linear.search_issues", "--args-file", "-", "--token-file", "-"}, "can't both read stdin"},
	}

	for _, tt := range tests {
		if _, stderr, code := run(t, host, tt.args...); code == 0 || !strings.Contains(stderr, tt.want) {
			t.Errorf("%v: exit %d, stderr %q; want %q", tt.args, code, stderr, tt.want)
		}
	}

	if len(d.got()) != 0 {
		t.Errorf("a refused command called the tool: %+v", d.got())
	}
}

// TestCallSendsTheTokenFromItsFile: the token is read from the file,
// trimmed, and sent in the body, never on the command line.
func TestCallSendsTheTokenFromItsFile(t *testing.T) {
	host, d := newCallDaemon(t, okResult)
	file := filepath.Join(t.TempDir(), "nightly.token")

	if err := os.WriteFile(file, []byte("wf_s_nightly\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, stderr, code := run(t, host, "call", "linear.search_issues", "--token-file", file); code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if got := d.got(); len(got) != 1 || got[0].Token != "wf_s_nightly" {
		t.Errorf("requests %+v, want one carrying the trimmed token", got)
	}
}

// TestCallCutsTheTextAt4KiBAndWritesTheWholeResult: the preview stops at 4
// KiB with a note, --output holds the whole result, and the text is made
// printable.
func TestCallCutsTheTextAt4KiBAndWritesTheWholeResult(t *testing.T) {
	long := strings.Repeat("a", 5000)
	res, _ := json.Marshal(map[string]any{"content": []map[string]any{{"type": "text", "text": "red\x1b[31m\n" + long}}})
	host, _ := newCallDaemon(t, string(res))
	out := filepath.Join(t.TempDir(), "result.json")
	stdout, stderr, code := run(t, host, "call", "linear.search_issues", "--output", out)

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if strings.Contains(stdout, "\x1b") || !strings.Contains(stdout, "red�[31m\n") {
		t.Errorf("stdout carries the escape sequence: %q", stdout[:40])
	}

	if !strings.Contains(stdout, "… cut at 4 KiB of 4.9 KiB; the whole result is in "+out+".") || strings.Contains(stdout, long) {
		t.Errorf("stdout does not end with the cut note: %q", stdout[len(stdout)-120:])
	}

	b, err := os.ReadFile(out) //nolint:gosec // G304: the result file under the test's temp root

	if err != nil {
		t.Fatalf("read --output: %v", err)
	}

	var whole admin.ToolResult

	if err := json.Unmarshal(b, &whole); err != nil || len(whole.Content) != 1 || !strings.HasSuffix(whole.Content[0].Text, long) {
		t.Errorf("--output holds %d bytes (%v), want the whole result", len(b), err)
	}
}

const formQuestion = `{"content":null,"resultType":"input_required","requestState":"sealed-1","inputRequests":{` +
	`"linear-1":{"method":"elicitation/create","params":{"mode":"form","message":"linear asks: Which team?",` +
	`"requestedSchema":{"type":"object","properties":{"team":{"type":"string","enum":["core","web"]},` +
	`"count":{"type":"integer"},"urgent":{"type":"boolean"},"note":{"type":"string"}},"required":["team","count"]}}}}}`

// TestCallAnswersAFormOnStdin: each field is asked, a bad answer is asked
// again, an empty optional one is left out, and the answers go back with
// the sealed request state.
func TestCallAnswersAFormOnStdin(t *testing.T) {
	host, d := newCallDaemon(t, formQuestion, okResult)
	host.Stdin = strings.NewReader("y\nmobile\ncore\nthree\n3\nyes\n\n")
	stdout, stderr, code := run(t, host, "call", "linear.search_issues")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "linear asks: Which team?") || !strings.Contains(stderr, "want one of") ||
		!strings.Contains(stderr, "want a whole number") {
		t.Errorf("stderr = %q, want the question and the two re-asks", stderr)
	}

	if !strings.Contains(stdout, "Issue ENG-481 created.") {
		t.Errorf("stdout = %q, want the final result", stdout)
	}

	got := d.got()

	if len(got) != 2 {
		t.Fatalf("%d requests, want the call and its answer", len(got))
	}

	want := admin.CallRequest{
		Name: "linear.search_issues", Arguments: json.RawMessage(`{}`), Client: admin.CallClientCLI,
		RequestState: "sealed-1",
		InputResponses: map[string]admin.InputResponse{"linear-1": {Action: "accept",
			Content: map[string]any{"team": "core", "count": float64(3), "urgent": true}}},
	}

	if diff := cmp.Diff(want, got[1]); diff != "" {
		t.Errorf("answer (-want +got):\n%s", diff)
	}
}

func TestCallAnswersAURLRequestAfterEnter(t *testing.T) {
	question := `{"resultType":"input_required","requestState":"sealed-2","inputRequests":{"linear-1":{` +
		`"method":"elicitation/create","params":{"mode":"url","message":"linear asks: Connect your account.",` +
		`"url":"https://linear.app/connect?x=1","elicitationId":"e1"}}}}`
	host, d := newCallDaemon(t, question, okResult)
	host.Stdin = strings.NewReader("\n")

	if _, stderr, code := run(t, host, "call", "linear.search_issues"); code != 0 || !strings.Contains(stderr, "Open https://linear.app/connect?x=1") {
		t.Fatalf("exit %d, stderr %q; want the URL printed", code, stderr)
	}

	if got := d.got(); len(got) != 2 || got[1].InputResponses["linear-1"].Action != "accept" || got[1].RequestState != "sealed-2" {
		t.Errorf("requests %+v, want the answer accept with the state", got)
	}
}

// TestCallJSONLeavesTheQuestionToTheCaller: with --json an input_required
// result is printed and the command exits 0 without asking.
func TestCallJSONLeavesTheQuestionToTheCaller(t *testing.T) {
	host, d := newCallDaemon(t, formQuestion)
	stdout, stderr, code := run(t, host, "call", "linear.search_issues", "--json")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	var resp admin.CallResponse

	if err := json.Unmarshal([]byte(stdout), &resp); err != nil || resp.Class != "write" {
		t.Fatalf("stdout is not the response (%v): %s", err, stdout)
	}

	if res, err := resp.Decode(); err != nil || !res.InputRequired() || res.RequestState != "sealed-1" {
		t.Errorf("result %+v (%v), want the question", res, err)
	}

	if len(d.got()) != 1 {
		t.Errorf("%d requests, want 1", len(d.got()))
	}
}

func TestCallFailsWhenStdinEndsBeforeTheAnswers(t *testing.T) {
	host, _ := newCallDaemon(t, formQuestion)
	host.Stdin = strings.NewReader("y\ncore\n")

	if _, stderr, code := run(t, host, "call", "linear.search_issues"); code == 0 || !strings.Contains(stderr, "stdin ended") {
		t.Errorf("exit %d, stderr %q; want a failure saying stdin ended", code, stderr)
	}
}

// TestCallStillCallsWhenTheListingFails: a listing the daemon refuses, as
// for a connector whose credential is broken, doesn't stop the call, so the
// gateway's own error and the call log show why. The class is unknown, so
// the call is confirmed as a destructive one's.
func TestCallStillCallsWhenTheListingFails(t *testing.T) {
	refused := `{"content":[{"type":"text","text":"linear needs you to sign in again."}],"isError":true}`
	notReady := &admin.ToolsError{Kind: "unavailable", Advice: admin.Advice{Summary: "linear is not ready."}}

	t.Run("off a terminal without --yes", func(t *testing.T) {
		host, d := newCallDaemon(t, refused)
		d.toolsErr = notReady
		_, stderr, code := run(t, host, "call", "linear.search_issues")

		if code == 0 || !strings.Contains(stderr, "linear's tools could not be listed: linear is not ready.") ||
			!strings.Contains(stderr, "pass --yes to run it") {
			t.Errorf("exit %d, stderr %q; want the listing's error and a failure asking for --yes", code, stderr)
		}

		if len(d.got()) != 0 {
			t.Errorf("the tool was called: %+v", d.got())
		}
	})

	t.Run("with --yes", func(t *testing.T) {
		host, d := newCallDaemon(t, refused)
		d.toolsErr = notReady
		stdout, _, code := run(t, host, "call", "linear.search_issues", "--yes")

		if code != 1 || len(d.got()) != 1 || !strings.Contains(stdout, "needs you to sign in again") {
			t.Errorf("exit %d, %d calls, stdout %q; want the call made and the gateway's error", code, len(d.got()), stdout)
		}
	})

	t.Run("--scaffold needs the listing", func(t *testing.T) {
		host, d := newCallDaemon(t, refused)
		d.toolsErr = notReady

		if _, stderr, code := run(t, host, "call", "linear.search_issues", "--scaffold"); code == 0 || !strings.Contains(stderr, "linear is not ready.") {
			t.Errorf("exit %d, stderr %q; want the listing's error", code, stderr)
		}
	})
}

// TestParseAnswer is the form prompts' decision table: an answer is read by
// its field's type into a value JSON can hold.
func TestParseAnswer(t *testing.T) {
	tests := []struct {
		name  string
		field admin.SchemaField
		line  string
		want  any
	}{
		{"integer with leading zeros", admin.SchemaField{Type: "integer"}, "007", int64(7)},
		{"number", admin.SchemaField{Type: "number"}, "1.5", 1.5},
		{"NaN", admin.SchemaField{Type: "number"}, "NaN", nil},
		{"infinity", admin.SchemaField{Type: "number"}, "-Inf", nil},
		{"out of range", admin.SchemaField{Type: "number"}, "1e400", nil},
		{"boolean", admin.SchemaField{Type: "boolean"}, "yes", true},
		{"enum without quotes", admin.SchemaField{Enum: []json.RawMessage{[]byte(`"core"`)}}, "core", "core"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAnswer(tt.field, tt.line)

			if (err != nil) != (tt.want == nil) || got != tt.want {
				t.Errorf("parseAnswer(%q) = %v, %v; want %v", tt.line, got, err, tt.want)
			}

			if err == nil {
				if _, err := json.Marshal(got); err != nil {
					t.Errorf("the answer does not marshal: %v", err)
				}
			}
		})
	}
}

// failingReader fails every read with err.
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestReadLineWrapsAFailedRead(t *testing.T) {
	cause := errors.New("i/o broke")

	if _, err := readLine(bufio.NewReader(failingReader{cause})); !errors.Is(err, cause) {
		t.Errorf("readLine = %v, want it to wrap the read's error", err)
	}

	if _, err := readLine(bufio.NewReader(strings.NewReader(""))); !errors.Is(err, errNoAnswer) {
		t.Errorf("readLine at EOF = %v, want errNoAnswer", err)
	}
}
