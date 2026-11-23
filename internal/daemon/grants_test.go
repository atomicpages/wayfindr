package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/oauthserver"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/drivers"
)

// The token service's side of OAuth grants (design §7.11, daemon): the one
// lock over the store and the registry, the shared name space, and what the
// registry accepts after each change. They run against a real SQLite store
// and a real registry, through oauthserver.Grants and admin's ClientTokens,
// the seams the server and the admin API use. What a config change or the
// background loop does (applyOAuth, run) is tested through the running
// daemon, in grants_flow_test.go.

const (
	grantIssuer  = "https://wf.example.com"
	testClientID = "https://chatgpt.com/oauth/client.json"
)

// heldClock is a clock that moves only when a test says so; its timers never
// fire.
type heldClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *heldClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *heldClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *heldClock) NewTimer(time.Duration) Timer { return idleTimer{make(chan time.Time)} }

type idleTimer struct{ c chan time.Time }

func (t idleTimer) C() <-chan time.Time { return t.c }
func (idleTimer) Stop() bool            { return true }

var grantEpoch = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

// grantHost is a token service over a real store.
type grantHost struct {
	t      *testing.T
	tokens *clientTokens
	store  store.Store
	user   string
	clock  *heldClock
}

// newGrantHost is a token service with OAuth on for grantIssuer and
// testClientID.
func newGrantHost(t *testing.T) *grantHost {
	t.Helper()

	return newGrantHostFor(t, oauthServer(grantIssuer, true, testClientID))
}

// newGrantHostFor is a token service built as daemon.New builds it, for the
// running [server] table s.
func newGrantHostFor(t *testing.T, s config.Server) *grantHost {
	t.Helper()

	st, err := drivers.Open(t.Context(), "sqlite", filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatal(err)
	}

	clock := &heldClock{now: grantEpoch}
	tokens := &clientTokens{
		store: st, userID: user, registry: clienttoken.NewRegistry(nil, clock.Now), clock: clock,
		log: zerolog.Nop(), clients: map[string]struct{}{}, live: map[string]struct{}{},
	}

	if err := tokens.applyOAuth(t.Context(), s); err != nil {
		t.Fatalf("applyOAuth: %v", err)
	}

	return &grantHost{t: t, tokens: tokens, store: st, user: user, clock: clock}
}

func oauthServer(publicURL string, enabled bool, clients ...string) config.Server {
	return config.Server{PublicURL: publicURL, OAuth: config.OAuth{Enabled: enabled, Clients: clients}}
}

// grant creates a grant of the allowed client for github and linear, and
// returns when it was created.
func (h *grantHost) grant(name string) time.Time {
	h.t.Helper()
	created := h.clock.Now()

	err := h.tokens.CreateGrant(h.t.Context(), store.OAuthGrant{
		Name: name, ClientID: testClientID, Resource: oauthserver.Resource(grantIssuer),
		Connectors: []string{"github", "linear"}, CreatedAt: created,
	})

	if err != nil {
		h.t.Fatalf("CreateGrant %s: %v", name, err)
	}

	return created
}

// pair is an access and refresh token of grant, and their values' digests.
type pair struct{ access, refresh store.OAuthToken }

func (h *grantHost) pair(grant string) pair {
	h.t.Helper()
	a, err := clienttoken.Generate()

	if err != nil {
		h.t.Fatal(err)
	}

	r, err := clienttoken.Generate()

	if err != nil {
		h.t.Fatal(err)
	}

	exp := h.clock.Now().Add(oauthserver.AccessTTL)

	return pair{
		access:  store.OAuthToken{Digest: clienttoken.Digest(a), Grant: grant, Kind: store.OAuthAccess, CreatedAt: h.clock.Now(), ExpiresAt: &exp},
		refresh: store.OAuthToken{Digest: clienttoken.Digest(r), Grant: grant, Kind: store.OAuthRefresh, CreatedAt: h.clock.Now()},
	}
}

