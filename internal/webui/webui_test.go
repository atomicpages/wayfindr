package webui_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/upstream"
	"github.com/atomicpages/wayfindr/internal/webui"
)

// fakeAdmin stands in for the admin API's Client: canned answers, and a
// record of the actions asked for.
type fakeAdmin struct {
	mu      sync.Mutex
	status  admin.Status
	authURL string
	// device, when set, is the flow StartAuth hands out instead of one
	// with authURL.
	device    *admin.AuthFlow
	revoked   []string
	created   []string
	createErr error
	adds      []admin.ConnectorSpec
	addErr    error
	// createdRemote and createdConns record what CreateToken was asked for.
	createdRemote []bool
	createdConns  [][]string
	// publicURL is server.public_url; setURLs records each SetPublicURL.
	publicURL string
	setURLs   []string
	setURLErr error
	// testResult is what TestRemote answers; tested records its tokens.
	testResult admin.RemoteTestResult
	testErr    error
	tested     []string
	// statusErr, listErr, and publicURLErr make Status, ListTokens, and
	// PublicURL fail.
	statusErr, listErr, publicURLErr error
	// oauth is the authorization server's side (oauth_test.go).
	oauth fakeOAuth
	// toolList, when set, is what Tools answers; callAnswers are what Call
	// answers in turn (the last repeats), and callReqs what it was asked.
	toolList    *admin.ToolList
	toolsErr    error
	callAnswers []string
	callErr     error
	callReqs    []admin.CallRequest
	// approvals are the queue's entries; approvalsErr makes Approvals fail,
	// answerErr the answers; answered records each, as "approve id" or
	// "deny id", then " created <RFC 3339>" when it named a created time;
	// listings counts the calls to Approvals.
	approvals    []admin.ApprovalRow
	approvalsErr error
	answerErr    error
	answered     []string
	listings     int
}

func (f *fakeAdmin) Approvals(context.Context) (admin.ApprovalList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.listings++

	return admin.ApprovalList{Approvals: f.approvals}, f.approvalsErr
}

func (f *fakeAdmin) ApproveApproval(_ context.Context, id string, created time.Time) error {
	return f.answer("approve", id, created)
}

func (f *fakeAdmin) DenyApproval(_ context.Context, id string, created time.Time) error {
	return f.answer("deny", id, created)
}

func (f *fakeAdmin) answer(verb, id string, created time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	answer := verb + " " + id

	if !created.IsZero() {
		answer += " created " + created.UTC().Format(time.RFC3339Nano)
	}

	f.answered = append(f.answered, answer)

	return f.answerErr
}

func (f *fakeAdmin) PublicURL(context.Context) (admin.PublicURL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return admin.PublicURL{URL: f.publicURL}, f.publicURLErr
}

func (f *fakeAdmin) SetPublicURL(_ context.Context, u string) (admin.PublicURL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setURLs = append(f.setURLs, u)

	if f.setURLErr != nil {
		return admin.PublicURL{}, f.setURLErr
	}

	f.publicURL = u

	return admin.PublicURL{URL: u}, nil
}

func (f *fakeAdmin) TestRemote(_ context.Context, token string) (admin.RemoteTestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tested = append(f.tested, token)

	return f.testResult, f.testErr
}

func (f *fakeAdmin) AddConnector(_ context.Context, spec admin.ConnectorSpec) (admin.ConnectorAdded, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adds = append(f.adds, spec)

	if f.addErr != nil {
		return admin.ConnectorAdded{}, f.addErr
	}

	return admin.ConnectorAdded{Result: admin.Connector{Name: spec.Name, State: upstream.StateReady}, OAuth: admin.OAuthNotChecked}, nil
}

func (f *fakeAdmin) addCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.adds)
}

func (f *fakeAdmin) Status(context.Context) (admin.Status, error) { return f.status, f.statusErr }

func (f *fakeAdmin) StartAuth(_ context.Context, c string, _ admin.AuthStart) (admin.AuthFlow, error) {
	if f.device != nil {
		return *f.device, nil
	}

	return admin.AuthFlow{FlowID: "f-1", Connector: c, AuthorizeURL: f.authURL}, nil
}

