package auth_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

const (
	testClientID = "wayfindr-test"
	testListen   = "127.0.0.1:7777"
	wantAuthURL  = "http://127.0.0.1:7777/auth/linear"
)

// needsAuther and authConfiger are how upstream detects broker errors
// without importing auth.
type needsAuther interface {
	NeedsAuth() (link string, stepUp bool)
}
type authConfiger interface{ AuthConfig() bool }

// providerCoder is how upstream reads the provider code of an auth error
// without importing auth.
type providerCoder interface{ AuthProviderCode() string }

// testClock is an injected clock that tests move by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// brokerHarness is a Broker over a real SQLite store and Sealer, with a
// manual OAuth connector "linear" pointing at a fakeas resource.
type brokerHarness struct {
	as     *fakeas.Server
	store  *sqlite.Store
	user   string
	clock  *testClock
	logs   *syncBuffer
	broker *auth.Broker
	conn   config.Connector
	// removed drops conn from the running config the flows look up.
	removed bool
	// changed records the broker's OnCredentialChanged calls.
	changed *names
	// opts built broker; a test that needs another HTTP client rebuilds
	// the broker from it.
	opts auth.BrokerOptions
}

// names records strings, safely across goroutines.
type names struct {
	mu   sync.Mutex
	list []string
}

func (n *names) add(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.list = append(n.list, s)
}

func (n *names) get() []string {
	n.mu.Lock()
	defer n.mu.Unlock()

	return append([]string(nil), n.list...)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func newBrokerHarness(t *testing.T, opts ...fakeas.Option) *brokerHarness {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })
	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}

	sealer, err := secrets.NewSealer(testKey)

	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}

	opts = append([]fakeas.Option{fakeas.WithClient(testClientID, "", defaultRedirect)}, opts...)
	as := fakeas.Start(t, opts...)
	h := &brokerHarness{
		as:      as,
		store:   st,
		user:    user,
		clock:   &testClock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)},
		logs:    &syncBuffer{},
		changed: &names{},
		conn: config.Connector{
			Name: "linear",
			URL:  config.Template(as.ResourceURL),
			Auth: &config.Auth{
				Type: config.AuthTypeOAuth, ClientID: testClientID,
				PKCE: true, ValidateIssuer: true, ResourceIndicator: true,
			},
		},
	}
	h.opts = auth.BrokerOptions{
		Store:    st,
		Sealer:   sealer,
		Resolver: config.ResolverFunc(func(context.Context, config.Ref) (string, error) { return "", config.ErrNotFound }),
		Client:   http.DefaultClient,
		Logger:   zerolog.New(h.logs),
		UserID:   user,
		Listen:   testListen,
		Now:      h.clock.Now,

		OnCredentialChanged: h.changed.add,
	}
	h.broker = auth.NewBroker(h.opts)

	return h
}

// authorize runs the authorization code flow against the fake, as the
// broker's flow will, and returns the issued access and refresh tokens.
func (h *brokerHarness) authorize(t *testing.T, scope string) (access, refresh string) {
	t.Helper()

	return h.authorizeAs(t, auth.Registration{ClientID: testClientID}, scope)
}

// authorizeAs is authorize as client, which authenticates to the token
// endpoint with client_secret_post when it has a secret.
func (h *brokerHarness) authorizeAs(t *testing.T, client auth.Registration, scope string) (access, refresh string) {
	t.Helper()
	verifier := "verifier-0123456789-0123456789-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {client.ClientID},
		"redirect_uri":          {defaultRedirect},
		"state":                 {"state"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"resource":              {h.as.ResourceURL},
		"scope":                 {scope},
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := send(t, noRedirect, http.MethodGet, h.as.AuthorizationEndpoint+"?"+q.Encode(), "")

	if err != nil {
		t.Fatalf("authorize: %v", err)
	}

	_ = resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))

	if err != nil || loc.Query().Get("code") == "" {
		t.Fatalf("authorize redirect %q has no code", resp.Header.Get("Location"))
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {loc.Query().Get("code")},
		"redirect_uri":  {defaultRedirect},
		"code_verifier": {verifier},
		"client_id":     {client.ClientID},
		"resource":      {h.as.ResourceURL},
	}

	if client.ClientSecret != "" {
		form.Set("client_secret", client.ClientSecret)
	}

	resp, err = send(t, http.DefaultClient, http.MethodPost, h.as.TokenEndpoint, form.Encode())

	if err != nil {
		t.Fatalf("exchange code: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		t.Fatalf("exchange code: status %d, err %v", resp.StatusCode, err)
	}

	return tok.AccessToken, tok.RefreshToken
}

