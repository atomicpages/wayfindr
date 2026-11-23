package storetest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/atomicpages/wayfindr/internal/store"
)

// oauthBase is the fixed time the OAuth fixtures are made at.
var oauthBase = time.UnixMilli(1_762_000_000_000)

// oauthDigest is the digest of the fixture OAuth token called tag.
func oauthDigest(tag string) [sha256.Size]byte { return sha256.Sum256([]byte("wf_o_" + tag)) }

func oauthGrant(user, name string, connectors ...string) store.OAuthGrant {
	return store.OAuthGrant{
		UserID:     user,
		Name:       name,
		ClientID:   "https://chatgpt.example/client.json",
		Resource:   "https://wayfindr.example/mcp",
		Connectors: connectors,
		CreatedAt:  oauthBase,
	}
}

// accessToken is a fixture access token of grant, made at oauthBase, that
// expires an hour later; its digest is oauthDigest(tag).
func accessToken(user, grant, tag string) store.OAuthToken {
	exp := oauthBase.Add(time.Hour)

	return store.OAuthToken{
		UserID: user, Digest: oauthDigest(tag), Grant: grant, Kind: store.OAuthAccess,
		CreatedAt: oauthBase, ExpiresAt: &exp,
	}
}

// refreshToken is a fixture refresh token of grant; its digest is
// oauthDigest(tag).
func refreshToken(user, grant, tag string) store.OAuthToken {
	return store.OAuthToken{
		UserID: user, Digest: oauthDigest(tag), Grant: grant, Kind: store.OAuthRefresh,
		CreatedAt: oauthBase,
	}
}

func createOAuthGrants(t *testing.T, s store.Store, grants ...store.OAuthGrant) {
	t.Helper()

	for _, g := range grants {
		if err := s.CreateOAuthGrant(context.Background(), g); err != nil {
			t.Fatalf("CreateOAuthGrant(%s): %v", g.Name, err)
		}
	}
}

// issue stores a grant's first tokens, access "<grant>-a1" and refresh
// "<grant>-r1".
func issue(t *testing.T, s store.Store, user, grant string) {
	t.Helper()

	if err := s.IssueOAuthTokens(context.Background(), user,
		accessToken(user, grant, grant+"-a1"), refreshToken(user, grant, grant+"-r1")); err != nil {
		t.Fatalf("IssueOAuthTokens(%s): %v", grant, err)
	}
}

func listOAuthGrants(t *testing.T, s store.Store, user string) []store.OAuthGrant {
	t.Helper()
	got, err := s.ListOAuthGrants(context.Background(), user)

	if err != nil {
		t.Fatalf("ListOAuthGrants: %v", err)
	}

	return got
}

// oauthToken returns the user's token with the fixture digest tag, or nil.
func oauthToken(t *testing.T, s store.Store, user, tag string) *store.OAuthToken {
	t.Helper()
	got, err := s.GetOAuthToken(context.Background(), user, oauthDigest(tag))

	if errors.Is(err, store.ErrNotFound) {
		return nil
	}

	if err != nil {
		t.Fatalf("GetOAuthToken(%s): %v", tag, err)
	}

	return &got
}

var approxTime = cmpopts.EquateApproxTime(0)

func testOAuthGrantsCreateGetListAndDelete(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)

	if got := listOAuthGrants(t, s, user); len(got) != 0 {
		t.Fatalf("ListOAuthGrants before any create = %v, want none", got)
	}

	chat := oauthGrant(user, "chatgpt", "linear", "github")
	// A sub-millisecond creation time is kept to the millisecond, and
	// LastUsedAt is the store's to set.
	chat.CreatedAt = chat.CreatedAt.Add(456 * time.Microsecond)
	chat.LastUsedAt = &chat.CreatedAt
	ci := oauthGrant(user, "ci", "github")
	createOAuthGrants(t, s, chat, ci)

	wantChat, wantCI := chat, ci
	wantChat.CreatedAt = oauthBase
	wantChat.LastUsedAt = nil

	// Ordered by name, connectors as given.
	if diff := cmp.Diff([]store.OAuthGrant{wantChat, wantCI}, listOAuthGrants(t, s, user), approxTime); diff != "" {
		t.Fatalf("ListOAuthGrants (-want +got):\n%s", diff)
	}

	got, err := s.GetOAuthGrant(ctx, user, "chatgpt")

	if err != nil {
		t.Fatalf("GetOAuthGrant: %v", err)
	}

	if diff := cmp.Diff(wantChat, got, approxTime); diff != "" {
		t.Errorf("GetOAuthGrant (-want +got):\n%s", diff)
	}

	if _, err := s.GetOAuthGrant(ctx, user, "nightly"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetOAuthGrant(nightly): err = %v, want ErrNotFound", err)
	}

	if _, err := s.GetOAuthGrant(ctx, unknownUser(t), "chatgpt"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another user's GetOAuthGrant(chatgpt): err = %v, want ErrNotFound", err)
	}

	if got := listOAuthGrants(t, s, unknownUser(t)); len(got) != 0 {
		t.Errorf("another user's ListOAuthGrants = %v, want none", got)
	}

	if err := s.CreateOAuthGrant(ctx, oauthGrant(user, "chatgpt", "linear")); !errors.Is(err, store.ErrExists) {
		t.Errorf("CreateOAuthGrant(chatgpt) again: err = %v, want ErrExists", err)
	}

	if err := s.DeleteOAuthGrant(ctx, user, "ci"); err != nil {
		t.Fatalf("DeleteOAuthGrant: %v", err)
	}

	if diff := cmp.Diff([]store.OAuthGrant{wantChat}, listOAuthGrants(t, s, user), approxTime); diff != "" {
		t.Errorf("ListOAuthGrants after delete (-want +got):\n%s", diff)
	}

	if err := s.DeleteOAuthGrant(ctx, user, "ci"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteOAuthGrant(ci) again: err = %v, want ErrNotFound", err)
	}

	if err := s.DeleteOAuthGrant(ctx, unknownUser(t), "chatgpt"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another user's DeleteOAuthGrant(chatgpt): err = %v, want ErrNotFound", err)
	}

	// The name is free again once its grant is deleted.
	createOAuthGrants(t, s, ci)
}

