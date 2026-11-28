package storetest

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/atomicpages/wayfindr/internal/store"
)

// digestOf is the digest of the fixture token called name.
func digestOf(name string) [sha256.Size]byte { return sha256.Sum256([]byte("wf_s_" + name)) }

// clientToken is a fixture token of user, created at a fixed time; its
// digest is digestOf(name).
func clientToken(user, name string, connectors ...string) store.ClientToken {
	return store.ClientToken{
		UserID:     user,
		Name:       name,
		Digest:     digestOf(name),
		Connectors: connectors,
		CreatedAt:  time.UnixMilli(1_762_000_000_000),
	}
}

func createClientTokens(t *testing.T, s store.Store, tokens ...store.ClientToken) {
	t.Helper()

	for _, tok := range tokens {
		if err := s.CreateClientToken(context.Background(), tok); err != nil {
			t.Fatalf("CreateClientToken(%s): %v", tok.Name, err)
		}
	}
}

func listClientTokens(t *testing.T, s store.Store, user string) []store.ClientToken {
	t.Helper()
	got, err := s.ListClientTokens(context.Background(), user)

	if err != nil {
		t.Fatalf("ListClientTokens: %v", err)
	}

	return got
}

func testClientTokensCreateListAndDelete(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)

	if got := listClientTokens(t, s, user); len(got) != 0 {
		t.Fatalf("ListClientTokens before any create = %v, want none", got)
	}

	triage := clientToken(user, "nightly-triage", "linear", "github")
	// Remote is stored as given: set on one token, left false on the other.
	triage.Remote = true
	// Approval entries are kept as given and in order, opaque to the store.
	triage.Approval = []string{"linear.delete_*", "github.merge_pull_request"}
	// A sub-millisecond creation time is kept to the millisecond.
	triage.CreatedAt = triage.CreatedAt.Add(456 * time.Microsecond)
	ci := clientToken(user, "ci", "github")
	// LastUsedAt is the store's to set; Create ignores it.
	ci.LastUsedAt = &ci.CreatedAt
	createClientTokens(t, s, triage, ci)

	wantTriage, wantCI := triage, ci
	wantTriage.CreatedAt = time.UnixMilli(1_762_000_000_000)
	wantCI.LastUsedAt = nil

	// Ordered by name; connectors as given; only triage is remote and has
	// approval entries (ci's read back nil).
	if diff := cmp.Diff([]store.ClientToken{wantCI, wantTriage}, listClientTokens(t, s, user), cmpopts.EquateApproxTime(0)); diff != "" {
		t.Fatalf("ListClientTokens (-want +got):\n%s", diff)
	}

	if got := listClientTokens(t, s, unknownUser(t)); len(got) != 0 {
		t.Errorf("another user's ListClientTokens = %v, want none", got)
	}

	if err := s.DeleteClientToken(ctx, user, "ci"); err != nil {
		t.Fatalf("DeleteClientToken: %v", err)
	}

	if diff := cmp.Diff([]store.ClientToken{wantTriage}, listClientTokens(t, s, user), cmpopts.EquateApproxTime(0)); diff != "" {
		t.Errorf("ListClientTokens after delete (-want +got):\n%s", diff)
	}

	// The name is free again once its token is deleted.
	createClientTokens(t, s, ci)
}

func testClientTokenEmptyApprovalReadsBackNil(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	tok := clientToken(user, "ci", "github")
	tok.Approval = []string{}
	createClientTokens(t, s, tok)

	got := listClientTokens(t, s, user)

	if len(got) != 1 || got[0].Approval != nil {
		t.Errorf("ListClientTokens = %#v, want one token with nil Approval", got)
	}
}

func testCreateClientTokenRefusesADuplicateNameOrDigest(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	original := clientToken(user, "ci", "github")
	createClientTokens(t, s, original)

	sameName := clientToken(user, "ci", "linear")
	sameName.Digest = sha256.Sum256([]byte("another value"))
	sameDigest := clientToken(user, "deploy", "linear")
	sameDigest.Digest = original.Digest

	for _, tok := range []store.ClientToken{sameName, sameDigest} {
		if err := s.CreateClientToken(context.Background(), tok); !errors.Is(err, store.ErrExists) {
			t.Errorf("CreateClientToken(%s): err = %v, want ErrExists", tok.Name, err)
		}
	}

	if diff := cmp.Diff([]store.ClientToken{original}, listClientTokens(t, s, user), cmpopts.EquateApproxTime(0)); diff != "" {
		t.Errorf("ListClientTokens after refused creates (-want +got):\n%s", diff)
	}
}

