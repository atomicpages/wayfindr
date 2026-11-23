package daemon_test

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/daemon"
	"github.com/atomicpages/wayfindr/internal/oauthserver"
	"github.com/atomicpages/wayfindr/internal/webui"
)

// Connecting a client through the running daemon (ADR 0022, design §7.10 and
// §7.11): arm from the console's side, the client's authorization request
// through a tunnel, the user's claim and approval in the console, the code
// exchange through the tunnel, and /mcp with the access token. A TLS server
// stands in for the client's ID metadata document, which the daemon fetches
// through Options.OAuthClient (the test seam that replaces its public-only
// client, which refuses a loopback address).

const (
	// The PKCE pair of RFC 7636, Appendix B.
	connectVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	connectChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	connectRedirect  = "https://chatgpt.example/connector/oauth/callback"
	connectIssuer    = "https://" + tunnelHost
	connectState     = "state-7f3a9c"
)

// connect is a running daemon with OAuth on for one client, whose document a
// TLS server holds.
type connect struct {
	r        running
	clientID string
	// cookie is the console's session, once signIn has run.
	cookie *http.Cookie
}

// startConnect serves a daemon with sign-in on for a test client; each tune
// adjusts the daemon's options first (a fake clock, say).
func startConnect(t *testing.T, tune ...func(*daemon.Options)) *connect {
	t.Helper()
	var clientID string

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":                  clientID,
			"client_name":                "ChatGPT",
			"redirect_uris":              []string{connectRedirect},
			"token_endpoint_auth_method": "none",
		})
	}))
	t.Cleanup(ts.Close)
	clientID = ts.URL + "/client.json"

	h := newHost(t)
	cfg := tokenConfig(t)
	cfg["server"] = map[string]any{
		"listen":     cfg["server"].(map[string]any)["listen"],
		"public_url": connectIssuer,
		"oauth":      map[string]any{"enabled": true, "clients": []string{clientID}},
	}
	opts := h.options(t)
	opts.OAuthClient = ts.Client()

	for _, fn := range tune {
		fn(&opts)
	}

	return &connect{r: serve(t, h, opts, cfg), clientID: clientID}
}

// reply is an answer to a request the test made without following redirects.
type reply struct {
	code     int
	header   http.Header
	body     string
	location string
}

// roundTrip makes req with no kept connection and no redirect followed.
func roundTrip(t *testing.T, req *http.Request) reply {
	t.Helper()

	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)

	if err != nil {
		t.Fatal(err)
	}

	return reply{code: resp.StatusCode, header: resp.Header, body: string(body), location: resp.Header.Get("Location")}
}

// forwarded sends method path to the daemon as a tunnel delivers it. form is
// the query for a GET and the body for a POST.
func (c *connect) forwarded(t *testing.T, method, path string, form url.Values) reply {
	t.Helper()
	target, body := "http://"+c.r.addr+path, ""

	if method == http.MethodGet && len(form) > 0 {
		target += "?" + form.Encode()
	}

	if method == http.MethodPost {
		body = form.Encode()
	}

	req, err := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))

	if err != nil {
		t.Fatal(err)
	}

	req.Host = tunnelHost
	req.Header.Set("X-Forwarded-Host", tunnelHost)
	req.Header.Set("X-Forwarded-For", "203.0.113.7")

	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	return roundTrip(t, req)
}

// console sends method path to the daemon's loopback address as a browser on
// this computer would: with the session cookie when there is one, and, for a
// POST, the form as the body and the same-origin Origin. header adds to or
// overrides the rest.
func (c *connect) console(t *testing.T, method, path string, form url.Values, header http.Header) reply {
	t.Helper()
	var body io.Reader

	if method == http.MethodPost {
		body = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+c.r.addr+path, body)

	if err != nil {
		t.Fatal(err)
	}

	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://"+c.r.addr)
	}

	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}

	for k, v := range header {
		req.Header[k] = v
	}

	return roundTrip(t, req)
}

// navigate is the headers of a browser following a link.
func navigate() http.Header {
	return http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}}
}