// tokenEndpoint is where a flow for the connector would get its token, and
// so where a seeded grant is bound: the configured token_endpoint, else
// the fake's.
func (h *brokerHarness) tokenEndpoint() string {
	if h.conn.Auth.TokenEndpoint != "" {
		return h.conn.Auth.TokenEndpoint
	}

	return h.as.TokenEndpoint
}

// seed authorizes with scope and stores the token with an expiry of
// expiresIn from the test clock.
func (h *brokerHarness) seed(t *testing.T, scope string, expiresIn time.Duration) (access, refresh string) {
	t.Helper()
	access, refresh = h.authorize(t, scope)

	if err := auth.SeedToken(t.Context(), h.broker, "linear", h.as.ResourceURL, h.tokenEndpoint(), access, refresh,
		h.clock.Now().Add(expiresIn), scope); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	return access, refresh
}

// register runs the Registrar for "linear" on the broker's store, with the
// connector's current config, as the auth flow will.
func (h *brokerHarness) register(t *testing.T) (auth.Registration, error) {
	t.Helper()
	sealer, err := secrets.NewSealer(testKey)

	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}

	a := *h.conn.Auth
	disc, err := auth.NewDiscoverer(h.store, http.DefaultClient, zerolog.Nop()).Discover(t.Context(),
		auth.DiscoverRequest{Connector: "linear", ResourceURL: h.as.ResourceURL, Auth: a})

	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	resolver := config.ResolverFunc(func(context.Context, config.Ref) (string, error) { return "", config.ErrNotFound })

	return auth.NewRegistrar(h.store, sealer, h.user, resolver, http.DefaultClient, zerolog.Nop()).Register(t.Context(),
		auth.RegisterRequest{Connector: "linear", Auth: a, Discovery: disc, RedirectURI: defaultRedirect})
}

// seedDCR makes "linear" a connector with no client_id, registers it
// through DCR, authorizes as that client, and stores the token with an
// expiry of expiresIn from the test clock.
func (h *brokerHarness) seedDCR(t *testing.T, expiresIn time.Duration) auth.Registration {
	t.Helper()
	h.conn.Auth.ClientID = ""
	reg, err := h.register(t)

	if err != nil || reg.Method != auth.MethodDCR {
		t.Fatalf("Register = %s, %v; want a new DCR client", reg.Method, err)
	}

	access, refresh := h.authorizeAs(t, reg, "")

	if err := auth.SeedToken(t.Context(), h.broker, "linear", h.as.ResourceURL, h.tokenEndpoint(), access, refresh,
		h.clock.Now().Add(expiresIn), ""); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	return reg
}

// client is an HTTP client whose transport is the broker's for "linear",
// dialed with the connector's current config.
func (h *brokerHarness) client() *http.Client {
	return &http.Client{Transport: h.broker.Transport(h.conn, http.DefaultTransport)}
}

