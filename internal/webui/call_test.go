package webui_test

import (
	"errors"
	"flag"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

// linearList is the listing the tool-page tests serve: one tool with an
// argument of each kind, a read tool, and a hidden one.
func linearList() *admin.ToolList {
	return &admin.ToolList{Connector: "linear", Tools: []admin.ToolEntry{
		{Name: "create_issue", Class: "write", Exposed: true, RequiredArgs: 2, Definition: []byte(`{"name":"create_issue",` +
			`"description":"Create a <b>Linear</b> issue.","inputSchema":{"type":"object","properties":{` +
			`"team":{"type":"string","description":"The team's key."},"title":{"type":"string"},` +
			`"priority":{"type":"integer"},"state":{"enum":["todo","done"]},"urgent":{"type":"boolean"},` +
			`"labels":{"type":"array","description":"Label names."},"notify":{"type":"boolean"}},"required":["team","title","notify"]}}`)},
		{Name: "delete_issue", Class: "destructive", Exposed: false},
	}}
}

// toolConsole is a signed-in console whose daemon lists linearList and
// answers calls with answers.
func toolConsole(t *testing.T, answers ...string) (*console, *http.Cookie, string) {
	t.Helper()
	c := newConsole(t)
	c.admin.toolList = linearList()
	c.admin.callAnswers = answers
	ck := c.signIn(t)
	rec := c.get("/ui/connectors/linear/tools/create_issue", ck)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET the tool page = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	return c, ck, csrfOf(t, rec.Body.String())
}

// content is a page's <main>, with the session's CSRF token replaced, so
// the golden file holds what the page's template lays out.
func content(t *testing.T, body, csrf string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, "<main")

	if !ok {
		t.Fatalf("the page has no <main>: %s", body)
	}

	main, _, _ := strings.Cut(rest, "</main>")

	return strings.ReplaceAll("<main"+main+"</main>\n", csrf, "CSRF")
}

// golden compares got with testdata/name, or writes it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)

	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	want, err := os.ReadFile(path) //nolint:gosec // G304: a golden file under testdata

	if err != nil {
		t.Fatalf("read %s (run go test -update to write it): %v", path, err)
	}

	if diff := cmp.Diff(string(want), got); diff != "" {
		t.Errorf("%s (-want +got):\n%s", path, diff)
	}
}

func TestToolPageRendersTheForm(t *testing.T) {
	c, ck, csrf := toolConsole(t)
	rec := c.get("/ui/connectors/linear/tools/create_issue", ck)
	golden(t, "tool_form.html", content(t, rec.Body.String(), csrf))
}

// TestToolPageMakesARequiredBooleanAChoice: a required boolean's select
// starts blank and is required, so the browser sends nothing until the
// user picks; it never preselects true.
func TestToolPageMakesARequiredBooleanAChoice(t *testing.T) {
	c, ck, _ := toolConsole(t)
	body := c.get("/ui/connectors/linear/tools/create_issue", ck).Body.String()
	want := `<select id="arg-6" name="arg.notify" required>
    <option value="">—</option><option value="true">true</option><option value="false">false</option>`

	if !strings.Contains(body, want) {
		t.Errorf("the page lacks the required boolean's blank-first select:\n%s", want)
	}
}