// signIn opens a console session, as `wayfindr ui` does.
func (c *connect) signIn(t *testing.T) {
	t.Helper()
	login, err := c.r.admin().UILogin(t.Context())

	if err != nil {
		t.Fatalf("UILogin: %v", err)
	}

	_, code, _ := strings.Cut(login.Path, "#")
	got := c.console(t, http.MethodPost, webui.LoginPath, url.Values{"code": {code}}, nil)

	if got.code != http.StatusSeeOther {
		t.Fatalf("console login = %d, want 303", got.code)
	}

	for _, ck := range (&http.Response{Header: got.header}).Cookies() {
		c.cookie = ck
	}

	if c.cookie == nil {
		t.Fatal("console login set no cookie")
	}
}

// authorize is the client's authorization request, through the tunnel.
func (c *connect) authorize(t *testing.T) reply {
	t.Helper()

	return c.forwarded(t, http.MethodGet, "/oauth/authorize", url.Values{
		"client_id":             {c.clientID},
		"response_type":         {"code"},
		"code_challenge":        {connectChallenge},
		"code_challenge_method": {"S256"},
		"resource":              {connectIssuer + "/mcp"},
		"state":                 {connectState},
		"redirect_uri":          {connectRedirect},
	})
}

// pendingID authorizes and returns the id the browser is sent to the approve
// page with.
func (c *connect) pendingID(t *testing.T) string {
	t.Helper()
	got := c.authorize(t)

	if got.code != http.StatusFound {
		t.Fatalf("authorize = %d %q, want 302 to the approve page", got.code, got.body)
	}

	u, err := url.Parse(got.location)

	if err != nil || u.Path != "/ui/oauth/approve" || u.Host != c.r.addr {
		t.Fatalf("authorize sent the browser to %q, want the approve page on %s", got.location, c.r.addr)
	}

	return u.Query().Get("req")
}

// approvePage is the signed-in navigation to the approve page for id.
func (c *connect) approvePage(t *testing.T, id string) reply {
	t.Helper()

	return c.console(t, http.MethodGet, "/ui/oauth/approve?req="+url.QueryEscape(id), nil, navigate())
}

// csrf is the token a console page's forms carry.
func csrfIn(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `name="csrf" value="`)

	if !ok {
		t.Fatalf("no csrf field in %q", body)
	}

	v, _, _ := strings.Cut(rest, `"`)

	return v
}

// suggested is the name the approve page fills in.
func suggested(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `id="grant-name" name="name" type="text" value="`)

	if !ok {
		t.Fatalf("no name field in %q", body)
	}

	v, _, _ := strings.Cut(rest, `"`)

	return v
}

// approve posts the approve form for id.
func (c *connect) approve(t *testing.T, id, csrf, name string, connectors ...string) reply {
	t.Helper()

	return c.console(t, http.MethodPost, "/ui/oauth/approve", url.Values{"csrf": {csrf}, "req": {id}, "name": {name}, "connector": connectors}, nil)
}

// codeFrom reads the authorization code, and checks state and iss, in a
// redirect back to the client.
func codeFrom(t *testing.T, redirect string) string {
	t.Helper()
	u, err := url.Parse(redirect)

	if err != nil || u.Scheme+"://"+u.Host+u.Path != connectRedirect {
		t.Fatalf("redirect %q is not to %s", redirect, connectRedirect)
	}

	q := u.Query()

	if q.Get("state") != connectState || q.Get("iss") != connectIssuer {
		t.Errorf("redirect has state %q and iss %q, want %q and %q", q.Get("state"), q.Get("iss"), connectState, connectIssuer)
	}

	return q.Get("code")
}