func (f *fakeAdmin) Revoke(_ context.Context, c string) (admin.Revoked, error) {
	return admin.Revoked{Connector: c}, nil
}

func (f *fakeAdmin) Calls(context.Context, admin.CallsQuery) (admin.Calls, error) {
	return admin.Calls{Retention: "720h0m0s"}, nil
}

func (f *fakeAdmin) Search(context.Context, admin.SearchRequest) (admin.SearchResult, error) {
	return admin.SearchResult{}, nil
}

func (f *fakeAdmin) Tools(_ context.Context, c string) (admin.ToolList, error) {
	if f.toolsErr != nil {
		return admin.ToolList{}, f.toolsErr
	}

	if f.toolList != nil {
		return *f.toolList, nil
	}

	return admin.ToolList{Connector: c}, nil
}

func (f *fakeAdmin) Call(_ context.Context, req admin.CallRequest) (admin.CallResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callReqs = append(f.callReqs, req)

	if f.callErr != nil {
		return admin.CallResponse{}, f.callErr
	}

	i := min(len(f.callReqs), len(f.callAnswers)) - 1

	return admin.CallResponse{Result: []byte(f.callAnswers[i]), Class: "write", ElapsedMS: 1400}, nil
}

func (f *fakeAdmin) calledWith() []admin.CallRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.callReqs
}

func (f *fakeAdmin) ListTokens(context.Context) (admin.TokenList, error) {
	return admin.TokenList{}, f.listErr
}

func (f *fakeAdmin) CreateToken(_ context.Context, spec admin.TokenSpec) (admin.TokenCreated, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, spec.Name)
	f.createdRemote = append(f.createdRemote, spec.Remote)
	f.createdConns = append(f.createdConns, spec.Connectors)

	if f.createErr != nil {
		return admin.TokenCreated{}, f.createErr
	}

	return admin.TokenCreated{Name: spec.Name, Connectors: spec.Connectors, Token: "wf_s_shown-once"}, nil //nolint:gosec // G101: a test fixture
}

func (f *fakeAdmin) RevokeToken(_ context.Context, name string) (admin.TokenRevoked, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, name)

	return admin.TokenRevoked{Name: name}, nil
}

// fakeClock is a settable clock for the session store.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// console is a console handler under test with its fakes.
type console struct {
	h        http.Handler
	sessions *webui.Sessions
	admin    *fakeAdmin
	clock    *fakeClock
}

func newConsole(t *testing.T) *console {
	t.Helper()

	return newConsoleLog(t, zerolog.NewTestWriter(t))
}

// newConsoleLog is newConsole with the console's log going to w.
func newConsoleLog(t *testing.T, w io.Writer) *console {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	sessions := webui.NewSessions(clock.Now)
	fa := &fakeAdmin{status: admin.Status{Version: "v1.2.3"}}
	h, err := webui.New(webui.Options{Admin: fa, Sessions: sessions, Now: clock.Now, Logger: zerolog.New(w)})

	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return &console{h: h, sessions: sessions, admin: fa, clock: clock}
}

func (c *console) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)

	return rec
}

// redeem posts the code in path's fragment to the login form.
func (c *console) redeem(path string) *httptest.ResponseRecorder {
	_, code, _ := strings.Cut(path, "#")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, webui.LoginPath, strings.NewReader(url.Values{"code": {code}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return c.do(req)
}

// signIn opens a session and returns its cookie.
func (c *console) signIn(t *testing.T) *http.Cookie {
	t.Helper()
	path, _ := c.sessions.IssueLogin()
	rec := c.redeem(path)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", rec.Code)
	}

	cookies := rec.Result().Cookies()

	if len(cookies) != 1 {
		t.Fatalf("login set %d cookies, want 1", len(cookies))
	}

	return cookies[0]
}

func (c *console) get(target string, ck *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)

	if ck != nil {
		req.AddCookie(ck)
	}

	return c.do(req)
}

func (c *console) post(target string, ck *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(ck)

	return c.do(req)
}

// csrfOf reads the CSRF token the console put in a page's forms.
func csrfOf(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `name="csrf" value="`)

	if !ok {
		t.Fatal("the page has no csrf field")
	}

	tok, _, _ := strings.Cut(rest, `"`)

	return tok
}

