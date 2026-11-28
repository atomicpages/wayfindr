package webui_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// approvalFixtures is a queue with an entry in each state, at the fake
// clock's 2026-11-12 10:00 UTC. ap-waiting was created at
// 2026-11-12T09:55:00.000000123Z.
func approvalFixtures(now time.Time) []admin.ApprovalRow {
	return []admin.ApprovalRow{
		{ID: "ap-denied", Client: "muse", Tool: "github.create_issue", Class: "write", State: "denied", Arguments: []byte(`{"title":"spam"}`),
			Created: now.Add(-3 * time.Hour), Expires: now.Add(21 * time.Hour)},
		{ID: "ap-granted", Client: "grok", Tool: "linear.get_issue", Class: "read", State: "granted", Arguments: []byte(`{}`),
			Created: now.Add(-20 * time.Minute), Expires: now.Add(40 * time.Minute)},
		{ID: "ap-waiting", Client: "grok", Tool: "github.delete_repo", Class: "destructive", State: "pending", Arguments: []byte(`{"repo":"acme/<app>","force":true}`),
			Created: now.Add(-5*time.Minute + 123*time.Nanosecond), Expires: now.Add(24*time.Hour - 5*time.Minute)},
		{ID: "ap-older", Client: "chatgpt", Tool: "github.merge", Class: "write", State: "pending", Arguments: []byte(`{"pr":7}`),
			Created: now.Add(-2 * time.Hour), Expires: now.Add(22 * time.Hour)},
	}
}

func TestApprovalsPageGolden(t *testing.T) {
	c := newConsole(t)
	ck := c.signIn(t)

	t.Run("one entry in each state", func(t *testing.T) {
		c.admin.approvals = approvalFixtures(c.clock.Now())
		rec := c.get("/ui/approvals", ck)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d, want 200", rec.Code)
		}

		golden(t, "approvals.html", content(t, rec.Body.String(), csrfOf(t, rec.Body.String())))
	})

	t.Run("empty", func(t *testing.T) {
		c.admin.approvals = nil
		rec := c.get("/ui/approvals", ck)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d, want 200", rec.Code)
		}

		golden(t, "approvals_empty.html", content(t, rec.Body.String(), "no-csrf-in-an-empty-page"))
	})
}

// TestApprovalsPageShowsButtonsByState: a pending entry can be approved or
// denied, a granted one only revoked (a deny), a denied one only approved.
func TestApprovalsPageShowsButtonsByState(t *testing.T) {
	c := newConsole(t)
	c.admin.approvals = approvalFixtures(c.clock.Now())
	body := c.get("/ui/approvals", c.signIn(t)).Body.String()

	for id, want := range map[string][2]bool{
		"ap-waiting": {true, true},
		"ap-granted": {false, true},
		"ap-denied":  {true, false},
	} {
		for i, verb := range []string{"approve", "deny"} {
			if got := strings.Contains(body, `action="/ui/approvals/`+id+`/`+verb+`"`); got != want[i] {
				t.Errorf("%s has a %s form = %t, want %t", id, verb, got, want[i])
			}
		}
	}

	if !strings.Contains(body, ">Revoke</button>") {
		t.Error("a granted entry's deny button is not worded Revoke")
	}

	// The waiting entries come first, the oldest of them at the top.
	older, waiting, granted, denied := strings.Index(body, "ap-older"), strings.Index(body, "ap-waiting"), strings.Index(body, "ap-granted"), strings.Index(body, "ap-denied")

	if older >= waiting || waiting >= granted || granted >= denied {
		t.Errorf("rows are out of order: older %d, waiting %d, granted %d, denied %d", older, waiting, granted, denied)
	}
}

func TestApprovalsPageNeedsASession(t *testing.T) {
	c := newConsole(t)

	if rec := c.get("/ui/approvals", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("GET without a session = %d, want 303 to login", rec.Code)
	}
}

// waitingCreated is ap-waiting's created time as its forms carry it.
const waitingCreated = "2026-11-12T09:55:00.000000123Z"