func TestConnectAClientEndToEnd(t *testing.T) {
	c := startConnect(t)
	c.signIn(t)

	// The user chooses Connect a client in the console.
	if _, err := c.r.admin().ArmOAuth(t.Context()); err != nil {
		t.Fatalf("ArmOAuth: %v", err)
	}

	id := c.pendingID(t)
	page := c.approvePage(t, id)

	if page.code != http.StatusOK {
		t.Fatalf("approve page = %d, want 200", page.code)
	}

	for _, want := range []string{
		"<dt>Client</dt><dd>ChatGPT</dd>", "<code>127.0.0.1:", "<code>chatgpt.example</code>", "reported by your tunnel", "<code>203.0.113.7</code>",
		"Approve only if you clicked Connect in ChatGPT in the last few minutes. If someone sent you this link, or this page opened in a tab you didn't open, choose Deny.",
	} {
		if !strings.Contains(page.body, want) {
			t.Errorf("approve page lacks %q", want)
		}
	}

	if got := suggested(t, page.body); got != "oauth" {
		t.Errorf("suggested name = %q, want oauth (the client is at an IP address)", got)
	}

	done := c.approve(t, id, csrfIn(t, page.body), "chatgpt-test", "files")

	if done.code != http.StatusSeeOther || done.header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("approve = %d, Referrer-Policy %q; want 303 and no-referrer", done.code, done.header.Get("Referrer-Policy"))
	}

	code := codeFrom(t, done.location)

	if code == "" {
		t.Fatal("the redirect has no code")
	}

	// The client exchanges the code, through the tunnel.
	tok := c.forwarded(t, http.MethodPost, "/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {c.clientID},
		"code":          {code},
		"redirect_uri":  {connectRedirect},
		"code_verifier": {connectVerifier},
		"resource":      {connectIssuer + "/mcp"},
	})

	var granted struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Type    string `json:"token_type"`
	}

	if err := json.Unmarshal([]byte(tok.body), &granted); tok.code != http.StatusOK || err != nil || granted.Access == "" || granted.Refresh == "" {
		t.Fatalf("token = %d %q (%v), want 200 with an access and a refresh token", tok.code, tok.body, err)
	}

	list, err := c.r.admin().ListTokens(t.Context())

	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}

	var got []admin.Token

	for _, tk := range list.Tokens {
		if tk.Kind == admin.TokenKindOAuth {
			got = append(got, admin.Token{Name: tk.Name, Kind: tk.Kind, Connectors: tk.Connectors, Client: tk.Client, Remote: tk.Remote})
		}
	}

	want := []admin.Token{{Name: "chatgpt-test", Kind: admin.TokenKindOAuth, Connectors: []string{"files"}, Client: c.clientID, Remote: true}}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("grants (-want +got):\n%s", diff)
	}

	// The client refreshes through the tunnel: the new pair is served, and the
	// access token it replaces is refused at once.
	ref := c.forwarded(t, http.MethodPost, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {c.clientID},
		"refresh_token": {granted.Refresh},
		"resource":      {connectIssuer + "/mcp"},
	})

	var refreshed struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}

	if err := json.Unmarshal([]byte(ref.body), &refreshed); ref.code != http.StatusOK || err != nil || refreshed.Access == "" || refreshed.Refresh == "" {
		t.Fatalf("refresh = %d %q (%v), want 200 with a new access and refresh token", ref.code, ref.body, err)
	}

	if refreshed.Access == granted.Access || refreshed.Refresh == granted.Refresh {
		t.Error("the refresh returned a token it had already issued")
	}

	if code, body := c.r.initialize(t, refreshed.Access, requestWays()["tunnel"]); code != http.StatusOK || !strings.Contains(body, `"serverInfo"`) {
		t.Fatalf("/mcp with the refreshed access token through the tunnel = %d %q, want served", code, body)
	}

	if code, body := c.r.initialize(t, granted.Access, requestWays()["tunnel"]); code != http.StatusUnauthorized {
		t.Errorf("/mcp with the access token the refresh replaced = %d %q, want 401", code, body)
	}

	// From here on, the live pair is the refreshed one.
	granted.Access, granted.Refresh = refreshed.Access, refreshed.Refresh

	// The access token works through the tunnel, and not as the console's
	// admin bearer.
	if code, body := c.r.initialize(t, granted.Access, requestWays()["tunnel"]); code != http.StatusOK || !strings.Contains(body, `"serverInfo"`) {
		t.Fatalf("/mcp with the access token through the tunnel = %d %q, want served", code, body)
	}

	if code := c.r.statusAs(t, granted.Access); code != http.StatusForbidden {
		t.Errorf("/admin/status with the access token = %d, want 403", code)
	}

	// Revoking the grant ends it at once.
	if _, err := c.r.admin().RevokeToken(t.Context(), "chatgpt-test"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	if code, body := c.r.initialize(t, granted.Access, requestWays()["tunnel"]); code != http.StatusUnauthorized {
		t.Errorf("/mcp with the revoked access token = %d %q, want 401", code, body)
	}
}