func TestLogin(t *testing.T) {
	c := newConsole(t)

	t.Run("no session goes to the login page", func(t *testing.T) {
		rec := c.get("/ui/", nil)

		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != webui.LoginPath {
			t.Errorf("GET /ui/ = %d to %q, want 303 to %s", rec.Code, rec.Header().Get("Location"), webui.LoginPath)
		}
	})

	t.Run("an htmx request is told to navigate", func(t *testing.T) {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ui/", nil)
		req.Header.Set("HX-Request", "true")
		rec := c.do(req)

		if got := rec.Header().Get("HX-Redirect"); got != webui.LoginPath {
			t.Errorf("HX-Redirect = %q, want %s", got, webui.LoginPath)
		}
	})

	t.Run("a code opens one session", func(t *testing.T) {
		path, expires := c.sessions.IssueLogin()

		if !strings.HasPrefix(path, webui.LoginPath+"#") {
			t.Errorf("login path = %q, want the code in the fragment", path)
		}

		if want := c.clock.Now().Add(webui.LoginCodeTTL); !expires.Equal(want) {
			t.Errorf("expires = %v, want %v", expires, want)
		}

		rec := c.redeem(path)

		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/" {
			t.Fatalf("login = %d to %q, want 303 to /ui/", rec.Code, rec.Header().Get("Location"))
		}

		ck := rec.Result().Cookies()[0]

		if !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/ui" {
			t.Errorf("cookie = %+v, want HttpOnly, SameSite=Strict, Path=/ui", ck)
		}

		if got := c.get("/ui/", ck).Code; got != http.StatusOK {
			t.Errorf("GET /ui/ with the session = %d, want 200", got)
		}

		if got := c.redeem(path).Code; got != http.StatusUnauthorized {
			t.Errorf("the code again = %d, want 401", got)
		}
	})

	t.Run("an expired code opens none", func(t *testing.T) {
		path, _ := c.sessions.IssueLogin()
		c.clock.advance(webui.LoginCodeTTL)

		if got := c.redeem(path).Code; got != http.StatusUnauthorized {
			t.Errorf("expired code = %d, want 401", got)
		}
	})

	t.Run("an idle session ends", func(t *testing.T) {
		ck := c.signIn(t)
		c.clock.advance(webui.SessionIdle)

		if got := c.get("/ui/", ck).Code; got != http.StatusSeeOther {
			t.Errorf("GET /ui/ after idling = %d, want 303 to login", got)
		}
	})
}

func TestPagesEscapeUpstreamText(t *testing.T) {
	c := newConsole(t)
	c.admin.status.Connectors = []admin.Connector{{
		Name:  "linear",
		State: upstream.StateNeedsAuth,
		Advice: &admin.Advice{
			Summary: `linear needs you to sign in. <script>alert(1)</script>`,
			Action:  "Run `wayfindr auth login linear`.",
			Link:    "http://127.0.0.1:7777/auth/linear",
		},
	}}
	ck := c.signIn(t)
	rec := c.get("/ui/", ck)
	body := rec.Body.String()

	if strings.Contains(body, "<script>alert(1)") {
		t.Error("the advice's markup reached the page unescaped")
	}

	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("the advice is missing, escaped")
	}

	if !strings.Contains(body, "<code>wayfindr auth login linear</code>") {
		t.Error("the action's quoted command is not set as code")
	}

	if !strings.Contains(body, `action="/ui/connectors/linear/sign-in"`) {
		t.Error("a connector whose advice links an auth flow has no sign-in form")
	}

	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'self'") || !strings.Contains(got, "img-src 'self' https://cdn.jsdelivr.net;") || strings.Contains(got, "unsafe") {
		t.Errorf("CSP = %q, want script-src 'self', img-src 'self' and jsDelivr, and nothing unsafe", got)
	}
}

