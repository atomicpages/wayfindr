package daemon_test

import (
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/oauthserver"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/drivers"
)

// OAuth grants through the running daemon (design §7.11, daemon): a grant
// stored before the daemon started is accepted from its first request, it
// lists beside the client tokens, either kind revokes through the same
// route, changing the config suspends and restores it, a failed restore is
// retried, and the daemon prunes what OAuth no longer needs.

const (
	flowIssuer = "https://wf.example.com"
	flowClient = "https://chatgpt.com/oauth/client.json"
)

func grantsConfig(t *testing.T) map[string]any {
	t.Helper()
	cfg := tokenConfig(t)
	cfg["server"] = map[string]any{
		"listen":     cfg["server"].(map[string]any)["listen"],
		"public_url": flowIssuer,
		"oauth":      map[string]any{"enabled": true},
	}

	return cfg
}

// seedGrant stores a grant called chatgpt of the default client for files, with an access
// token valid for an hour and a refresh token, on the initialized host h. It
// returns the two values.
func seedGrant(t *testing.T, h host) (access, refresh string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)

	return seedOne(t, h, seeded{name: "chatgpt", client: flowClient, created: now, expires: now.Add(time.Hour)})
}

// seeded is a grant for files to store before the daemon starts.
type seeded struct {
	name, client string
	created      time.Time
	// expires is when its access token expires. Zero stores no tokens: a
	// grant approved whose code was never exchanged.
	expires time.Time
}

// seedOne stores s on the initialized host h, and returns its access and
// refresh token values ("" when it has none).
func seedOne(t *testing.T, h host, s seeded) (access, refresh string) {
	t.Helper()
	st, err := drivers.Open(t.Context(), "sqlite", h.paths.DBFile())

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	defer func() { _ = st.Close() }()

	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatal(err)
	}

	if err := st.CreateOAuthGrant(t.Context(), store.OAuthGrant{
		UserID: user, Name: s.name, ClientID: s.client, Resource: oauthserver.Resource(flowIssuer),
		Connectors: []string{"files"}, CreatedAt: s.created,
	}); err != nil {
		t.Fatalf("CreateOAuthGrant: %v", err)
	}

	if s.expires.IsZero() {
		return "", ""
	}

	if access, err = clienttoken.Generate(); err != nil {
		t.Fatal(err)
	}

	if refresh, err = clienttoken.Generate(); err != nil {
		t.Fatal(err)
	}

	err = st.IssueOAuthTokens(t.Context(), user,
		store.OAuthToken{UserID: user, Digest: clienttoken.Digest(access), Grant: s.name, Kind: store.OAuthAccess, CreatedAt: s.created, ExpiresAt: &s.expires},
		store.OAuthToken{UserID: user, Digest: clienttoken.Digest(refresh), Grant: s.name, Kind: store.OAuthRefresh, CreatedAt: s.created})

	if err != nil {
		t.Fatalf("IssueOAuthTokens: %v", err)
	}

	return access, refresh
}

func TestGrantStoredBeforeStartupIsAcceptedFromTheFirstRequest(t *testing.T) {
	h := newHost(t)
	initHost(t, h, grantsConfig(t))
	access, refresh := seedGrant(t, h)

	r := start(t, h, h.options(t))

	if code := r.statusAs(t, access); code != http.StatusForbidden {
		t.Errorf("/admin/status with the grant's access token = %d, want 403 (accepted as a scoped client, not for admin)", code)
	}

	if code := r.statusAs(t, refresh); code != http.StatusUnauthorized {
		t.Errorf("/admin/status with the grant's refresh token = %d, want 401", code)
	}
}