func testCreateOAuthGrantRefusesIncompleteGrants(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)

	for name, mutate := range map[string]func(*store.OAuthGrant){
		"nil connectors":   func(g *store.OAuthGrant) { g.Connectors = nil },
		"empty connectors": func(g *store.OAuthGrant) { g.Connectors = []string{} },
		"no client ID":     func(g *store.OAuthGrant) { g.ClientID = "" },
		"no resource":      func(g *store.OAuthGrant) { g.Resource = "" },
		"no creation time": func(g *store.OAuthGrant) { g.CreatedAt = time.Time{} },
	} {
		g := oauthGrant(user, "chatgpt", "linear")
		mutate(&g)

		if err := s.CreateOAuthGrant(context.Background(), g); err == nil {
			t.Errorf("CreateOAuthGrant(%s): err = nil, want an error", name)
		} else if errors.Is(err, store.ErrExists) {
			t.Errorf("CreateOAuthGrant(%s): err = %v, want a refusal that is not ErrExists", name, err)
		}
	}

	if got := listOAuthGrants(t, s, user); len(got) != 0 {
		t.Errorf("ListOAuthGrants = %v, want none", got)
	}
}

func testOAuthGrantsAndClientTokensShareANameSpace(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)

	// A client token's name is refused for a grant, and the other way round.
	createClientTokens(t, s, clientToken(user, "ci", "github"))
	createOAuthGrants(t, s, oauthGrant(user, "chatgpt", "linear"))

	if err := s.CreateOAuthGrant(ctx, oauthGrant(user, "ci", "github")); !errors.Is(err, store.ErrExists) {
		t.Errorf("CreateOAuthGrant(ci), a client token's name: err = %v, want ErrExists", err)
	}

	taken := clientToken(user, "chatgpt", "linear")

	if err := s.CreateClientToken(ctx, taken); !errors.Is(err, store.ErrExists) {
		t.Errorf("CreateClientToken(chatgpt), a grant's name: err = %v, want ErrExists", err)
	}

	if got := listClientTokens(t, s, user); len(got) != 1 || got[0].Name != "ci" {
		t.Errorf("ListClientTokens = %v, want only ci", got)
	}

	if got := listOAuthGrants(t, s, user); len(got) != 1 || got[0].Name != "chatgpt" {
		t.Errorf("ListOAuthGrants = %v, want only chatgpt", got)
	}

	// Each name is free again once its owner is deleted.
	if err := s.DeleteOAuthGrant(ctx, user, "chatgpt"); err != nil {
		t.Fatalf("DeleteOAuthGrant: %v", err)
	}

	createClientTokens(t, s, taken)

	if err := s.DeleteClientToken(ctx, user, "ci"); err != nil {
		t.Fatalf("DeleteClientToken: %v", err)
	}

	createOAuthGrants(t, s, oauthGrant(user, "ci", "github"))

	// And the swap holds the other way: chatgpt is now a client token's name.
	if err := s.CreateOAuthGrant(ctx, oauthGrant(user, "chatgpt", "linear")); !errors.Is(err, store.ErrExists) {
		t.Errorf("CreateOAuthGrant(chatgpt), now a client token's name: err = %v, want ErrExists", err)
	}
}

// testOAuthNameSpaceHoldsUnderRacingCreates races a grant and a client token
// for one name: exactly one is stored.
func testOAuthNameSpaceHoldsUnderRacingCreates(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)

	const rounds = 20

	for i := range rounds {
		name := fmt.Sprintf("race-%d", i)
		errs := make([]error, 2)

		var wg sync.WaitGroup

		start := make(chan struct{})

		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start

			errs[0] = s.CreateOAuthGrant(ctx, oauthGrant(user, name, "linear"))
		}()
		go func() {
			defer wg.Done()
			<-start

			errs[1] = s.CreateClientToken(ctx, clientToken(user, name, "linear"))
		}()

		close(start)
		wg.Wait()

		wins := 0

		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, store.ErrExists):
				t.Fatalf("round %d: err = %v, want nil or ErrExists", i, err)
			}
		}

		if wins != 1 {
			t.Fatalf("round %d: %d creates of %s won (%v), want exactly 1", i, wins, name, errs)
		}
	}
}