func TestForms(t *testing.T) {
	c := newConsole(t)
	ck := c.signIn(t)
	csrf := csrfOf(t, c.get("/ui/tokens", ck).Body.String())

	t.Run("without the csrf token", func(t *testing.T) {
		rec := c.post("/ui/tokens/ci-bot/revoke", ck, url.Values{})

		if rec.Code != http.StatusForbidden {
			t.Errorf("revoke without csrf = %d, want 403", rec.Code)
		}

		if len(c.admin.revoked) != 0 {
			t.Errorf("revoked %v, want nothing", c.admin.revoked)
		}
	})

	t.Run("with another session's csrf token", func(t *testing.T) {
		other := csrfOf(t, c.get("/ui/tokens", c.signIn(t)).Body.String())
		rec := c.post("/ui/tokens/ci-bot/revoke", ck, url.Values{"csrf": {other}})

		if rec.Code != http.StatusForbidden {
			t.Errorf("revoke with another csrf = %d, want 403", rec.Code)
		}
	})

	t.Run("revoke", func(t *testing.T) {
		rec := c.post("/ui/tokens/ci-bot/revoke", ck, url.Values{"csrf": {csrf}})

		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/ui/tokens?") {
			t.Errorf("revoke = %d to %q, want 303 to /ui/tokens", rec.Code, rec.Header().Get("Location"))
		}

		if len(c.admin.revoked) != 1 || c.admin.revoked[0] != "ci-bot" {
			t.Errorf("revoked %v, want [ci-bot]", c.admin.revoked)
		}
	})

	t.Run("create shows the value once", func(t *testing.T) {
		rec := c.post("/ui/tokens", ck, url.Values{"csrf": {csrf}, "name": {"cursor"}, "connector": {"github"}})

		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "wf_s_shown-once") {
			t.Errorf("create = %d, want 200 with the token's value", rec.Code)
		}

		if strings.Contains(c.get("/ui/tokens", ck).Body.String(), "wf_s_shown-once") {
			t.Error("the token's value shows again on the next visit")
		}
	})

	t.Run("a refused create keeps the form", func(t *testing.T) {
		c.admin.status.Connectors = []admin.Connector{{Name: "github", State: upstream.StateReady}, {Name: "jira", State: upstream.StateReady}}
		c.admin.createErr = &admin.APIError{Status: http.StatusBadRequest, Message: `A client token named "cursor" already exists.`}
		rec := c.post("/ui/tokens", ck, url.Values{"csrf": {csrf}, "name": {"cursor"}, "connector": {"github"}})
		body := rec.Body.String()

		if rec.Code != http.StatusBadRequest {
			t.Errorf("refused create = %d, want 400", rec.Code)
		}

		if !strings.Contains(body, "already exists.") || !strings.Contains(body, `value="cursor"`) {
			t.Error("the refusal's message or the entered name is missing")
		}

		if !strings.Contains(body, `value="github" checked`) || strings.Contains(body, `value="jira" checked`) ||
			strings.Count(body, `class="badge secondary chip"`) != 1 {
			t.Error("the picked connector is not kept checked and shown as a chip")
		}
	})
}

