package webui_test

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/upstream"
	"github.com/atomicpages/wayfindr/internal/webui"
)

const shownOnce = "wf_s_shown-once" //nolint:gosec // G101: a test fixture

// cloudConsole is a signed-in console with two connectors and its CSRF token.
func cloudConsole(t *testing.T, log *bytes.Buffer) (*console, *http.Cookie, string) {
	t.Helper()

	c := newConsoleLog(t, log)
	c.admin.status.Connectors = []admin.Connector{{Name: "github", State: upstream.StateReady}, {Name: "jira", State: upstream.StateReady}}
	ck := c.signIn(t)

	// Step 3 is the first with a POST form.
	return c, ck, csrfOf(t, c.get("/ui/cloud-agents?step=url&agent=grok&connector=github", ck).Body.String())
}

func wantAll(t *testing.T, what, body string, want ...string) {
	t.Helper()

	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("%s: missing %q", what, w)
		}
	}
}

// The progress list marks the current step with aria-current. The answer to
// the token form is both Test and Set up, so it marks both.
func TestCloudAgentsProgressAriaCurrent(t *testing.T) {
	c, ck, csrf := cloudConsole(t, &bytes.Buffer{})
	c.admin.publicURL = "https://wf.ngrok-free.app"

	t.Run("a step before the done stage marks one step", func(t *testing.T) {
		body := c.get("/ui/cloud-agents?step=url&agent=grok&connector=github", ck).Body.String()

		if got := strings.Count(body, `aria-current="step"`); got != 1 {
			t.Errorf("aria-current=\"step\" appears %d times, want 1", got)
		}

		wantAll(t, "step 3", body, `<li class="current" aria-current="step"><span class="n">3</span>Public URL`)
	})

	t.Run("the done stage marks Test and Set up, aria-current on Test only", func(t *testing.T) {
		body := c.post("/ui/cloud-agents/token", ck, url.Values{"csrf": {csrf}, "agent": {"grok"}, "name": {"grok-bot"}, "connector": {"github"}}).Body.String()

		if got := strings.Count(body, `aria-current="step"`); got != 1 {
			t.Errorf("aria-current=\"step\" appears %d times, want 1", got)
		}

		wantAll(t, "done", body,
			`<li class="done"><span class="n">4</span>Token`,
			`<li class="current" aria-current="step"><span class="n">5</span>Test`,
			`<li class="current"><span class="n">6</span>Set up`)
	})
}