func testIssueOAuthTokens(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	createOAuthGrants(t, s, oauthGrant(user, "chatgpt", "linear"))

	access := accessToken(user, "chatgpt", "a1")
	// Times are kept to the millisecond.
	access.CreatedAt = access.CreatedAt.Add(456 * time.Microsecond)
	refresh := refreshToken(user, "chatgpt", "r1")

	if err := s.IssueOAuthTokens(ctx, user, access, refresh); err != nil {
		t.Fatalf("IssueOAuthTokens: %v", err)
	}

	wantAccess := access
	wantAccess.CreatedAt = oauthBase

	got := oauthToken(t, s, user, "a1")

	if got == nil {
		t.Fatal("GetOAuthToken(a1): not found")
	}

	if diff := cmp.Diff(wantAccess, *got, approxTime); diff != "" {
		t.Errorf("access token (-want +got):\n%s", diff)
	}

	got = oauthToken(t, s, user, "r1")

	if got == nil {
		t.Fatal("GetOAuthToken(r1): not found")
	}

	if diff := cmp.Diff(refresh, *got, approxTime); diff != "" {
		t.Errorf("refresh token (-want +got):\n%s", diff)
	}

	if _, err := s.GetOAuthToken(ctx, unknownUser(t), oauthDigest("a1")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another user's GetOAuthToken(a1): err = %v, want ErrNotFound", err)
	}

	// The grant already has tokens: a second exchange is refused and
	// stores nothing.
	err := s.IssueOAuthTokens(ctx, user, accessToken(user, "chatgpt", "a2"), refreshToken(user, "chatgpt", "r2"))

	if !errors.Is(err, store.ErrExists) {
		t.Errorf("IssueOAuthTokens again: err = %v, want ErrExists", err)
	}

	if oauthToken(t, s, user, "a2") != nil || oauthToken(t, s, user, "r2") != nil {
		t.Error("a refused IssueOAuthTokens stored a token")
	}

	// No such grant.
	err = s.IssueOAuthTokens(ctx, user, accessToken(user, "nightly", "a3"), refreshToken(user, "nightly", "r3"))

	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("IssueOAuthTokens for a missing grant: err = %v, want ErrNotFound", err)
	}
}

func testIssueOAuthTokensRefusesMalformedTokens(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	createOAuthGrants(t, s, oauthGrant(user, "chatgpt", "linear"), oauthGrant(user, "other", "linear"))
	spentAt := oauthBase

	for name, mutate := range map[string]func(access, refresh *store.OAuthToken){
		"access without expiry":    func(a, _ *store.OAuthToken) { a.ExpiresAt = nil },
		"refresh with expiry":      func(_, r *store.OAuthToken) { r.ExpiresAt = a1Expiry() },
		"access of the wrong kind": func(a, _ *store.OAuthToken) { a.Kind = store.OAuthRefresh },
		"refresh of the wrong kind": func(_, r *store.OAuthToken) {
			r.Kind = store.OAuthAccess
		},
		"access already spent": func(a, _ *store.OAuthToken) { a.UsedAt = &spentAt },
		"refresh already spent": func(_, r *store.OAuthToken) {
			r.UsedAt = &spentAt
		},
		"different grants": func(_, r *store.OAuthToken) { r.Grant = "other" },
		"one digest":       func(a, r *store.OAuthToken) { r.Digest = a.Digest },
		"no creation time": func(a, _ *store.OAuthToken) { a.CreatedAt = time.Time{} },
	} {
		access, refresh := accessToken(user, "chatgpt", "a1"), refreshToken(user, "chatgpt", "r1")
		mutate(&access, &refresh)

		err := s.IssueOAuthTokens(context.Background(), user, access, refresh)

		if err == nil || errors.Is(err, store.ErrExists) || errors.Is(err, store.ErrNotFound) {
			t.Errorf("IssueOAuthTokens(%s): err = %v, want a refusal", name, err)
		}
	}

	if oauthToken(t, s, user, "a1") != nil || oauthToken(t, s, user, "r1") != nil {
		t.Error("a refused IssueOAuthTokens stored a token")
	}
}

func a1Expiry() *time.Time {
	exp := oauthBase.Add(time.Hour)

	return &exp
}