// TestAPendingAuthorizationExpiresWithTheArm: a request the client made while
// armed is not claimable once PendingTTL has passed, whatever the user does
// next: the approve page says it expired (404), and nothing is let through
// until the user arms again.
func TestAPendingAuthorizationExpiresWithTheArm(t *testing.T) {
	clock := newFakeClock()
	c := startConnect(t, func(o *daemon.Options) { o.Clock = clock })
	c.signIn(t)

	if _, err := c.r.admin().ArmOAuth(t.Context()); err != nil {
		t.Fatalf("ArmOAuth: %v", err)
	}

	id := c.pendingID(t)

	clock.advance(oauthserver.PendingTTL + time.Second)

	if page := c.approvePage(t, id); page.code != http.StatusNotFound || !strings.Contains(page.body, "expired or was already answered") {
		t.Errorf("claim after PendingTTL = %d %q, want the 404 expired page", page.code, page.body)
	}

	if got := c.authorize(t); got.code != http.StatusBadRequest || !strings.Contains(got.body, `data-case="not_armed"`) {
		t.Errorf("authorize after the arm ended = %d, want the not_armed page", got.code)
	}
}

// TestApprovePageWithoutASessionNeverClaims: a browser sent to the approve
// page from another site has no session (SameSite=Strict), and gets the
// Continue page. It must not claim: the arm survives, and the server's
// pending authorizations stay unclaimed, whatever id it was sent with.
func TestApprovePageWithoutASessionNeverClaims(t *testing.T) {
	c := startConnect(t)

	if _, err := c.r.admin().ArmOAuth(t.Context()); err != nil {
		t.Fatalf("ArmOAuth: %v", err)
	}

	id := c.pendingID(t)
	got := c.approvePage(t, id)

	if got.code != http.StatusOK || !strings.Contains(got.body, ">Continue<") || strings.Contains(got.body, "ChatGPT") {
		t.Fatalf("approve page without a session = %d %q, want the Continue page", got.code, got.body)
	}

	// The arm survived: another request is still let through, which a claim
	// would have ended.
	second := c.pendingID(t)

	if second == id {
		t.Fatal("a second authorize reused the first one's id")
	}

	// And both are still unclaimed: the signed-in user claims the first, which
	// drops the second. Two arrived, so the page offers only Deny.
	c.signIn(t)

	if page := c.approvePage(t, id); page.code != http.StatusOK || !strings.Contains(page.body, "2 connection requests arrived") {
		t.Fatalf("the signed-in claim of the first request = %d, want the approve page for a contested request", page.code)
	}

	if page := c.approvePage(t, second); page.code != http.StatusNotFound {
		t.Errorf("the signed-in claim of the dropped second request = %d, want 404", page.code)
	}

	// The claim consumed the arm.
	if got := c.authorize(t); got.code != http.StatusBadRequest || !strings.Contains(got.body, `data-case="not_armed"`) {
		t.Errorf("authorize after the claim = %d, want the not_armed page", got.code)
	}
}