// send sends a request with body (form-encoded for POST, none if empty)
// through client.
func send(t *testing.T, client *http.Client, method, target, body string) (*http.Response, error) {
	t.Helper()
	var r io.Reader

	if body != "" {
		r = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(t.Context(), method, target, r)

	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	return client.Do(req)
}

// get GETs the resource through the broker and returns the status.
func (h *brokerHarness) get(t *testing.T) (int, error) {
	t.Helper()
	resp, err := send(t, h.client(), http.MethodGet, h.as.ResourceURL, "")

	if err != nil {
		return 0, err
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	return resp.StatusCode, nil
}

// refreshes counts refresh_token grants the fake saw, and their errors.
func (h *brokerHarness) refreshes() []string {
	var out []string

	for _, tr := range h.as.TokenRequests() {
		if tr.GrantType == "refresh_token" {
			out = append(out, tr.Error)
		}
	}

	return out
}

func TestTransportSendsStoredToken(t *testing.T) {
	h := newBrokerHarness(t)
	h.seed(t, "", time.Hour)

	status, err := h.get(t)

	if err != nil || status != http.StatusOK {
		t.Fatalf("get = %d, %v; want 200", status, err)
	}

	if got := h.refreshes(); len(got) != 0 {
		t.Errorf("refreshes = %q, want none for a fresh token", got)
	}
}

// A stored token from a new grant changes the connector's credential, and
// fires OnCredentialChanged; a refresh renews the same grant, and doesn't.
func TestNewTokenFiresOnCredentialChangedAndARefreshDoesNot(t *testing.T) {
	h := newBrokerHarness(t)

	h.seed(t, "", 30*time.Second)

	if diff := cmp.Diff([]string{"linear"}, h.changed.get()); diff != "" {
		t.Errorf("OnCredentialChanged after a new token (-want +got):\n%s", diff)
	}

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Fatalf("get = %d, %v; want 200", status, err)
	}

	if got := h.refreshes(); len(got) != 1 {
		t.Fatalf("refreshes = %q, want one for a token about to expire", got)
	}

	if diff := cmp.Diff([]string{"linear"}, h.changed.get()); diff != "" {
		t.Errorf("OnCredentialChanged after a refresh (-want +got):\n%s", diff)
	}
}

func TestTransportConcurrentCallsRefreshExpiringTokenOnce(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithRotatingRefreshTokens())
	h.seed(t, "", 30*time.Second)
	client := h.client()

	const calls = 50
	var wg sync.WaitGroup
	errs := make(chan error, calls)

	for range calls {
		wg.Go(func() {
			resp, err := send(t, client, http.MethodGet, h.as.ResourceURL, "")

			if err != nil {
				errs <- err

				return
			}

			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				errs <- errors.New(resp.Status)
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("call failed: %v", err)
	}

	if diff := cmp.Diff([]string{""}, h.refreshes()); diff != "" {
		t.Errorf("refresh grants (-want +got):\n%s", diff)
	}

	// The rotated refresh token was persisted: once the new access token
	// nears expiry, the next refresh spends it successfully.
	h.clock.Advance(2 * time.Hour)

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Fatalf("get after second expiry = %d, %v; want 200", status, err)
	}

	if diff := cmp.Diff([]string{"", ""}, h.refreshes()); diff != "" {
		t.Errorf("refresh grants after second expiry (-want +got):\n%s", diff)
	}
}

func TestTransportLeavesConnectorsWithoutOAuthAlone(t *testing.T) {
	h := newBrokerHarness(t)
	next := http.DefaultTransport

	for _, c := range []config.Connector{
		{Name: "plain", URL: config.Template(h.as.ResourceURL)},
		{Name: "apikey", URL: config.Template(h.as.ResourceURL), Headers: map[string]config.Template{"Authorization": "Bearer k"}},
	} {
		if got := h.broker.Transport(c, next); got != next {
			t.Errorf("Transport for %s, which has no oauth = %T, want next unchanged", c.Name, got)
		}
	}
}

func TestTransportRefreshesAndReplaysAfter401(t *testing.T) {
	// The resource echoes the request body, so the replay is visibly the
	// whole original request.
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, r.Body) })
	h := newBrokerHarness(t, fakeas.WithRotatingRefreshTokens(), fakeas.WithResourceHandler(echo))
	access, refresh := h.seed(t, "", time.Hour)
	h.as.ExpireAccessTokens() // revoked early: the stored expiry still says an hour

	resp, err := send(t, h.client(), http.MethodPost, h.as.ResourceURL, `{"jsonrpc":"2.0"}`)

	if err != nil {
		t.Fatalf("post: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || string(body) != `{"jsonrpc":"2.0"}` {
		t.Errorf("post = %d %q, want 200 with the request body replayed", resp.StatusCode, body)
	}

	if diff := cmp.Diff([]string{""}, h.refreshes()); diff != "" {
		t.Errorf("refresh grants (-want +got):\n%s", diff)
	}

	for _, secret := range []string{access, refresh} {
		if strings.Contains(h.logs.String(), secret) {
			t.Errorf("logs contain a token:\n%s", h.logs)
		}
	}
}