func testRotateOAuthTokens(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	createOAuthGrants(t, s, oauthGrant(user, "chatgpt", "linear"), oauthGrant(user, "claude", "github"))
	issue(t, s, user, "chatgpt")
	issue(t, s, user, "claude")

	at := oauthBase.Add(50*time.Minute + 456*time.Microsecond)
	newAccess := accessToken(user, "chatgpt", "chatgpt-a2")
	newRefresh := refreshToken(user, "chatgpt", "chatgpt-r2")

	if err := s.RotateOAuthTokens(ctx, user, oauthDigest("chatgpt-r1"), at, newAccess, newRefresh); err != nil {
		t.Fatalf("RotateOAuthTokens: %v", err)
	}

	// The old refresh token stays, spent at the given time to the
	// millisecond.
	spent := oauthToken(t, s, user, "chatgpt-r1")

	if spent == nil || spent.UsedAt == nil {
		t.Fatalf("old refresh token = %+v, want it kept and spent", spent)
	}

	if want := oauthBase.Add(50 * time.Minute); !spent.UsedAt.Equal(want) {
		t.Errorf("old refresh token UsedAt = %v, want %v", spent.UsedAt, want)
	}

	// The grant's old access token is gone; the successors are stored and
	// unspent.
	if oauthToken(t, s, user, "chatgpt-a1") != nil {
		t.Error("old access token still stored")
	}

	for _, tag := range []string{"chatgpt-a2", "chatgpt-r2"} {
		if got := oauthToken(t, s, user, tag); got == nil || got.UsedAt != nil {
			t.Errorf("successor %s = %+v, want it stored and unspent", tag, got)
		}
	}

	// Another grant's tokens are untouched.
	for _, tag := range []string{"claude-a1", "claude-r1"} {
		if got := oauthToken(t, s, user, tag); got == nil || got.UsedAt != nil {
			t.Errorf("other grant's %s = %+v, want it untouched", tag, got)
		}
	}

	// The new refresh token rotates in turn, and the chain of spent tokens
	// is kept.
	if err := s.RotateOAuthTokens(ctx, user, oauthDigest("chatgpt-r2"), at.Add(time.Minute),
		accessToken(user, "chatgpt", "chatgpt-a3"), refreshToken(user, "chatgpt", "chatgpt-r3")); err != nil {
		t.Fatalf("second RotateOAuthTokens: %v", err)
	}

	for _, tag := range []string{"chatgpt-r1", "chatgpt-r2"} {
		if got := oauthToken(t, s, user, tag); got == nil || got.UsedAt == nil {
			t.Errorf("spent %s = %+v, want it kept and spent", tag, got)
		}
	}
}

func testRotateOAuthTokensRefusesASpentRefreshToken(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	createOAuthGrants(t, s, oauthGrant(user, "chatgpt", "linear"))
	issue(t, s, user, "chatgpt")

	at := oauthBase.Add(time.Minute)

	if err := s.RotateOAuthTokens(ctx, user, oauthDigest("chatgpt-r1"), at,
		accessToken(user, "chatgpt", "a2"), refreshToken(user, "chatgpt", "r2")); err != nil {
		t.Fatalf("RotateOAuthTokens: %v", err)
	}

	// Presenting the spent token again is a reuse: ErrSpent, and nothing
	// changes, so the first rotation's tokens stand and UsedAt stays put.
	err := s.RotateOAuthTokens(ctx, user, oauthDigest("chatgpt-r1"), at.Add(time.Hour),
		accessToken(user, "chatgpt", "a3"), refreshToken(user, "chatgpt", "r3"))

	if !errors.Is(err, store.ErrSpent) {
		t.Fatalf("RotateOAuthTokens of a spent token: err = %v, want ErrSpent", err)
	}

	if oauthToken(t, s, user, "a3") != nil || oauthToken(t, s, user, "r3") != nil {
		t.Error("a reuse stored a token")
	}

	if got := oauthToken(t, s, user, "a2"); got == nil {
		t.Error("a reuse deleted the access token of the first rotation")
	}

	if got := oauthToken(t, s, user, "chatgpt-r1"); got == nil || got.UsedAt == nil || !got.UsedAt.Equal(at) {
		t.Errorf("spent token after a reuse = %+v, want UsedAt %v", got, at)
	}
}

func testRotateOAuthTokensUnknownRefreshTokenIsNotFound(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	createOAuthGrants(t, s, oauthGrant(user, "chatgpt", "linear"))
	issue(t, s, user, "chatgpt")

	rotate := func(name string, u string, digest [sha256.Size]byte) {
		t.Helper()
		err := s.RotateOAuthTokens(ctx, u, digest, oauthBase,
			accessToken(u, "chatgpt", "a2"), refreshToken(u, "chatgpt", "r2"))

		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("RotateOAuthTokens(%s): err = %v, want ErrNotFound", name, err)
		}
	}

	rotate("an unknown digest", user, oauthDigest("nothing"))
	// An access token is not a refresh token.
	rotate("an access token", user, oauthDigest("chatgpt-a1"))
	// Another user's refresh token is no one's.
	rotate("another user's refresh token", unknownUser(t), oauthDigest("chatgpt-r1"))

	if got := oauthToken(t, s, user, "chatgpt-r1"); got == nil || got.UsedAt != nil {
		t.Errorf("refresh token after the refused rotations = %+v, want it unspent", got)
	}

	if oauthToken(t, s, user, "chatgpt-a1") == nil {
		t.Error("a refused rotation deleted the access token")
	}
}

