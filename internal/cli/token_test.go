package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/store"
)

// scopedValue is the value fakeTokens hands out, so a test can look for it
// where it must never be.
const scopedValue = "wf_s_cli-test-value" //nolint:gosec // G101: a test fixture

// fakeTokens is the daemon's side of the client tokens, in memory: every
// token is created at the same time, and the first one listed was used.
type fakeTokens struct {
	mu     sync.Mutex
	tokens []store.ClientToken
	grants []store.OAuthGrant
}

func (f *fakeTokens) Create(_ context.Context, name string, connectors []string, remote bool) (store.ClientToken, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if slices.ContainsFunc(f.tokens, func(t store.ClientToken) bool { return t.Name == name }) {
		return store.ClientToken{}, "", fmt.Errorf("create client token %s: %w", name, store.ErrExists)
	}

	t := store.ClientToken{Name: name, Connectors: connectors, Remote: remote, CreatedAt: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)}
	f.tokens = append(f.tokens, t)

	return t, scopedValue, nil
}

func (f *fakeTokens) List(context.Context) ([]store.ClientToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.tokens), nil
}

func (f *fakeTokens) ListGrants(context.Context) ([]store.OAuthGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.grants), nil
}

func (f *fakeTokens) Revoke(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

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

// tokenHost serves the admin API over f, with github, linear, and slack
// configured, and returns a host that reaches it.
func tokenHost(t *testing.T, f *fakeTokens) Host {
	t.Helper()
	addr := serveAdminAPI(t, admin.Options{
		Upstreams: &fakeUpstreams{},
		Store:     &fakeStore{},
		UserID:    "local",
		Config:    func() admin.ConfigState { return admin.ConfigState{Connectors: []string{"github", "linear", "slack"}} },
		Tokens:    f,
		Logger:    zerolog.New(zerolog.NewTestWriter(t)),
	})
	host, root := testHost(t)
	writeStatusHost(t, root, addr)

	return host
}

func runToken(t *testing.T, host Host, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = RunWith(append([]string{"token"}, args...), &out, &errOut, host)

	return code, out.String(), errOut.String()
}

func TestTokenCreatePrintsTheValueOnce(t *testing.T) {
	host := tokenHost(t, &fakeTokens{})

	code, stdout, stderr := runToken(t, host, "create", "nightly-triage", "--connector", "github", "--connector", "linear")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr)
	}

	want := "Created client token nightly-triage. It can use: github, linear.\n" +
		scopedValue + "\n" +
		"This is the only time it is shown; store it where the client can read it.\n"

	if diff := cmp.Diff(want, stdout); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

func TestTokenCreateRemoteIsStoredAndListed(t *testing.T) {
	f := &fakeTokens{}
	host := tokenHost(t, f)

	code, stdout, stderr := runToken(t, host, "create", "grok", "--connector", "github", "--remote")

	if code != 0 {
		t.Fatalf("create --remote: exit code = %d, want 0; stderr: %s", code, stderr)
	}

	want := "Created remote client token grok. It can use: github.\n" +
		scopedValue + "\n" +
		"This is the only time it is shown; store it where the client can read it.\n"

	if diff := cmp.Diff(want, stdout); diff != "" {
		t.Errorf("create --remote stdout (-want +got):\n%s", diff)
	}

	if code, _, stderr := runToken(t, host, "create", "local", "--connector", "github"); code != 0 {
		t.Fatalf("create without --remote: exit code = %d; stderr: %s", code, stderr)
	}

	f.mu.Lock()
	got := map[string]bool{}

	for _, tok := range f.tokens {
		got[tok.Name] = tok.Remote
	}

	f.mu.Unlock()

	if diff := cmp.Diff(map[string]bool{"grok": true, "local": false}, got); diff != "" {
		t.Errorf("stored remote (-want +got):\n%s", diff)
	}

	code, stdout, stderr = runToken(t, host, "list")

	if code != 0 {
		t.Fatalf("list: exit code = %d; stderr: %s", code, stderr)
	}

	wantList := "NAME   KIND   CONNECTORS  REMOTE  CREATED               LAST USED\n" +
		"grok   token  github      yes     2026-11-08T10:00:00Z  -\n" +
		"local  token  github      no      2026-11-08T10:00:00Z  -\n"

	if diff := cmp.Diff(wantList, stdout); diff != "" {
		t.Errorf("list (-want +got):\n%s", diff)
	}

	code, stdout, stderr = runToken(t, host, "list", "--json")

	if code != 0 {
		t.Fatalf("list --json: exit code = %d; stderr: %s", code, stderr)
	}

	var list admin.TokenList

	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("list --json: %v\n%s", err, stdout)
	}

	if len(list.Tokens) != 2 || !list.Tokens[0].Remote || list.Tokens[1].Remote {
		t.Errorf("list --json = %+v, want grok remote and local not", list.Tokens)
	}
}