func TestCloudAgentsSteps(t *testing.T) {
	c, ck, _ := cloudConsole(t, &bytes.Buffer{})

	t.Run("step 1 lists the agents, ChatGPT disabled", func(t *testing.T) {
		rec := c.get("/ui/cloud-agents", ck)
		body := rec.Body.String()

		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d, want 200", rec.Code)
		}

		wantAll(t, "step 1", body, "Step 1 of 6", "Grok Bot", "Muse", "Not yet: needs sign-in support",
			`value="grok" required`, `value="muse" required`, `value="chatgpt" disabled`,
			`href="/ui/cloud-agents"`, // the nav entry
			"No cloud agents yet.")

		if strings.Contains(body, `value="chatgpt" required`) {
			t.Error("ChatGPT can be picked")
		}
	})

	t.Run("a disabled or unknown agent starts over", func(t *testing.T) {
		for _, agent := range []string{"chatgpt", "nobody"} {
			body := c.get("/ui/cloud-agents?step=connectors&agent="+agent, ck).Body.String()

			if !strings.Contains(body, "Step 1 of 6") {
				t.Errorf("agent %q did not go back to step 1", agent)
			}
		}
	})

	t.Run("step 2 offers the configured connectors", func(t *testing.T) {
		body := c.get("/ui/cloud-agents?step=connectors&agent=grok&connector=jira", ck).Body.String()

		wantAll(t, "step 2", body, "Step 2 of 6", "Pick connectors for Grok Bot", `name="connector"`, `value="github">`, `value="jira" checked>`, `value="url"`)
	})

	t.Run("step 3 needs a connector", func(t *testing.T) {
		body := c.get("/ui/cloud-agents?step=url&agent=grok", ck).Body.String()

		wantAll(t, "no connector", body, "Step 2 of 6", "Pick at least one connector")
	})

	t.Run("step 3 prefills the public URL and hints at the tunnel", func(t *testing.T) {
		c.admin.publicURL = "https://wf.ngrok-free.app"
		body := c.get("/ui/cloud-agents?step=url&agent=muse&connector=github&connector=jira", ck).Body.String()

		wantAll(t, "step 3", body, "Step 3 of 6", `value="https://wf.ngrok-free.app"`, "ngrok, Cloudflare Tunnel", `action="/ui/cloud-agents/url"`,
			`name="connector" value="github"`, `name="connector" value="jira"`, `name="agent" value="muse"`)
	})

	t.Run("step 4 names the token after the agent", func(t *testing.T) {
		body := c.get("/ui/cloud-agents?step=token&agent=grok&connector=github", ck).Body.String()

		wantAll(t, "step 4", body, "Step 4 of 6", `value="grok-bot"`, `action="/ui/cloud-agents/token"`)

		muse := c.get("/ui/cloud-agents?step=token&agent=muse&connector=github", ck).Body.String()

		if !strings.Contains(muse, `value="muse"`) {
			t.Error("Muse's token is not named muse by default")
		}
	})

	t.Run("step 4 needs a public URL", func(t *testing.T) {
		c.admin.publicURL = ""
		body := c.get("/ui/cloud-agents?step=token&agent=grok&connector=github", ck).Body.String()

		wantAll(t, "no URL", body, "Step 3 of 6", "Set your public URL first.")
	})
}

func TestCloudAgentsPublicURL(t *testing.T) {
	c, ck, csrf := cloudConsole(t, &bytes.Buffer{})
	form := func(u string) url.Values {
		return url.Values{"csrf": {csrf}, "agent": {"grok"}, "connector": {"github", "jira"}, "url": {u}}
	}

	t.Run("saves and goes on to the token", func(t *testing.T) {
		rec := c.post("/ui/cloud-agents/url", ck, form(" https://wf.ngrok-free.app "))
		want := "/ui/cloud-agents?agent=grok&connector=github&connector=jira&step=token"

		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Errorf("save = %d to %q, want 303 to %q", rec.Code, rec.Header().Get("Location"), want)
		}

		if diff := cmp.Diff([]string{"https://wf.ngrok-free.app"}, c.admin.setURLs); diff != "" {
			t.Errorf("SetPublicURL calls (-want +got):\n%s", diff)
		}
	})

	t.Run("an unchanged address is not written again", func(t *testing.T) {
		rec := c.post("/ui/cloud-agents/url", ck, form("https://wf.ngrok-free.app"))

		if rec.Code != http.StatusSeeOther || len(c.admin.setURLs) != 1 {
			t.Errorf("save = %d after %d writes, want 303 and still 1 write", rec.Code, len(c.admin.setURLs))
		}
	})

	t.Run("a refusal shows its message and keeps the form", func(t *testing.T) {
		c.admin.setURLErr = &admin.APIError{Status: http.StatusBadRequest, Message: "The public URL must be https."}
		t.Cleanup(func() { c.admin.setURLErr = nil })
		rec := c.post("/ui/cloud-agents/url", ck, form("http://nope.example"))

		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Fatalf("refused save = %d to %q, want 400 and no redirect", rec.Code, rec.Header().Get("Location"))
		}

		wantAll(t, "refused save", rec.Body.String(), "The public URL must be https.", `value="http://nope.example"`, "Step 3 of 6")
	})

	t.Run("an empty address is refused", func(t *testing.T) {
		before := len(c.admin.setURLs)
		rec := c.post("/ui/cloud-agents/url", ck, form("  "))

		if rec.Code != http.StatusBadRequest || len(c.admin.setURLs) != before {
			t.Errorf("empty save = %d with %d writes, want 400 and none", rec.Code, len(c.admin.setURLs)-before)
		}
	})

	t.Run("an unknown agent starts over", func(t *testing.T) {
		f := form("https://x.example")
		f.Set("agent", "chatgpt")
		rec := c.post("/ui/cloud-agents/url", ck, f)

		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/cloud-agents" {
			t.Errorf("save = %d to %q, want 303 to /ui/cloud-agents", rec.Code, rec.Header().Get("Location"))
		}
	})
}