func testFailedRotateOAuthTokensLeavesTheRefreshTokenUnspent(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	createOAuthGrants(t, s, oauthGrant(user, "chatgpt", "linear"), oauthGrant(user, "claude", "github"))
	issue(t, s, user, "chatgpt")
	issue(t, s, user, "claude")

	for name, rotation := range map[string]struct{ access, refresh store.OAuthToken }{
		// The successor's digest is already stored, so the insert fails
		// after the UPDATE and the DELETE ran.
		"a successor digest already stored": {
			accessToken(user, "chatgpt", "claude-a1"), refreshToken(user, "chatgpt", "r2"),
		},
		"a refresh successor digest already stored": {
			accessToken(user, "chatgpt", "a2"), refreshToken(user, "chatgpt", "claude-r1"),
		},
		// The successors belong to another grant.
		"successors of another grant": {
			accessToken(user, "claude", "a2"), refreshToken(user, "claude", "r2"),
		},
		"successors that are malformed": {
			func() store.OAuthToken { a := accessToken(user, "chatgpt", "a2"); a.ExpiresAt = nil; return a }(),
			refreshToken(user, "chatgpt", "r2"),
		},
	} {
		err := s.RotateOAuthTokens(ctx, user, oauthDigest("chatgpt-r1"), oauthBase.Add(time.Minute),
			rotation.access, rotation.refresh)

		if err == nil || errors.Is(err, store.ErrSpent) || errors.Is(err, store.ErrNotFound) {
			t.Errorf("RotateOAuthTokens(%s): err = %v, want a failure", name, err)
		}

		if got := oauthToken(t, s, user, "chatgpt-r1"); got == nil || got.UsedAt != nil {
			t.Errorf("%s: refresh token = %+v, want it unspent", name, got)
		}

		if oauthToken(t, s, user, "chatgpt-a1") == nil {
			t.Errorf("%s: the access token was deleted by a failed rotation", name)
		}

		if oauthToken(t, s, user, "a2") != nil || oauthToken(t, s, user, "r2") != nil {
			t.Errorf("%s: a failed rotation stored a token", name)
		}
	}

	// It still rotates after the failures.
	if err := s.RotateOAuthTokens(ctx, user, oauthDigest("chatgpt-r1"), oauthBase.Add(time.Minute),
		accessToken(user, "chatgpt", "a2"), refreshToken(user, "chatgpt", "r2")); err != nil {
		t.Fatalf("RotateOAuthTokens after the failures: %v", err)
	}
}

func testRacingRotationsOfOneRefreshTokenHaveOneWinner(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	createOAuthGrants(t, s, oauthGrant(user, "chatgpt", "linear"))
	issue(t, s, user, "chatgpt")

	const racers = 8

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make([]error, racers)
	)

	for i := range racers {
		wg.Add(1)

		go func() {
			defer wg.Done()
			<-start

			errs[i] = s.RotateOAuthTokens(ctx, user, oauthDigest("chatgpt-r1"), oauthBase.Add(time.Minute),
				accessToken(user, "chatgpt", fmt.Sprintf("a-%d", i)), refreshToken(user, "chatgpt", fmt.Sprintf("r-%d", i)))
		}()
	}

	close(start)
	wg.Wait()

	winner := -1

	for i, err := range errs {
		switch {
		case err == nil:
			if winner >= 0 {
				t.Fatalf("rotations %d and %d both won", winner, i)
			}

			winner = i
		case !errors.Is(err, store.ErrSpent):
			t.Errorf("rotation %d: err = %v, want nil or ErrSpent", i, err)
		}
	}

	if winner < 0 {
		t.Fatalf("no rotation won: %v", errs)
	}

	// Only the winner's successors are stored.
	for i := range racers {
		stored := oauthToken(t, s, user, fmt.Sprintf("a-%d", i)) != nil
		storedRefresh := oauthToken(t, s, user, fmt.Sprintf("r-%d", i)) != nil

		if want := i == winner; stored != want || storedRefresh != want {
			t.Errorf("racer %d: access stored = %v, refresh stored = %v, want both %v", i, stored, storedRefresh, want)
		}
	}
}

func testRotateOAuthTokensRefusesAZeroTime(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	createOAuthGrants(t, s, oauthGrant(user, "chatgpt", "linear"))
	issue(t, s, user, "chatgpt")

	err := s.RotateOAuthTokens(ctx, user, oauthDigest("chatgpt-r1"), time.Time{},
		accessToken(user, "chatgpt", "a2"), refreshToken(user, "chatgpt", "r2"))

	if err == nil || errors.Is(err, store.ErrSpent) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RotateOAuthTokens at the zero time: err = %v, want a refusal", err)
	}

	if got := oauthToken(t, s, user, "chatgpt-r1"); got == nil || got.UsedAt != nil {
		t.Errorf("refresh token after the refusal = %+v, want it unspent", got)
	}

	if oauthToken(t, s, user, "a2") != nil || oauthToken(t, s, user, "r2") != nil {
		t.Error("a refused rotation stored a token")
	}
}