func TestTransportRevokedTokenNeedsAuth(t *testing.T) {
	h := newBrokerHarness(t)
	h.seed(t, "", time.Hour)
	h.as.Revoke()

	_, err := h.get(t)
	var ue *url.Error

	if !errors.As(err, &ue) {
		t.Fatalf("get error = %v, want it wrapped in *url.Error by net/http", err)
	}

	var na needsAuther

	if !errors.As(err, &na) {
		t.Fatalf("get error = %v, want NeedsAuth() through errors.As", err)
	}

	link, stepUp := na.NeedsAuth()

	if diff := cmp.Diff([]any{wantAuthURL, false}, []any{link, stepUp}); diff != "" {
		t.Errorf("NeedsAuth() (-want +got):\n%s", diff)
	}

	if !strings.Contains(err.Error(), wantAuthURL) {
		t.Errorf("error text %q lacks the auth link %s", err, wantAuthURL)
	}

	if diff := cmp.Diff([]string{"invalid_grant"}, h.refreshes()); diff != "" {
		t.Errorf("refresh grants (-want +got):\n%s", diff)
	}
}

func TestTransportReplayStill401NeedsAuth(t *testing.T) {
	// The upstream rejects every token, even a freshly refreshed one.
	reject := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	h := newBrokerHarness(t, fakeas.WithResourceHandler(reject))
	h.seed(t, "", time.Hour)

	_, err := h.get(t)
	var na needsAuther

	if !errors.As(err, &na) {
		t.Fatalf("get error = %v, want needs_auth after one refresh and one replay", err)
	}

	if diff := cmp.Diff([]string{""}, h.refreshes()); diff != "" {
		t.Errorf("refresh grants (-want +got):\n%s", diff)
	}
}

func TestTransportInsufficientScopeStepsUp(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithRequiredScopes("read", "write"))
	h.seed(t, "read", time.Hour)

	_, err := h.get(t)
	var na needsAuther

	if !errors.As(err, &na) {
		t.Fatalf("get error = %v, want needs_auth with step-up", err)
	}

	link, stepUp := na.NeedsAuth()

	if diff := cmp.Diff([]any{wantAuthURL, true}, []any{link, stepUp}); diff != "" {
		t.Errorf("NeedsAuth() (-want +got):\n%s", diff)
	}

	if !strings.Contains(err.Error(), wantAuthURL) {
		t.Errorf("error text %q lacks the auth link %s", err, wantAuthURL)
	}

	// The auth flow reads the larger scope set from the credential.
	cred, err := h.store.GetCredential(t.Context(), h.user, "linear")

	if err != nil {
		t.Fatalf("get credential: %v", err)
	}

	if cred.RequestedScopes != "read write" {
		t.Errorf("requested_scopes = %q, want %q", cred.RequestedScopes, "read write")
	}

	if got := h.refreshes(); len(got) != 0 {
		t.Errorf("refreshes = %q, want none for a step-up", got)
	}
}