func testCreateClientTokenRefusesNoConnectors(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)

	for _, connectors := range [][]string{nil, {}} {
		tok := clientToken(user, "ci")
		tok.Connectors = connectors

		if err := s.CreateClientToken(context.Background(), tok); err == nil {
			t.Errorf("CreateClientToken(connectors %#v): err = nil, want an error", connectors)
		}
	}

	if got := listClientTokens(t, s, user); len(got) != 0 {
		t.Errorf("ListClientTokens = %v, want none", got)
	}
}

func testDeleteClientTokenMissingIsNotFound(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	createClientTokens(t, s, clientToken(user, "ci", "github"))

	if err := s.DeleteClientToken(context.Background(), user, "deploy"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteClientToken(deploy): err = %v, want ErrNotFound", err)
	}

	if err := s.DeleteClientToken(context.Background(), unknownUser(t), "ci"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another user's DeleteClientToken(ci): err = %v, want ErrNotFound", err)
	}
}

func testTouchClientTokensNeverMovesBackwardsAndIgnoresUnknownDigests(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	createClientTokens(t, s, clientToken(user, "ci", "github"), clientToken(user, "deploy", "github"),
		clientToken(user, "idle", "linear"))
	base := time.UnixMilli(1_762_000_100_000)

	lastUsed := func() map[string]*time.Time {
		t.Helper()
		out := map[string]*time.Time{}

		for _, tok := range listClientTokens(t, s, user) {
			out[tok.Name] = tok.LastUsedAt
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
			map[string]*time.Time{"ci": nil, "deploy": nil, "idle": nil}},
		{"first use sets it, to the millisecond; an unknown digest is ignored",
			map[[sha256.Size]byte]time.Time{
				digestOf("ci"): base.Add(456 * time.Microsecond), digestOf("deploy"): base.Add(time.Second), digestOf("revoked"): base,
			},
			map[string]*time.Time{"ci": at(0), "deploy": at(time.Second), "idle": nil}},
		{"a later use moves it forward, an earlier one does not move it back",
			map[[sha256.Size]byte]time.Time{digestOf("ci"): base.Add(time.Minute), digestOf("deploy"): base},
			map[string]*time.Time{"ci": at(time.Minute), "deploy": at(time.Second), "idle": nil}},
	}

	for _, st := range steps {
		if err := s.TouchClientTokens(ctx, user, st.used); err != nil {
			t.Fatalf("%s: TouchClientTokens: %v", st.name, err)
		}

		if diff := cmp.Diff(st.want, lastUsed(), cmpopts.EquateApproxTime(0)); diff != "" {
			t.Fatalf("%s: LastUsedAt (-want +got):\n%s", st.name, diff)
		}
	}

	// Another user's touch reaches none of this user's tokens.
	if err := s.TouchClientTokens(ctx, unknownUser(t), map[[sha256.Size]byte]time.Time{digestOf("idle"): base}); err != nil {
		t.Fatalf("another user's TouchClientTokens: %v", err)
	}

	if got := lastUsed()["idle"]; got != nil {
		t.Errorf("idle LastUsedAt after another user's touch = %v, want nil", got)
	}
}

func testTouchClientTokensNeverCreditsANewTokenOfTheSameName(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	old := clientToken(user, "ci", "github")
	createClientTokens(t, s, old)

	// ci is revoked and created again, with a new value, before the old
	// value's last use is flushed.
	if err := s.DeleteClientToken(ctx, user, "ci"); err != nil {
		t.Fatalf("DeleteClientToken: %v", err)
	}

	recreated := clientToken(user, "ci", "github")
	recreated.Digest = sha256.Sum256([]byte("wf_s_ci-again"))
	createClientTokens(t, s, recreated)

	if err := s.TouchClientTokens(ctx, user, map[[sha256.Size]byte]time.Time{old.Digest: time.UnixMilli(1_762_000_100_000)}); err != nil {
		t.Fatalf("TouchClientTokens: %v", err)
	}

	if diff := cmp.Diff([]store.ClientToken{recreated}, listClientTokens(t, s, user), cmpopts.EquateApproxTime(0)); diff != "" {
		t.Errorf("ListClientTokens after touching the old value (-want +got):\n%s", diff)
	}
}