func TestCloudAgentsCreate(t *testing.T) {
	var log bytes.Buffer

	c, ck, csrf := cloudConsole(t, &log)
	c.admin.publicURL = "https://wf.ngrok-free.app"

	t.Run("without the csrf token", func(t *testing.T) {
		for _, path := range []string{"/ui/cloud-agents/url", "/ui/cloud-agents/token", "/ui/cloud-agents/test"} {
			if got := c.post(path, ck, url.Values{"agent": {"grok"}}).Code; got != http.StatusForbidden {
				t.Errorf("POST %s without csrf = %d, want 403", path, got)
			}
		}

		if len(c.admin.created) != 0 {
			t.Errorf("created %v without csrf, want nothing", c.admin.created)
		}
	})

	t.Run("creates a remote token for the picked connectors", func(t *testing.T) {
		rec := c.post("/ui/cloud-agents/token", ck, url.Values{"csrf": {csrf}, "agent": {"grok"}, "name": {" grok-bot "}, "connector": {"github", "jira"}})
		body := rec.Body.String()

		if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
			t.Fatalf("create = %d to %q, want 200 and no redirect", rec.Code, rec.Header().Get("Location"))
		}

		if diff := cmp.Diff([]string{"grok-bot"}, c.admin.created); diff != "" {
			t.Errorf("token names (-want +got):\n%s", diff)
		}

		if diff := cmp.Diff([]bool{true}, c.admin.createdRemote); diff != "" {
			t.Errorf("remote flags (-want +got):\n%s", diff)
		}

		if diff := cmp.Diff([][]string{{"github", "jira"}}, c.admin.createdConns); diff != "" {
			t.Errorf("connectors (-want +got):\n%s", diff)
		}

		wantAll(t, "created page", body, "Copy the token for grok-bot now", `value="`+shownOnce+`"`, "Step 5 of 6", "Test connection", "Step 6 of 6")

		if strings.Contains(c.get("/ui/cloud-agents", ck).Body.String(), shownOnce) {
			t.Error("the token's value shows again on the next visit")
		}

		if strings.Contains(c.get("/ui/tokens", ck).Body.String(), shownOnce) {
			t.Error("the token's value shows on the tokens page")
		}
	})

	t.Run("a refusal keeps the token step", func(t *testing.T) {
		c.admin.createErr = &admin.APIError{Status: http.StatusBadRequest, Message: `A client token named "grok-bot" already exists.`}
		t.Cleanup(func() { c.admin.createErr = nil })
		rec := c.post("/ui/cloud-agents/token", ck, url.Values{"csrf": {csrf}, "agent": {"grok"}, "name": {"grok-bot"}, "connector": {"github"}})

		if rec.Code != http.StatusBadRequest {
			t.Errorf("refused create = %d, want 400", rec.Code)
		}

		wantAll(t, "refused create", rec.Body.String(), "already exists.", `value="grok-bot"`, "Step 4 of 6")
	})

	t.Run("the token stays out of the log", func(t *testing.T) {
		if strings.Contains(log.String(), shownOnce) {
			t.Errorf("the log holds the token: %s", log.String())
		}
	})
}