// issue gives grant, created at created, its first tokens through the
// service.
func (h *grantHost) issue(grant string, created time.Time) pair {
	h.t.Helper()
	p := h.pair(grant)

	if err := h.tokens.IssueGrantTokens(h.t.Context(), created, p.access, p.refresh); err != nil {
		h.t.Fatalf("IssueGrantTokens %s: %v", grant, err)
	}

	return p
}

// accepts reports whether the registry resolves the access token d.
func (h *grantHost) accepts(d [sha256.Size]byte) bool {
	_, ok := h.tokens.registry.Lookup(d)

	return ok
}

func TestCreateGrantRefusesNamesAClientTokenCouldNotHave(t *testing.T) {
	h := newGrantHost(t)

	for _, name := range []string{"default", "", "Has Caps", "-lead"} {
		err := h.tokens.CreateGrant(t.Context(), store.OAuthGrant{
			Name: name, ClientID: testClientID, Resource: oauthserver.Resource(grantIssuer),
			Connectors: []string{"github"}, CreatedAt: grantEpoch,
		})

		if !errors.Is(err, oauthserver.ErrInvalidName) {
			t.Errorf("CreateGrant(%q) = %v, want ErrInvalidName", name, err)
		}
	}

	grants, err := h.tokens.ListGrants(t.Context())

	if err != nil || len(grants) != 0 {
		t.Errorf("ListGrants after refused names = %v, %v; want none stored", grants, err)
	}
}

func TestGrantsAndClientTokensShareOneNameSpace(t *testing.T) {
	h := newGrantHost(t)

	if _, _, err := h.tokens.Create(t.Context(), "ci", []string{"github"}, false); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := h.tokens.CreateGrant(t.Context(), store.OAuthGrant{
		Name: "ci", ClientID: testClientID, Resource: oauthserver.Resource(grantIssuer),
		Connectors: []string{"github"}, CreatedAt: grantEpoch,
	})

	if !errors.Is(err, store.ErrExists) {
		t.Errorf("CreateGrant of a client token's name = %v, want ErrExists", err)
	}

	h.grant("chatgpt")

	if _, _, err := h.tokens.Create(t.Context(), "chatgpt", []string{"github"}, false); !errors.Is(err, store.ErrExists) {
		t.Errorf("Create of a grant's name = %v, want ErrExists", err)
	}

	if err := h.tokens.CreateGrant(t.Context(), store.OAuthGrant{
		Name: "chatgpt", ClientID: testClientID, Resource: oauthserver.Resource(grantIssuer),
		Connectors: []string{"github"}, CreatedAt: grantEpoch,
	}); !errors.Is(err, store.ErrExists) {
		t.Errorf("CreateGrant of a grant's name = %v, want ErrExists", err)
	}
}

func TestIssuedGrantTokenResolvesAsARemoteScopedClient(t *testing.T) {
	h := newGrantHost(t)
	created := h.grant("chatgpt")

	if h.accepts(h.pair("chatgpt").access.Digest) {
		t.Fatal("an access token nobody issued is accepted")
	}

	p := h.issue("chatgpt", created)
	got, ok := h.tokens.registry.Lookup(p.access.Digest)

	if want := (clienttoken.Client{Name: "chatgpt", Connectors: []string{"github", "linear"}, Remote: true}); !ok || !cmp.Equal(want, got) {
		t.Fatalf("Lookup of the issued access token = %+v, %v; want %+v", got, ok, want)
	}

	if h.accepts(p.refresh.Digest) {
		t.Error("a refresh token is accepted as a bearer")
	}

	stored, err := h.tokens.GrantToken(t.Context(), p.refresh.Digest)

	if err != nil || stored.UserID != h.user || stored.Kind != store.OAuthRefresh {
		t.Errorf("GrantToken(refresh) = %+v, %v; want the daemon's user set on the row", stored, err)
	}

	// The access token stops at its expiry without anything removing it.
	h.clock.advance(oauthserver.AccessTTL)

	if h.accepts(p.access.Digest) {
		t.Error("the access token is accepted at its expiry")
	}
}