func TestTokenListNeverPrintsAValue(t *testing.T) {
	f := &fakeTokens{}
	host := tokenHost(t, f)

	for _, name := range []string{"nightly-triage", "ci"} {
		if code, _, stderr := runToken(t, host, "create", name, "--connector", "github", "--connector", "linear"); code != 0 {
			t.Fatalf("create %s: exit code = %d; stderr: %s", name, code, stderr)
		}
	}

	used := time.Date(2026, 11, 8, 22, 0, 4, 0, time.UTC)
	f.mu.Lock()
	f.tokens[0].LastUsedAt = &used
	f.mu.Unlock()

	code, stdout, stderr := runToken(t, host, "list")

	if code != 0 {
		t.Fatalf("list: exit code = %d, want 0; stderr: %s", code, stderr)
	}

	want := "NAME            KIND   CONNECTORS      REMOTE  CREATED               LAST USED\n" +
		"nightly-triage  token  github, linear  no      2026-11-08T10:00:00Z  2026-11-08T22:00:04Z\n" +
		"ci              token  github, linear  no      2026-11-08T10:00:00Z  -\n"

	if diff := cmp.Diff(want, stdout); diff != "" {
		t.Errorf("list (-want +got):\n%s", diff)
	}

	code, stdout, stderr = runToken(t, host, "list", "--json")

	if code != 0 {
		t.Fatalf("list --json: exit code = %d, want 0; stderr: %s", code, stderr)
	}

	if strings.Contains(stdout, scopedValue) || strings.Contains(stdout, `"token":`) {
		t.Errorf("list --json holds a token value:\n%s", stdout)
	}

	var got admin.TokenList

	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("list --json is not a TokenList: %v\n%s", err, stdout)
	}

	if len(got.Tokens) != 2 || got.Tokens[0].LastUsedAt == nil || got.Tokens[1].LastUsedAt != nil {
		t.Errorf("list --json = %+v, want both tokens, only the first used", got)
	}
}

func TestTokenListShowsOAuthGrantsWithTheirClientHost(t *testing.T) {
	f := &fakeTokens{
		tokens: []store.ClientToken{{Name: "ci", Connectors: []string{"github"}, CreatedAt: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)}},
		grants: []store.OAuthGrant{{
			Name: "chatgpt", ClientID: "https://chatgpt.com/oauth/client.json", Connectors: []string{"github", "linear"},
			CreatedAt: time.Date(2026, 11, 9, 8, 30, 0, 0, time.UTC),
		}},
	}
	host := tokenHost(t, f)

	code, stdout, stderr := runToken(t, host, "list")

	if code != 0 {
		t.Fatalf("list: exit code = %d, want 0; stderr: %s", code, stderr)
	}

	want := "NAME     KIND                 CONNECTORS      REMOTE  CREATED               LAST USED\n" +
		"chatgpt  oauth (chatgpt.com)  github, linear  yes     2026-11-09T08:30:00Z  -\n" +
		"ci       token                github          no      2026-11-08T10:00:00Z  -\n"

	if diff := cmp.Diff(want, stdout); diff != "" {
		t.Errorf("list (-want +got):\n%s", diff)
	}

	code, stdout, stderr = runToken(t, host, "list", "--json")

	if code != 0 {
		t.Fatalf("list --json: exit code = %d, want 0; stderr: %s", code, stderr)
	}

	var got admin.TokenList

	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("list --json is not a TokenList: %v\n%s", err, stdout)
	}

	if len(got.Tokens) != 2 || got.Tokens[0].Kind != admin.TokenKindOAuth || got.Tokens[0].Client != "https://chatgpt.com/oauth/client.json" || got.Tokens[1].Kind != admin.TokenKindToken {
		t.Errorf("list --json = %+v, want the grant (oauth, with its client) then the token", got.Tokens)
	}

	if code, _, stderr := runToken(t, host, "revoke", "chatgpt"); code != 0 {
		t.Fatalf("revoke of a grant: exit code = %d, want 0; stderr: %s", code, stderr)
	}

	f.mu.Lock()
	left := len(f.grants)
	f.mu.Unlock()

	if left != 0 {
		t.Errorf("grants after revoke = %d, want 0", left)
	}
}

func TestTokenListWithNoneSaysHowToCreateOne(t *testing.T) {
	for _, name := range []string{"list", "ls"} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runToken(t, tokenHost(t, &fakeTokens{}), name)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr)
			}

			if want := "No scoped client tokens. Create one with `wayfindr token create <name> --connector <connector>`.\n"; stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
		})
	}
}

func TestTokenRevoke(t *testing.T) {
	f := &fakeTokens{tokens: []store.ClientToken{{Name: "nightly", Connectors: []string{"github"}}}}
	host := tokenHost(t, f)

	code, stdout, stderr := runToken(t, host, "revoke", "nightly")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr)
	}

	if diff := cmp.Diff("Revoked nightly. Clients using it are refused from now on.\n", stdout); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	code, stdout, stderr = runToken(t, host, "revoke", "nightly")

	if code == 0 || stdout != "" || !strings.Contains(stderr, `no client token or OAuth grant is called "nightly"`) {
		t.Errorf("second revoke = %d, stdout %q, stderr %q; want an error saying there is no such token", code, stdout, stderr)
	}
}

func TestTokenCreateRefusals(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no connector", []string{"create", "nightly"}, `required flag(s) "connector" not set`},
		{"unknown connector", []string{"create", "nightly", "--connector", "gmail"}, `no connector is called "gmail"; configured: github, linear, slack`},
		{"reserved name", []string{"create", "default", "--connector", "github"}, `"default" is reserved for the default client token`},
		{"bad name", []string{"create", "Nightly", "--connector", "github"}, `client token name "Nightly" must match`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeTokens{}
			code, stdout, stderr := runToken(t, tokenHost(t, f), tt.args...)

			if code == 0 || stdout != "" || !strings.Contains(stderr, tt.want) {
				t.Errorf("token %v = %d, stdout %q, stderr %q; want an error containing %q", tt.args, code, stdout, stderr, tt.want)
			}

			if len(f.tokens) != 0 {
				t.Errorf("tokens = %+v, want none created", f.tokens)
			}
		})
	}
}