func TestCloudAgentsTest(t *testing.T) {
	var log bytes.Buffer

	c, ck, csrf := cloudConsole(t, &log)
	c.admin.publicURL = "https://wf.ngrok-free.app"
	read := func(t *testing.T) (string, int) {
		t.Helper()
		rec := c.post("/ui/cloud-agents/test", ck, url.Values{"csrf": {csrf}, "agent": {"grok"}, "name": {"grok-bot"}, "token": {shownOnce}})

		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("the test redirected to %q, want an answer", loc)
		}

		return rec.Body.String(), rec.Code
	}

	t.Run("ok", func(t *testing.T) {
		c.admin.testResult = admin.RemoteTestResult{
			Outcome: admin.RemoteOK, OK: true, Message: "Your agent can reach Wayfindr.", URL: "https://wf.ngrok-free.app/mcp", Connectors: 2, Tools: 2,
		}
		body, code := read(t)

		if code != http.StatusOK {
			t.Fatalf("test = %d, want 200", code)
		}

		wantAll(t, "ok", body, `class="alert ok" role="status"`, "The connection works", "Your agent can reach Wayfindr.", "Listed 2 tools", "<code>https://wf.ngrok-free.app/mcp</code>", "Test again")

		if strings.Contains(body, "prominent") {
			t.Error("a passing test is styled as the proxy warning")
		}

		if diff := cmp.Diff([]string{shownOnce}, c.admin.tested); diff != "" {
			t.Errorf("tokens tested (-want +got):\n%s", diff)
		}

		// The answer carries the token again, so the test can be repeated.
		wantAll(t, "ok", body, `name="token" value="`+shownOnce+`"`)
	})

	t.Run("a failure says what to do", func(t *testing.T) {
		c.admin.testResult = admin.RemoteTestResult{
			Outcome: admin.RemoteTLSError, Message: "The secure connection failed.", Action: "Check the address is the https one your tunnel prints.",
			URL: "https://wf.ngrok-free.app/mcp",
		}
		body, _ := read(t)

		wantAll(t, "failure", body, `class="alert err" role="alert"`, "The test failed", "The secure connection failed.", "What to do:", "Check the address is the https one your tunnel prints.")

		if strings.Contains(body, "prominent") || strings.Contains(body, "The connection works") {
			t.Error("a failed test is styled as the proxy warning or as a pass")
		}
	})

	t.Run("a proxy that hides remote requests is a prominent warning", func(t *testing.T) {
		c.admin.testResult = admin.RemoteTestResult{
			Outcome: admin.RemoteProxyTreatsRemoteAsLocal, Message: "Your tunnel hides that requests come from outside.",
			Action: "Make the proxy add an X-Forwarded-For header.", URL: "https://wf.ngrok-free.app/mcp",
		}
		body, _ := read(t)

		wantAll(t, "proxy", body, `class="alert err prominent" role="alert"`, "Warning: your tunnel makes outside requests look local", "Your tunnel hides that requests come from outside.", "Make the proxy add an X-Forwarded-For header.")
	})

	t.Run("a call that cannot run keeps the token on the page", func(t *testing.T) {
		c.admin.testErr = &admin.APIError{Status: http.StatusNotImplemented, Message: "testing the connection is not available"}
		t.Cleanup(func() { c.admin.testErr = nil })
		body, code := read(t)

		if code != http.StatusNotImplemented {
			t.Errorf("test = %d, want 501", code)
		}

		wantAll(t, "error", body, "testing the connection is not available", `name="token" value="`+shownOnce+`"`)
	})

	t.Run("without a token", func(t *testing.T) {
		rec := c.post("/ui/cloud-agents/test", ck, url.Values{"csrf": {csrf}, "agent": {"grok"}})

		if rec.Code != http.StatusBadRequest {
			t.Errorf("test without a token = %d, want 400", rec.Code)
		}
	})

	t.Run("the token stays out of the log", func(t *testing.T) {
		if strings.Contains(log.String(), shownOnce) {
			t.Errorf("the log holds the token: %s", log.String())
		}
	})
}

