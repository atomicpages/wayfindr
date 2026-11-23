package webui_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/upstream"
	"github.com/atomicpages/wayfindr/internal/webui"
)

// The console's side of Wayfindr's authorization server (ADR 0022, design
// §7.10): the approve page and its two answers, and the wizard's ChatGPT
// path. Through the console's handler, with the admin API faked.

const (
	approveURL = "/ui/oauth/approve?req=r1"
	// authorizationCode stands for the one-use code in the redirect, which
	// no page, log, or header but the 303's Location may carry.
	authorizationCode = "C0DE-7f3a"
	clientRedirect    = "https://chatgpt.com/connector/oauth/cb?code=" + authorizationCode + "&state=S&iss=https%3A%2F%2Fwf.example.com"
	denyRedirect      = "https://chatgpt.com/connector/oauth/cb?error=access_denied&state=S&iss=https%3A%2F%2Fwf.example.com"
)

// fakeOAuth is the authorization server's side of the fake admin API.
type fakeOAuth struct {
	mu sync.Mutex
	// enabled is server.oauth.enabled; setCalls records each SetOAuthEnabled.
	enabled  bool
	setCalls []bool
	setErr   error
	// armErr is what ArmOAuth answers instead of arming; armEnds is when an
	// arm ends.
	armErr  error
	armEnds time.Time
	arms    int
	// request is the pending authorization "r1"; requestErr overrides it;
	// asked records every id OAuthRequest was asked for.
	request    admin.OAuthRequest
	requestErr error
	asked      []string
	// approveErr and approveTo are the approve's answer; approvals records
	// each; denied records each deny.
	approveErr error
	approveTo  string
	approvals  []admin.OAuthApproval
	denied     []string
	denyTo     string
	// test is what TestRemoteOAuth answers; tests counts the calls.
	test    admin.RemoteTestResult
	testErr error
	tests   int
}

func (f *fakeAdmin) OAuthSettings(context.Context) (admin.OAuthSettings, error) {
	f.oauth.mu.Lock()
	defer f.oauth.mu.Unlock()

	return admin.OAuthSettings{Enabled: f.oauth.enabled, Clients: []string{}}, nil
}

func (f *fakeAdmin) SetOAuthEnabled(_ context.Context, enabled bool) (admin.OAuthSettings, error) {
	f.oauth.mu.Lock()
	defer f.oauth.mu.Unlock()
	f.oauth.setCalls = append(f.oauth.setCalls, enabled)

	if f.oauth.setErr != nil {
		return admin.OAuthSettings{}, f.oauth.setErr
	}

	f.oauth.enabled = enabled

	return admin.OAuthSettings{Enabled: enabled, Clients: []string{}}, nil
}

func (f *fakeAdmin) TestRemoteOAuth(context.Context) (admin.RemoteTestResult, error) {
	f.oauth.mu.Lock()
	defer f.oauth.mu.Unlock()
	f.oauth.tests++

	return f.oauth.test, f.oauth.testErr
}

func (f *fakeAdmin) ArmOAuth(context.Context) (admin.OAuthArm, error) {
	f.oauth.mu.Lock()
	defer f.oauth.mu.Unlock()

	if f.oauth.armErr != nil {
		return admin.OAuthArm{}, f.oauth.armErr
	}

	f.oauth.arms++

	return admin.OAuthArm{ExpiresAt: f.oauth.armEnds}, nil
}

func (f *fakeAdmin) OAuthRequest(_ context.Context, id string) (admin.OAuthRequest, error) {
	f.oauth.mu.Lock()
	defer f.oauth.mu.Unlock()
	f.oauth.asked = append(f.oauth.asked, id)

	if f.oauth.requestErr != nil {
		return admin.OAuthRequest{}, f.oauth.requestErr
	}

	if id != f.oauth.request.ID {
		return admin.OAuthRequest{}, &admin.APIError{Status: http.StatusNotFound, Message: "no such pending authorization"}
	}

	return f.oauth.request, nil
}

func (f *fakeAdmin) ApproveOAuth(_ context.Context, _ string, a admin.OAuthApproval) (admin.OAuthDecision, error) {
	f.oauth.mu.Lock()
	defer f.oauth.mu.Unlock()
	f.oauth.approvals = append(f.oauth.approvals, a)

	if f.oauth.approveErr != nil {
		return admin.OAuthDecision{}, f.oauth.approveErr
	}

	return admin.OAuthDecision{Redirect: f.oauth.approveTo, Name: a.Name}, nil
}

