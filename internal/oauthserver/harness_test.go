package oauthserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/oauthserver"
	"github.com/atomicpages/wayfindr/internal/store"
)

const (
	issuer      = "https://w.example"
	resource    = "https://w.example/mcp"
	redirectURI = "https://chatgpt.example/connector/oauth/callback"
	// otherClient is a second allowed client, never fetched.
	otherClient = "https://other.example/client.json"
	state       = "state-7f3a9c"
	// The PKCE pair from RFC 7636, Appendix B.
	verifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
)

var start = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

// clock is the test's clock, shared by the server and the fake grants.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeGrants is the daemon's token service as Grants describes it, in
// memory. Like the store behind the real one, every method fails once its
// ctx is done.
type fakeGrants struct {
	now func() time.Time

	mu        sync.Mutex
	grants    map[string]store.OAuthGrant
	tokens    map[[sha256.Size]byte]store.OAuthToken
	revoked   []string
	suspended bool
	// rotateErr, when set, is what the next RotateGrantTokens returns.
	rotateErr error
	// lookedUp, when set, runs after each GrantToken: a test's way to have
	// the client go away once the server has read its token.
	lookedUp func()
}

func newFakeGrants(now func() time.Time) *fakeGrants {
	return &fakeGrants{
		now:    now,
		grants: map[string]store.OAuthGrant{},
		tokens: map[[sha256.Size]byte]store.OAuthToken{},
	}
}

func (f *fakeGrants) CreateGrant(ctx context.Context, g store.OAuthGrant) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.grants[g.Name]; ok {
		return fmt.Errorf("grant %q: %w", g.Name, store.ErrExists)
	}

	f.grants[g.Name] = g

	return nil
}

func (f *fakeGrants) Grant(ctx context.Context, name string) (store.OAuthGrant, error) {
	if err := ctx.Err(); err != nil {
		return store.OAuthGrant{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	g, ok := f.grants[name]
	if !ok {
		return store.OAuthGrant{}, store.ErrNotFound
	}

	return g, nil
}

func (f *fakeGrants) GrantToken(ctx context.Context, d [sha256.Size]byte) (store.OAuthToken, error) {
	if err := ctx.Err(); err != nil {
		return store.OAuthToken{}, err
	}

	f.mu.Lock()
	t, ok := f.tokens[d]
	lookedUp := f.lookedUp
	f.mu.Unlock()

	if lookedUp != nil {
		lookedUp()
	}

	if !ok {
		return store.OAuthToken{}, store.ErrNotFound
	}

	return t, nil
}

func (f *fakeGrants) IssueGrantTokens(ctx context.Context, created time.Time, access, refresh store.OAuthToken) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	g, ok := f.grants[access.Grant]
	if !ok || !g.CreatedAt.Truncate(time.Millisecond).Equal(created.Truncate(time.Millisecond)) {
		return store.ErrNotFound
	}

	if f.suspended {
		return oauthserver.ErrSuspended
	}

	if err := store.CheckOAuthTokenPair("", access, refresh); err != nil {
		return err
	}

	f.tokens[access.Digest] = access
	f.tokens[refresh.Digest] = refresh

	return nil
}

func (f *fakeGrants) RotateGrantTokens(ctx context.Context, spent [sha256.Size]byte, access, refresh store.OAuthToken) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.rotateErr; err != nil {
		f.rotateErr = nil

		return err
	}

	if f.suspended {
		return oauthserver.ErrSuspended
	}

	old, ok := f.tokens[spent]
	if !ok || old.Kind != store.OAuthRefresh {
		return store.ErrNotFound
	}

	if old.UsedAt != nil {
		return store.ErrSpent
	}

	if err := store.CheckOAuthTokenPair("", access, refresh); err != nil {
		return err
	}

	at := f.now().Truncate(time.Millisecond)
	old.UsedAt = &at
	f.tokens[spent] = old

	for d, t := range f.tokens {
		if t.Grant == old.Grant && t.Kind == store.OAuthAccess {
			delete(f.tokens, d)
		}
	}

	f.tokens[access.Digest] = access
	f.tokens[refresh.Digest] = refresh

	return nil
}