func TestCloudAgentsSetup(t *testing.T) {
	c, ck, csrf := cloudConsole(t, &bytes.Buffer{})
	c.admin.publicURL = "https://wf.ngrok-free.app/"
	create := func(agent string) string {
		return c.post("/ui/cloud-agents/token", ck, url.Values{"csrf": {csrf}, "agent": {agent}, "name": {agent}, "connector": {"github"}}).Body.String()
	}

	t.Run("Grok Bot", func(t *testing.T) {
		body := create("grok")

		wantAll(t, "grok", body, "Connect apps", "Add a custom connector", "<b>Add plugin</b>", "Remote HTTPS", "ngrok-skip-browser-warning",
			`<code id="g-url">https://wf.ngrok-free.app/mcp</code>`, `<code id="g-hname">Authorization</code>`, `<code id="g-hvalue">Bearer `+shownOnce+`</code>`,
			"Use a personal Bot, not a Team Bot", "everyone on your team uses your token", `data-copy="#g-url"`)

		if strings.Contains(body, "app/mcp/mcp") || strings.Contains(body, "app//mcp") {
			t.Error("the server URL joins the public URL and /mcp badly")
		}

		if strings.Contains(body, "secure credential prompt") {
			t.Error("Grok's setup shows Muse's text")
		}
	})

	t.Run("Muse", func(t *testing.T) {
		body := create("muse")

		wantAll(t, "muse", body, "Streamable HTTP at https://wf.ngrok-free.app/mcp", "Authorization: Bearer header", "secure credential prompt",
			"I will not type it into this chat", `data-copy="#muse-message"`, "Paste the token only into Muse's secure credential prompt")

		if strings.Contains(body, "Team Bot") {
			t.Error("Muse's setup shows Grok's warning")
		}

		_, rest, _ := strings.Cut(body, `id="muse-message"`)
		message, _, _ := strings.Cut(rest, "</pre>")

		if strings.Contains(message, shownOnce) {
			t.Error("the message for Muse holds the token")
		}
	})

	t.Run("no public URL", func(t *testing.T) {
		c.admin.publicURL = ""
		body := create("grok")

		wantAll(t, "no URL", body, "No public URL is set", shownOnce)
	})
}

func TestCloudAgentsListAndRevoke(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	sessions := webui.NewSessions(clock.Now)
	fa := &richAdmin{fakeAdmin{status: admin.Status{Connectors: []admin.Connector{{Name: "github", State: upstream.StateReady}}}}}
	h, err := webui.New(webui.Options{Admin: fa, Sessions: sessions, Now: clock.Now, Logger: zerolog.New(zerolog.NewTestWriter(t))})

	if err != nil {
		t.Fatalf("New: %v", err)
	}

	c := &console{h: h, sessions: sessions, admin: &fa.fakeAdmin, clock: clock}
	ck := c.signIn(t)
	page := c.get("/ui/cloud-agents", ck).Body.String()
	csrf := csrfOf(t, page)

	t.Run("lists only remote tokens", func(t *testing.T) {
		wantAll(t, "list", page, `<td class="name">grok `, "Never", `action="/ui/tokens/grok/revoke"`, `name="return" value="cloud-agents"`, `aria-label="Revoke grok"`)

		if strings.Contains(page, "ci-bot") {
			t.Error("a token that is not remote is listed")
		}
	})

	t.Run("revoke comes back to the page", func(t *testing.T) {
		rec := c.post("/ui/tokens/grok/revoke", ck, url.Values{"csrf": {csrf}, "return": {"cloud-agents"}})
		want := "/ui/cloud-agents?done=revoked&name=grok"

		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Errorf("revoke = %d to %q, want 303 to %q", rec.Code, rec.Header().Get("Location"), want)
		}

		if diff := cmp.Diff([]string{"grok"}, fa.revoked); diff != "" {
			t.Errorf("revoked (-want +got):\n%s", diff)
		}

		if !strings.Contains(c.get(want, ck).Body.String(), "Revoked grok.") {
			t.Error("the page does not confirm the revoke")
		}
	})

	t.Run("any other return goes to the tokens page", func(t *testing.T) {
		rec := c.post("/ui/tokens/grok/revoke", ck, url.Values{"csrf": {csrf}, "return": {"https://evil.example/"}})

		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/ui/tokens?") {
			t.Errorf("revoke went to %q, want /ui/tokens", loc)
		}
	})

	t.Run("the tokens page links here and makes no remote token", func(t *testing.T) {
		body := c.get("/ui/tokens", ck).Body.String()

		wantAll(t, "tokens page", body, `href="/ui/cloud-agents"`, ">remote</span>", `aria-label="Revoke ci-bot"`, `aria-label="Revoke grok"`)

		if strings.Contains(body, `name="return"`) {
			t.Error("the tokens page's revoke sends the browser back to the Cloud agents page")
		}

		c.post("/ui/tokens", ck, url.Values{"csrf": {csrf}, "name": {"cursor"}, "connector": {"github"}})

		for i, remote := range fa.createdRemote {
			if remote {
				t.Errorf("the tokens page made token %d remote", i)
			}
		}
	})
}