// TestToolPageStillCallsWhenTheListingFails: a listing the daemon refused,
// as for a connector whose sign-in broke, still gets a form, with the class
// unknown and the arguments as one JSON object, run as a destructive tool;
// the call goes out, so the gateway's error and the call log show why.
func TestToolPageStillCallsWhenTheListingFails(t *testing.T) {
	c := newConsole(t)
	c.admin.toolsErr = &admin.APIError{Status: http.StatusServiceUnavailable, Message: "linear needs you to sign in again."}
	c.admin.callAnswers = []string{`{"content":[{"type":"text","text":"linear needs you to sign in again."}],"isError":true}`}
	ck := c.signIn(t)
	rec := c.get("/ui/connectors/linear/tools/create_issue", ck)
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200 with the form", rec.Code)
	}

	for _, s := range []string{"tools could not be listed", "class unknown", `name="arguments_json"`, "btn btn-destructive", "Run — class unknown"} {
		if !strings.Contains(body, s) {
			t.Errorf("the page lacks %q", s)
		}
	}

	csrf := csrfOf(t, body)

	if rec := c.post("/ui/connectors/linear/tools/create_issue", ck, url.Values{"csrf": {csrf}, "arguments_json": {"[1]"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("arguments that aren't an object = %d, want 400", rec.Code)
	}

	rec = c.post("/ui/connectors/linear/tools/create_issue", ck, url.Values{"csrf": {csrf}, "arguments_json": {`{"team":"core"}`}})

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "needs you to sign in again") {
		t.Errorf("POST = %d, want 200 with the gateway's error", rec.Code)
	}

	want := []admin.CallRequest{{Name: "linear.create_issue", Client: admin.CallClientConsole, Arguments: []byte(`{"team":"core"}`)}}

	if diff := cmp.Diff(want, c.admin.calledWith()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
}

// TestConnectorPageLinksExposedToolsOnly: a hidden tool has no form, since
// agents can't call it, and its page is a 404 that calls nothing.
func TestConnectorPageLinksExposedToolsOnly(t *testing.T) {
	c, ck, csrf := toolConsole(t)
	c.admin.status = admin.Status{Version: "v1.2.3", Connectors: []admin.Connector{{Name: "linear"}}}
	body := c.get("/ui/connectors/linear", ck).Body.String()

	if !strings.Contains(body, `<a class="try" href="/ui/connectors/linear/tools/create_issue">Try it</a>`) {
		t.Error("the exposed tool has no link to its form")
	}

	if strings.Contains(body, "/tools/delete_issue") {
		t.Error("the hidden tool links to a form")
	}

	if rec := c.get("/ui/connectors/linear/tools/delete_issue", ck); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), "hidden by the connector") {
		t.Errorf("GET the hidden tool = %d, want 404 saying it is hidden", rec.Code)
	}

	if rec := c.post("/ui/connectors/linear/tools/delete_issue", ck, url.Values{"csrf": {csrf}}); rec.Code != http.StatusNotFound {
		t.Errorf("POST the hidden tool = %d, want 404", rec.Code)
	}

	if rec := c.get("/ui/connectors/linear/tools/nope", ck); rec.Code != http.StatusNotFound {
		t.Errorf("GET an unknown tool = %d, want 404", rec.Code)
	}

	if got := c.admin.calledWith(); len(got) != 0 {
		t.Errorf("a refused page called the tool: %+v", got)
	}
}

func TestToolFormNeedsItsCSRFToken(t *testing.T) {
	c, ck, _ := toolConsole(t, `{"content":[]}`)

	if rec := c.post("/ui/connectors/linear/tools/create_issue", ck, url.Values{"arg.team": {"core"}}); rec.Code != http.StatusForbidden {
		t.Errorf("POST without the csrf token = %d, want 403", rec.Code)
	}

	if got := c.admin.calledWith(); len(got) != 0 {
		t.Errorf("the tool was called: %+v", got)
	}
}

func TestToolFormRunsTheCallAndShowsTheResult(t *testing.T) {
	result := `{"content":[{"type":"text","text":"Issue <ENG-481> created."},{"type":"image","data":"AAECAw==","mimeType":"image/png"}],` +
		`"structuredContent":{"id":"ENG-481"}}`
	c, ck, csrf := toolConsole(t, result)
	rec := c.post("/ui/connectors/linear/tools/create_issue", ck, url.Values{
		"csrf": {csrf}, "arg.team": {"core"}, "arg.title": {"Test"}, "arg.priority": {"2"}, "arg.state": {`"done"`},
		"arg.urgent": {""}, "arg.labels": {`["bug"]`},
	})
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200: %s", rec.Code, body)
	}

	want := []admin.CallRequest{{
		Name: "linear.create_issue", Client: admin.CallClientConsole,
		Arguments: []byte(`{"labels":["bug"],"priority":2,"state":"done","team":"core","title":"Test"}`),
	}}

	if diff := cmp.Diff(want, c.admin.calledWith()); diff != "" {
		t.Errorf("call (-want +got):\n%s", diff)
	}

	for _, s := range []string{"Issue &lt;ENG-481&gt; created.", "image, 4 B", "1.4 s", "&#34;id&#34;: &#34;ENG-481&#34;",
		`value="core"`, `<option value="&#34;done&#34;" selected>done</option>`} {
		if !strings.Contains(body, s) {
			t.Errorf("the page lacks %q", s)
		}
	}

	if strings.Contains(body, "data:") || strings.Contains(body, "<img src") {
		t.Error("the image is rendered, which the CSP forbids")
	}
}

