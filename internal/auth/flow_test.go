package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// The tests in this file run the authorization-code flow (design §7.3,
// ADR 0007) at the flow entry's seam (auth.Flows): Start, the browser's
// trip through the fake authorization server, HandleCallback, and Wait.

// lookup is the harness's running config: conn, unless removed.
func (h *brokerHarness) lookup(name string) (config.Connector, bool) {
	if h.removed || name != h.conn.Name {
		return config.Connector{}, false
	}

	return h.conn, true
}

// flows is the flow entry over the harness's broker and running config.
func (h *brokerHarness) flows() *auth.Flows { return auth.NewFlows(h.broker, h.lookup) }

// consent plays the browser: it opens the authorize URL, which the fake
// answers by redirecting straight back as if the user consented, and
// returns the callback URL it redirected to.
func consent(t *testing.T, authorizeURL string) *url.URL {
	t.Helper()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := send(t, noRedirect, http.MethodGet, authorizeURL, "")

	if err != nil {
		t.Fatalf("authorize: %v", err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302", resp.StatusCode)
	}

	loc, err := url.Parse(resp.Header.Get("Location"))

	if err != nil {
		t.Fatalf("authorize redirect: %v", err)
	}

	return loc
}

// callback delivers the browser's callback request to the broker and
// returns the status and page.
func (h *brokerHarness) callback(t *testing.T, u *url.URL) (int, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, u.String(), nil)
	rec := httptest.NewRecorder()
	h.flows().HandleCallback(rec, req)

	return rec.Code, rec.Body.String()
}

// start starts a flow for the connector's current config.
func (h *brokerHarness) start(t *testing.T) auth.Flow {
	t.Helper()
	f, err := h.flows().Start(t.Context(), h.conn.Name, auth.StartOptions{})

	if err != nil {
		t.Fatalf("StartFlow: %v", err)
	}

	return f
}

// result returns the flow's result without waiting.
func (h *brokerHarness) result(t *testing.T, id string) auth.FlowResult {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res, ok := h.flows().Wait(ctx, id)

	if !ok {
		t.Fatalf("WaitFlow(%s): unknown flow", id)
	}

	return res
}

// codeExchanges are the authorization_code grants the fake saw, by error.
func (h *brokerHarness) codeExchanges() []string {
	var out []string

	for _, tr := range h.as.TokenRequests() {
		if tr.GrantType == "authorization_code" {
			out = append(out, tr.Error)
		}
	}

	return out
}

// lastAuthorization is the last authorize request the fake saw.
func (h *brokerHarness) lastAuthorization(t *testing.T) fakeas.Authorization {
	t.Helper()
	as := h.as.Authorizations()

	if len(as) == 0 {
		t.Fatal("the authorization server saw no authorize request")
	}

	return as[len(as)-1]
}

// TestCallbackPageSendsNoReferrer: the callback URL holds the code, so its
// page must not let the browser send that URL on as a Referer.
func TestCallbackPageSendsNoReferrer(t *testing.T) {
	h := newBrokerHarness(t)
	f := h.start(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, consent(t, f.AuthorizeURL).String(), nil)
	rec := httptest.NewRecorder()
	h.flows().HandleCallback(rec, req)

	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("callback Referrer-Policy = %q, want no-referrer", got)
	}
}

func caseOf(err error) auth.Case {
	if e, ok := auth.AsError(err); ok {
		return e.Case
	}

	return ""
}

