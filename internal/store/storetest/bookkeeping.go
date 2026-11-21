package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/store"
)

// testApplySyncKeepsTheLatestOutcomeAndHiddenToolsAcrossAFailure pins the
// sync record ApplySync keeps: each sync replaces the last one, a
// successful sync records its hidden tools, and a failed one leaves the
// catalog as it was, even when its outcome carries tools, and so keeps
// them; a later success with none clears them.
func testApplySyncKeepsTheLatestOutcomeAndHiddenToolsAcrossAFailure(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	hidden := []store.HiddenTool{{Name: "send_message", Class: "write"}, {Name: "delete_thread", Class: "destructive"}}
	read, other := tool(t, "gmail", "get_thread", "Gets a thread.", 1), tool(t, "gmail", "list_labels", "Lists labels.", 2)

	if _, err := s.LastSync(ctx, user, "gmail"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LastSync before any sync: err = %v, want ErrNotFound", err)
	}

	steps := []struct {
		name        string
		outcome     store.SyncOutcome
		wantChanged []string
		wantCatalog []string
		want        store.SyncRecord
	}{
		{"failure before any success",
			store.SyncOutcome{At: time.UnixMilli(1_762_000_000_000), Error: "connection refused", Tools: []store.Tool{read}, Hidden: hidden},
			nil, nil,
			store.SyncRecord{At: time.UnixMilli(1_762_000_000_000), Error: "connection refused"}},
		{"success records them",
			store.SyncOutcome{At: time.UnixMilli(1_762_000_060_000), Tools: []store.Tool{read}, Hidden: hidden},
			[]string{"get_thread"}, []string{"get_thread"},
			store.SyncRecord{At: time.UnixMilli(1_762_000_060_000), Hidden: hidden}},
		{"failure keeps them and the catalog",
			store.SyncOutcome{At: time.UnixMilli(1_762_000_120_000), Error: "refused", Tools: []store.Tool{other}},
			nil, []string{"get_thread"},
			store.SyncRecord{At: time.UnixMilli(1_762_000_120_000), Error: "refused", Hidden: hidden}},
		{"success with none clears them",
			store.SyncOutcome{At: time.UnixMilli(1_762_000_180_000), Tools: []store.Tool{read}},
			nil, []string{"get_thread"},
			store.SyncRecord{At: time.UnixMilli(1_762_000_180_000)}},
	}

	for _, st := range steps {
		st.outcome.Connector = "gmail"
		changed, err := s.ApplySync(ctx, user, st.outcome)

		if err != nil {
			t.Fatalf("%s: ApplySync: %v", st.name, err)
		}

		if diff := cmp.Diff(st.wantChanged, changed); diff != "" {
			t.Errorf("%s: changed (-want +got):\n%s", st.name, diff)
		}

		if diff := cmp.Diff(st.wantCatalog, catalogNames(t, s, user, "gmail")); diff != "" {
			t.Errorf("%s: catalog (-want +got):\n%s", st.name, diff)
		}

		got, err := s.LastSync(ctx, user, "gmail")

		if err != nil {
			t.Fatalf("%s: LastSync: %v", st.name, err)
		}

		if diff := cmp.Diff(st.want, got); diff != "" {
			t.Errorf("%s: LastSync (-want +got):\n%s", st.name, diff)
		}
	}
}

func testCredentialRoundTripAndReplace(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	full := store.Credential{
		UserID:          user,
		Connector:       "linear",
		Resource:        "https://mcp.linear.app/mcp",
		Issuer:          "https://linear.app",
		RedirectURI:     "http://127.0.0.1:8765/oauth/callback",
		DCRClientID:     "client-123",
		ClientID:        "client-123",
		DCRClient:       []byte{1, 2, 3},
		Token:           []byte{4, 5, 6},
		GrantedScopes:   "read write",
		RequestedScopes: "read write admin",
	}
	minimal := store.Credential{UserID: user, Connector: "linear", Resource: "https://mcp.linear.app/v2"}

	if _, err := s.GetCredential(ctx, user, "linear"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetCredential before put: err = %v, want ErrNotFound", err)
	}

	for _, want := range []store.Credential{full, minimal} {
		if err := s.PutCredential(ctx, want); err != nil {
			t.Fatalf("PutCredential: %v", err)
		}

		got, err := s.GetCredential(ctx, user, "linear")

		if err != nil {
			t.Fatalf("GetCredential: %v", err)
		}

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("GetCredential (-want +got):\n%s", diff)
		}
	}
}