func (f *fakeAdmin) DenyOAuth(_ context.Context, id string) (admin.OAuthDecision, error) {
	f.oauth.mu.Lock()
	defer f.oauth.mu.Unlock()
	f.oauth.denied = append(f.oauth.denied, id)

	return admin.OAuthDecision{Redirect: f.oauth.denyTo}, nil
}

// asked is the ids OAuthRequest has been asked for.
func (f *fakeAdmin) askedFor() []string {
	f.oauth.mu.Lock()
	defer f.oauth.mu.Unlock()

	return append([]string(nil), f.oauth.asked...)
}

func (f *fakeAdmin) approved() []admin.OAuthApproval {
	f.oauth.mu.Lock()
	defer f.oauth.mu.Unlock()

	return append([]admin.OAuthApproval(nil), f.oauth.approvals...)
}

// oauthConsole is a console with a pending authorization r1 from ChatGPT, two
// connectors, and OAuth on.
func oauthConsole(t *testing.T, log *bytes.Buffer) *console {
	t.Helper()
	c := newConsoleLog(t, log)
	c.admin.status.Connectors = []admin.Connector{{Name: "github", State: upstream.StateReady}, {Name: "jira", State: upstream.StateReady}}
	c.admin.publicURL = "https://wf.example.com"
	c.admin.oauth.enabled = true
	c.admin.oauth.armEnds = c.clock.Now().Add(10 * time.Minute)
	c.admin.oauth.request = admin.OAuthRequest{
		ID: "r1", ClientID: "https://chatgpt.com/oauth/client.json", ClientName: "ChatGPT", ClientHost: "chatgpt.com",
		RedirectHost: "chatgpt.com", StartedAt: c.clock.Now().Add(-90 * time.Second), ReportedIP: "203.0.113.7", Name: "chatgpt",
		ExpiresAt: c.clock.Now().Add(8 * time.Minute),
	}
	c.admin.oauth.approveTo = clientRedirect
	c.admin.oauth.denyTo = denyRedirect

	return c
}

// getAs sends GET target with the cookie (nil for none) and headers.
func (c *console) getAs(target string, ck *http.Cookie, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)

	for k, v := range header {
		req.Header[k] = v
	}

	if ck != nil {
		req.AddCookie(ck)
	}

	return c.do(req)
}

// navigation is the headers of a browser following a link.
func navigation() http.Header {
	return http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}, "Sec-Fetch-Site": {"same-origin"}}
}

func TestApprovePageWithoutASessionClaimsNothing(t *testing.T) {
	c := oauthConsole(t, &bytes.Buffer{})
	rec := c.getAs(approveURL, nil, navigation())
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("GET approve without a session = %d, want 200 (never the login redirect)", rec.Code)
	}

	wantAll(t, "no session", body, `href="/ui/oauth/approve?req=r1"`, ">Continue<", "wayfindr ui",
		"Continue only if you just selected Connect in an app. If this tab opened without you opening it, or from a link someone sent you, close it instead.", "then select Continue again", "The request waits until the time the Cloud agents page showed after you selected Connect a client.")

	if got := c.admin.askedFor(); len(got) != 0 {
		t.Errorf("the page asked the admin API for %v without a session: it must claim nothing", got)
	}

	// The page names no client and holds no form.
	for _, leak := range []string{"ChatGPT", "chatgpt.com", "203.0.113.7", `name="csrf"`} {
		if strings.Contains(body, leak) {
			t.Errorf("the page without a session shows %q", leak)
		}
	}

	t.Run("an id that is not plain is escaped in the link", func(t *testing.T) {
		rec := c.getAs("/ui/oauth/approve?req="+url.QueryEscape(`"><script>x</script>`), nil, navigation())

		if strings.Contains(rec.Body.String(), "<script>x") {
			t.Error("the id is not escaped")
		}
	})
}

func TestApprovePageClaimsOnlyForASignedInNavigation(t *testing.T) {
	c := oauthConsole(t, &bytes.Buffer{})
	ck := c.signIn(t)

	for _, tt := range []struct {
		name   string
		header http.Header
		claims bool
	}{
		{"a navigation", navigation(), true},
		{"no Sec-Fetch headers, an older browser", nil, true},
		{"a typed address", http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}, "Sec-Fetch-Site": {"none"}}, true},
		{"a link from another site", http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}, "Sec-Fetch-Site": {"cross-site"}}, false},
		{"a link from a page on another loopback port", http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}, "Sec-Fetch-Site": {"same-site"}}, false},
		{"an htmx fetch", http.Header{"Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Dest": {"empty"}}, false},
		{"an image", http.Header{"Sec-Fetch-Mode": {"no-cors"}, "Sec-Fetch-Dest": {"image"}}, false},
		{"an iframe", http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"iframe"}}, false},
		{"a prefetch", http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}, "Sec-Purpose": {"prefetch"}}, false},
		{"a prerender", http.Header{"Sec-Purpose": {"prefetch;prerender"}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(c.admin.askedFor())
			body := c.getAs(approveURL, ck, tt.header).Body.String()
			claimed := len(c.admin.askedFor()) - before

			switch {
			case tt.claims && (claimed != 1 || !strings.Contains(body, "Approve only if")):
				t.Errorf("claimed %d times, page %q; want one claim and the approve page", claimed, body)
			case !tt.claims && (claimed != 0 || !strings.Contains(body, ">Continue<")):
				t.Errorf("claimed %d times; want none and the Continue page", claimed)
			}
		})
	}
}