func TestCloudAgentsNoLocationHoldsToken(t *testing.T) {
	c, ck, csrf := cloudConsole(t, &bytes.Buffer{})
	c.admin.publicURL = "https://wf.ngrok-free.app"
	c.admin.testResult = admin.RemoteTestResult{Outcome: admin.RemoteOK, OK: true}

	for _, tc := range []struct {
		path string
		form url.Values
	}{
		{"/ui/cloud-agents/url", url.Values{"csrf": {csrf}, "agent": {"grok"}, "connector": {"github"}, "url": {"https://other.ngrok-free.app"}}},
		{"/ui/cloud-agents/token", url.Values{"csrf": {csrf}, "agent": {"grok"}, "name": {"grok-bot"}, "connector": {"github"}}},
		{"/ui/cloud-agents/test", url.Values{"csrf": {csrf}, "agent": {"grok"}, "name": {"grok-bot"}, "token": {shownOnce}}},
		{"/ui/tokens/grok-bot/revoke", url.Values{"csrf": {csrf}, "return": {"cloud-agents"}}},
	} {
		rec := c.post(tc.path, ck, tc.form)

		if loc := rec.Header().Get("Location"); strings.Contains(loc, shownOnce) || strings.Contains(loc, "wf_s_") {
			t.Errorf("POST %s redirected to %q, which holds a token", tc.path, loc)
		}

		for _, ck := range rec.Result().Cookies() {
			if strings.Contains(ck.Value, "wf_s_") {
				t.Errorf("POST %s set cookie %q holding a token", tc.path, ck.Name)
			}
		}

		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("POST %s Cache-Control = %q, want no-store", tc.path, got)
		}
	}
}

// A POST here answers with a page, so htmx must not push the POST's path into
// the history: there is no GET page at it, and a reload or the back button
// would 404 (wayfindr-4ppx).
func TestCloudAgentsKeepAddressOnGETablePath(t *testing.T) {
	c, ck, csrf := cloudConsole(t, &bytes.Buffer{})
	c.admin.publicURL = "https://wf.ngrok-free.app"
	c.admin.testResult = admin.RemoteTestResult{Outcome: admin.RemoteOK, OK: true}

	posts := []struct {
		name string
		path string
		form url.Values
		code int
	}{
		{"url refused", "/ui/cloud-agents/url", url.Values{"csrf": {csrf}, "agent": {"grok"}, "connector": {"github"}, "url": {" "}}, http.StatusBadRequest},
		{"token created", "/ui/cloud-agents/token", url.Values{"csrf": {csrf}, "agent": {"grok"}, "name": {"grok-bot"}, "connector": {"github"}}, http.StatusOK},
		{"test run", "/ui/cloud-agents/test", url.Values{"csrf": {csrf}, "agent": {"grok"}, "name": {"grok-bot"}, "token": {shownOnce}}, http.StatusOK},
		{"test without a token", "/ui/cloud-agents/test", url.Values{"csrf": {csrf}, "agent": {"grok"}}, http.StatusBadRequest},
	}

	for _, tt := range posts {
		t.Run(tt.name+" is not pushed into the history", func(t *testing.T) {
			rec := c.post(tt.path, ck, tt.form)

			if rec.Code != tt.code {
				t.Fatalf("POST %s = %d, want %d", tt.path, rec.Code, tt.code)
			}

			if got := rec.Header().Get("HX-Push-Url"); got != "false" {
				t.Errorf("HX-Push-Url = %q, want false", got)
			}
		})
	}

	t.Run("a refused token create is not pushed either", func(t *testing.T) {
		c.admin.createErr = &admin.APIError{Status: http.StatusBadRequest, Message: "no"}
		t.Cleanup(func() { c.admin.createErr = nil })
		rec := c.post("/ui/cloud-agents/token", ck, url.Values{"csrf": {csrf}, "agent": {"grok"}, "name": {"x"}, "connector": {"github"}})

		if got := rec.Header().Get("HX-Push-Url"); rec.Code != http.StatusBadRequest || got != "false" {
			t.Errorf("refused create = %d with HX-Push-Url %q, want 400 and false", rec.Code, got)
		}
	})

	for _, path := range []string{"/ui/cloud-agents/url", "/ui/cloud-agents/token", "/ui/cloud-agents/test"} {
		t.Run("GET "+path+" goes back to the wizard", func(t *testing.T) {
			rec := c.get(path, ck)

			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/cloud-agents" {
				t.Errorf("GET %s = %d to %q, want 303 to /ui/cloud-agents", path, rec.Code, rec.Header().Get("Location"))
			}

			if loc := c.get(path, nil).Header().Get("Location"); loc != webui.LoginPath {
				t.Errorf("GET %s without a session went to %q, want the login page", path, loc)
			}
		})
	}
}