func TestSignIn(t *testing.T) {
	c := newConsole(t)
	ck := c.signIn(t)
	csrf := csrfOf(t, c.get("/ui/tokens", ck).Body.String())

	t.Run("goes on to the authorization server", func(t *testing.T) {
		c.admin.authURL = "https://linear.app/oauth/authorize?state=s"
		rec := c.post("/ui/connectors/linear/sign-in", ck, url.Values{"csrf": {csrf}})

		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != c.admin.authURL {
			t.Errorf("sign-in = %d to %q, want 303 to %s", rec.Code, rec.Header().Get("Location"), c.admin.authURL)
		}
	})

	t.Run("refuses another scheme", func(t *testing.T) {
		c.admin.authURL = "javascript:alert(1)"
		rec := c.post("/ui/connectors/linear/sign-in", ck, url.Values{"csrf": {csrf}})

		if rec.Code != http.StatusBadGateway || rec.Header().Get("Location") != "" {
			t.Errorf("sign-in = %d to %q, want 502 and no redirect", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("shows a device code", func(t *testing.T) {
		c.admin.device = &admin.AuthFlow{
			FlowID: "f-2", Connector: "linear", UserCode: "WDJB-MJHT", VerificationURI: "https://linear.app/device",
			VerificationURIComplete: "https://linear.app/device?user_code=WDJB-MJHT",
			ExpiresAt:               time.Date(2026, 11, 12, 9, 40, 0, 0, time.UTC),
		}
		t.Cleanup(func() { c.admin.device = nil })
		rec := c.post("/ui/connectors/linear/sign-in", ck, url.Values{"csrf": {csrf}})
		body := rec.Body.String()

		if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" || !strings.Contains(body, "WDJB-MJHT") ||
			!strings.Contains(body, `href="https://linear.app/device?user_code=WDJB-MJHT"`) {
			t.Errorf("sign-in = %d %q, want 200 with the code and a link to the complete verification URI", rec.Code, body)
		}

		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
			rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("headers = %v, want the console's frame guards", rec.Header())
		}
	})

	t.Run("refuses a device link of another scheme", func(t *testing.T) {
		c.admin.device = &admin.AuthFlow{FlowID: "f-3", Connector: "linear", UserCode: "WDJB-MJHT", VerificationURI: "javascript:alert(1)"}
		t.Cleanup(func() { c.admin.device = nil })
		rec := c.post("/ui/connectors/linear/sign-in", ck, url.Values{"csrf": {csrf}})

		if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "javascript:") {
			t.Errorf("sign-in = %d, want 502 without the link", rec.Code)
		}
	})
}

// richAdmin answers every page with data, so each template runs every
// branch it has for a filled page.
type richAdmin struct{ fakeAdmin }

func (*richAdmin) Calls(context.Context, admin.CallsQuery) (admin.Calls, error) {
	ms := int64(812)
	small, kib, mib := int64(312), int64(48*1024), int64(1_258_291)

	return admin.Calls{Retention: "720h0m0s", Calls: []admin.Call{
		{StartedAt: time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC), TokenName: "default", Connector: "github", Tool: "create_issue", DurationMS: &ms, Result: "ok", ResultBytes: &mib},
		{StartedAt: time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC), Connector: "jira", Tool: "create_issue", Result: "failed", Case: "unauthorized",
			Error: "jira needs you to <b>sign in</b>."},
		{StartedAt: time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC), Connector: "github", Tool: "get_issue", Result: "ok", ResultBytes: &small},
		{StartedAt: time.Date(2026, 11, 9, 9, 30, 0, 0, time.UTC), Connector: "github", Tool: "list_issues", Result: "tool_error", ResultBytes: &kib},
	}}, nil
}

func (*richAdmin) Search(context.Context, admin.SearchRequest) (admin.SearchResult, error) {
	return admin.SearchResult{
		Embedding:   admin.SearchEmbedding{Mode: "on", Model: "m"},
		Tools:       []admin.SearchTool{{Name: "github.create_issue", Summary: "Create an issue.", Score: &admin.SearchScore{LexicalRank: 1, VectorRank: 2, Fused: 1.0/6 + 1.0/7}}},
		Unavailable: []admin.SearchUnavailable{{Connector: "linear", Status: "needs_auth", Advice: admin.Advice{Summary: "linear needs you to sign in."}}},
	}, nil
}

func (*richAdmin) Tools(_ context.Context, c string) (admin.ToolList, error) {
	return admin.ToolList{Connector: c, Tools: []admin.ToolEntry{
		{Name: "get_issue", Class: "read", Exposed: true, Definition: []byte(`{"name":"get_issue","description":"<b>Get</b> an issue.","inputSchema":{"type":"object"}}`)},
		{Name: "delete_repo", Class: "destructive"},
	}}, nil
}

func (*richAdmin) ListTokens(context.Context) (admin.TokenList, error) {
	used := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

	return admin.TokenList{Tokens: []admin.Token{{Name: "ci-bot", Connectors: []string{"github"}, LastUsedAt: &used}, {Name: "grok", Connectors: []string{"github"}, Remote: true}}}, nil
}