func TestApprovePageShowsTheRequest(t *testing.T) {
	c := oauthConsole(t, &bytes.Buffer{})
	ck := c.signIn(t)
	rec := c.getAs(approveURL, ck, navigation())
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("GET approve = %d, want 200", rec.Code)
	}

	if diff := cmp.Diff([]string{"r1"}, c.admin.askedFor()); diff != "" {
		t.Errorf("claims (-want +got):\n%s", diff)
	}

	wantAll(t, "approve page", body,
		"<dt>Client</dt><dd>ChatGPT</dd>",
		"<dt>Client address</dt><dd><code>chatgpt.com</code></dd>",
		"<dt>Returns to</dt><dd><code>chatgpt.com</code></dd>",
		"<dt>Started</dt><dd>09:58:30 (1 min ago)</dd>",
		"<dt>IP address</dt><dd><code>203.0.113.7</code>", "reported by your tunnel",
		"Approve only if you clicked Connect in ChatGPT in the last few minutes. If someone sent you this link, or this page opened in a tab you didn't open, choose Deny.",
		`name="name" type="text" value="chatgpt"`,
		`name="connector"`, `value="github"`, `value="jira"`,
		`action="/ui/oauth/approve" hx-boost="false"`, `name="req" value="r1"`,
		`type="submit">Approve</button>`, `formaction="/ui/oauth/deny"`, ">Deny</button>")

	if strings.Contains(body, " checked>") {
		t.Error("a connector is ticked before the user picks")
	}

	for header, want := range map[string]string{
		"X-Frame-Options": "DENY", "Referrer-Policy": "same-origin", "Cache-Control": "no-store",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("Content-Security-Policy = %q, want frame-ancestors 'none'", csp)
	}

	t.Run("without a reported IP, the label is left out", func(t *testing.T) {
		c.admin.oauth.request.ReportedIP = ""
		body := c.getAs(approveURL, ck, navigation()).Body.String()

		if strings.Contains(body, "reported by your tunnel") || strings.Contains(body, "IP address") {
			t.Error("the page labels an IP the tunnel did not report")
		}
	})

	t.Run("with no free name to suggest, the page says to type one", func(t *testing.T) {
		const hint = "Every suggested name is taken; type one."

		if body := c.getAs(approveURL, ck, navigation()).Body.String(); strings.Contains(body, hint) {
			t.Error("the page says every name is taken though one was suggested")
		}

		c.admin.oauth.request.Name = ""
		body := c.getAs(approveURL, ck, navigation()).Body.String()

		if !strings.Contains(body, hint) {
			t.Errorf("no hint for an empty suggestion: %s", body)
		}
	})

	t.Run("an unknown or expired request says to start again", func(t *testing.T) {
		rec := c.getAs("/ui/oauth/approve?req=other", ck, navigation())

		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "This request expired or was already answered. Start connecting again in the app.") {
			t.Errorf("GET of an unknown request = %d %q, want the 404 page", rec.Code, rec.Body.String())
		}
	})
}

func TestApproveRedirectsToTheClientWithTheCode(t *testing.T) {
	var log bytes.Buffer

	c := oauthConsole(t, &log)
	ck := c.signIn(t)
	csrf := csrfOf(t, c.getAs(approveURL, ck, navigation()).Body.String())
	rec := c.post("/ui/oauth/approve", ck, url.Values{"csrf": {csrf}, "req": {"r1"}, "name": {" chatgpt "}, "connector": {"github", "jira"}})

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != clientRedirect {
		t.Fatalf("approve = %d to %q, want 303 to %q", rec.Code, rec.Header().Get("Location"), clientRedirect)
	}

	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("the 303 to the client has Referrer-Policy %q, want no-referrer so no address of the console follows it", got)
	}

	if rec.Body.Len() != 0 {
		t.Errorf("the 303 has a body %q: it would show the code", rec.Body.String())
	}

	want := []admin.OAuthApproval{{Name: "chatgpt", Connectors: []string{"github", "jira"}}}

	if diff := cmp.Diff(want, c.admin.approved()); diff != "" {
		t.Errorf("approvals (-want +got):\n%s", diff)
	}

	if strings.Contains(log.String(), authorizationCode) || strings.Contains(log.String(), "chatgpt.com/connector") {
		t.Errorf("the console logged the redirect: %s", log.String())
	}
}