// The answer to the token form holds the only copy of the token, so a failure
// to read what the page merely decorates with must not replace it
// (wayfindr-4ppx).
func TestCloudAgentsDoneStageKeepsTokenOnReadFailure(t *testing.T) {
	broken := &admin.APIError{Status: http.StatusInternalServerError, Message: "the daemon is on fire"}
	create := url.Values{"agent": {"grok"}, "name": {"grok-bot"}, "connector": {"github"}}
	test := url.Values{"agent": {"grok"}, "name": {"grok-bot"}, "token": {shownOnce}}

	tests := []struct {
		name    string
		breakIt func(*fakeAdmin)
		note    bool
	}{
		{"the token list cannot be read", func(f *fakeAdmin) { f.listErr = broken }, true},
		{"the status cannot be read", func(f *fakeAdmin) { f.statusErr = broken }, false},
		{"neither can be read", func(f *fakeAdmin) { f.listErr, f.statusErr = broken, broken }, true},
		{"the public URL cannot be read", func(f *fakeAdmin) { f.publicURLErr = broken }, false},
	}

	for _, tt := range tests {
		for path, form := range map[string]url.Values{"/ui/cloud-agents/token": create, "/ui/cloud-agents/test": test} {
			t.Run(tt.name+", "+path, func(t *testing.T) {
				var log bytes.Buffer

				c, ck, csrf := cloudConsole(t, &log)
				c.admin.publicURL = "https://wf.ngrok-free.app"
				c.admin.testResult = admin.RemoteTestResult{Outcome: admin.RemoteOK, OK: true}
				tt.breakIt(c.admin)

				f := url.Values{"csrf": {csrf}}

				for k, v := range form {
					f[k] = v
				}

				rec := c.post(path, ck, f)
				body := rec.Body.String()

				if rec.Code != http.StatusOK {
					t.Fatalf("POST %s = %d, want 200 with the token page", path, rec.Code)
				}

				wantAll(t, "done page", body, `value="`+shownOnce+`"`, "Step 5 of 6", "Step 6 of 6")

				if strings.Contains(body, "Something went wrong") {
					t.Error("the error page replaced the token")
				}

				if got := strings.Contains(body, "could not be read just now"); got != tt.note {
					t.Errorf("the unavailable-list note shown = %v, want %v", got, tt.note)
				}

				if strings.Contains(log.String(), shownOnce) {
					t.Errorf("the log holds the token: %s", log.String())
				}

				if !strings.Contains(log.String(), "the daemon is on fire") && tt.name != "the public URL cannot be read" {
					t.Errorf("the failure was not logged: %s", log.String())
				}
			})
		}
	}

	t.Run("before the done stage a read failure is still the error page", func(t *testing.T) {
		c, ck, csrf := cloudConsole(t, &bytes.Buffer{})
		c.admin.listErr = broken
		rec := c.post("/ui/cloud-agents/url", ck, url.Values{"csrf": {csrf}, "agent": {"grok"}, "connector": {"github"}, "url": {" "}})

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("POST = %d, want 500", rec.Code)
		}
	})
}