func TestApproveRefusesWhatIsNotTheConsole(t *testing.T) {
	c := startConnect(t)
	c.signIn(t)

	if _, err := c.r.admin().ArmOAuth(t.Context()); err != nil {
		t.Fatalf("ArmOAuth: %v", err)
	}

	id := c.pendingID(t)
	page := c.approvePage(t, id)
	csrf := csrfIn(t, page.body)

	t.Run("no CSRF token, or another's", func(t *testing.T) {
		for _, token := range []string{"", "not-the-token"} {
			if got := c.approve(t, id, token, "chatgpt", "files"); got.code != http.StatusForbidden || got.location != "" {
				t.Errorf("approve with csrf %q = %d to %q, want 403", token, got.code, got.location)
			}
		}
	})

	t.Run("a post from another site", func(t *testing.T) {
		got := c.console(t, http.MethodPost, "/ui/oauth/approve", url.Values{"csrf": {csrf}, "req": {id}, "name": {"chatgpt"}, "connector": {"files"}},
			http.Header{"Origin": {"https://evil.example"}})

		if got.code != http.StatusForbidden || got.location != "" {
			t.Errorf("approve from another origin = %d to %q, want 403", got.code, got.location)
		}
	})

	t.Run("no session", func(t *testing.T) {
		out := &connect{r: c.r, clientID: c.clientID}
		got := out.approve(t, id, csrf, "chatgpt", "files")

		if got.code != http.StatusSeeOther || got.location != "/ui/login" {
			t.Errorf("approve without a session = %d to %q, want 303 to the login page", got.code, got.location)
		}
	})

	t.Run("a taken name is 409 and the request stays claimed", func(t *testing.T) {
		if _, err := c.r.admin().CreateToken(t.Context(), admin.TokenSpec{Name: "taken", Connectors: []string{"files"}}); err != nil {
			t.Fatal(err)
		}

		got := c.approve(t, id, csrf, "taken", "files")

		if got.code != http.StatusConflict || got.location != "" || !strings.Contains(got.body, "already exists") || !strings.Contains(got.body, "Approve only if") {
			t.Errorf("approve with a taken name = %d to %q, want the page again with 409", got.code, got.location)
		}
	})

	t.Run("a bad name is 400", func(t *testing.T) {
		for name, want := range map[string]string{"Not A Name": "must match", "default": "reserved"} {
			got := c.approve(t, id, csrf, name, "files")

			if got.code != http.StatusBadRequest || !strings.Contains(got.body, want) {
				t.Errorf("approve as %q = %d, want 400 mentioning %q", name, got.code, want)
			}
		}
	})

	t.Run("an unknown connector is 400", func(t *testing.T) {
		if got := c.approve(t, id, csrf, "chatgpt", "nosuch"); got.code != http.StatusBadRequest {
			t.Errorf("approve with an unknown connector = %d, want 400", got.code)
		}
	})

	t.Run("after all that, the same request is still approvable", func(t *testing.T) {
		got := c.approve(t, id, csrf, "chatgpt", "files")

		if got.code != http.StatusSeeOther || codeFrom(t, got.location) == "" {
			t.Fatalf("approve = %d to %q, want 303 to the client with a code", got.code, got.location)
		}

		// Answered once.
		if again := c.approve(t, id, csrf, "chatgpt-2", "files"); again.code != http.StatusNotFound {
			t.Errorf("a second approve = %d, want 404", again.code)
		}
	})
}

func TestDenyRedirectsWithAccessDenied(t *testing.T) {
	c := startConnect(t)
	c.signIn(t)

	if _, err := c.r.admin().ArmOAuth(t.Context()); err != nil {
		t.Fatalf("ArmOAuth: %v", err)
	}

	id := c.pendingID(t)
	csrf := csrfIn(t, c.approvePage(t, id).body)
	got := c.console(t, http.MethodPost, "/ui/oauth/deny", url.Values{"csrf": {csrf}, "req": {id}}, nil)

	if got.code != http.StatusSeeOther {
		t.Fatalf("deny = %d, want 303", got.code)
	}

	u, err := url.Parse(got.location)

	if err != nil || u.Scheme+"://"+u.Host+u.Path != connectRedirect {
		t.Fatalf("deny redirected to %q", got.location)
	}

	if q := u.Query(); q.Get("error") != "access_denied" || q.Get("state") != connectState || q.Get("iss") != connectIssuer || q.Get("code") != "" {
		t.Errorf("deny redirect query = %v, want error=access_denied with state and iss and no code", q)
	}

	if got := c.approve(t, id, csrf, "chatgpt", "files"); got.code != http.StatusNotFound {
		t.Errorf("approve after deny = %d, want 404", got.code)
	}
}