func TestApproveRefusesWhatIsNotTheSignedInConsole(t *testing.T) {
	c := oauthConsole(t, &bytes.Buffer{})
	ck := c.signIn(t)
	form := url.Values{"req": {"r1"}, "name": {"chatgpt"}, "connector": {"github"}}

	t.Run("a post without the CSRF token", func(t *testing.T) {
		rec := c.post("/ui/oauth/approve", ck, form)

		if rec.Code != http.StatusForbidden || rec.Header().Get("Location") != "" {
			t.Errorf("approve without csrf = %d to %q, want 403", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("a post with another session's CSRF token", func(t *testing.T) {
		other := c.signIn(t)
		csrf := csrfOf(t, c.get("/ui/cloud-agents", other).Body.String())
		f := url.Values{"csrf": {csrf}}

		for k, v := range form {
			f[k] = v
		}

		if rec := c.post("/ui/oauth/approve", ck, f); rec.Code != http.StatusForbidden {
			t.Errorf("approve with another session's csrf = %d, want 403", rec.Code)
		}
	})

	t.Run("a post without a session goes to the login page", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ui/oauth/approve", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := c.do(req)

		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/login" {
			t.Errorf("approve without a session = %d to %q, want 303 to the login page", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("the same for deny", func(t *testing.T) {
		if rec := c.post("/ui/oauth/deny", ck, url.Values{"req": {"r1"}}); rec.Code != http.StatusForbidden {
			t.Errorf("deny without csrf = %d, want 403", rec.Code)
		}
	})

	if got := c.admin.approved(); len(got) != 0 {
		t.Errorf("a refused post approved %v", got)
	}

	if len(c.admin.oauth.denied) != 0 {
		t.Errorf("a refused post denied %v", c.admin.oauth.denied)
	}
}

func TestApproveRerendersWhenTheNameOrConnectorsAreRefused(t *testing.T) {
	c := oauthConsole(t, &bytes.Buffer{})
	ck := c.signIn(t)
	csrf := csrfOf(t, c.getAs(approveURL, ck, navigation()).Body.String())

	for _, tt := range []struct {
		name   string
		err    error
		status int
		text   string
	}{
		{"a taken name is 409", &admin.APIError{Status: http.StatusConflict, Message: `a client token or OAuth grant called "mine" already exists`}, http.StatusConflict, `a client token or OAuth grant called &#34;mine&#34; already exists`},
		{"a bad name is 400", &admin.APIError{Status: http.StatusBadRequest, Message: "client token name must match [a-z0-9]"}, http.StatusBadRequest, "client token name must match"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c.admin.oauth.approveErr = tt.err
			before := len(c.admin.askedFor())
			rec := c.post("/ui/oauth/approve", ck, url.Values{"csrf": {csrf}, "req": {"r1"}, "name": {"mine"}, "connector": {"jira"}})
			body := rec.Body.String()

			if rec.Code != tt.status || rec.Header().Get("Location") != "" {
				t.Fatalf("approve = %d to %q, want %d and no redirect", rec.Code, rec.Header().Get("Location"), tt.status)
			}

			wantAll(t, "re-render", body, tt.text, "Approve only if you clicked Connect in ChatGPT", `name="name" type="text" value="mine"`, `value="jira" checked>`)

			if got := len(c.admin.askedFor()) - before; got != 1 {
				t.Errorf("the page asked for the request %d times, want 1 (the request stays claimed)", got)
			}
		})
	}

	t.Run("then the same request can still be approved", func(t *testing.T) {
		c.admin.oauth.approveErr = nil
		rec := c.post("/ui/oauth/approve", ck, url.Values{"csrf": {csrf}, "req": {"r1"}, "name": {"mine-2"}, "connector": {"jira"}})

		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != clientRedirect {
			t.Errorf("second approve = %d to %q, want 303 to the client", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("a request that is gone says to start again", func(t *testing.T) {
		c.admin.oauth.approveErr = &admin.APIError{Status: http.StatusNotFound, Message: "no such pending authorization"}
		rec := c.post("/ui/oauth/approve", ck, url.Values{"csrf": {csrf}, "req": {"r1"}, "name": {"mine"}, "connector": {"jira"}})

		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "Start connecting again in the app.") {
			t.Errorf("approve of a gone request = %d, want the 404 page", rec.Code)
		}
	})
}

// A request that was not the only one to arrive during its arm may be a
// stranger's, opened in a tab the user didn't open: the page says so and
// offers only Deny, and a 409 from approve shows the same page.
func TestApprovePageForAContestedRequest(t *testing.T) {
	const contested = "3 connection requests arrived after you selected Connect a client. Only one was expected, so this can't be approved. Choose Deny, then select Connect a client and try again in ChatGPT."

	c := oauthConsole(t, &bytes.Buffer{})
	c.admin.oauth.request.Arrived = 3
	ck := c.signIn(t)

	check := func(t *testing.T, body string) {
		t.Helper()
		wantAll(t, "contested", body, contested, `name="req" value="r1"`, `formaction="/ui/oauth/deny"`, ">Deny</button>")

		for _, gone := range []string{">Approve</button>", `name="name"`, `name="connector"`, "Approve only if", "That didn't work"} {
			if strings.Contains(body, gone) {
				t.Errorf("the contested page shows %q", gone)
			}
		}
	}

	page := c.getAs(approveURL, ck, navigation())

	if page.Code != http.StatusOK {
		t.Fatalf("GET approve = %d, want 200", page.Code)
	}

	check(t, page.Body.String())

	t.Run("a 409 from approve shows it again", func(t *testing.T) {
		c.admin.oauth.approveErr = &admin.APIError{Status: http.StatusConflict, Message: "More than one connection request arrived after you selected Connect a client, so this one can't be approved."}
		rec := c.post("/ui/oauth/approve", ck, url.Values{"csrf": {csrfOf(t, page.Body.String())}, "req": {"r1"}, "name": {"mine"}, "connector": {"jira"}})

		if rec.Code != http.StatusConflict || rec.Header().Get("Location") != "" {
			t.Fatalf("approve = %d to %q, want 409 and no redirect", rec.Code, rec.Header().Get("Location"))
		}

		check(t, rec.Body.String())
	})

	t.Run("one request is the usual page", func(t *testing.T) {
		c.admin.oauth.request.Arrived = 1
		body := c.getAs(approveURL, ck, navigation()).Body.String()

		wantAll(t, "one request", body, "Approve only if", `type="submit">Approve</button>`)

		if strings.Contains(body, "connection requests arrived") {
			t.Error("the page for one request says more arrived")
		}
	})
}

func TestApproveRefusesAReturnAddressThatIsNotAWebPage(t *testing.T) {
	c := oauthConsole(t, &bytes.Buffer{})
	ck := c.signIn(t)
	csrf := csrfOf(t, c.getAs(approveURL, ck, navigation()).Body.String())

	for _, to := range []string{"javascript:alert(1)", "data:text/html,x", "/relative", "https:///nohost"} {
		c.admin.oauth.approveTo = to
		rec := c.post("/ui/oauth/approve", ck, url.Values{"csrf": {csrf}, "req": {"r1"}, "name": {"x"}, "connector": {"github"}})

		if rec.Code != http.StatusBadGateway || rec.Header().Get("Location") != "" {
			t.Errorf("approve with a return address %q = %d to %q, want 502 and no redirect", to, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestDenyRedirectsToTheClientWithAnError(t *testing.T) {
	c := oauthConsole(t, &bytes.Buffer{})
	ck := c.signIn(t)
	csrf := csrfOf(t, c.getAs(approveURL, ck, navigation()).Body.String())
	rec := c.post("/ui/oauth/deny", ck, url.Values{"csrf": {csrf}, "req": {"r1"}})

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != denyRedirect {
		t.Fatalf("deny = %d to %q, want 303 to %q", rec.Code, rec.Header().Get("Location"), denyRedirect)
	}

	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("the 303 has Referrer-Policy %q, want no-referrer", got)
	}

	if diff := cmp.Diff([]string{"r1"}, c.admin.oauth.denied); diff != "" {
		t.Errorf("denied (-want +got):\n%s", diff)
	}

	if got := c.admin.approved(); len(got) != 0 {
		t.Errorf("deny approved %v", got)
	}

	t.Run("a GET of the deny path goes to the console", func(t *testing.T) {
		rec := c.get("/ui/oauth/deny", ck)

		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/" {
			t.Errorf("GET deny = %d to %q, want 303 to /ui/", rec.Code, rec.Header().Get("Location"))
		}
	})
}

// The wizard's ChatGPT path (design §7.10): agent, url, oauth, done.

func TestCloudAgentsChatGPTPath(t *testing.T) {
	c := oauthConsole(t, &bytes.Buffer{})
	c.admin.oauth.enabled = false
	ck := c.signIn(t)
	csrf := csrfOf(t, c.get("/ui/cloud-agents?step=url&agent=chatgpt", ck).Body.String())

	t.Run("step 1 has no Connect button while sign-in is off", func(t *testing.T) {
		body := c.get("/ui/cloud-agents", ck).Body.String()

		wantAll(t, "step 1", body, "Step 1 of 6", `value="chatgpt" required`)

		if strings.Contains(body, "Connect a client") {
			t.Error("the button shows while sign-in is off")
		}
	})

	t.Run("choosing ChatGPT skips the connectors", func(t *testing.T) {
		for _, step := range []string{"connectors", "url"} {
			body := c.get("/ui/cloud-agents?step="+step+"&agent=chatgpt", ck).Body.String()

			wantAll(t, "url step", body, "Step 2 of 4: Your public address", `<li class="current" aria-current="step"><span class="n">2</span>Public URL`,
				`value="https://wf.example.com"`, `name="agent"`, `value="chatgpt"`)

			if strings.Contains(body, "Pick connectors") || strings.Contains(body, `name="connector"`) {
				t.Errorf("step=%s shows the connectors step for ChatGPT", step)
			}
		}
	})

	t.Run("saving the URL goes to the sign-in step", func(t *testing.T) {
		rec := c.post("/ui/cloud-agents/url", ck, url.Values{"csrf": {csrf}, "agent": {"chatgpt"}, "url": {"https://wf.example.com"}})

		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/cloud-agents?agent=chatgpt&step=oauth" {
			t.Errorf("save URL = %d to %q, want 303 to the oauth step", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("the sign-in step says what it opens and turns it on", func(t *testing.T) {
		body := c.get("/ui/cloud-agents?step=oauth&agent=chatgpt", ck).Body.String()

		wantAll(t, "oauth step", body, "Step 3 of 4: Turn on sign-in for ChatGPT", "/.well-known/oauth-protected-resource", "/oauth/authorize", "/oauth/token",
			`action="/ui/cloud-agents/oauth"`, `name="enabled" value="true"`, "Turn on sign-in (OAuth)", `<li class="current" aria-current="step"><span class="n">3</span>Sign-in`)
	})

	t.Run("the done step without a public URL is the url step", func(t *testing.T) {
		c.admin.publicURL = ""
		defer func() { c.admin.publicURL = "https://wf.example.com" }()
		body := c.get("/ui/cloud-agents?step=oauth&agent=chatgpt", ck).Body.String()

		wantAll(t, "no url", body, "Set your public URL first.", "Step 2 of 4")
	})

	t.Run("turning it on arms and renders the done step", func(t *testing.T) {
		rec := c.post("/ui/cloud-agents/oauth", ck, url.Values{"csrf": {csrf}, "agent": {"chatgpt"}, "enabled": {"true"}})
		body := rec.Body.String()

		if rec.Code != http.StatusOK || rec.Header().Get("HX-Push-Url") != "false" {
			t.Fatalf("turn on = %d, HX-Push-Url %q; want 200 and false", rec.Code, rec.Header().Get("HX-Push-Url"))
		}

		if diff := cmp.Diff([]bool{true}, c.admin.oauth.setCalls); diff != "" {
			t.Errorf("SetOAuthEnabled calls (-want +got):\n%s", diff)
		}

		if c.admin.oauth.arms != 1 {
			t.Errorf("armed %d times, want 1: turning sign-in on also arms", c.admin.oauth.arms)
		}

		wantAll(t, "done", body, "Step 4 of 4: Connect ChatGPT",
			"Ready for ChatGPT until 10:10:00. Add the connector in ChatGPT now; you will approve it on this computer.",
			"<dt>Server URL</dt>", "https://wf.example.com/mcp", "<dt>Authentication</dt><dd>OAuth</dd>",
			`action="/ui/cloud-agents/arm"`, "Connect a client", `action="/ui/cloud-agents/test"`, "Test the connection",
			`name="enabled" value="false"`, "Turn off sign-in", `<li class="current" aria-current="step"><span class="n">4</span>Connect`)

		if strings.Contains(body, `name="token"`) {
			t.Error("the ChatGPT done step carries a token field")
		}
	})

	t.Run("the done step is a page a reload shows again, without an arm", func(t *testing.T) {
		body := c.get("/ui/cloud-agents?step=done&agent=chatgpt", ck).Body.String()

		wantAll(t, "reload", body, "Step 4 of 4: Connect ChatGPT", "Connect a client")

		if strings.Contains(body, "Ready for ChatGPT until") {
			t.Error("a reload claims an arm it does not know of")
		}
	})

	t.Run("step 1 offers Connect a client while sign-in is on", func(t *testing.T) {
		body := c.get("/ui/cloud-agents", ck).Body.String()

		wantAll(t, "step 1", body, `form="arm-form"`, "Connect a client", `<form id="arm-form" method="post" action="/ui/cloud-agents/arm">`)
	})

	t.Run("Connect a client arms", func(t *testing.T) {
		before := c.admin.oauth.arms
		rec := c.post("/ui/cloud-agents/arm", ck, url.Values{"csrf": {csrf}, "agent": {"chatgpt"}})

		if rec.Code != http.StatusOK || c.admin.oauth.arms != before+1 || !strings.Contains(rec.Body.String(), "Ready for ChatGPT until 10:10:00.") {
			t.Errorf("arm = %d, arms %d (was %d); want 200, one more arm, and the time", rec.Code, c.admin.oauth.arms, before)
		}
	})

	t.Run("an arm refused because sign-in is off shows the step that turns it on", func(t *testing.T) {
		c.admin.oauth.armErr = &admin.APIError{Status: http.StatusConflict, Message: "Turn on sign-in for cloud agents first."}
		defer func() { c.admin.oauth.armErr = nil }()
		rec := c.post("/ui/cloud-agents/arm", ck, url.Values{"csrf": {csrf}, "agent": {"chatgpt"}})

		if rec.Code != http.StatusConflict {
			t.Errorf("arm while off = %d, want 409", rec.Code)
		}

		wantAll(t, "arm refused", rec.Body.String(), "Turn on sign-in for cloud agents first.", "Turn on sign-in (OAuth)")
	})

	t.Run("a refusal to turn it on shows the oauth step with its message", func(t *testing.T) {
		c.admin.oauth.setErr = &admin.APIError{Status: http.StatusBadRequest, Message: "Set the public URL first."}
		defer func() { c.admin.oauth.setErr = nil }()
		rec := c.post("/ui/cloud-agents/oauth", ck, url.Values{"csrf": {csrf}, "agent": {"chatgpt"}, "enabled": {"true"}})

		if rec.Code != http.StatusBadRequest {
			t.Errorf("refused turn on = %d, want 400", rec.Code)
		}

		wantAll(t, "refused", rec.Body.String(), "Set the public URL first.", "Step 3 of 4")
	})

	t.Run("turning it off shows the sign-in step", func(t *testing.T) {
		rec := c.post("/ui/cloud-agents/oauth", ck, url.Values{"csrf": {csrf}, "agent": {"chatgpt"}, "enabled": {"false"}})

		wantAll(t, "off", rec.Body.String(), "Sign-in is off. ChatGPT is refused until you turn it on again.", "Turn on sign-in (OAuth)")

		if got := c.admin.oauth.setCalls[len(c.admin.oauth.setCalls)-1]; got {
			t.Error("turn off called SetOAuthEnabled(true)")
		}
	})

	t.Run("another agent or a bad value is not a ChatGPT action", func(t *testing.T) {
		rec := c.post("/ui/cloud-agents/oauth", ck, url.Values{"csrf": {csrf}, "agent": {"grok"}, "enabled": {"true"}})

		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/cloud-agents" {
			t.Errorf("oauth for grok = %d to %q, want 303 to the wizard", rec.Code, rec.Header().Get("Location"))
		}

		if rec := c.post("/ui/cloud-agents/oauth", ck, url.Values{"csrf": {csrf}, "agent": {"chatgpt"}, "enabled": {"maybe"}}); rec.Code != http.StatusBadRequest {
			t.Errorf("enabled=maybe = %d, want 400", rec.Code)
		}
	})

	t.Run("the POST paths' GETs go back to the wizard", func(t *testing.T) {
		for _, p := range []string{"/ui/cloud-agents/oauth", "/ui/cloud-agents/arm"} {
			rec := c.get(p, ck)

			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/cloud-agents" {
				t.Errorf("GET %s = %d to %q, want 303 to /ui/cloud-agents", p, rec.Code, rec.Header().Get("Location"))
			}
		}
	})
}

func TestCloudAgentsChatGPTTest(t *testing.T) {
	c := oauthConsole(t, &bytes.Buffer{})
	ck := c.signIn(t)
	csrf := csrfOf(t, c.get("/ui/cloud-agents?step=url&agent=chatgpt", ck).Body.String())
	post := func() (*httptest.ResponseRecorder, string) {
		rec := c.post("/ui/cloud-agents/test", ck, url.Values{"csrf": {csrf}, "agent": {"chatgpt"}})

		return rec, rec.Body.String()
	}

	t.Run("a pass says what to do next, not how many tools", func(t *testing.T) {
		c.admin.oauth.test = admin.RemoteTestResult{Outcome: admin.RemoteOK, OK: true, Message: "It works. Through the tunnel, a cloud agent finds Wayfindr's sign-in at https://wf.example.com/mcp.", URL: "https://wf.example.com/mcp"}
		rec, body := post()

		if rec.Code != http.StatusOK || c.admin.oauth.tests != 1 || len(c.admin.tested) != 0 {
			t.Fatalf("test = %d, oauth tests %d, token tests %v; want 200, 1, none", rec.Code, c.admin.oauth.tests, c.admin.tested)
		}

		wantAll(t, "pass", body, "The connection works", "Wayfindr&#39;s sign-in information is reachable through your public URL. Next, select Connect a client, add the connector in ChatGPT, and approve it here.", "Test again")

		if strings.Contains(body, "Listed 0 tools") {
			t.Error("the OAuth test reports tools")
		}
	})

	t.Run("sign-in off", func(t *testing.T) {
		c.admin.oauth.test = admin.RemoteTestResult{Outcome: admin.RemoteOAuthOff, Message: "Sign-in with OAuth is off, so nothing is tested.", Action: "Turn on sign-in for cloud agents (server.oauth.enabled), then test again."}
		_, body := post()

		wantAll(t, "off", body, `class="alert err"`, "Sign-in is off", "Sign-in with OAuth is off", "What to do:", "Turn on sign-in for cloud agents")
	})

	t.Run("the sign-in information is missing or names another address", func(t *testing.T) {
		for outcome, title := range map[admin.RemoteOutcome]string{
			admin.RemoteMetadataMissing:  "Wayfindr&#39;s sign-in information is not there",
			admin.RemoteMetadataMismatch: "The sign-in information names another address",
		} {
			c.admin.oauth.test = admin.RemoteTestResult{Outcome: outcome, Message: "m", Action: "a"}
			_, body := post()

			wantAll(t, string(outcome), body, title)
		}
	})

	t.Run("a call that cannot run", func(t *testing.T) {
		c.admin.oauth.testErr = &admin.APIError{Status: http.StatusNotImplemented, Message: "testing the connection is not available"}
		rec, body := post()

		if rec.Code != http.StatusNotImplemented {
			t.Errorf("test = %d, want 501", rec.Code)
		}

		wantAll(t, "error", body, "The test could not run", "testing the connection is not available")
	})

	t.Run("any other agent without a token is still a 400", func(t *testing.T) {
		rec := c.post("/ui/cloud-agents/test", ck, url.Values{"csrf": {csrf}, "agent": {"grok"}})

		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "There is no token to test.") {
			t.Errorf("tokenless test for grok = %d, want 400", rec.Code)
		}
	})
}

// grantAdmin lists an OAuth grant beside a remote token.
type grantAdmin struct{ fakeAdmin }

func (*grantAdmin) ListTokens(context.Context) (admin.TokenList, error) {
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

	return admin.TokenList{Tokens: []admin.Token{
		{Name: "chatgpt", Kind: admin.TokenKindOAuth, Client: "https://chatgpt.com/oauth/client.json", Connectors: []string{"github"}, Remote: true, CreatedAt: at},
		{Name: "grok", Kind: admin.TokenKindToken, Connectors: []string{"github"}, Remote: true, CreatedAt: at},
	}}, nil
}

func TestGrantsListWithTheirKindAndClientHost(t *testing.T) {
	c := newConsole(t)
	ga := &grantAdmin{}
	ga.status = admin.Status{Connectors: []admin.Connector{{Name: "github", State: upstream.StateReady}}}
	rebuild(t, c, ga)
	ck := c.signIn(t)

	for _, page := range []string{"/ui/tokens", "/ui/cloud-agents"} {
		body := c.get(page, ck).Body.String()

		wantAll(t, page, body, `title="Signed in with OAuth">oauth · chatgpt.com</span>`, `aria-label="Revoke chatgpt"`, `title="Can reach Wayfindr through a tunnel">remote</span>`)
	}
}

// rebuild points c at a console over a, keeping its sessions and clock.
func rebuild(t *testing.T, c *console, a webui.Admin) {
	t.Helper()
	h, err := webui.New(webui.Options{Admin: a, Sessions: c.sessions, Now: c.clock.Now, Logger: zerolog.New(zerolog.NewTestWriter(t))})

	if err != nil {
		t.Fatalf("New: %v", err)
	}

	c.h = h
}