// A code names its grant and the grant's creation time: a grant revoked and
// re-created under the same name since is another grant, and the old code's
// creation time gets it no tokens.
func TestIssueRefusesTheCreationTimeOfAGrantReCreatedSince(t *testing.T) {
	h := newGrantHost(t)
	old := h.grant("chatgpt")

	if err := h.tokens.RevokeGrant(t.Context(), "chatgpt"); err != nil {
		t.Fatal(err)
	}

	// Within a millisecond the store could not tell the two apart.
	h.clock.advance(time.Second)
	current := h.grant("chatgpt")
	p := h.pair("chatgpt")

	if err := h.tokens.IssueGrantTokens(t.Context(), old, p.access, p.refresh); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("IssueGrantTokens with the old grant's creation time = %v, want ErrNotFound", err)
	}

	if _, err := h.tokens.GrantToken(t.Context(), p.refresh.Digest); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a refused issue stored its refresh token: %v", err)
	}

	if h.accepts(p.access.Digest) {
		t.Error("a refused issue's access token is accepted")
	}

	// The store keeps milliseconds: a creation time with more is the same.
	if err := h.tokens.IssueGrantTokens(t.Context(), current.Add(time.Microsecond), p.access, p.refresh); err != nil || !h.accepts(p.access.Digest) {
		t.Errorf("IssueGrantTokens with the current grant's creation time = %v, accepted = %v", err, h.accepts(p.access.Digest))
	}
}

func TestRotationReplacesTheAccessTokenAndRevokeStopsTheNewOne(t *testing.T) {
	h := newGrantHost(t)
	created := h.grant("chatgpt")
	first := h.issue("chatgpt", created)
	next := h.pair("chatgpt")

	if err := h.tokens.RotateGrantTokens(t.Context(), first.refresh.Digest, next.access, next.refresh); err != nil {
		t.Fatalf("RotateGrantTokens: %v", err)
	}

	if h.accepts(first.access.Digest) || !h.accepts(next.access.Digest) {
		t.Fatalf("after a rotation old accepted = %v, new accepted = %v; want only the new", h.accepts(first.access.Digest), h.accepts(next.access.Digest))
	}

	// The spent refresh token is a reuse, and changes nothing.
	again := h.pair("chatgpt")

	if err := h.tokens.RotateGrantTokens(t.Context(), first.refresh.Digest, again.access, again.refresh); !errors.Is(err, store.ErrSpent) {
		t.Fatalf("RotateGrantTokens of a spent token = %v, want ErrSpent", err)
	}

	if h.accepts(again.access.Digest) || !h.accepts(next.access.Digest) {
		t.Error("a refused rotation changed what the registry accepts")
	}

	if err := h.tokens.RevokeGrant(t.Context(), "chatgpt"); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}

	if h.accepts(next.access.Digest) {
		t.Error("the access token is accepted after its grant was revoked")
	}

	if _, err := h.tokens.Grant(t.Context(), "chatgpt"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Grant after revoke = %v, want ErrNotFound", err)
	}

	if _, err := h.tokens.GrantToken(t.Context(), next.refresh.Digest); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GrantToken after revoke = %v, want ErrNotFound (the tokens went with the grant)", err)
	}

	// A revoked grant gets no new tokens.
	late := h.pair("chatgpt")

	if err := h.tokens.IssueGrantTokens(t.Context(), created, late.access, late.refresh); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("IssueGrantTokens for a revoked grant = %v, want ErrNotFound", err)
	}

	if h.accepts(late.access.Digest) {
		t.Error("a revoked grant's late access token is accepted")
	}
}

func TestRevokeDeletesEitherKindAndRevokeGrantOnlyGrants(t *testing.T) {
	h := newGrantHost(t)
	p := h.issue("chatgpt", h.grant("chatgpt"))

	_, value, err := h.tokens.Create(t.Context(), "ci", []string{"github"}, false)

	if err != nil {
		t.Fatal(err)
	}

	ci := clienttoken.Digest(value)

	if err := h.tokens.RevokeGrant(t.Context(), "ci"); !errors.Is(err, store.ErrNotFound) || !h.accepts(ci) {
		t.Errorf("RevokeGrant of a client token = %v, accepted = %v; want ErrNotFound and the token kept", err, h.accepts(ci))
	}

	if err := h.tokens.Revoke(t.Context(), "ci"); err != nil || h.accepts(ci) {
		t.Errorf("Revoke of a client token = %v, accepted = %v; want it gone", err, h.accepts(ci))
	}

	if err := h.tokens.Revoke(t.Context(), "chatgpt"); err != nil || h.accepts(p.access.Digest) {
		t.Errorf("Revoke of a grant = %v, accepted = %v; want it gone", err, h.accepts(p.access.Digest))
	}

	if err := h.tokens.Revoke(t.Context(), "chatgpt"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Revoke of a name neither table has = %v, want ErrNotFound", err)
	}
}