func TestTransportWithoutCredentialNeedsAuth(t *testing.T) {
	h := newBrokerHarness(t)

	_, err := h.get(t)
	var na needsAuther

	if !errors.As(err, &na) {
		t.Fatalf("get error = %v, want needs_auth for a connector never authorized", err)
	}

	if link, _ := na.NeedsAuth(); link != wantAuthURL {
		t.Errorf("link = %q, want %q", link, wantAuthURL)
	}
}

func TestTransportURLChangedNeedsAuth(t *testing.T) {
	h := newBrokerHarness(t)
	h.seed(t, "", time.Hour)
	other := h.as.URL + "/other-mcp"
	h.conn.URL = config.Template(other)
	// The new url's requests, as they would leave the broker.
	var sent []string
	client := &http.Client{Transport: h.broker.Transport(h.conn, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent = append(sent, r.Header.Get("Authorization"))

		return http.DefaultTransport.RoundTrip(r)
	}))}

	resp, err := send(t, client, http.MethodGet, other, "")

	if err == nil {
		_ = resp.Body.Close()
	}

	var na needsAuther

	if !errors.As(err, &na) || caseOf(err) != auth.CaseURLChanged {
		t.Fatalf("get error = %v, want needs_auth url_changed: the token is bound to the old url", err)
	}

	if len(sent) != 0 {
		t.Errorf("requests sent to the new url = %q, want none", sent)
	}
}

func TestTransportPassesNetworkErrorsThrough(t *testing.T) {
	h := newBrokerHarness(t)
	h.seed(t, "", time.Hour)
	down := errors.New("connection refused")
	client := &http.Client{Transport: h.broker.Transport(h.conn, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, down
	}))}

	resp, err := send(t, client, http.MethodGet, h.as.ResourceURL, "")

	if err == nil {
		_ = resp.Body.Close()
	}

	if !errors.Is(err, down) {
		t.Fatalf("get error = %v, want the transport error unchanged", err)
	}

	var na needsAuther
	var ac authConfiger

	if errors.As(err, &na) || errors.As(err, &ac) {
		t.Errorf("get error = %v classifies as an auth problem, want unreachable", err)
	}
}

// TestRefreshStaysAtTheGrantsAuthorizationServer: an upstream whose
// protected resource metadata now names another authorization server gets
// the connector to need auth; that server never sees the refresh token or
// the client (wayfindr-afb1).
func TestRefreshStaysAtTheGrantsAuthorizationServer(t *testing.T) {
	h := newBrokerHarness(t)
	rogue := fakeas.Start(t, fakeas.WithClient(testClientID, "", defaultRedirect))
	_, refresh := h.seed(t, "", 30*time.Second)

	// What the upstream serves now, as the cache a lazy refresh reads has it.
	prm, err := json.Marshal(map[string]any{
		"resource": h.as.ResourceURL, "authorization_servers": []string{rogue.Issuer},
	})

	if err != nil {
		t.Fatal(err)
	}

	if err := h.store.PutMetadata(t.Context(), store.Metadata{
		URL: h.as.PRMURL, Kind: store.MetadataPRM, Document: prm, FetchedAt: h.clock.Now(),
	}); err != nil {
		t.Fatalf("put metadata: %v", err)
	}

	_, err = h.get(t)

	if e, ok := auth.AsError(err); !ok || e.Case != auth.CaseAuthorizationServerChanged {
		t.Errorf("get error = %v, want case authorization_server_changed", err)
	}

	var na needsAuther

	if !errors.As(err, &na) {
		t.Errorf("get error = %v, want it to read as needs_auth", err)
	}

	if n := len(rogue.TokenRequests()); n != 0 {
		t.Errorf("the other authorization server got %d token requests, want none", n)
	}

	if strings.Contains(h.logs.String(), refresh) {
		t.Errorf("logs contain the refresh token:\n%s", h.logs)
	}
}