// TestASecondRequestDuringTheArmIsNeverApproved: only the user's own request
// is expected while armed. When two arrive through the tunnel, the one the
// console claims may be a stranger's, opened in a tab the user didn't open:
// the page says so and offers only Deny, and an approve posted anyway is
// refused with 409 and creates no grant. The user denies, arms again, and the
// retry is approved.
func TestASecondRequestDuringTheArmIsNeverApproved(t *testing.T) {
	c := startConnect(t)
	c.signIn(t)

	if _, err := c.r.admin().ArmOAuth(t.Context()); err != nil {
		t.Fatalf("ArmOAuth: %v", err)
	}

	c.pendingID(t)
	id := c.pendingID(t)
	page := c.approvePage(t, id)

	const contested = "2 connection requests arrived after you selected Connect a client. Only one was expected, so this can't be approved. Choose Deny, then select Connect a client and try again in ChatGPT."

	if page.code != http.StatusOK || !strings.Contains(page.body, contested) || strings.Contains(page.body, ">Approve</button>") {
		t.Fatalf("approve page = %d %q, want the contested page without Approve", page.code, page.body)
	}

	csrf := csrfIn(t, page.body)

	if got := c.approve(t, id, csrf, "chatgpt", "files"); got.code != http.StatusConflict || got.location != "" || !strings.Contains(got.body, contested) {
		t.Errorf("approve of a contested request = %d to %q, want 409 with the contested page", got.code, got.location)
	}

	if names := c.r.grantNames(t); len(names) != 0 {
		t.Errorf("grants after a refused approve = %v, want none", names)
	}

	if got := c.console(t, http.MethodPost, "/ui/oauth/deny", url.Values{"csrf": {csrf}, "req": {id}}, nil); got.code != http.StatusSeeOther || !strings.Contains(got.location, "error=access_denied") {
		t.Fatalf("deny = %d to %q, want 303 with access_denied", got.code, got.location)
	}

	if _, err := c.r.admin().ArmOAuth(t.Context()); err != nil {
		t.Fatalf("ArmOAuth: %v", err)
	}

	retry := c.pendingID(t)

	if got := c.approve(t, retry, csrfIn(t, c.approvePage(t, retry).body), "chatgpt", "files"); got.code != http.StatusSeeOther || codeFrom(t, got.location) == "" {
		t.Errorf("approve of the retry = %d to %q, want 303 with a code", got.code, got.location)
	}
}

// TestTurningOAuthOffAndOnDisarms: a request claimed before sign-in was
// turned off is not answered once it is on again, even within PendingTTL,
// and nothing is armed.
func TestTurningOAuthOffAndOnDisarms(t *testing.T) {
	c := startConnect(t)
	c.signIn(t)

	if _, err := c.r.admin().ArmOAuth(t.Context()); err != nil {
		t.Fatalf("ArmOAuth: %v", err)
	}

	id := c.pendingID(t)
	csrf := csrfIn(t, c.approvePage(t, id).body)

	// Armed again, so an arm would be in force but for the toggle.
	if _, err := c.r.admin().ArmOAuth(t.Context()); err != nil {
		t.Fatalf("ArmOAuth: %v", err)
	}

	for _, on := range []bool{false, true} {
		if _, err := c.r.admin().SetOAuthEnabled(t.Context(), on); err != nil {
			t.Fatalf("SetOAuthEnabled(%v): %v", on, err)
		}
	}

	if got := c.approvePage(t, id); got.code != http.StatusNotFound {
		t.Errorf("approve page after the toggle = %d, want 404", got.code)
	}

	if got := c.approve(t, id, csrf, "chatgpt", "files"); got.code != http.StatusNotFound {
		t.Errorf("approve after the toggle = %d, want 404", got.code)
	}

	if got := c.authorize(t); got.code != http.StatusBadRequest || !strings.Contains(got.body, `data-case="not_armed"`) {
		t.Errorf("authorize after the toggle = %d, want the not_armed page", got.code)
	}
}