// testRacingRotationAndGrantDeleteLeaveNoTokens races a rotation against the
// delete of its grant. Whichever wins, neither fails with anything but the
// grant or token being gone, and the grant's tokens end up gone: on Postgres
// the two take their row locks in opposite orders unless the store orders
// them, and a deadlock shows as an unexpected error.
func testRacingRotationAndGrantDeleteLeaveNoTokens(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)

	const rounds = 100

	for i := range rounds {
		grant := fmt.Sprintf("race-%d", i)
		r1, a2, r2 := grant+"-r1", grant+"-a2", grant+"-r2"

		createOAuthGrants(t, s, oauthGrant(user, grant, "linear"))
		issue(t, s, user, grant)

		var (
			wg                   sync.WaitGroup
			start                = make(chan struct{})
			rotateErr, deleteErr error
		)

		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start

			rotateErr = s.RotateOAuthTokens(ctx, user, oauthDigest(r1), oauthBase.Add(time.Minute),
				accessToken(user, grant, a2), refreshToken(user, grant, r2))
		}()

		go func() {
			defer wg.Done()
			<-start

			// Sweep the delete across the rotation's few statements, so
			// some round lands it between the token UPDATE and the insert.
			time.Sleep(time.Duration(i%10) * 150 * time.Microsecond)

			deleteErr = s.DeleteOAuthGrant(ctx, user, grant)
		}()

		close(start)
		wg.Wait()

		if rotateErr != nil && !errors.Is(rotateErr, store.ErrNotFound) && !errors.Is(rotateErr, store.ErrSpent) {
			t.Fatalf("round %d: RotateOAuthTokens: err = %v, want nil, ErrNotFound, or ErrSpent", i, rotateErr)
		}

		if deleteErr != nil && !errors.Is(deleteErr, store.ErrNotFound) {
			t.Fatalf("round %d: DeleteOAuthGrant: err = %v, want nil or ErrNotFound", i, deleteErr)
		}

		if _, err := s.GetOAuthGrant(ctx, user, grant); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("round %d: GetOAuthGrant after the delete: err = %v, want ErrNotFound", i, err)
		}

		for _, tag := range []string{grant + "-a1", r1, a2, r2} {
			if oauthToken(t, s, user, tag) != nil {
				t.Fatalf("round %d: token %s outlived its grant", i, tag)
			}
		}
	}
}

func testDeleteOAuthGrantDeletesItsTokens(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	createOAuthGrants(t, s, oauthGrant(user, "chatgpt", "linear"), oauthGrant(user, "claude", "github"))
	issue(t, s, user, "chatgpt")
	issue(t, s, user, "claude")

	// A spent refresh token goes with its grant too.
	if err := s.RotateOAuthTokens(ctx, user, oauthDigest("chatgpt-r1"), oauthBase,
		accessToken(user, "chatgpt", "chatgpt-a2"), refreshToken(user, "chatgpt", "chatgpt-r2")); err != nil {
		t.Fatalf("RotateOAuthTokens: %v", err)
	}

	if err := s.DeleteOAuthGrant(ctx, user, "chatgpt"); err != nil {
		t.Fatalf("DeleteOAuthGrant: %v", err)
	}

	for _, tag := range []string{"chatgpt-a1", "chatgpt-a2", "chatgpt-r1", "chatgpt-r2"} {
		if oauthToken(t, s, user, tag) != nil {
			t.Errorf("token %s outlived its grant", tag)
		}
	}

	for _, tag := range []string{"claude-a1", "claude-r1"} {
		if oauthToken(t, s, user, tag) == nil {
			t.Errorf("token %s of another grant was deleted", tag)
		}
	}

	// A grant created again under the name starts with no token.
	createOAuthGrants(t, s, oauthGrant(user, "chatgpt", "linear"))
	issue(t, s, user, "chatgpt")
}

func testListOAuthAccessTokensKeepsOnlyUnexpiredAccessTokens(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	createOAuthGrants(t, s, oauthGrant(user, "b", "linear"), oauthGrant(user, "a", "linear"), oauthGrant(user, "c", "linear"))

	for _, g := range []string{"b", "a", "c"} {
		issue(t, s, user, g)
	}

	// Access tokens expire at oauthBase+1h: the list is of those that expire
	// after now, so one expiring at now is out.
	live := oauthBase.Add(time.Hour - time.Millisecond)

	got, err := s.ListOAuthAccessTokens(ctx, user, live)

	if err != nil {
		t.Fatalf("ListOAuthAccessTokens: %v", err)
	}

	want := []store.OAuthToken{
		accessToken(user, "a", "a-a1"), accessToken(user, "b", "b-a1"), accessToken(user, "c", "c-a1"),
	}

	if diff := cmp.Diff(want, got, approxTime); diff != "" {
		t.Errorf("ListOAuthAccessTokens (-want +got):\n%s", diff)
	}

	for _, now := range []time.Time{oauthBase.Add(time.Hour), oauthBase.Add(2 * time.Hour)} {
		got, err := s.ListOAuthAccessTokens(ctx, user, now)

		if err != nil || len(got) != 0 {
			t.Errorf("ListOAuthAccessTokens at %v = %v, %v, want none", now, got, err)
		}
	}

	got, err = s.ListOAuthAccessTokens(ctx, unknownUser(t), live)

	if err != nil || len(got) != 0 {
		t.Errorf("another user's ListOAuthAccessTokens = %v, %v, want none", got, err)
	}
}