// TestRefreshDoesNotFollowARedirect: a token endpoint that redirects gets
// an error, not a resent refresh token: a 307 would send the form, refresh
// token and client authentication included, on to wherever it points
// (wayfindr-twbw).
func TestRefreshDoesNotFollowARedirect(t *testing.T) {
	var leaked atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	t.Cleanup(sink.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/token", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	h := newBrokerHarness(t)
	h.conn.Auth.AuthorizationEndpoint = h.as.URL + "/authorize"
	h.conn.Auth.TokenEndpoint = redirector.URL + "/token"
	h.seed(t, "", 30*time.Second)

	if _, err := h.get(t); err == nil {
		t.Error("get succeeded, want the refresh to fail")
	}

	if n := leaked.Load(); n != 0 {
		t.Errorf("the redirect target got %d requests, want none", n)
	}
}

func TestTransportRefreshConfigError(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithIssuerMismatch())
	h.seed(t, "", 30*time.Second)

	_, err := h.get(t)
	var ac authConfiger

	if !errors.As(err, &ac) || !ac.AuthConfig() {
		t.Fatalf("get error = %v, want AuthConfig() through errors.As", err)
	}

	if e, ok := auth.AsError(err); !ok || e.Case != auth.CaseIssuerMismatch {
		t.Errorf("get error = %v, want case issuer_mismatch", err)
	}

	var na needsAuther

	if errors.As(err, &na) {
		t.Errorf("get error = %v also reads as needs_auth", err)
	}
}

// TestTransportRefreshRejectedByBody covers token endpoints that report a
// rejected refresh in the body, whatever the HTTP status: the connector
// needs auth, with the case and the provider's code.
func TestTransportRefreshRejectedByBody(t *testing.T) {
	tests := []struct {
		name     string
		opts     []fakeas.Option
		fail     string // FailTokenRequests code; empty means Revoke
		wantCase auth.Case
		wantCode string
	}{
		{
			name:     "Slack style 200 ok false with invalid_refresh_token",
			opts:     []fakeas.Option{fakeas.WithSlackStyleErrors()},
			fail:     "invalid_refresh_token",
			wantCase: auth.CaseTokenRevoked, wantCode: "invalid_refresh_token",
		},
		{
			name:     "Slack style 200 ok false after revocation",
			opts:     []fakeas.Option{fakeas.WithSlackStyleErrors()},
			wantCase: auth.CaseTokenRevoked, wantCode: "invalid_grant",
		},
		{
			name:     "200 with an RFC 6749 error body",
			opts:     []fakeas.Option{fakeas.WithTokenErrorStatus(http.StatusOK)},
			wantCase: auth.CaseTokenRevoked, wantCode: "invalid_grant",
		},
		{
			name:     "Slack style unknown code",
			opts:     []fakeas.Option{fakeas.WithSlackStyleErrors()},
			fail:     "team_added_to_org",
			wantCase: auth.CaseProviderError, wantCode: "team_added_to_org",
		},
		{
			name:     "RFC 6749 unknown code on 400",
			fail:     "some_vendor_code",
			wantCase: auth.CaseProviderError, wantCode: "some_vendor_code",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newBrokerHarness(t, tt.opts...)
			access, refresh := h.seed(t, "", 30*time.Second)

			if tt.fail != "" {
				h.as.FailTokenRequests(tt.fail)
			} else {
				h.as.Revoke()
			}

			_, err := h.get(t)
			e, ok := auth.AsError(err)

			if !ok || e.Kind != auth.KindNeedsAuth {
				t.Fatalf("get error = %v, want a needs_auth *auth.Error", err)
			}

			if diff := cmp.Diff([]any{tt.wantCase, tt.wantCode, wantAuthURL}, []any{e.Case, e.ProviderCode, e.Link}); diff != "" {
				t.Errorf("Case, ProviderCode, Link (-want +got):\n%s", diff)
			}

			var ue *url.Error

			if !errors.As(err, &ue) {
				t.Errorf("get error = %v, want it wrapped in *url.Error by net/http", err)
			}

			var pc providerCoder

			if !errors.As(err, &pc) {
				t.Fatalf("get error = %v, want AuthProviderCode() through errors.As", err)
			}

			if got := pc.AuthProviderCode(); got != tt.wantCode {
				t.Errorf("AuthProviderCode() = %q, want %q", got, tt.wantCode)
			}

			var ac authConfiger

			if errors.As(err, &ac) && ac.AuthConfig() {
				t.Errorf("get error = %v also reads as an auth config error", err)
			}

			if diff := cmp.Diff([]string{tt.wantCode}, h.refreshes()); diff != "" {
				t.Errorf("refresh grants (-want +got):\n%s", diff)
			}

			for _, secret := range []string{access, refresh} {
				if strings.Contains(h.logs.String(), secret) || strings.Contains(err.Error(), secret) {
					t.Errorf("logs or error text contain a token:\n%s\n%v", h.logs, err)
				}
			}
		})
	}
}