func TestTokenListHasGrantsAndRevokeTakesEitherKind(t *testing.T) {
	h := newHost(t)
	initHost(t, h, grantsConfig(t))
	access, _ := seedGrant(t, h)
	r := start(t, h, h.options(t))

	created, err := r.admin().CreateToken(t.Context(), admin.TokenSpec{Name: "ci", Connectors: []string{"files"}})

	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	list, err := r.admin().ListTokens(t.Context())

	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}

	if len(list.Tokens) != 2 {
		t.Fatalf("ListTokens = %+v, want the grant and the token", list.Tokens)
	}

	got := list.Tokens[0]
	want := admin.Token{Name: "chatgpt", Kind: admin.TokenKindOAuth, Connectors: []string{"files"}, Client: flowClient, Remote: true, CreatedAt: got.CreatedAt}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the grant as listed (-want +got):\n%s", diff)
	}

	if list.Tokens[1].Name != "ci" || list.Tokens[1].Kind != admin.TokenKindToken || list.Tokens[1].Client != "" {
		t.Errorf("the client token as listed = %+v", list.Tokens[1])
	}

	if _, err := r.admin().RevokeToken(t.Context(), "chatgpt"); err != nil {
		t.Fatalf("RevokeToken of the grant: %v", err)
	}

	if code := r.statusAs(t, access); code != http.StatusUnauthorized {
		t.Errorf("/admin/status with the revoked grant's access token = %d, want 401", code)
	}

	if code := r.statusAs(t, created.Token); code != http.StatusForbidden {
		t.Errorf("/admin/status with the client token after the grant's revoke = %d, want 403 (untouched)", code)
	}

	if _, err := r.admin().RevokeToken(t.Context(), "chatgpt"); err == nil {
		t.Error("revoking the grant twice succeeded, want a 404")
	}

	list, err = r.admin().ListTokens(t.Context())

	if err != nil || len(list.Tokens) != 1 || list.Tokens[0].Name != "ci" {
		t.Errorf("ListTokens after the revoke = %+v, %v; want only ci", list.Tokens, err)
	}
}

func TestTurningOAuthOffSuspendsGrantsAtOnceAndOnRestoresThem(t *testing.T) {
	h := newHost(t)
	initHost(t, h, grantsConfig(t))
	access, _ := seedGrant(t, h)
	r := start(t, h, h.options(t))

	if code := r.statusAs(t, access); code != http.StatusForbidden {
		t.Fatalf("with OAuth on: %d, want 403", code)
	}

	if _, err := r.admin().SetOAuthEnabled(t.Context(), false); err != nil {
		t.Fatalf("SetOAuthEnabled(false): %v", err)
	}

	if code := r.statusAs(t, access); code != http.StatusUnauthorized {
		t.Errorf("with OAuth off: %d, want 401 (suspended)", code)
	}

	// Suspended, not revoked: it still lists.
	if list, err := r.admin().ListTokens(t.Context()); err != nil || len(list.Tokens) != 1 {
		t.Errorf("ListTokens while suspended = %+v, %v; want the grant", list.Tokens, err)
	}

	if _, err := r.admin().SetOAuthEnabled(t.Context(), true); err != nil {
		t.Fatalf("SetOAuthEnabled(true): %v", err)
	}

	if code := r.statusAs(t, access); code != http.StatusForbidden {
		t.Errorf("with OAuth on again: %d, want 403 (the unexpired token is back)", code)
	}
}

func TestChangingPublicURLSuspendsGrantsOfTheOldOne(t *testing.T) {
	h := newHost(t)
	initHost(t, h, grantsConfig(t))
	access, _ := seedGrant(t, h)
	r := start(t, h, h.options(t))

	if _, err := r.admin().SetOAuthEnabled(t.Context(), false); err != nil {
		t.Fatal(err)
	}

	if _, err := r.admin().SetPublicURL(t.Context(), "https://other.example.com"); err != nil {
		t.Fatalf("SetPublicURL: %v", err)
	}

	if _, err := r.admin().SetOAuthEnabled(t.Context(), true); err != nil {
		t.Fatal(err)
	}

	if code := r.statusAs(t, access); code != http.StatusUnauthorized {
		t.Errorf("under another public_url: %d, want 401 (the grant was approved for the old resource)", code)
	}

	if _, err := r.admin().SetOAuthEnabled(t.Context(), false); err != nil {
		t.Fatal(err)
	}

	if _, err := r.admin().SetPublicURL(t.Context(), flowIssuer); err != nil {
		t.Fatal(err)
	}

	if _, err := r.admin().SetOAuthEnabled(t.Context(), true); err != nil {
		t.Fatal(err)
	}

	if code := r.statusAs(t, access); code != http.StatusForbidden {
		t.Errorf("with the old public_url back: %d, want 403", code)
	}
}