// TestApprovalsAnswers: each answer form carries the created time of the
// entry it shows, to the nanosecond, and the answer passes it on, so the
// daemon applies it only to that entry.
func TestApprovalsAnswers(t *testing.T) {
	for _, tc := range []struct {
		verb, done, toast string
	}{
		{"approve", "approved", "Approved ap-waiting."},
		{"deny", "denied", "Denied ap-waiting."},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			c := newConsole(t)
			c.admin.approvals = approvalFixtures(c.clock.Now())
			ck := c.signIn(t)
			page := c.get("/ui/approvals", ck).Body.String()
			form := `<form method="post" action="/ui/approvals/ap-waiting/` + tc.verb + `">`
			i := strings.Index(page, form)

			if field := `<input type="hidden" name="created" value="` + waitingCreated + `">`; i < 0 || !strings.Contains(page[i:i+300], field) {
				t.Fatalf("the %s form of ap-waiting lacks %s", tc.verb, field)
			}

			rec := c.post("/ui/approvals/ap-waiting/"+tc.verb, ck, url.Values{"csrf": {csrfOf(t, page)}, "created": {waitingCreated}})
			want := "/ui/approvals?done=" + tc.done + "&name=ap-waiting"

			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
				t.Fatalf("POST = %d to %q, want 303 to %q", rec.Code, rec.Header().Get("Location"), want)
			}

			if diff := cmp.Diff([]string{tc.verb + " ap-waiting created " + waitingCreated}, c.admin.answered); diff != "" {
				t.Errorf("answered (-want +got):\n%s", diff)
			}

			if body := c.get(want, ck).Body.String(); !strings.Contains(body, tc.toast) {
				t.Errorf("the page after the answer lacks the toast %q", tc.toast)
			}
		})
	}
}