// TestApprovePageSuggestsAFreeName is the seam the suggestion is tested
// through: names taken by a client token or an OAuth grant are skipped, and
// when every suggestion is taken the field is empty.
func TestApprovePageSuggestsAFreeName(t *testing.T) {
	c := startConnect(t)
	c.signIn(t)

	ask := func() string {
		t.Helper()

		if _, err := c.r.admin().ArmOAuth(t.Context()); err != nil {
			t.Fatalf("ArmOAuth: %v", err)
		}

		return suggested(t, c.approvePage(t, c.pendingID(t)).body)
	}

	if got := ask(); got != "oauth" {
		t.Fatalf("suggestion on an empty store = %q, want oauth", got)
	}

	// A client token takes oauth, and a grant takes oauth-2.
	if _, err := c.r.admin().CreateToken(t.Context(), admin.TokenSpec{Name: "oauth", Connectors: []string{"files"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.r.admin().ArmOAuth(t.Context()); err != nil {
		t.Fatalf("ArmOAuth: %v", err)
	}

	id := c.pendingID(t)
	page := c.approvePage(t, id)

	if got := suggested(t, page.body); got != "oauth-2" {
		t.Fatalf("suggestion with a token in the way = %q, want oauth-2", got)
	}

	if got := c.approve(t, id, csrfIn(t, page.body), "oauth-2", "files"); got.code != http.StatusSeeOther {
		t.Fatalf("approve = %d, want 303", got.code)
	}

	if got := ask(); got != "oauth-3" {
		t.Fatalf("suggestion with a token and a grant in the way = %q, want oauth-3", got)
	}

	for i := 3; i <= 99; i++ {
		if _, err := c.r.admin().CreateToken(t.Context(), admin.TokenSpec{Name: fmt.Sprintf("oauth-%d", i), Connectors: []string{"files"}}); err != nil {
			t.Fatal(err)
		}
	}

	if got := ask(); got != "" {
		t.Errorf("suggestion with all 99 taken = %q, want none", got)
	}
}

func TestArmWhileOAuthIsOffIs409(t *testing.T) {
	c := startConnect(t)

	if _, err := c.r.admin().SetOAuthEnabled(t.Context(), false); err != nil {
		t.Fatalf("SetOAuthEnabled(false): %v", err)
	}

	_, err := c.r.admin().ArmOAuth(t.Context())
	ae := (*admin.APIError)(nil)

	if !errors.As(err, &ae) || ae.Status != http.StatusConflict || !strings.Contains(ae.Message, "Turn on sign-in for cloud agents first.") {
		t.Errorf("ArmOAuth while off = %v, want 409 with the advice", err)
	}

	// Nothing was armed: turned on again, a request is still refused.
	if _, err := c.r.admin().SetOAuthEnabled(t.Context(), true); err != nil {
		t.Fatalf("SetOAuthEnabled(true): %v", err)
	}

	if got := c.authorize(t); got.code != http.StatusBadRequest || !strings.Contains(got.body, `data-case="not_armed"`) {
		t.Errorf("authorize after a refused arm = %d, want the not_armed page", got.code)
	}
}

// TestApproveRoutesAreLoopbackOnly: through a tunnel the approve and deny
// pages and the admin routes of the requests are the one 404.
func TestApproveRoutesAreLoopbackOnly(t *testing.T) {
	c := startConnect(t)

	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/ui/oauth/approve?req=r1"},
		{http.MethodPost, "/ui/oauth/approve"},
		{http.MethodPost, "/ui/oauth/deny"},
		{http.MethodGet, "/ui/oauth/deny"},
		{http.MethodPost, "/ui/cloud-agents/arm"},
		{http.MethodPost, "/ui/cloud-agents/oauth"},
		{http.MethodPost, "/admin/oauth/arm"},
		{http.MethodGet, "/admin/oauth/requests/r1"},
		{http.MethodPost, "/admin/oauth/requests/r1/approve"},
		{http.MethodPost, "/admin/oauth/requests/r1/deny"},
	} {
		got := c.forwarded(t, rt.method, rt.path, nil)

		if got.code != http.StatusNotFound || got.body != "404 page not found\n" {
			t.Errorf("%s %s through the tunnel = %d %q, want the 404", rt.method, rt.path, got.code, got.body)
		}
	}
}