func testTouchOAuthGrantsNeverMovesBackwardsAndIgnoresOtherDigests(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	createOAuthGrants(t, s, oauthGrant(user, "chatgpt", "linear"), oauthGrant(user, "claude", "github"),
		oauthGrant(user, "idle", "linear"))
	issue(t, s, user, "chatgpt")
	issue(t, s, user, "claude")
	issue(t, s, user, "idle")
	createClientTokens(t, s, clientToken(user, "ci", "github"))
	base := time.UnixMilli(1_762_000_100_000)

	lastUsed := func() map[string]*time.Time {
		t.Helper()
		out := map[string]*time.Time{}

		for _, g := range listOAuthGrants(t, s, user) {
			out[g.Name] = g.LastUsedAt
		}

		return out
	}
	at := func(d time.Duration) *time.Time {
		v := base.Add(d)

		return &v
	}

	steps := []struct {
		name string
		used map[[sha256.Size]byte]time.Time
		want map[string]*time.Time
	}{
		{"nothing to touch", nil,
			map[string]*time.Time{"chatgpt": nil, "claude": nil, "idle": nil}},
		{"first use sets it, to the millisecond; a refresh token, a client token, and an unknown digest are ignored",
			map[[sha256.Size]byte]time.Time{
				oauthDigest("chatgpt-a1"): base.Add(456 * time.Microsecond),
				oauthDigest("claude-a1"):  base.Add(time.Second),
				oauthDigest("idle-r1"):    base,
				digestOf("ci"):            base,
				oauthDigest("revoked"):    base,
			},
			map[string]*time.Time{"chatgpt": at(0), "claude": at(time.Second), "idle": nil}},
		{"a later use moves it forward, an earlier one does not move it back",
			map[[sha256.Size]byte]time.Time{oauthDigest("chatgpt-a1"): base.Add(time.Minute), oauthDigest("claude-a1"): base},
			map[string]*time.Time{"chatgpt": at(time.Minute), "claude": at(time.Second), "idle": nil}},
	}

	for _, st := range steps {
		if err := s.TouchOAuthGrants(ctx, user, st.used); err != nil {
			t.Fatalf("%s: TouchOAuthGrants: %v", st.name, err)
		}

		if diff := cmp.Diff(st.want, lastUsed(), approxTime); diff != "" {
			t.Fatalf("%s: LastUsedAt (-want +got):\n%s", st.name, diff)
		}
	}

	// A rotation replaces the access token; the old digest, revoked since it
	// was used, no longer touches the grant, and the new one does.
	if err := s.RotateOAuthTokens(ctx, user, oauthDigest("idle-r1"), base,
		accessToken(user, "idle", "idle-a2"), refreshToken(user, "idle", "idle-r2")); err != nil {
		t.Fatalf("RotateOAuthTokens: %v", err)
	}

	if err := s.TouchOAuthGrants(ctx, user, map[[sha256.Size]byte]time.Time{oauthDigest("idle-a1"): base}); err != nil {
		t.Fatalf("TouchOAuthGrants of a replaced access token: %v", err)
	}

	if got := lastUsed()["idle"]; got != nil {
		t.Errorf("idle LastUsedAt after its old access token was touched = %v, want nil", got)
	}

	if err := s.TouchOAuthGrants(ctx, user, map[[sha256.Size]byte]time.Time{oauthDigest("idle-a2"): base}); err != nil {
		t.Fatalf("TouchOAuthGrants of the new access token: %v", err)
	}

	if got := lastUsed()["idle"]; got == nil || !got.Equal(base) {
		t.Errorf("idle LastUsedAt after its new access token was touched = %v, want %v", got, base)
	}

	// Another user's touch reaches none of this user's grants.
	if err := s.TouchOAuthGrants(ctx, unknownUser(t), map[[sha256.Size]byte]time.Time{oauthDigest("claude-a1"): base.Add(time.Hour)}); err != nil {
		t.Fatalf("another user's TouchOAuthGrants: %v", err)
	}

	if got := lastUsed()["claude"]; got == nil || !got.Equal(base.Add(time.Second)) {
		t.Errorf("claude LastUsedAt after another user's touch = %v, want %v", got, base.Add(time.Second))
	}
}

