package admin_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/store"
)

// testTokenValue is the value fakeTokens hands out, so a test can look for
// it where it must never be.
const testTokenValue = "wf_s_test-value-never-shown-twice" //nolint:gosec // G101: a test fixture

// created is when fakeTokens creates every token.
var created = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

// fakeTokens stands in for the daemon's adapter of the store and the
// registry: it keeps tokens in memory, as the store would, and records
// what it was asked to create.
type fakeTokens struct {
	mu      sync.Mutex
	tokens  []store.ClientToken
	grants  []store.OAuthGrant
	creates [][]string
	remotes []bool
	err     error
}

func (f *fakeTokens) Create(_ context.Context, name string, connectors []string, remote bool) (store.ClientToken, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, append([]string{name}, connectors...))
	f.remotes = append(f.remotes, remote)

	if f.err != nil {
		return store.ClientToken{}, "", f.err
	}

	if slices.ContainsFunc(f.tokens, func(t store.ClientToken) bool { return t.Name == name }) {
		return store.ClientToken{}, "", fmt.Errorf("create client token %s: %w", name, store.ErrExists)
	}

	t := store.ClientToken{Name: name, Connectors: connectors, Remote: remote, CreatedAt: created}
	f.tokens = append(f.tokens, t)

	return t, testTokenValue, nil
}

func (f *fakeTokens) List(context.Context) ([]store.ClientToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.tokens), f.err
}

func (f *fakeTokens) ListGrants(context.Context) ([]store.OAuthGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.grants), f.err
}

func (f *fakeTokens) Revoke(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}

	if g := slices.IndexFunc(f.grants, func(g store.OAuthGrant) bool { return g.Name == name }); g >= 0 {
		f.grants = slices.Delete(f.grants, g, g+1)

		return nil
	}

	i := slices.IndexFunc(f.tokens, func(t store.ClientToken) bool { return t.Name == name })

	if i < 0 {
		return fmt.Errorf("delete client token %s: %w", name, store.ErrNotFound)
	}

	f.tokens = slices.Delete(f.tokens, i, i+1)

	return nil
}

// createRemotes is the remote argument of each Create call.
func (f *fakeTokens) createRemotes() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.remotes)
}

func (f *fakeTokens) createCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.creates)
}

// serveTokens serves the admin API over f with github, linear, and slack
// configured.
func serveTokens(t *testing.T, f *fakeTokens) (*admin.Client, *httptest.Server) {
	t.Helper()
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{Connectors: []string{"slack", "github", "linear"}})
	opts.Tokens = f

	return serve(t, opts)
}

func TestCreateTokenAnswersItsValueOnce(t *testing.T) {
	f := &fakeTokens{}
	c, _ := serveTokens(t, f)

	got, err := c.CreateToken(t.Context(), admin.TokenSpec{Name: "nightly-triage", Connectors: []string{"github", "linear", "github"}})

	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	want := admin.TokenCreated{Name: "nightly-triage", Connectors: []string{"github", "linear"}, Token: testTokenValue, CreatedAt: created}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("CreateToken (-want +got):\n%s", diff)
	}

	// A repeated connector counts once, and the order given is kept.
	if diff := cmp.Diff([][]string{{"nightly-triage", "github", "linear"}}, f.createCalls()); diff != "" {
		t.Errorf("Create calls (-want +got):\n%s", diff)
	}
}

func TestCreateTokenWire(t *testing.T) {
	_, srv := serveTokens(t, &fakeTokens{})
	code, body := post(t, srv, srv.URL+"/admin/tokens", `{"name":"nightly-triage","connectors":["github","linear"]}`)

	if code != http.StatusOK {
		t.Fatalf("POST /admin/tokens = %d, want 200; body: %s", code, body)
	}

	want := `{"name":"nightly-triage","connectors":["github","linear"],"remote":false,"token":"` + testTokenValue + `","created_at":"2026-11-08T10:00:00Z"}` + "\n"

	if diff := cmp.Diff(want, string(body)); diff != "" {
		t.Errorf("body (-want +got):\n%s", diff)
	}
}