func TestPagesRender(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	sessions := webui.NewSessions(clock.Now)
	expires := time.Date(2026, 12, 28, 0, 0, 0, 0, time.UTC)
	fa := &richAdmin{fakeAdmin{status: admin.Status{Version: "v1.2.3", ConfigError: "line 3: unknown key", OrphanedCredentials: []string{"old"}, Connectors: []admin.Connector{
		{Name: "github", State: upstream.StateReady, Tools: 39, HiddenTools: 3, LastSync: &admin.Sync{At: clock.now.Add(-2 * time.Minute)}, TokenExpiresAt: &expires,
			Warnings: []admin.Warning{{Code: "token_expiring", Advice: admin.Advice{Summary: "The token expires soon.", Link: "http://127.0.0.1:7777/auth/github"}}}},
		{Name: "jira", State: upstream.StateUnreachable, Advice: &admin.Advice{Summary: "jira can't be reached."}},
		{Name: "postgres", State: upstream.StateStarting},
	}}}}
	h, err := webui.New(webui.Options{Admin: fa, Sessions: sessions, Now: clock.Now, Logger: zerolog.New(zerolog.NewTestWriter(t))})

	if err != nil {
		t.Fatalf("New: %v", err)
	}

	c := &console{h: h, sessions: sessions, admin: &fa.fakeAdmin, clock: clock}
	ck := c.signIn(t)

	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/ui/", []string{"Sign in again", "Can&#39;t reach", "Starting", "line 3: unknown key", "<code>old</code>", "+3 hidden", "/public/icons/github/dark.svg", "/public/icons/postgresql/default.svg", `<span class="letter">P</span>`,
			// The poll's hx-select and hx-target must not reach the connector links.
			`hx-disinherit="*"`}},
		{"/ui/connectors/github", []string{"get_issue", "cls-destructive", "Hidden by the tools list", "Sign out", "Dec 28",
			// A tool's definition opens under its row, indented and escaped;
			// one without a definition has no row to open.
			`<summary aria-controls="spec-0">get_issue`, `<tr class="why-row" id="spec-0">`, "\n  &#34;inputSchema&#34;: {\n    &#34;type&#34;: &#34;object&#34;",
			"&lt;b&gt;Get&lt;/b&gt; an issue.", `<td class="name">delete_repo</td>`}},
		{"/ui/calls?connector=github&result=failed", []string{"create_issue", "812 ms", "<th class=\"r hide-sm\">Size</th>", "312 B", "48 KiB", "1.2 MiB", "unauthorized", "Kept for 30 days", "Nov 11 09:30",
			`<details class="why"><summary aria-controls="why-1">`, `<tr class="why-row" id="why-1">`, "jira needs you to &lt;b&gt;sign in&lt;/b&gt;."}},
		{"/ui/search?q=open+a+ticket", []string{"github.", "0.3095", "w-100", "word rank 1, meaning rank 2", "linear: linear needs you to sign in."}},
		{"/ui/tokens", []string{"ci-bot", "1 h ago", `<details class="multi">`, `name="connector" value="jira"`, "/ui/static/console.js", ">remote</span>"}},
		{"/ui/tokens?done=revoked&name=ci-bot", []string{"Revoked ci-bot."}},
	} {
		rec := c.get(tc.path, ck)
		body := rec.Body.String()

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", tc.path, rec.Code)

			continue
		}

		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("GET %s: missing %q", tc.path, w)
			}
		}
	}

	// Only the remote token wears the badge.
	if got := strings.Count(c.get("/ui/tokens", ck).Body.String(), ">remote</span>"); got != 1 {
		t.Errorf("GET /ui/tokens shows %d remote badges, want 1 (grok only)", got)
	}

	if got := c.get("/ui/connectors/nope", ck).Code; got != http.StatusNotFound {
		t.Errorf("GET an unknown connector = %d, want 404", got)
	}

	if got := c.get("/ui/nowhere", ck).Code; got != http.StatusNotFound {
		t.Errorf("GET an unknown page = %d, want 404", got)
	}
}

func TestStatic(t *testing.T) {
	c := newConsole(t)

	for _, tc := range []struct{ path, contentType string }{
		{"/ui/static/app.css", "text/css; charset=utf-8"},
		{"/ui/static/vendor/htmx.min.js", "text/javascript; charset=utf-8"},
		{"/ui/static/console.js", "text/javascript; charset=utf-8"},
		{"/ui/static/fonts/geist.woff2", "font/woff2"},
		{"/ui/static/favicon.svg", "image/svg+xml"},
	} {
		rec := c.get(tc.path, nil)

		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != tc.contentType {
			t.Errorf("GET %s = %d %q, want 200 %q", tc.path, rec.Code, rec.Header().Get("Content-Type"), tc.contentType)
		}
	}
}