func (f *fakeGrants) RevokeGrant(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.grants[name]; !ok {
		return store.ErrNotFound
	}

	delete(f.grants, name)

	for d, t := range f.tokens {
		if t.Grant == name {
			delete(f.tokens, d)
		}
	}

	f.revoked = append(f.revoked, name)

	return nil
}

func (f *fakeGrants) Revoked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.revoked...)
}

// tokenCount is how many tokens are stored, of every grant.
func (f *fakeGrants) tokenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.tokens)
}

func (f *fakeGrants) hasGrant(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, ok := f.grants[name]

	return ok
}

// countingTransport counts the requests it carries: the CIMD fetches.
type countingTransport struct {
	base http.RoundTripper
	n    atomic.Int64
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)

	return c.base.RoundTrip(r)
}

// syncBuffer is a log sink the server may write from several goroutines.
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

// harness is one server with a fake token service, a test clock, and a
// TLS server holding the allowed client's CIMD document.
type harness struct {
	clock     *clock
	grants    *fakeGrants
	transport *countingTransport
	srv       *oauthserver.Server
	logs      *syncBuffer
	// clientID is the CIMD URL on the TLS server, the first allowed client.
	clientID   string
	clientHost string

	mu      sync.Mutex
	cfg     config.Server
	serveFn http.HandlerFunc
}

type harnessOption func(*oauthserver.Options)

func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()

	h := &harness{
		clock: &clock{t: start},
		logs:  &syncBuffer{},
	}
	h.grants = newFakeGrants(h.clock.Now)

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		fn := h.serveFn
		h.mu.Unlock()

		fn(w, r)
	}))
	t.Cleanup(ts.Close)

	h.clientID = ts.URL + "/client.json"
	h.clientHost = strings.TrimPrefix(ts.URL, "https://")
	h.serveFn = h.serveDocument(h.document())
	h.cfg = config.Server{
		Listen:    "127.0.0.1:7777",
		PublicURL: issuer + "/",
		OAuth:     config.OAuth{Enabled: true, Clients: []string{h.clientID, otherClient}},
	}
	h.transport = &countingTransport{base: ts.Client().Transport}

	o := oauthserver.Options{
		Server: func() config.Server {
			h.mu.Lock()
			defer h.mu.Unlock()

			return h.cfg
		},
		Grants:     h.grants,
		ApproveURL: approveURL,
		Client:     &http.Client{Transport: h.transport},
		Now:        h.clock.Now,
		Logger:     zerolog.New(h.logs).Level(zerolog.DebugLevel),
	}
	for _, opt := range opts {
		opt(&o)
	}

	h.srv = oauthserver.New(o)

	return h
}

func approveURL(id string) string {
	return "http://127.0.0.1:7777/ui/oauth/approve?req=" + url.QueryEscape(id)
}

// document is the allowed client's valid CIMD document.
func (h *harness) document() map[string]any {
	return map[string]any{
		"client_id":                  h.clientID,
		"client_name":                "ChatGPT",
		"redirect_uris":              []string{redirectURI, "https://chatgpt.example/cb?x=1"},
		"token_endpoint_auth_method": "none",
	}
}

func (h *harness) serveDocument(doc any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}
}

func (h *harness) serve(fn http.HandlerFunc) {
	h.mu.Lock()
	h.serveFn = fn
	h.mu.Unlock()
}

func (h *harness) setServer(fn func(*config.Server)) {
	h.mu.Lock()
	fn(&h.cfg)
	h.mu.Unlock()
}

func (h *harness) fetches() int64 { return h.transport.n.Load() }

// query is a valid authorization request's query.
func (h *harness) query() url.Values {
	return url.Values{
		"client_id":             {h.clientID},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {resource},
		"state":                 {state},
		"redirect_uri":          {redirectURI},
		"scope":                 {"anything"},
	}
}

func (h *harness) authorize(rawQuery string, header http.Header) *httptest.ResponseRecorder {
	return h.authorizeCtx(context.Background(), rawQuery, header)
}