func TestCreateTokenRemoteIsAnswerStoredAndListed(t *testing.T) {
	f := &fakeTokens{}
	c, srv := serveTokens(t, f)

	got, err := c.CreateToken(t.Context(), admin.TokenSpec{Name: "grok", Connectors: []string{"github"}, Remote: true})

	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	want := admin.TokenCreated{Name: "grok", Connectors: []string{"github"}, Remote: true, Token: testTokenValue, CreatedAt: created}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("CreateToken (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff([]bool{true}, f.createRemotes()); diff != "" {
		t.Errorf("Create remote (-want +got):\n%s", diff)
	}

	// The wire body takes "remote", and the list shows it beside a token
	// that is not remote.
	code, body := post(t, srv, srv.URL+"/admin/tokens", `{"name":"local","connectors":["linear"]}`)

	if code != http.StatusOK || !strings.Contains(string(body), `"remote":false`) {
		t.Errorf("POST without remote = %d %s, want 200 answering remote false", code, body)
	}

	code, body = post(t, srv, srv.URL+"/admin/tokens", `{"name":"muse","connectors":["linear"],"remote":true}`)

	if code != http.StatusOK || !strings.Contains(string(body), `"remote":true`) {
		t.Errorf("POST with remote = %d %s, want 200 answering remote true", code, body)
	}

	list, err := c.ListTokens(t.Context())

	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}

	remote := map[string]bool{}

	for _, tok := range list.Tokens {
		remote[tok.Name] = tok.Remote
	}

	if diff := cmp.Diff(map[string]bool{"grok": true, "local": false, "muse": true}, remote); diff != "" {
		t.Errorf("ListTokens remote (-want +got):\n%s", diff)
	}
}

func TestCreateTokenRefusals(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		code       int
		want       string
		wantCreate bool
	}{
		{"malformed body", `{"name":"a","connectors":["github"],"extra":1}`, 400, `body must be {"name": "…", "connectors": ["…"]}`, false},
		{"bad name", `{"name":"Nightly","connectors":["github"]}`, 400, `client token name "Nightly" must match [a-z0-9][a-z0-9_-]{0,62}`, false},
		{"empty name", `{"name":"","connectors":["github"]}`, 400, `client token name "" must match`, false},
		{"reserved name", `{"name":"default","connectors":["github"]}`, 400, `"default" is reserved for the default client token`, false},
		{"no connector", `{"name":"nightly","connectors":[]}`, 400, "a client token needs at least one connector", false},
		{"connectors missing", `{"name":"nightly"}`, 400, "a client token needs at least one connector", false},
		{"unknown connector", `{"name":"nightly","connectors":["github","gmail"]}`, 400,
			`no connector is called "gmail"; configured: github, linear, slack`, false},
		{"name taken", `{"name":"taken","connectors":["github"]}`, 409, `a client token or OAuth grant called "taken" already exists`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeTokens{tokens: []store.ClientToken{{Name: "taken", Connectors: []string{"slack"}, CreatedAt: created}}}
			_, srv := serveTokens(t, f)
			code, body := post(t, srv, srv.URL+"/admin/tokens", tt.body)

			if code != tt.code || !strings.Contains(string(body), tt.want) {
				t.Errorf("POST /admin/tokens = %d %q, want %d containing %q", code, body, tt.code, tt.want)
			}

			if got := len(f.createCalls()) > 0; got != tt.wantCreate {
				t.Errorf("Create called = %v, want %v", got, tt.wantCreate)
			}
		})
	}
}

func TestCreateTokenStoreFailureNeverEchoesAValue(t *testing.T) {
	c, _ := serveTokens(t, &fakeTokens{err: errors.New("database is locked")})

	_, err := c.CreateToken(t.Context(), admin.TokenSpec{Name: "nightly", Connectors: []string{"github"}})

	if statusOf(err) != http.StatusInternalServerError || !strings.Contains(err.Error(), "creating the client token failed; see the daemon log") {
		t.Errorf("CreateToken = %v, want 500 pointing at the daemon log", err)
	}
}

func TestListTokensNeverHoldsAValue(t *testing.T) {
	used := time.Date(2026, 11, 8, 22, 0, 4, 0, time.FixedZone("UTC+2", 2*60*60))
	f := &fakeTokens{}
	c, srv := serveTokens(t, f)

	if _, err := c.CreateToken(t.Context(), admin.TokenSpec{Name: "nightly-triage", Connectors: []string{"github", "linear"}}); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.tokens = append(f.tokens, store.ClientToken{Name: "used", Connectors: []string{"slack"}, CreatedAt: created, LastUsedAt: &used})
	f.mu.Unlock()

	code, body := get(t, srv, srv.URL+"/admin/tokens")

	if code != http.StatusOK {
		t.Fatalf("GET /admin/tokens = %d, want 200; body: %s", code, body)
	}

	// last_used_at is absent for a token never used, and every time is UTC.
	want := `{"tokens":[` +
		`{"name":"nightly-triage","kind":"token","connectors":["github","linear"],"remote":false,"created_at":"2026-11-08T10:00:00Z"},` +
		`{"name":"used","kind":"token","connectors":["slack"],"remote":false,"created_at":"2026-11-08T10:00:00Z","last_used_at":"2026-11-08T20:00:04Z"}` +
		`]}` + "\n"

	if diff := cmp.Diff(want, string(body)); diff != "" {
		t.Errorf("body (-want +got):\n%s", diff)
	}

	got, err := c.ListTokens(t.Context())

	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}

	usedUTC := used.UTC()
	wantList := admin.TokenList{Tokens: []admin.Token{
		{Name: "nightly-triage", Kind: admin.TokenKindToken, Connectors: []string{"github", "linear"}, CreatedAt: created},
		{Name: "used", Kind: admin.TokenKindToken, Connectors: []string{"slack"}, CreatedAt: created, LastUsedAt: &usedUTC},
	}}

	if diff := cmp.Diff(wantList, got); diff != "" {
		t.Errorf("ListTokens (-want +got):\n%s", diff)
	}
}