// waitAccess polls /admin/status with token until it answers want: a config
// file edit reaches the daemon through its watcher, a moment later.
func (r running) waitAccess(t *testing.T, token string, want int, why string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)

	for {
		got := r.statusAs(t, token)

		if got == want {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("%s: /admin/status with the access token = %d, want %d", why, got, want)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// grantNames lists the OAuth grants through the admin API.
func (r running) grantNames(t *testing.T) []string {
	t.Helper()
	list, err := r.admin().ListTokens(t.Context())

	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}

	names := []string{}

	for _, tok := range list.Tokens {
		if tok.Kind == admin.TokenKindOAuth {
			names = append(names, tok.Name)
		}
	}

	return names
}

// execSQL runs stmt on h's database behind the running daemon's back: the
// test's way to make the daemon's store fail, and then recover.
func execSQL(t *testing.T, h host, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.paths.DBFile()+"?_pragma=busy_timeout(5000)")

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = db.Close() }()

	if _, err := db.ExecContext(t.Context(), stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

func TestStartupAcceptsOnlyUnexpiredTokensOfGrantsNotSuspended(t *testing.T) {
	h := newHost(t)
	initHost(t, h, grantsConfig(t))
	now := time.Now().UTC().Truncate(time.Millisecond)
	live, _ := seedOne(t, h, seeded{name: "chatgpt", client: flowClient, created: now, expires: now.Add(time.Hour)})
	expired, _ := seedOne(t, h, seeded{name: "dormant", client: flowClient, created: now.Add(-2 * time.Hour), expires: now.Add(-time.Hour)})
	otherClient, _ := seedOne(t, h, seeded{name: "gone-client", client: "https://other.example.org/client.json", created: now, expires: now.Add(time.Hour)})
	r := start(t, h, h.options(t))

	for _, tc := range []struct {
		what, token string
		want        int
	}{
		{"an unexpired access token", live, http.StatusForbidden},
		{"an expired access token", expired, http.StatusUnauthorized},
		{"the access token of a grant whose client is no longer allowed", otherClient, http.StatusUnauthorized},
	} {
		if got := r.statusAs(t, tc.token); got != tc.want {
			t.Errorf("/admin/status with %s = %d, want %d", tc.what, got, tc.want)
		}
	}
}

func TestRemovingAClientSuspendsItsGrantsUntilItIsBack(t *testing.T) {
	h := newHost(t)
	cfg := grantsConfig(t)
	initHost(t, h, cfg)
	access, _ := seedGrant(t, h)
	r := start(t, h, h.options(t))

	if code := r.statusAs(t, access); code != http.StatusForbidden {
		t.Fatalf("with the client allowed: %d, want 403", code)
	}

	server := cfg["server"].(map[string]any)
	server["oauth"] = map[string]any{"enabled": true, "clients": []string{"https://other.example.org/client.json"}}
	h.writeConfig(t, cfg)
	r.waitAccess(t, access, http.StatusUnauthorized, "with its client removed from server.oauth.clients")

	// Suspended, not revoked: it still lists.
	if diff := cmp.Diff([]string{"chatgpt"}, r.grantNames(t)); diff != "" {
		t.Errorf("grants while suspended (-want +got):\n%s", diff)
	}

	server["oauth"] = map[string]any{"enabled": true}
	h.writeConfig(t, cfg)
	r.waitAccess(t, access, http.StatusForbidden, "with the client allowed again")
}

func TestATrailingSlashOnPublicURLDoesNotSuspendAGrant(t *testing.T) {
	h := newHost(t)
	initHost(t, h, grantsConfig(t))
	access, _ := seedGrant(t, h)
	r := start(t, h, h.options(t))

	if _, err := r.admin().SetPublicURL(t.Context(), flowIssuer+"/"); err != nil {
		t.Fatalf("SetPublicURL: %v", err)
	}

	if code := r.statusAs(t, access); code != http.StatusForbidden {
		t.Errorf("with a trailing slash on public_url: %d, want 403 (the issuer is the same)", code)
	}
}

// A store error while a reload restores grants leaves their access tokens
// refused (it fails closed), and the daemon retries every minute until the
// store answers, with no further reload.
func TestAFailedRestoreOfGrantsIsRetriedEachMinute(t *testing.T) {
	h := newHost(t)
	initHost(t, h, grantsConfig(t))
	access, _ := seedGrant(t, h)
	clock := newFakeClock()
	opts := h.options(t)
	opts.Clock = clock
	r := start(t, h, opts)
	clock.waitArmed(t, epoch.Add(time.Minute))

	if _, err := r.admin().SetOAuthEnabled(t.Context(), false); err != nil {
		t.Fatalf("SetOAuthEnabled(false): %v", err)
	}

	execSQL(t, h, "ALTER TABLE oauth_tokens RENAME TO oauth_tokens_hidden")

	if _, err := r.admin().SetOAuthEnabled(t.Context(), true); err != nil {
		t.Fatalf("SetOAuthEnabled(true): %v", err)
	}

	if code := r.statusAs(t, access); code != http.StatusUnauthorized {
		t.Fatalf("after a reload the store failed: %d, want 401 (fails closed)", code)
	}

	execSQL(t, h, "ALTER TABLE oauth_tokens_hidden RENAME TO oauth_tokens")

	if code := r.statusAs(t, access); code != http.StatusUnauthorized {
		t.Fatalf("before the retry: %d, want 401", code)
	}

	// The minute passes: the retry loads the grants, and the next is armed.
	clock.advance(time.Minute)
	clock.waitArmed(t, epoch.Add(2*time.Minute))

	if code := r.statusAs(t, access); code != http.StatusForbidden {
		t.Errorf("after the retry: %d, want 403", code)
	}
}

// The daemon prunes what OAuth no longer needs when it starts and every
// hour after, and records a grant's use with the client tokens' each minute.
func TestRunPrunesOAuthHourlyAndRecordsGrantUse(t *testing.T) {
	h := newHost(t)
	initHost(t, h, grantsConfig(t))
	access, _ := seedOne(t, h, seeded{name: "chatgpt", client: flowClient, created: epoch, expires: epoch.Add(3 * time.Hour)})
	// Approved, never exchanged: one long ago, pruned at start; one just
	// now, pruned an hour later.
	seedOne(t, h, seeded{name: "long-ago", client: flowClient, created: epoch.Add(-time.Hour)})
	seedOne(t, h, seeded{name: "just-now", client: flowClient, created: epoch})
	clock := newFakeClock()
	opts := h.options(t)
	opts.Clock = clock
	r := start(t, h, opts)
	// The touch timer is armed once the prune at start is done.
	clock.waitArmed(t, epoch.Add(time.Minute))

	if diff := cmp.Diff([]string{"chatgpt", "just-now"}, r.grantNames(t)); diff != "" {
		t.Errorf("grants after the prune at start (-want +got):\n%s", diff)
	}

	if code := r.statusAs(t, access); code != http.StatusForbidden {
		t.Fatalf("/admin/status with the access token = %d, want 403", code)
	}

	clock.advance(time.Minute)
	clock.waitArmed(t, epoch.Add(2*time.Minute))

	list, err := r.admin().ListTokens(t.Context())

	if err != nil || len(list.Tokens) == 0 || list.Tokens[0].LastUsedAt == nil || !list.Tokens[0].LastUsedAt.Equal(epoch) {
		t.Errorf("after a minute: %+v, %v; want chatgpt last used at %v", list.Tokens, err, epoch)
	}

	clock.advance(time.Hour)
	clock.waitArmed(t, epoch.Add(time.Hour+2*time.Minute))

	if diff := cmp.Diff([]string{"chatgpt"}, r.grantNames(t)); diff != "" {
		t.Errorf("grants after the hourly prune (-want +got):\n%s", diff)
	}
}