// A revoke racing a rotation must never leave the revoked grant's access
// token accepted: both hold the one lock across the store and the registry.
func TestRevokeRacingARotationLeavesNoAcceptedToken(t *testing.T) {
	h := newGrantHost(t)

	for i := range 60 {
		name := fmt.Sprintf("g%d", i)
		first := h.issue(name, h.grant(name))
		next := h.pair(name)
		start := make(chan struct{})

		var (
			wg        sync.WaitGroup
			rotateErr error
			revokeErr error
		)

		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start

			rotateErr = h.tokens.RotateGrantTokens(context.Background(), first.refresh.Digest, next.access, next.refresh)
		}()

		go func() {
			defer wg.Done()
			<-start

			// Both ways in: the grant revoke the server uses, and the admin's.
			if i%2 == 0 {
				revokeErr = h.tokens.RevokeGrant(context.Background(), name)
			} else {
				revokeErr = h.tokens.Revoke(context.Background(), name)
			}
		}()

		close(start)
		wg.Wait()

		if revokeErr != nil {
			t.Fatalf("revoke %s: %v", name, revokeErr)
		}

		if rotateErr != nil && !errors.Is(rotateErr, store.ErrNotFound) {
			t.Fatalf("rotate %s: %v, want success or ErrNotFound", name, rotateErr)
		}

		for what, d := range map[string][sha256.Size]byte{"first": first.access.Digest, "rotated": next.access.Digest} {
			if h.accepts(d) {
				t.Fatalf("grant %s revoked, but its %s access token is still accepted (rotate = %v)", name, what, rotateErr)
			}
		}
	}
}

// A suspended grant gets no tokens through the service, and a refused
// rotation leaves its refresh token unspent. The server refuses such
// requests before it asks (design §7.11, Token), so through the running
// daemon only a config change racing a token request reaches these
// refusals; here the service starts with OAuth off.
func TestSuspendedGrantGetsNoTokensAndKeepsItsRefreshToken(t *testing.T) {
	h := newGrantHostFor(t, oauthServer(grantIssuer, false, testClientID))
	created := h.grant("chatgpt")
	p := h.pair("chatgpt")

	if err := h.tokens.IssueGrantTokens(t.Context(), created, p.access, p.refresh); !errors.Is(err, oauthserver.ErrSuspended) {
		t.Fatalf("IssueGrantTokens while off = %v, want ErrSuspended", err)
	}

	if _, err := h.tokens.GrantToken(t.Context(), p.refresh.Digest); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a refused issue stored its refresh token: %v", err)
	}

	// Tokens issued before OAuth went off.
	if err := h.store.IssueOAuthTokens(t.Context(), h.user, p.access, p.refresh); err != nil {
		t.Fatal(err)
	}

	next := h.pair("chatgpt")

	if err := h.tokens.RotateGrantTokens(t.Context(), p.refresh.Digest, next.access, next.refresh); !errors.Is(err, oauthserver.ErrSuspended) {
		t.Errorf("RotateGrantTokens while off = %v, want ErrSuspended", err)
	}

	if h.accepts(p.access.Digest) || h.accepts(next.access.Digest) {
		t.Error("a suspended grant's access token is accepted")
	}

	if got, err := h.tokens.GrantToken(t.Context(), p.refresh.Digest); err != nil || got.UsedAt != nil {
		t.Errorf("refresh token after a refused rotation = %+v, %v; want it unspent", got, err)
	}
}