func TestApprovalsAnswerRefusals(t *testing.T) {
	c := newConsole(t)
	ck := c.signIn(t)
	csrf := csrfOf(t, c.get("/ui/tokens", ck).Body.String())

	t.Run("an unknown id shows the admin API's refusal", func(t *testing.T) {
		c.admin.answerErr = &admin.APIError{Status: http.StatusNotFound, Message: `no approval "nope" is waiting: it expired, was spent, or never existed`}
		rec := c.post("/ui/approvals/nope/approve", ck, url.Values{"csrf": {csrf}, "created": {waitingCreated}})

		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "it expired, was spent, or never existed") {
			t.Errorf("POST an unknown id = %d, want 404 with the daemon's message: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an entry that is not the one shown shows the admin API's refusal", func(t *testing.T) {
		c.admin.answerErr = &admin.APIError{Status: http.StatusConflict, Message: `approval "ap-1" is not the call you were shown; list approvals again`}
		rec := c.post("/ui/approvals/ap-1/approve", ck, url.Values{"csrf": {csrf}, "created": {waitingCreated}})

		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "is not the call you were shown") {
			t.Errorf("POST = %d, want 409 with the daemon's message: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a form without the entry's created time", func(t *testing.T) {
		c.admin.answerErr = nil
		before := len(c.admin.answered)

		for _, created := range [][]string{nil, {""}, {"yesterday"}} {
			form := url.Values{"csrf": {csrf}}

			if created != nil {
				form["created"] = created
			}

			if rec := c.post("/ui/approvals/ap-1/approve", ck, form); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Reload the approvals page") {
				t.Errorf("POST with created %v = %d, want 400 asking for a reload", created, rec.Code)
			}
		}

		if got := len(c.admin.answered); got != before {
			t.Errorf("%d answers reached the admin API without a created time", got-before)
		}
	})

	t.Run("a failure points to the log", func(t *testing.T) {
		c.admin.answerErr = errors.New("boom")
		rec := c.post("/ui/approvals/ap-1/deny", ck, url.Values{"csrf": {csrf}, "created": {waitingCreated}})

		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "boom") {
			t.Errorf("POST = %d, want 500 without the cause", rec.Code)
		}
	})

	t.Run("without the csrf token", func(t *testing.T) {
		c.admin.answerErr = nil
		before := len(c.admin.answered)

		for _, verb := range []string{"approve", "deny"} {
			if rec := c.post("/ui/approvals/ap-1/"+verb, ck, url.Values{}); rec.Code != http.StatusForbidden {
				t.Errorf("POST %s without csrf = %d, want 403", verb, rec.Code)
			}

			if rec := c.post("/ui/approvals/ap-1/"+verb, ck, url.Values{"csrf": {"wrong"}}); rec.Code != http.StatusForbidden {
				t.Errorf("POST %s with another csrf = %d, want 403", verb, rec.Code)
			}
		}

		if got := len(c.admin.answered); got != before {
			t.Errorf("%d answers reached the admin API despite the refusals", got-before)
		}
	})

	t.Run("a GET is not an answer", func(t *testing.T) {
		before := len(c.admin.answered)

		if rec := c.get("/ui/approvals/ap-1/approve", ck); rec.Code != http.StatusNotFound {
			t.Errorf("GET an answer path = %d, want 404", rec.Code)
		}

		if len(c.admin.answered) != before {
			t.Error("a GET answered an approval")
		}
	})
}

func TestApprovalsCountInTheSidebar(t *testing.T) {
	const (
		hidden = `<span id="nav-approvals" class="nav-count"></span>`
		shown  = `<span class="count attn" title="2 waiting for you">2</span>`
	)

	c := newConsole(t)
	ck := c.signIn(t)

	// Only the two pending entries count, not the granted or denied one.
	c.admin.approvals = approvalFixtures(c.clock.Now())

	for _, path := range []string{"/ui/", "/ui/calls", "/ui/tokens", "/ui/approvals", "/ui/nowhere"} {
		body := c.get(path, ck).Body.String()

		if !strings.Contains(body, `href="/ui/approvals"`) || !strings.Contains(body, shown) {
			t.Errorf("GET %s lacks the Approvals link with a count of 2", path)
		}
	}

	c.admin.approvals = approvalFixtures(c.clock.Now())[:2] // granted and denied only

	for _, path := range []string{"/ui/", "/ui/approvals"} {
		body := c.get(path, ck).Body.String()

		if !strings.Contains(body, hidden) || strings.Contains(body, `class="count attn"`) {
			t.Errorf("GET %s with nothing pending shows a count, want none", path)
		}

		if !strings.Contains(body, `href="/ui/approvals"`) {
			t.Errorf("GET %s lost the Approvals link", path)
		}
	}
}

// TestApprovalsFailureDoesNotBreakOtherPages: the count only decorates
// the sidebar, so a failing queue is logged and the page still renders.
func TestApprovalsFailureDoesNotBreakOtherPages(t *testing.T) {
	var logs bytes.Buffer

	c := newConsoleLog(t, &logs)
	c.admin.approvalsErr = errors.New("queue exploded")
	ck := c.signIn(t)

	for _, path := range []string{"/ui/", "/ui/calls", "/ui/tokens", "/ui/nowhere"} {
		rec := c.get(path, ck)
		want := http.StatusOK

		if path == "/ui/nowhere" {
			want = http.StatusNotFound
		}

		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}

		if !strings.Contains(rec.Body.String(), `<span id="nav-approvals" class="nav-count"></span>`) {
			t.Errorf("GET %s shows a count, or none of the sidebar", path)
		}
	}

	if !strings.Contains(logs.String(), "queue exploded") {
		t.Errorf("the failure was not logged: %q", logs.String())
	}

	// The approvals page itself has nothing to show without the queue.
	rec := c.get("/ui/approvals", ck)

	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "queue exploded") {
		t.Errorf("GET /ui/approvals = %d, want 500 without the cause", rec.Code)
	}
}