// TestTransportRefreshTransientCodeIsUnreachable covers a token endpoint
// that answers a refresh with a known transient code (rate limiting, an
// outage): the connector is unreachable and retried, not sent back through
// the auth flow, and the provider's code is kept.
func TestTransportRefreshTransientCodeIsUnreachable(t *testing.T) {
	tests := []struct {
		name string
		opts []fakeas.Option
		code string
	}{
		{name: "Slack style ratelimited", opts: []fakeas.Option{fakeas.WithSlackStyleErrors()}, code: "ratelimited"},
		{
			name: "RFC 6749 temporarily_unavailable on 503",
			opts: []fakeas.Option{fakeas.WithTokenErrorStatus(http.StatusServiceUnavailable)},
			code: "temporarily_unavailable",
		},
		{name: "RFC 6749 server_error", code: "server_error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newBrokerHarness(t, tt.opts...)
			h.seed(t, "", 30*time.Second)
			h.as.FailTokenRequests(tt.code)

			_, err := h.get(t)

			e, ok := auth.AsError(err)

			if !ok {
				t.Fatalf("get error = %v, want an *auth.Error", err)
			}

			want := []any{auth.KindUnreachable, auth.CaseProviderUnavailable, tt.code, ""}

			if diff := cmp.Diff(want, []any{e.Kind, e.Case, e.ProviderCode, e.Link}); diff != "" {
				t.Errorf("Kind, Case, ProviderCode, Link (-want +got):\n%s", diff)
			}

			var na needsAuther

			if errors.As(err, &na) {
				t.Errorf("get error = %v reads as needs_auth", err)
			}

			var ac authConfiger

			if errors.As(err, &ac) && ac.AuthConfig() {
				t.Errorf("get error = %v reads as an auth config error", err)
			}
		})
	}
}

// TestTransportRefreshClientRejectedIsAuthConfig covers a token endpoint
// that rejects the configured client: the config has to change, so the
// connector is in auth_error, not needs_auth.
func TestTransportRefreshClientRejectedIsAuthConfig(t *testing.T) {
	tests := []struct {
		name string
		opts []fakeas.Option
	}{
		{name: "RFC 6749 401"},
		{name: "Slack style 200 ok false", opts: []fakeas.Option{fakeas.WithSlackStyleErrors()}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newBrokerHarness(t, tt.opts...)
			h.seed(t, "", 30*time.Second)
			h.conn.Auth.ClientID = "not-registered"

			_, err := h.get(t)
			var ac authConfiger

			if !errors.As(err, &ac) || !ac.AuthConfig() {
				t.Fatalf("get error = %v, want AuthConfig() through errors.As", err)
			}

			e, ok := auth.AsError(err)

			if !ok {
				t.Fatalf("get error = %v, want an *auth.Error", err)
			}

			if diff := cmp.Diff([]any{auth.KindConfig, auth.CaseClientRejected, "invalid_client"}, []any{e.Kind, e.Case, e.ProviderCode}); diff != "" {
				t.Errorf("Case, ProviderCode (-want +got):\n%s", diff)
			}

			var na needsAuther

			if errors.As(err, &na) {
				t.Errorf("get error = %v also reads as needs_auth", err)
			}
		})
	}
}