func TestListTokensMergesOAuthGrantsByName(t *testing.T) {
	used := time.Date(2026, 11, 9, 1, 0, 0, 0, time.FixedZone("x", 3600))
	f := &fakeTokens{
		tokens: []store.ClientToken{
			{Name: "ci", Connectors: []string{"github"}, CreatedAt: created},
			{Name: "zeta", Connectors: []string{"slack"}, CreatedAt: created},
		},
		grants: []store.OAuthGrant{{
			Name: "chatgpt", ClientID: "https://chatgpt.com/oauth/client.json", Resource: "https://wf.example/mcp",
			Connectors: []string{"github", "linear"}, CreatedAt: created, LastUsedAt: &used,
		}},
	}
	c, srv := serveTokens(t, f)

	code, body := get(t, srv, srv.URL+"/admin/tokens")

	if code != http.StatusOK {
		t.Fatalf("GET /admin/tokens = %d, want 200; body: %s", code, body)
	}

	// A grant is remote, names its client, and lists beside the tokens, by name.
	want := `{"tokens":[` +
		`{"name":"chatgpt","kind":"oauth","connectors":["github","linear"],"client":"https://chatgpt.com/oauth/client.json","remote":true,"created_at":"2026-11-08T10:00:00Z","last_used_at":"2026-11-09T00:00:00Z"},` +
		`{"name":"ci","kind":"token","connectors":["github"],"remote":false,"created_at":"2026-11-08T10:00:00Z"},` +
		`{"name":"zeta","kind":"token","connectors":["slack"],"remote":false,"created_at":"2026-11-08T10:00:00Z"}` +
		`]}` + "\n"

	if diff := cmp.Diff(want, string(body)); diff != "" {
		t.Errorf("body (-want +got):\n%s", diff)
	}

	if _, err := c.RevokeToken(t.Context(), "chatgpt"); err != nil {
		t.Fatalf("RevokeToken of a grant: %v", err)
	}

	got, err := c.ListTokens(t.Context())

	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}

	if len(got.Tokens) != 2 || got.Tokens[0].Name != "ci" {
		t.Errorf("tokens after revoking the grant = %+v, want ci and zeta", got.Tokens)
	}
}

func TestListTokensWithNoneIsAnEmptyArray(t *testing.T) {
	_, srv := serveTokens(t, &fakeTokens{})

	if code, body := get(t, srv, srv.URL+"/admin/tokens"); code != http.StatusOK || string(body) != `{"tokens":[]}`+"\n" {
		t.Errorf("GET /admin/tokens = %d %s, want 200 {\"tokens\":[]}", code, body)
	}
}

func TestRevokeToken(t *testing.T) {
	f := &fakeTokens{tokens: []store.ClientToken{{Name: "nightly", Connectors: []string{"github"}, CreatedAt: created}}}
	c, _ := serveTokens(t, f)

	got, err := c.RevokeToken(t.Context(), "nightly")

	if err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	if diff := cmp.Diff(admin.TokenRevoked{Name: "nightly"}, got); diff != "" {
		t.Errorf("RevokeToken (-want +got):\n%s", diff)
	}

	_, err = c.RevokeToken(t.Context(), "nightly")

	if statusOf(err) != http.StatusNotFound || !strings.Contains(err.Error(), `no client token or OAuth grant is called "nightly"`) {
		t.Errorf("second RevokeToken = %v, want 404 saying there is no such token", err)
	}
}

func TestTokensWithoutClientTokensAreNotImplemented(t *testing.T) {
	st, user := newStore(t)
	c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{Connectors: []string{"github"}}))

	_, errCreate := c.CreateToken(t.Context(), admin.TokenSpec{Name: "nightly", Connectors: []string{"github"}})
	_, errList := c.ListTokens(t.Context())
	_, errRevoke := c.RevokeToken(t.Context(), "nightly")

	for name, err := range map[string]error{"create": errCreate, "list": errList, "revoke": errRevoke} {
		if statusOf(err) != http.StatusNotImplemented {
			t.Errorf("%s = %v, want 501", name, err)
		}
	}
}

// post sends body to url on srv and returns the status and the body.
func post(t *testing.T, srv *httptest.Server, url, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))

	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)

	if err != nil {
		t.Fatal(err)
	}

	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, b
}