func TestFlowStoresTheTokenAndTheConnectorIsAuthorized(t *testing.T) {
	h := newBrokerHarness(t)

	f := h.start(t)
	status, page := h.callback(t, consent(t, f.AuthorizeURL))

	if status != http.StatusOK || !strings.Contains(page, "You can close this tab") {
		t.Errorf("callback = %d %q, want 200 and the close-this-tab page", status, page)
	}

	if got := h.result(t, f.ID); got.State != auth.FlowDone || got.Connector != "linear" || got.Err != nil {
		t.Errorf("WaitFlow = %+v, want linear done", got)
	}

	if want := h.clock.Now().Add(10 * time.Minute); !f.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v (10 minutes)", f.ExpiresAt, want)
	}

	a := h.lastAuthorization(t)

	if a.RedirectURI != defaultRedirect || a.CodeChallengeMethod != "S256" || a.Resource != h.as.ResourceURL || len(a.State) < 43 {
		t.Errorf("authorize request = %+v, want the loopback redirect, S256, the resource, and a 32-byte state", a)
	}

	if diff := cmp.Diff([]string{""}, h.codeExchanges()); diff != "" {
		t.Errorf("code exchanges (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff([]string{"linear"}, h.changed.get()); diff != "" {
		t.Errorf("OnCredentialChanged calls (-want +got):\n%s", diff)
	}

	// The stored token authorizes the connector's requests.
	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Errorf("resource through the broker = %d, %v; want 200", status, err)
	}

	for _, secret := range []string{"code=", "state=", "code_verifier"} {
		if strings.Contains(h.logs.String(), secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
}

func TestFlowStateIsSingleUse(t *testing.T) {
	h := newBrokerHarness(t)
	f := h.start(t)
	cb := consent(t, f.AuthorizeURL)

	if status, _ := h.callback(t, cb); status != http.StatusOK {
		t.Fatalf("first callback = %d, want 200", status)
	}

	status, page := h.callback(t, cb)

	if status != http.StatusBadRequest || !strings.Contains(page, "unknown or was already used") {
		t.Errorf("replayed callback = %d %q, want 400 unknown or used", status, page)
	}

	if diff := cmp.Diff([]string{""}, h.codeExchanges()); diff != "" {
		t.Errorf("code exchanges (-want +got): the replay must not exchange again\n%s", diff)
	}

	if got := h.result(t, f.ID); got.State != auth.FlowDone {
		t.Errorf("WaitFlow after the replay = %+v, want done still", got)
	}
}

func TestFlowExpiresAfterTenMinutes(t *testing.T) {
	h := newBrokerHarness(t)
	f := h.start(t)
	cb := consent(t, f.AuthorizeURL)

	h.clock.Advance(10*time.Minute + time.Second)
	status, page := h.callback(t, cb)

	if status != http.StatusBadRequest || !strings.Contains(page, "expired") {
		t.Errorf("late callback = %d %q, want 400 expired", status, page)
	}

	got := h.result(t, f.ID)

	if got.State != auth.FlowExpired || caseOf(got.Err) != auth.CaseFlowExpired {
		t.Errorf("WaitFlow = %+v, want expired (flow_expired)", got)
	}

	if n := len(h.codeExchanges()); n != 0 {
		t.Errorf("code exchanges = %d, want none for an expired flow", n)
	}

	if n := len(h.changed.get()); n != 0 {
		t.Errorf("OnCredentialChanged called %d times, want none", n)
	}
}

func TestWaitFlowReportsAPendingFlowWhenItsContextEnds(t *testing.T) {
	h := newBrokerHarness(t)
	f := h.start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	got, ok := h.flows().Wait(ctx, f.ID)

	if !ok || got.State != auth.FlowPending {
		t.Errorf("WaitFlow = %+v, %v; want pending", got, ok)
	}

	if _, ok := h.flows().Wait(ctx, "no-such-flow"); ok {
		t.Error("WaitFlow(unknown id) reported a flow")
	}
}

func TestWaitFlowReturnsWhenTheCallbackCompletesIt(t *testing.T) {
	h := newBrokerHarness(t)
	f := h.start(t)
	got := make(chan auth.FlowResult, 1)
	go func() {
		res, _ := h.flows().Wait(t.Context(), f.ID)
		got <- res
	}()

	h.callback(t, consent(t, f.AuthorizeURL))

	select {
	case res := <-got:
		if res.State != auth.FlowDone {
			t.Errorf("WaitFlow = %+v, want done", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitFlow did not return after the callback")
	}
}

// ADR 0007's PKCE rules.
func TestFlowPKCE(t *testing.T) {
	t.Run("a discovered connector uses S256 when it is advertised", func(t *testing.T) {
		h := newBrokerHarness(t)
		h.conn.Auth.ClientID = ""

		f := h.start(t)

		if status, page := h.callback(t, consent(t, f.AuthorizeURL)); status != http.StatusOK {
			t.Fatalf("callback = %d %q, want 200", status, page)
		}

		if a := h.lastAuthorization(t); a.CodeChallengeMethod != "S256" || a.Error != "" {
			t.Errorf("authorize request = %+v, want S256", a)
		}

		if got := h.result(t, f.ID); got.State != auth.FlowDone {
			t.Errorf("WaitFlow = %+v, want done", got)
		}
	})
	t.Run("a discovered connector without S256 advertised is an auth error", func(t *testing.T) {
		h := newBrokerHarness(t, fakeas.WithoutPKCEAdvertised())
		h.conn.Auth.ClientID = ""

		_, err := h.flows().Start(t.Context(), h.conn.Name, auth.StartOptions{})

		e, ok := auth.AsError(err)

		if !ok || e.Kind != auth.KindConfig || e.Case != auth.CasePKCEUnsupported {
			t.Fatalf("StartFlow = %v, want config pkce_unsupported", err)
		}

		if n := len(h.as.Clients()); n != 1 {
			t.Errorf("clients = %d, want only the pre-registered one: no DCR for a flow that cannot run", n)
		}
	})
	t.Run("a discovered connector on a legacy server without metadata uses S256", func(t *testing.T) {
		h := newBrokerHarness(t, fakeas.WithoutMetadata())
		h.conn.Auth.ClientID = ""

		f := h.start(t)

		if status, page := h.callback(t, consent(t, f.AuthorizeURL)); status != http.StatusOK {
			t.Fatalf("callback = %d %q, want 200", status, page)
		}

		if a := h.lastAuthorization(t); a.CodeChallengeMethod != "S256" || a.Error != "" {
			t.Errorf("authorize request = %+v, want S256", a)
		}

		if got := h.result(t, f.ID); got.State != auth.FlowDone {
			t.Errorf("WaitFlow = %+v, want done", got)
		}
	})
	t.Run("a manual connector uses S256 even when it is not advertised", func(t *testing.T) {
		h := newBrokerHarness(t, fakeas.WithoutPKCEAdvertised())

		f := h.start(t)
		h.callback(t, consent(t, f.AuthorizeURL))

		if a := h.lastAuthorization(t); a.CodeChallengeMethod != "S256" {
			t.Errorf("authorize request = %+v, want S256", a)
		}

		if got := h.result(t, f.ID); got.State != auth.FlowDone {
			t.Errorf("WaitFlow = %+v, want done", got)
		}
	})
	t.Run("a manual connector with pkce off and a client_secret sends no challenge", func(t *testing.T) {
		h := newBrokerHarness(t, fakeas.WithoutPKCEAdvertised(), fakeas.WithClient("confidential", "s3cret", defaultRedirect))
		h.conn.Auth.ClientID = "confidential"
		h.conn.Auth.ClientSecret = "s3cret"
		h.conn.Auth.PKCE = false

		f := h.start(t)
		h.callback(t, consent(t, f.AuthorizeURL))

		if a := h.lastAuthorization(t); a.CodeChallengeMethod != "" || a.Error != "" {
			t.Errorf("authorize request = %+v, want no code challenge", a)
		}

		if got := h.result(t, f.ID); got.State != auth.FlowDone {
			t.Errorf("WaitFlow = %+v, want done", got)
		}
	})
}

func TestFlowChecksTheIssuerOfTheCallback(t *testing.T) {
	t.Run("a different iss fails the flow", func(t *testing.T) {
		h := newBrokerHarness(t)
		f := h.start(t)
		cb := consent(t, f.AuthorizeURL)
		q := cb.Query()
		q.Set("iss", "https://attacker.example")
		cb.RawQuery = q.Encode()

		status, _ := h.callback(t, cb)

		got := h.result(t, f.ID)

		if status != http.StatusBadRequest || got.State != auth.FlowFailed || caseOf(got.Err) != auth.CaseCallbackIssuerMismatch {
			t.Errorf("callback = %d, WaitFlow = %+v; want 400 and failed callback_issuer_mismatch", status, got)
		}

		if n := len(h.codeExchanges()); n != 0 {
			t.Errorf("code exchanges = %d, want none: the code went to the wrong issuer", n)
		}

		// A possible mix-up attack: the advice never suggests turning the
		// check off (wayfindr-zgp.19).
		if e, ok := auth.AsError(got.Err); !ok {
			t.Errorf("WaitFlow error = %v, want an *auth.Error", got.Err)
		} else if a := auth.Advise(e); strings.Contains(a.Summary+a.Action, "validate_issuer") ||
			!strings.Contains(a.Summary, "mix-up") || a.Link == "" {
			t.Errorf("Advise = %+v, want a mix-up warning with the link, and no validate_issuer", a)
		}
	})
	t.Run("a missing iss fails when the server advertises it", func(t *testing.T) {
		h := newBrokerHarness(t)
		f := h.start(t)
		cb := consent(t, f.AuthorizeURL)
		q := cb.Query()
		q.Del("iss")
		cb.RawQuery = q.Encode()

		h.callback(t, cb)

		if got := h.result(t, f.ID); got.State != auth.FlowFailed || caseOf(got.Err) != auth.CaseCallbackIssuerMismatch {
			t.Errorf("WaitFlow = %+v, want failed callback_issuer_mismatch", got)
		}
	})
	t.Run("validate_issuer = false skips the check and says so", func(t *testing.T) {
		h := newBrokerHarness(t, fakeas.WithIssuerMismatch())
		h.conn.Auth.ValidateIssuer = false

		f := h.start(t)
		h.callback(t, consent(t, f.AuthorizeURL))

		if got := h.result(t, f.ID); got.State != auth.FlowDone {
			t.Errorf("WaitFlow = %+v, want done", got)
		}

		if !strings.Contains(h.logs.String(), "issuer validation skipped") {
			t.Error("the skipped check is not logged")
		}
	})
}

func TestFlowDeniedByTheUserFails(t *testing.T) {
	h := newBrokerHarness(t)
	f := h.start(t)
	cb := consent(t, f.AuthorizeURL)
	q := url.Values{"error": {"access_denied"}, "state": {cb.Query().Get("state")}, "iss": {h.as.MetadataIssuer}}
	cb.RawQuery = q.Encode()

	status, page := h.callback(t, cb)

	got := h.result(t, f.ID)
	e, ok := auth.AsError(got.Err)

	if !ok || got.State != auth.FlowFailed || e.Case != auth.CaseAuthorizationDenied || e.ProviderCode != "access_denied" {
		t.Errorf("WaitFlow = %+v, want failed authorization_denied (access_denied)", got)
	}

	if status != http.StatusBadRequest || !strings.Contains(page, "was not granted") {
		t.Errorf("callback = %d %q, want 400 with the advice", status, page)
	}
}

func TestFlowScopes(t *testing.T) {
	for _, tt := range []struct {
		name   string
		opts   []fakeas.Option
		config []string
		want   string
	}{
		{"the challenge's scope first", []fakeas.Option{fakeas.WithChallengeScope("a", "b"), fakeas.WithScopesSupported("x")}, []string{"c"}, "a b"},
		{"then PRM scopes_supported", []fakeas.Option{fakeas.WithScopesSupported("x", "y")}, []string{"c"}, "x y"},
		{"then the config", nil, []string{"c", "d"}, "c d"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newBrokerHarness(t, tt.opts...)
			h.conn.Auth.Scopes = tt.config

			h.start(t)

			if got := scopeOf(t, h); got != tt.want {
				t.Errorf("scope = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("plus the step-up additions", func(t *testing.T) {
		h := newBrokerHarness(t, fakeas.WithRequiredScopes("write"))
		h.conn.Auth.Scopes = []string{"read"}
		h.seed(t, "read", time.Hour)

		if _, err := h.get(t); caseOf(err) != auth.CaseStepUp {
			t.Fatalf("get = %v, want step_up", err)
		}

		h.start(t)

		if got := scopeOf(t, h); got != "read write" {
			t.Errorf("scope = %q, want %q", got, "read write")
		}
	})
}

// A Streamable HTTP server without an SSE stream answers the flow's probe
// GET with 405. The flow then probes with an initialize POST, and its 401
// challenge still names the scopes and the PRM, which here sits at no
// well-known URL, under an issuer with a path, so the flow finds the
// authorization server only through the challenge (wayfindr-zgp.11).
func TestFlowReadsTheChallengeOfAServerWithoutGET(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithoutGET(), fakeas.WithChallengeScope("a", "b"),
		fakeas.WithChallengeResourceMetadata(), fakeas.WithPRMLocation(fakeas.PRMUnlisted),
		fakeas.WithIssuerPath("/tenant1"), fakeas.WithScopesSupported("x"))

	f := h.start(t)

	if !strings.HasPrefix(f.AuthorizeURL, h.as.AuthorizationEndpoint+"?") {
		t.Fatalf("AuthorizeURL = %q, want the endpoint %s the challenge's PRM leads to", f.AuthorizeURL, h.as.AuthorizationEndpoint)
	}

	consent(t, f.AuthorizeURL)

	if got := h.lastAuthorization(t).Scope; got != "a b" {
		t.Errorf("scope = %q, want the challenge's %q", got, "a b")
	}
}

// scopeOf is the scope of the authorize URL of the last flow, read by
// opening it.
func scopeOf(t *testing.T, h *brokerHarness) string {
	t.Helper()
	f := h.start(t)
	consent(t, f.AuthorizeURL)

	return h.lastAuthorization(t).Scope
}

func TestFlowWithoutResourceIndicator(t *testing.T) {
	h := newBrokerHarness(t)
	h.conn.Auth.ResourceIndicator = false

	f := h.start(t)
	h.callback(t, consent(t, f.AuthorizeURL))

	if a := h.lastAuthorization(t); a.Resource != "" {
		t.Errorf("authorize resource = %q, want none", a.Resource)
	}

	for _, tr := range h.as.TokenRequests() {
		if tr.Resource != "" {
			t.Errorf("token request resource = %q, want none", tr.Resource)
		}
	}

	// The token is still bound to the connector's URL.
	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Errorf("resource through the broker = %d, %v; want 200", status, err)
	}
}

// TestFlowSendsAuthorizeParams: authorize_params reach the authorization
// server, next to protocol parameters they don't change.
func TestFlowSendsAuthorizeParams(t *testing.T) {
	h := newBrokerHarness(t)
	h.conn.Auth.AuthorizeParams = map[string]string{"access_type": "offline", "prompt": "consent"}

	f := h.start(t)
	h.callback(t, consent(t, f.AuthorizeURL))
	a := h.lastAuthorization(t)

	if diff := cmp.Diff(h.conn.Auth.AuthorizeParams, a.Extra); diff != "" {
		t.Errorf("extra authorize parameters (-want +got):\n%s", diff)
	}

	if a.Error != "" || a.State == "" || a.CodeChallengeMethod != "S256" || a.Resource != h.as.ResourceURL {
		t.Errorf("authorization = %+v, want no error, a state, S256, and the resource", a)
	}

	if got := h.codeExchanges(); !cmp.Equal(got, []string{""}) {
		t.Errorf("code exchanges = %q, want one that succeeded", got)
	}
}

// TestFlowAuthorizeParamsDontOverrideTheProtocol: a reserved key that
// reached the broker despite validation is overwritten.
func TestFlowAuthorizeParamsDontOverrideTheProtocol(t *testing.T) {
	h := newBrokerHarness(t)
	h.conn.Auth.AuthorizeParams = map[string]string{
		"state": "attacker", "code_challenge_method": "plain", "resource": "https://evil.example", "scope": "evil",
	}
	// Without a resource indicator the request sends no resource of its
	// own, so a reserved key would be the only one.
	h.conn.Auth.ResourceIndicator = false

	consent(t, h.start(t).AuthorizeURL)
	a := h.lastAuthorization(t)

	if a.State == "attacker" || a.CodeChallengeMethod != "S256" || a.Resource != "" || a.Scope == "evil" {
		t.Errorf("authorization = %+v, want the flow's own state, S256, no resource, and not the configured scope", a)
	}
}

// TestFlowWithATrailingSlashOnThePRMIssuer: PRM that lists the issuer with
// a terminating "/" (Google) completes with issuer validation on.
func TestFlowWithATrailingSlashOnThePRMIssuer(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithPRMIssuerTrailingSlash())

	f := h.start(t)
	status, page := h.callback(t, consent(t, f.AuthorizeURL))

	if status != http.StatusOK {
		t.Fatalf("callback status = %d, want 200; page: %s", status, page)
	}

	if res := h.result(t, f.ID); res.State != auth.FlowDone {
		t.Errorf("flow result = %+v, want done", res)
	}
}

func TestFlowRegistersWithTheRedirectURIOverride(t *testing.T) {
	const override = "https://wayfindr.example.ngrok.app/oauth/callback"
	h := newBrokerHarness(t)
	h.conn.Auth.ClientID = ""
	h.conn.Auth.RedirectURI = override

	f := h.start(t)
	cb := consent(t, f.AuthorizeURL)

	clients := h.as.Clients()

	if got := clients[len(clients)-1].RedirectURIs; !cmp.Equal(got, []string{override}) {
		t.Errorf("registered redirect URIs = %v, want [%s]", got, override)
	}

	if !strings.HasPrefix(cb.String(), override+"?") {
		t.Errorf("callback = %s, want the override", cb.Redacted())
	}

	if status, _ := h.callback(t, cb); status != http.StatusOK {
		t.Errorf("callback = %d, want 200", status)
	}
}

func TestConcurrentFlowsRegisterOneClient(t *testing.T) {
	h := newBrokerHarness(t)
	h.conn.Auth.ClientID = ""

	var wg sync.WaitGroup
	errs := make(chan error, 5)

	for range 5 {
		wg.Go(func() {
			_, err := h.flows().Start(t.Context(), h.conn.Name, auth.StartOptions{})
			errs <- err
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("StartFlow: %v", err)
		}
	}

	if n := len(h.as.Clients()); n != 2 {
		t.Errorf("clients = %d, want the pre-registered one and one DCR client", n)
	}
}

// The code exchange applies the refresh's rule: a stored DCR client the
// token endpoint rejects is forgotten, and the connector needs auth again,
// so the next flow registers a new client.
func TestFlowWithARejectedStoredDCRClientForgetsIt(t *testing.T) {
	h := newBrokerHarness(t)
	reg := h.seedDCR(t, time.Hour)
	f := h.start(t)
	cb := consent(t, f.AuthorizeURL)
	h.as.RevokeClient(reg.ClientID)

	h.callback(t, cb)

	got := h.result(t, f.ID)

	if got.State != auth.FlowFailed || caseOf(got.Err) != auth.CaseRegistrationExpired {
		t.Fatalf("WaitFlow = %+v, want failed registration_expired", got)
	}

	before := len(h.as.Clients())
	h.start(t)

	if after := len(h.as.Clients()); after != before+1 {
		t.Errorf("clients after the next flow = %d, want %d: a new DCR client", after, before+1)
	}
}

func TestStartFlowFailsBeforeTheBrowserWhenDiscoveryFails(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithIssuerMismatch())

	_, err := h.flows().Start(t.Context(), h.conn.Name, auth.StartOptions{})

	if caseOf(err) != auth.CaseIssuerMismatch {
		t.Errorf("StartFlow = %v, want issuer_mismatch", err)
	}

	if n := len(h.as.Authorizations()); n != 0 {
		t.Errorf("authorize requests = %d, want none", n)
	}
}

// A DCR registration writes the credentials row after its request to the
// registration endpoint returns. A refresh that stored a rotated token in
// the meantime must survive that write, and so must the new client
// (wayfindr-zgp.10).
func TestRegistrationDuringARefreshKeepsBothChanges(t *testing.T) {
	const override = "https://wayfindr.example.ngrok.app/oauth/callback"
	h := newBrokerHarness(t, fakeas.WithRotatingRefreshTokens())
	old := h.seedDCR(t, 30*time.Second)
	// The stored client was registered for the default redirect URI, so
	// the next flow registers a new one.
	h.conn.Auth.RedirectURI = override

	registering, release := make(chan struct{}), make(chan struct{})
	o := h.opts
	o.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && r.URL.String() == h.as.RegistrationEndpoint {
			close(registering)
			<-release
		}

		return http.DefaultTransport.RoundTrip(r)
	})}
	h.broker = auth.NewBroker(o)

	started := make(chan error, 1)
	go func() {
		_, err := h.flows().Start(t.Context(), h.conn.Name, auth.StartOptions{})
		started <- err
	}()
	<-registering
	// The token is about to expire: this request refreshes it, rotating
	// the refresh token, while the registration is in flight.
	status, err := h.get(t)
	close(release)

	if err != nil || status != http.StatusOK {
		t.Fatalf("get during registration = %d, %v; want 200", status, err)
	}

	if err := <-started; err != nil {
		t.Fatalf("StartFlow: %v", err)
	}

	cred, err := h.store.GetCredential(t.Context(), h.user, "linear")

	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}

	clients := h.as.Clients()

	if newest := clients[len(clients)-1].ID; cred.DCRClientID != newest || newest == old.ClientID || cred.RedirectURI != override {
		t.Errorf("stored client = %s for %s, want the new client %s for %s", cred.DCRClientID, cred.RedirectURI, newest, override)
	}

	// The refreshed token is still the stored one: it is used as it is,
	// with no second refresh (which would spend the rotated-out refresh
	// token and fail).
	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Errorf("get after registration = %d, %v; want 200", status, err)
	}

	if diff := cmp.Diff([]string{""}, h.refreshes()); diff != "" {
		t.Errorf("refreshes (-want +got):\n%s", diff)
	}
}