// TestTransportRefreshRejectedDCRClientIsForgotten covers a token endpoint
// that rejects a client Wayfindr registered through DCR (the server expired
// or deleted it): the stored client is cleared, so the connector needs auth
// and the next auth flow registers a new client instead of reusing the
// rejected one.
func TestTransportRefreshRejectedDCRClientIsForgotten(t *testing.T) {
	tests := []struct {
		name string
		opts []fakeas.Option
	}{
		{name: "RFC 6749 401"},
		{name: "Slack style 200 ok false", opts: []fakeas.Option{fakeas.WithSlackStyleErrors()}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newBrokerHarness(t, tt.opts...)
			old := h.seedDCR(t, 30*time.Second)
			h.as.RevokeClient(old.ClientID)

			_, err := h.get(t)

			e, ok := auth.AsError(err)

			if !ok {
				t.Fatalf("get error = %v, want an *auth.Error", err)
			}

			want := []any{auth.KindNeedsAuth, auth.CaseRegistrationExpired, "invalid_client", wantAuthURL}

			if diff := cmp.Diff(want, []any{e.Kind, e.Case, e.ProviderCode, e.Link}); diff != "" {
				t.Errorf("Kind, Case, ProviderCode, Link (-want +got):\n%s", diff)
			}

			var ac authConfiger

			if errors.As(err, &ac) && ac.AuthConfig() {
				t.Errorf("get error = %v reads as an auth config error", err)
			}

			cred, err := h.store.GetCredential(t.Context(), h.user, "linear")

			if err != nil {
				t.Fatalf("GetCredential: %v", err)
			}

			if cred.DCRClientID != "" || len(cred.DCRClient) != 0 {
				t.Errorf("stored client = %q (%d sealed bytes), want it cleared", cred.DCRClientID, len(cred.DCRClient))
			}

			reg, err := h.register(t)

			if err != nil {
				t.Fatalf("Register after rejection: %v", err)
			}

			if reg.Method != auth.MethodDCR || reg.ClientID == old.ClientID {
				t.Errorf("Register = %s %s, want a new DCR client (not %s)", reg.Method, reg.ClientID, old.ClientID)
			}

			if n := len(h.as.Clients()); n != 2 { // testClientID and the new one
				t.Errorf("fakeas has %d clients, want 2", n)
			}
		})
	}
}

// TestTransportRefreshRejectedConfigClientKeepsStoredDCR covers a config
// client_id rejected while the row still holds an older DCR client: the
// config is what is wrong, so the connector is in auth_error and the
// stored client is left alone.
func TestTransportRefreshRejectedConfigClientKeepsStoredDCR(t *testing.T) {
	h := newBrokerHarness(t)
	old := h.seedDCR(t, 30*time.Second)
	h.conn.Auth.ClientID = "not-registered"

	_, err := h.get(t)

	e, ok := auth.AsError(err)

	if !ok {
		t.Fatalf("get error = %v, want an *auth.Error", err)
	}

	if diff := cmp.Diff([]any{auth.KindConfig, auth.CaseClientRejected}, []any{e.Kind, e.Case}); diff != "" {
		t.Errorf("Kind, Case (-want +got):\n%s", diff)
	}

	cred, err := h.store.GetCredential(t.Context(), h.user, "linear")

	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}

	if cred.DCRClientID != old.ClientID || len(cred.DCRClient) == 0 {
		t.Errorf("stored client = %q, want %q kept", cred.DCRClientID, old.ClientID)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