// authorizeCtx is authorize with ctx as the request's context.
func (h *harness) authorizeCtx(ctx context.Context, rawQuery string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, issuer+oauthserver.AuthorizePath+"?"+rawQuery, nil)
	for k, v := range header {
		r.Header[k] = v
	}

	w := httptest.NewRecorder()
	h.srv.Authorize(w, r)

	return w
}

var caseAttr = regexp.MustCompile(`data-case="([a-z_]+)"`)

// pageCase is the error page's case, or "" when the answer is not one.
func pageCase(w *httptest.ResponseRecorder) string {
	if w.Code != http.StatusBadRequest {
		return ""
	}

	m := caseAttr.FindStringSubmatch(w.Body.String())
	if m == nil {
		return ""
	}

	return m[1]
}

// authorizeID authorizes with q, without arming first, and returns the
// pending authorization's id, or "" when it was refused.
func (h *harness) authorizeID(t *testing.T, q url.Values) string {
	t.Helper()

	w := h.authorize(q.Encode(), nil)
	if w.Code != http.StatusFound {
		return ""
	}

	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	return u.Query().Get("req")
}

// pendingID arms the server, authorizes with q, and returns the pending
// authorization's id, unclaimed.
func (h *harness) pendingID(t *testing.T, q url.Values) string {
	t.Helper()

	h.srv.Arm()

	id := h.authorizeID(t, q)
	if id == "" {
		t.Fatal("authorize refused a valid request")
	}

	return id
}

// claimedID arms the server, authorizes with q, claims the pending
// authorization as the console's approve page does, and returns its id.
func (h *harness) claimedID(t *testing.T, q url.Values) string {
	t.Helper()

	id := h.pendingID(t, q)
	if _, ok := h.srv.Claim(id); !ok {
		t.Fatal("Claim of the new pending authorization: false")
	}

	return id
}

// code runs authorize, Claim, and Approve, and returns the code.
func (h *harness) code(t *testing.T, name string) string {
	t.Helper()

	id := h.claimedID(t, h.query())

	redirect, err := h.srv.Approve(t.Context(), id, name, []string{"github"})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}

	return u.Query().Get("code")
}

func (h *harness) exchangeForm(code string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {h.clientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"resource":      {resource},
	}
}

func (h *harness) refreshForm(refresh string) url.Values {
	return url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {h.clientID},
		"refresh_token": {refresh},
		"resource":      {resource},
	}
}

// tokenAnswer is a decoded token endpoint answer, success or error.
type tokenAnswer struct {
	Status           int
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (h *harness) tokenRaw(body, contentType string, header http.Header) *httptest.ResponseRecorder {
	return h.tokenRawCtx(context.Background(), body, contentType, header)
}

// tokenRawCtx is tokenRaw with ctx as the request's context.
func (h *harness) tokenRawCtx(ctx context.Context, body, contentType string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, issuer+oauthserver.TokenPath, strings.NewReader(body))
	r.Header.Set("Content-Type", contentType)

	for k, v := range header {
		r.Header[k] = v
	}

	w := httptest.NewRecorder()
	h.srv.Token(w, r)

	return w
}

func (h *harness) token(t *testing.T, form url.Values) tokenAnswer {
	t.Helper()

	return decodeToken(t, h.tokenRaw(form.Encode(), "application/x-www-form-urlencoded", nil))
}

func decodeToken(t *testing.T, w *httptest.ResponseRecorder) tokenAnswer {
	t.Helper()

	var a tokenAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		t.Fatalf("token answer %d is not JSON: %v: %q", w.Code, err, w.Body.String())
	}

	a.Status = w.Code

	return a
}

// connect runs the whole flow and returns the access and refresh tokens.
func (h *harness) connect(t *testing.T) (access, refresh string) {
	t.Helper()

	a := h.token(t, h.exchangeForm(h.code(t, "chatgpt")))
	if a.Status != http.StatusOK {
		t.Fatalf("exchange: %d %s", a.Status, a.Error)
	}

	return a.AccessToken, a.RefreshToken
}