func testPruneOAuthDeletesExpiredSpentAndUnexchanged(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)

	prune := func(now time.Time) int64 {
		t.Helper()
		n, err := s.PruneOAuth(ctx, user, now)

		if err != nil {
			t.Fatalf("PruneOAuth(%v): %v", now, err)
		}

		return n
	}

	// An empty store has nothing to prune.
	if n := prune(oauthBase); n != 0 {
		t.Fatalf("PruneOAuth on an empty store = %d, want 0", n)
	}

	// Unexchanged grants: kept until OAuthUnexchangedTTL after creation,
	// pruned at that instant; a grant with tokens is never one.
	createOAuthGrants(t, s, oauthGrant(user, "never", "linear"), oauthGrant(user, "used", "linear"))
	issue(t, s, user, "used")

	if n := prune(oauthBase.Add(store.OAuthUnexchangedTTL - time.Millisecond)); n != 0 {
		t.Fatalf("PruneOAuth just before the unexchanged TTL = %d, want 0", n)
	}

	if n := prune(oauthBase.Add(store.OAuthUnexchangedTTL)); n != 1 {
		t.Fatalf("PruneOAuth at the unexchanged TTL = %d, want 1 (the grant never exchanged)", n)
	}

	if got := listOAuthGrants(t, s, user); len(got) != 1 || got[0].Name != "used" {
		t.Fatalf("grants after the unexchanged prune = %v, want only used", got)
	}

	// Rotate at oauthBase+10m: used-a1 is deleted, a2 expires at +40m, and
	// used-r1 is spent at +10m.
	spentAt := oauthBase.Add(10 * time.Minute)
	a2 := accessToken(user, "used", "used-a2")
	exp := oauthBase.Add(40 * time.Minute)
	a2.ExpiresAt = &exp

	if err := s.RotateOAuthTokens(ctx, user, oauthDigest("used-r1"), spentAt, a2, refreshToken(user, "used", "used-r2")); err != nil {
		t.Fatalf("RotateOAuthTokens: %v", err)
	}

	// Access tokens: kept until they expire, pruned at that instant.
	if n := prune(exp.Add(-time.Millisecond)); n != 0 {
		t.Fatalf("PruneOAuth just before the access token expires = %d, want 0", n)
	}

	if n := prune(exp); n != 1 {
		t.Fatalf("PruneOAuth when the access token expires = %d, want 1", n)
	}

	if oauthToken(t, s, user, "used-a2") != nil {
		t.Error("expired access token kept")
	}

	// The unexpired refresh tokens are never pruned, and the spent one is
	// kept until OAuthSpentRetention after it was spent.
	if n := prune(spentAt.Add(store.OAuthSpentRetention - time.Millisecond)); n != 0 {
		t.Fatalf("PruneOAuth just before the retention ends = %d, want 0", n)
	}

	if got := oauthToken(t, s, user, "used-r1"); got == nil || got.UsedAt == nil {
		t.Fatalf("spent refresh token before the retention ends = %+v, want it kept", got)
	}

	if n := prune(spentAt.Add(store.OAuthSpentRetention)); n != 1 {
		t.Fatalf("PruneOAuth when the retention ends = %d, want 1", n)
	}

	if oauthToken(t, s, user, "used-r1") != nil {
		t.Error("spent refresh token kept past its retention")
	}

	// The live refresh token, however old, and its grant stay.
	if oauthToken(t, s, user, "used-r2") == nil {
		t.Error("live refresh token pruned")
	}

	if n := prune(oauthBase.Add(10 * 365 * 24 * time.Hour)); n != 0 {
		t.Errorf("PruneOAuth ten years on = %d, want 0 (a live refresh token never expires)", n)
	}

	if got := listOAuthGrants(t, s, user); len(got) != 1 || got[0].Name != "used" {
		t.Errorf("grants after pruning = %v, want only used", got)
	}
}

func testPruneOAuthCountsEveryRowAndSparesOtherGrants(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	createOAuthGrants(t, s, oauthGrant(user, "a", "linear"), oauthGrant(user, "b", "linear"), oauthGrant(user, "never", "linear"))
	issue(t, s, user, "a")
	issue(t, s, user, "b")

	// a rotates twice, leaving two spent refresh tokens and a live pair; b
	// stays as issued.
	for i, prev := range []string{"a-r1", "a-r2"} {
		next := fmt.Sprintf("a-r%d", i+2)

		if err := s.RotateOAuthTokens(ctx, user, oauthDigest(prev), oauthBase.Add(time.Duration(i)*time.Minute),
			accessToken(user, "a", fmt.Sprintf("a-a%d", i+2)), refreshToken(user, "a", next)); err != nil {
			t.Fatalf("RotateOAuthTokens %d: %v", i, err)
		}
	}

	// Long after everything, the never-exchanged grant, b's and a's expired
	// access tokens, and a's two spent refresh tokens are pruned; the live
	// refresh tokens stay.
	now := oauthBase.Add(store.OAuthSpentRetention + time.Hour)

	n, err := s.PruneOAuth(ctx, user, now)

	if err != nil {
		t.Fatalf("PruneOAuth: %v", err)
	}

	if n != 5 {
		t.Errorf("PruneOAuth = %d, want 5 (1 grant, 2 access tokens, 2 spent refresh tokens)", n)
	}

	for tag, want := range map[string]bool{
		"a-a3": false, "b-a1": false, "a-r1": false, "a-r2": false, "a-r3": true, "b-r1": true,
	} {
		if got := oauthToken(t, s, user, tag) != nil; got != want {
			t.Errorf("token %s stored = %v, want %v", tag, got, want)
		}
	}

	var names []string

	for _, g := range listOAuthGrants(t, s, user) {
		names = append(names, g.Name)
	}

	if diff := cmp.Diff([]string{"a", "b"}, names); diff != "" {
		t.Errorf("grants after pruning (-want +got):\n%s", diff)
	}

	// Another user's prune reaches none of it.
	if n, err := s.PruneOAuth(ctx, unknownUser(t), now); err != nil || n != 0 {
		t.Errorf("another user's PruneOAuth = %d, %v, want 0, nil", n, err)
	}
}