func testListAndDeleteCredentials(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)

	for _, c := range []string{"linear", "github", "notion"} {
		if err := s.PutCredential(ctx, store.Credential{UserID: user, Connector: c, Resource: "https://" + c}); err != nil {
			t.Fatalf("PutCredential(%s): %v", c, err)
		}
	}

	if err := s.DeleteCredential(ctx, user, "linear"); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}

	if err := s.DeleteCredential(ctx, user, "never-existed"); err != nil {
		t.Errorf("DeleteCredential(missing): %v, want nil", err)
	}

	got, err := s.ListCredentialConnectors(ctx, user)

	if err != nil {
		t.Fatalf("ListCredentialConnectors: %v", err)
	}

	if diff := cmp.Diff([]string{"github", "notion"}, got); diff != "" {
		t.Errorf("ListCredentialConnectors (-want +got):\n%s", diff)
	}

	if _, err := s.GetCredential(ctx, user, "linear"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetCredential after delete: err = %v, want ErrNotFound", err)
	}

	if got, err := s.ListCredentialConnectors(ctx, unknownUser(t)); err != nil || len(got) != 0 {
		t.Errorf("ListCredentialConnectors(other user) = %v, %v; want empty, nil", got, err)
	}
}

func testDeleteSecret(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	other := unknownUser(t)

	if err := s.DeleteSecret(ctx, user, "linear_key"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteSecret before put: err = %v, want ErrNotFound", err)
	}

	for _, name := range []string{"linear_key", "github_key"} {
		if err := s.PutSecret(ctx, user, name, []byte("sealed-"+name)); err != nil {
			t.Fatalf("PutSecret(%s): %v", name, err)
		}
	}

	// Another user's secret of the same name is not this user's to delete.
	if err := s.DeleteSecret(ctx, other, "linear_key"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteSecret(other user): err = %v, want ErrNotFound", err)
	}

	if _, err := s.GetSecret(ctx, user, "linear_key"); err != nil {
		t.Errorf("GetSecret after another user's delete: %v", err)
	}

	if err := s.DeleteSecret(ctx, user, "linear_key"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}

	if _, err := s.GetSecret(ctx, user, "linear_key"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetSecret after delete: err = %v, want ErrNotFound", err)
	}

	if err := s.DeleteSecret(ctx, user, "linear_key"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second DeleteSecret: err = %v, want ErrNotFound", err)
	}

	if got, err := s.GetSecret(ctx, user, "github_key"); err != nil || string(got) != "sealed-github_key" {
		t.Errorf("GetSecret(other name) = %q, %v; want it untouched", got, err)
	}
}

func testListSecretsSortsTheUsersNames(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)

	if got, err := s.ListSecrets(ctx, user); err != nil || len(got) != 0 {
		t.Fatalf("ListSecrets before put = %v, %v; want empty, nil", got, err)
	}

	// Bytewise order: upper case before lower case, "." before "_".
	for _, name := range []string{"linear_key", "Zed", "github.key", "github_key"} {
		if err := s.PutSecret(ctx, user, name, []byte("sealed-"+name)); err != nil {
			t.Fatalf("PutSecret(%s): %v", name, err)
		}
	}

	if err := s.PutSecret(ctx, unknownUser(t), "other_key", []byte("sealed")); err != nil {
		t.Fatalf("PutSecret(other user): %v", err)
	}

	got, err := s.ListSecrets(ctx, user)

	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}

	if diff := cmp.Diff([]string{"Zed", "github.key", "github_key", "linear_key"}, got); diff != "" {
		t.Errorf("ListSecrets (-want +got):\n%s", diff)
	}
}

func testSecretRoundTripAndReplace(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)

	if _, err := s.GetSecret(ctx, user, "linear_key"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSecret before put: err = %v, want ErrNotFound", err)
	}

	for _, want := range [][]byte{{0x01, 0xaa}, {0x01, 0xbb, 0xcc}} {
		if err := s.PutSecret(ctx, user, "linear_key", want); err != nil {
			t.Fatalf("PutSecret: %v", err)
		}

		got, err := s.GetSecret(ctx, user, "linear_key")

		if err != nil {
			t.Fatalf("GetSecret: %v", err)
		}

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("GetSecret (-want +got):\n%s", diff)
		}
	}
}

func testMetadataRoundTripAndReplace(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, _ := newStore(t, newDatabase)
	const url = "https://mcp.linear.app/.well-known/oauth-protected-resource"

	if _, err := s.GetMetadata(ctx, url); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetMetadata before put: err = %v, want ErrNotFound", err)
	}

	for _, want := range []store.Metadata{
		{URL: url, Kind: store.MetadataPRM, Document: []byte(`{"resource":"a"}`), FetchedAt: time.UnixMilli(1_762_000_000_000)},
		{URL: url, Kind: store.MetadataPRM, Document: []byte(`{"resource":"b"}`), FetchedAt: time.UnixMilli(1_762_000_100_000)},
	} {
		if err := s.PutMetadata(ctx, want); err != nil {
			t.Fatalf("PutMetadata: %v", err)
		}

		got, err := s.GetMetadata(ctx, url)

		if err != nil {
			t.Fatalf("GetMetadata: %v", err)
		}

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("GetMetadata (-want +got):\n%s", diff)
		}
	}
}