// Accessible names are unique, a warning that is on the page when it loads is
// a note rather than an alert, and the token is never inside an alert
// (wayfindr-4ppx).
func TestCloudAgentsAccessibility(t *testing.T) {
	c, ck, csrf := cloudConsole(t, &bytes.Buffer{})
	c.admin.publicURL = "https://wf.ngrok-free.app"
	create := func(agent string) string {
		return c.post("/ui/cloud-agents/token", ck, url.Values{"csrf": {csrf}, "agent": {agent}, "name": {agent}, "connector": {"github"}}).Body.String()
	}

	t.Run("Grok Bot", func(t *testing.T) {
		body := create("grok")

		wantAll(t, "grok", body, `aria-label="Copy token for grok"`, `aria-label="Copy connector name"`, `aria-label="Copy server URL"`,
			`aria-label="Copy header name"`, `aria-label="Copy header value"`, `class="alert warn" role="note"`)

		if strings.Contains(body, `role="alert"`) {
			t.Error("the page announces an alert when it loads, before any test")
		}
	})

	t.Run("Muse", func(t *testing.T) {
		body := create("muse")

		wantAll(t, "muse", body, `aria-label="Copy message for Muse"`, `aria-label="Copy token for Muse"`, `class="alert warn" role="note"`)

		if strings.Contains(body, `role="alert"`) {
			t.Error("the page announces an alert when it loads, before any test")
		}

		_, rest, _ := strings.Cut(body, "Paste the token only into Muse")
		between, _, _ := strings.Cut(rest, `id="muse-token"`)

		if strings.Count(between, "</div>") < 2 {
			t.Errorf("the token is inside the Muse warning, not after it: %q", between)
		}
	})

	t.Run("no public URL", func(t *testing.T) {
		c.admin.publicURL = ""
		body := create("grok")

		wantAll(t, "no URL", body, `class="alert warn" role="note"`)

		if strings.Contains(body, `role="alert"`) {
			t.Error("the page announces an alert when it loads, before any test")
		}
	})

	t.Run("a test result is announced", func(t *testing.T) {
		c.admin.publicURL = "https://wf.ngrok-free.app"
		c.admin.testResult = admin.RemoteTestResult{Outcome: admin.RemoteTLSError, Message: "m", Action: "a"}
		body := c.post("/ui/cloud-agents/test", ck, url.Values{"csrf": {csrf}, "agent": {"muse"}, "name": {"muse"}, "token": {shownOnce}}).Body.String()

		wantAll(t, "result", body, `role="alert"`)

		_, rest, _ := strings.Cut(body, `role="alert"`)
		alert, _, _ := strings.Cut(rest, `<form method="post" action="/ui/cloud-agents/test"`)

		if strings.Contains(alert, shownOnce) {
			t.Errorf("the token is inside the alert: %q", alert)
		}
	})
}

func TestCloudAgentsSingularConnector(t *testing.T) {
	c, ck, csrf := cloudConsole(t, &bytes.Buffer{})
	c.admin.publicURL = "https://wf.ngrok-free.app"

	tests := []struct {
		connectors, tools int
		want              string
	}{
		{1, 2, "Listed 2 tools through your public URL."},
		{2, 1, "Listed 1 tool through your public URL."},
		{0, 0, "Listed 0 tools through your public URL."},
	}

	for _, tt := range tests {
		c.admin.testResult = admin.RemoteTestResult{Outcome: admin.RemoteOK, OK: true, Connectors: tt.connectors, Tools: tt.tools}
		body := c.post("/ui/cloud-agents/test", ck, url.Values{"csrf": {csrf}, "agent": {"grok"}, "name": {"g"}, "token": {shownOnce}}).Body.String()

		if !strings.Contains(body, tt.want) {
			t.Errorf("connectors %d, tools %d: page lacks %q", tt.connectors, tt.tools, tt.want)
		}
	}
}