// TestApprovalsArgumentsAreShownInFullAndVisible: every row shows all of
// its arguments without a click, never cut, escaped as HTML; and a
// character the owner could not see, or that would reorder the text, is
// shown as an escape (redact.Visible), in the arguments and in the ID,
// client, and tool.
func TestApprovalsArgumentsAreShownInFullAndVisible(t *testing.T) {
	long := strings.Repeat("a", 200) + "TAIL"
	c := newConsole(t)
	now := c.clock.Now()
	row := func(id, client, tool, args string) admin.ApprovalRow {
		return admin.ApprovalRow{ID: id, Client: client, Tool: tool, State: admin.ApprovalPending, Arguments: []byte(args), Created: now, Expires: now.Add(time.Hour)}
	}
	c.admin.approvals = []admin.ApprovalRow{
		row("ap-long", "grok", "x.y", `{"text":"`+long+`"}`),
		// Valid JSON that holds raw characters a reader can't see: a
		// right-to-left override, a zero-width joiner, a byte order mark, a
		// soft hyphen, a line separator, and "rm" in tag characters.
		row("ap-hidden", "grok", "x.y", "{\"name\":\"gpj.\u202eexe a\u200db \ufeffk de\u00adl x\u2028y \U000E0072\U000E006D\"}"),
		// Not valid JSON: a raw escape sequence, bell, and markup.
		row("ap-ctrl", "grok", "x.y", "{\"note\":\"\x1b[2Jred\x07<script>alert(1)</script>\"}"),
		// The cells are the agent's text too.
		row("ap-cells\u200b", "gr\u202eok", "x.\x1b[31my\u2060", `{}`),
		// A run of spaces would push the rest out of view; the layout's own
		// indentation stays.
		row("ap-spaces", "grok     bot", "x.y", `{"a":{"t":"ok      tail"}}`),
	}
	c.admin.approvals[3].Class = "write\u202e"
	body := c.get("/ui/approvals", c.signIn(t)).Body.String()

	if got := strings.Count(body, long); got != 1 {
		t.Errorf("the long argument appears %d times, want once, whole", got)
	}

	if strings.Contains(body, "<details") || strings.Contains(body, "…") {
		t.Error("the arguments are behind a click or cut short")
	}

	// Not in a scroll box (.spec has a max-height), so none of them is out
	// of view.
	if strings.Contains(body, `class="why-text spec"`) || strings.Count(body, `<pre class="why-text args">`) != len(c.admin.approvals) {
		t.Error("the arguments are not each in a <pre> without a height limit")
	}

	for _, bad := range []string{"\x1b", "\x07", "\u202e", "\u200d", "\ufeff", "\u00ad", "\u2028", "\U000E0072", "\u200b", "\u2060", "<script>alert(1)"} {
		if strings.Contains(body, bad) {
			t.Errorf("the page holds %q unescaped", bad)
		}
	}

	for _, want := range []string{
		`&#34;gpj.\u202Eexe a\u200Db \uFEFFk de\u00ADl x\u2028y \U000E0072\U000E006D&#34;`,
		`\u001B[2Jred\u0007&lt;script&gt;alert(1)&lt;/script&gt;`,
		`ap-cells\u200B`, `gr\u202Eok`, `x.\u001B[31my\u2060`, `write\u202E`,
		`grok  \u0020\u0020\u0020bot`,
		"{\n  &#34;a&#34;: {\n    &#34;t&#34;: &#34;ok  \\u0020\\u0020\\u0020\\u0020tail&#34;\n  }\n}",
		// The size, so arguments larger than what shows stand out.
		`Arguments, 215 B</div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// TestApprovalsPollPausesWhileInUse: console.js holds the refresh of an
// element marked data-poll-pause while the pointer or focus is in it and
// for 5 seconds after a touch, click, scroll, or key, and drops the answer
// of a poll in flight then; the approvals table is that element, and the
// script served keys on the same attribute.
func TestApprovalsPollPausesWhileInUse(t *testing.T) {
	c := newConsole(t)
	c.admin.approvals = approvalFixtures(c.clock.Now())
	ck := c.signIn(t)
	body := c.get("/ui/approvals", ck).Body.String()

	if !strings.Contains(body, `<div id="approvals" class="stack" data-poll-pause hx-get="/ui/approvals" hx-trigger="every 10s"`) {
		t.Error("the polled approvals table is not marked data-poll-pause")
	}

	js := c.get("/ui/static/console.js", ck).Body.String()

	for _, want := range []string{
		`"htmx:beforeRequest"`, `"[data-poll-pause]"`, `":hover, :focus-within"`, "preventDefault()",
		`"htmx:beforeSwap"`, "e.detail.shouldSwap = false",
		`["touchstart", "pointerdown", "scroll", "keydown"]`, "Date.now() - lastUse < 5000",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("console.js lacks %s", want)
		}
	}
}

// TestApprovalsPageListsTheQueueOnce: the sidebar's count and the table
// come from one listing.
func TestApprovalsPageListsTheQueueOnce(t *testing.T) {
	c := newConsole(t)
	c.admin.approvals = approvalFixtures(c.clock.Now())
	ck := c.signIn(t)
	before := c.admin.listings
	body := c.get("/ui/approvals", ck).Body.String()

	if got := c.admin.listings - before; got != 1 {
		t.Errorf("the page listed the queue %d times, want 1", got)
	}

	if !strings.Contains(body, `<span class="count attn" title="2 waiting for you">2</span>`) {
		t.Error("the sidebar lacks the count of 2")
	}
}