func TestToolFormRefusesABadValueAndShowsTheDaemonsRefusal(t *testing.T) {
	c, ck, csrf := toolConsole(t, `{}`)
	rec := c.post("/ui/connectors/linear/tools/create_issue", ck, url.Values{"csrf": {csrf}, "arg.priority": {"high"}})

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Priority must be a whole number.") {
		t.Errorf("a bad value = %d, want 400 naming the field", rec.Code)
	}

	if got := c.admin.calledWith(); len(got) != 0 {
		t.Errorf("the tool was called: %+v", got)
	}

	c.admin.callErr = &admin.APIError{Status: http.StatusRequestEntityTooLarge, Message: "the call is too large: its body may be at most 4 MiB"}
	rec = c.post("/ui/connectors/linear/tools/create_issue", ck, url.Values{"csrf": {csrf}, "arg.team": {"core"}})

	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "at most 4 MiB") {
		t.Errorf("the daemon's refusal = %d, want 413 with its message", rec.Code)
	}

	c.admin.callErr = errors.New("boom")

	if rec := c.post("/ui/connectors/linear/tools/create_issue", ck, url.Values{"csrf": {csrf}}); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failure = %d, want 500", rec.Code)
	}
}

// TestToolFormAnswersAQuestion: an input_required result renders the
// upstream's form with the sealed state; posting it sends the state, the
// same arguments, and the answers.
func TestToolFormAnswersAQuestion(t *testing.T) {
	question := `{"resultType":"input_required","requestState":"sealed-1","inputRequests":{"linear-1":{` +
		`"method":"elicitation/create","params":{"mode":"form","message":"linear asks: Which cycle?",` +
		`"requestedSchema":{"type":"object","properties":{"cycle":{"type":"integer"},"note":{"type":"string"}},"required":["cycle"]}}},` +
		`"linear-2":{"method":"elicitation/create","params":{"mode":"url","message":"linear asks: Connect GitHub.",` +
		`"url":"https://linear.app/connect"}}}}`
	c, ck, csrf := toolConsole(t, question, `{"content":[{"type":"text","text":"done"}]}`)
	rec := c.post("/ui/connectors/linear/tools/create_issue", ck, url.Values{"csrf": {csrf}, "arg.team": {"core"}, "arg.title": {"T"}})
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200", rec.Code)
	}

	golden(t, "tool_question.html", questionForm(t, body, csrf))

	form := url.Values{
		"csrf": {csrf}, "request_state": {"sealed-1"}, "action": {"accept"},
		"arguments": {`{"team":"core","title":"T"}`}, "in.linear-1.cycle": {"7"}, "in.linear-1.note": {""},
		"questions": {hiddenValue(t, body, "questions")},
	}
	rec = c.post("/ui/connectors/linear/tools/create_issue", ck, form)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "done") {
		t.Fatalf("the answer = %d, want 200 with the result", rec.Code)
	}

	got := c.admin.calledWith()

	if len(got) != 2 {
		t.Fatalf("%d calls, want the call and its answer", len(got))
	}

	want := admin.CallRequest{
		Name: "linear.create_issue", Client: admin.CallClientConsole, Arguments: []byte(`{"team":"core","title":"T"}`),
		RequestState: "sealed-1",
		InputResponses: map[string]admin.InputResponse{
			"linear-1": {Action: "accept", Content: map[string]any{"cycle": float64(7)}},
			"linear-2": {Action: "accept"},
		},
	}

	if diff := cmp.Diff(want, got[1]); diff != "" {
		t.Errorf("answer (-want +got):\n%s", diff)
	}
}

// questionForm is the question's form in body, the first form on the page.
func questionForm(t *testing.T, body, csrf string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `<form class="card add-form" method="post">`)

	if !ok {
		t.Fatal("the page has no question form")
	}

	form, _, _ := strings.Cut(rest, "</form>")

	return strings.ReplaceAll(form, csrf, "CSRF") + "\n"
}

// hiddenValue is the unescaped value of the hidden input name in body.
func hiddenValue(t *testing.T, body, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `name="`+name+`" value="`)

	if !ok {
		t.Fatalf("the page has no %s field", name)
	}

	v, _, _ := strings.Cut(rest, `"`)
	r := strings.NewReplacer("&#34;", `"`, "&amp;", "&", "&lt;", "<", "&gt;", ">", "&#39;", "'", "&#43;", "+")

	return r.Replace(v)
}
