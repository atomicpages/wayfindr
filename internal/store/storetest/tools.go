package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/store"
)

// tool builds a catalog entry whose definition carries a description, so two
// tools with the same name can differ in content.
func tool(t *testing.T, connector, name, description string, hash byte) store.Tool {
	t.Helper()
	def := fmt.Sprintf(`{"name":%q,"description":%q,"inputSchema":{"type":"object","properties":{"q":{"type":"string","description":"query"}}}}`, name, description)
	tl, err := store.NewTool(connector, json.RawMessage(def), []byte{hash})

	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}

	return tl
}

// appliedAt is when every sync that apply stores happened, in Unix
// milliseconds.
const appliedAt = 1_762_000_000_000

// apply stores a successful sync that found tools as the connector's
// catalog, and returns the names it reports changed.
func apply(t *testing.T, s store.Store, userID, connector string, tools ...store.Tool) []string {
	t.Helper()
	changed, err := s.ApplySync(context.Background(), userID,
		store.SyncOutcome{Connector: connector, At: time.UnixMilli(appliedAt), Tools: tools})

	if err != nil {
		t.Fatalf("ApplySync: %v", err)
	}

	return changed
}

// catalogNames returns the connector's catalog entry names, sorted.
func catalogNames(t *testing.T, s store.Store, userID, connector string) []string {
	t.Helper()
	// Nothing in these tests is embedded, so every entry is pending.
	pending, err := s.PendingEmbeddings(context.Background(), userID, "any-model", 1000)

	if err != nil {
		t.Fatalf("PendingEmbeddings: %v", err)
	}

	var names []string

	for _, tl := range pending {
		if tl.Connector == connector {
			names = append(names, tl.Name)
		}
	}

	return names
}

func testApplySyncIntoEmptyCatalogReportsEveryToolChanged(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)

	changed := apply(t, s, user, "linear",
		tool(t, "linear", "list_issues", "Lists issues.", 1),
		tool(t, "linear", "create_issue", "Creates an issue.", 2),
	)

	if diff := cmp.Diff([]string{"create_issue", "list_issues"}, changed); diff != "" {
		t.Errorf("changed (-want +got):\n%s", diff)
	}

	got, err := s.GetTool(ctx, user, "linear", "create_issue")

	if err != nil {
		t.Fatalf("GetTool: %v", err)
	}

	want := store.Tool{
		Connector:   "linear",
		Name:        "create_issue",
		Definition:  json.RawMessage(`{"name":"create_issue","description":"Creates an issue.","inputSchema":{"type":"object","properties":{"q":{"type":"string","description":"query"}}}}`),
		ContentHash: []byte{2},
		Description: "Creates an issue.",
		Params:      []store.Param{{Name: "q", Description: "query"}},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetTool (-want +got):\n%s", diff)
	}
}

func testApplySyncReturnsOnlyChangedHashesAndDeletesMissingRows(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "linear",
		tool(t, "linear", "same", "Unchanged.", 1),
		tool(t, "linear", "edited", "Old text.", 2),
		tool(t, "linear", "removed", "Goes away.", 3),
	)

	changed := apply(t, s, user, "linear",
		tool(t, "linear", "same", "Unchanged.", 1),
		tool(t, "linear", "edited", "New text.", 9),
		tool(t, "linear", "added", "Brand new.", 4),
	)

	if diff := cmp.Diff([]string{"added", "edited"}, changed); diff != "" {
		t.Errorf("changed (-want +got):\n%s", diff)
	}

	if _, err := s.GetTool(ctx, user, "linear", "removed"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTool(removed): err = %v, want ErrNotFound", err)
	}

	edited, err := s.GetTool(ctx, user, "linear", "edited")

	if err != nil {
		t.Fatalf("GetTool(edited): %v", err)
	}

	if edited.Description != "New text." || !cmp.Equal(edited.ContentHash, []byte{9}) {
		t.Errorf("edited = %q hash %v, want %q hash [9]", edited.Description, edited.ContentHash, "New text.")
	}
}

func testApplySyncStoresNewDefinitionWhenHashIsUnchanged(t *testing.T, newDatabase NewDatabase) {
	// Parts of a definition that are not embedded (outputSchema, annotations)
	// can change without changing the content hash; the new definition must
	// still be stored because call_tool validates against it.
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "linear", tool(t, "linear", "x", "Before.", 1))

	changed := apply(t, s, user, "linear", tool(t, "linear", "x", "After.", 1))

	if len(changed) != 0 {
		t.Errorf("changed = %v, want none", changed)
	}

	got, err := s.GetTool(ctx, user, "linear", "x")

	if err != nil {
		t.Fatalf("GetTool: %v", err)
	}

	if got.Description != "After." {
		t.Errorf("Description = %q, want %q", got.Description, "After.")
	}
}

func testApplySyncLeavesOtherConnectorsAlone(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "github", tool(t, "github", "create_pr", "Opens a PR.", 1))

	apply(t, s, user, "linear", tool(t, "linear", "create_issue", "Creates an issue.", 2))
	apply(t, s, user, "linear")

	if _, err := s.GetTool(ctx, user, "github", "create_pr"); err != nil {
		t.Errorf("GetTool(github.create_pr): %v", err)
	}

	if _, err := s.GetTool(ctx, user, "linear", "create_issue"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTool(linear.create_issue): err = %v, want ErrNotFound", err)
	}
}

func testApplySyncIsAllOrNothing(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "linear", tool(t, "linear", "keep", "Kept.", 1))

	tests := []struct {
		name  string
		tools []store.Tool
	}{
		{"duplicate name", []store.Tool{
			tool(t, "linear", "new", "A.", 2),
			tool(t, "linear", "new", "B.", 3),
		}},
		{"invalid definition", []store.Tool{
			tool(t, "linear", "new", "A.", 2),
			{Connector: "linear", Name: "broken", Definition: json.RawMessage(`{`), ContentHash: []byte{4}},
		}},
		{"name disagrees with definition", []store.Tool{
			func() store.Tool { tl := tool(t, "linear", "new", "A.", 2); tl.Name = "other"; return tl }(),
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := store.SyncOutcome{Connector: "linear", At: time.UnixMilli(appliedAt + 60_000), Tools: tt.tools,
				Hidden: []store.HiddenTool{{Name: "delete_issue", Class: "destructive"}}}

			if _, err := s.ApplySync(ctx, user, o); err == nil {
				t.Fatal("ApplySync: err = nil, want error")
			}

			if _, err := s.GetTool(ctx, user, "linear", "keep"); err != nil {
				t.Errorf("GetTool(keep) after failed apply: %v", err)
			}

			if _, err := s.GetTool(ctx, user, "linear", "new"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetTool(new) after failed apply: err = %v, want ErrNotFound", err)
			}

			// The sync record is written with the catalog or not at all.
			got, err := s.LastSync(ctx, user, "linear")

			if err != nil {
				t.Fatalf("LastSync: %v", err)
			}

			if diff := cmp.Diff(store.SyncRecord{At: time.UnixMilli(appliedAt)}, got); diff != "" {
				t.Errorf("LastSync after failed apply (-want +got):\n%s", diff)
			}
		})
	}
}

func testApplySyncRequiresAKnownUser(t *testing.T, newDatabase NewDatabase) {
	s, _ := newStore(t, newDatabase)

	_, err := s.ApplySync(context.Background(), unknownUser(t), store.SyncOutcome{
		Connector: "linear", At: time.UnixMilli(appliedAt), Tools: []store.Tool{tool(t, "linear", "x", "X.", 1)},
	})

	if err == nil {
		t.Fatal("ApplySync for unknown user: err = nil, want foreign-key error")
	}
}

// testNonUUIDUserIDFindsNothing pins the Store doc's promise for a user ID
// that is not a UUID: no row comes back, though the error may be the
// driver's own (Postgres: invalid input, 22P02) rather than ErrNotFound.
func testNonUUIDUserIDFindsNothing(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "linear", tool(t, "linear", "create_issue", "Creates.", 1))

	if err := s.PutSecret(ctx, user, "linear_key", []byte("sealed")); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}

	const bad = "not-a-uuid"

	if got, err := s.GetTool(ctx, bad, "linear", "create_issue"); err == nil {
		t.Errorf("GetTool(non-UUID user) = %+v, want an error", got)
	}

	if got, err := s.LastSync(ctx, bad, "linear"); err == nil {
		t.Errorf("LastSync(non-UUID user) = %+v, want an error", got)
	}

	if got, err := s.GetSecret(ctx, bad, "linear_key"); err == nil {
		t.Errorf("GetSecret(non-UUID user) = %x, want an error", got)
	}

	if got, err := s.ToolCounts(ctx, bad); len(got) != 0 {
		t.Errorf("ToolCounts(non-UUID user) = %v, %v; want none", got, err)
	}
}

func testApplySyncStoresAnyValidJSONDefinitionExactly(t *testing.T, newDatabase NewDatabase) {
	// Valid JSON that a stricter JSON type would refuse or rewrite: a NUL
	// escape and odd spacing. The definition is stored as received.
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	def := json.RawMessage(`{ "name":"odd", "description":"nul \u0000 here", "inputSchema":{"type":"object"}}`)
	odd, err := store.NewTool("linear", def, []byte{1})

	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}

	apply(t, s, user, "linear", odd)

	got, err := s.GetTool(ctx, user, "linear", "odd")

	if err != nil {
		t.Fatalf("GetTool: %v", err)
	}

	if diff := cmp.Diff(string(def), string(got.Definition)); diff != "" {
		t.Errorf("Definition (-want +got):\n%s", diff)
	}
}

func testGetToolMissingIsNotFound(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)

	if _, err := s.GetTool(context.Background(), user, "linear", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTool: err = %v, want ErrNotFound", err)
	}
}

// testDeleteCatalogDropsOnlyThatConnectorsToolsAndRecord pins what Remove
// needs: a removed connector's catalog and last sync are gone, other
// connectors keep theirs, its credential stays, and deleting again is not
// an error, so a failed Remove can be retried.
func testDeleteCatalogDropsOnlyThatConnectorsToolsAndRecord(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "linear", tool(t, "linear", "create_issue", "Creates.", 1))
	apply(t, s, user, "github", tool(t, "github", "create_pr", "Opens.", 2))
	cred := store.Credential{UserID: user, Connector: "linear", Resource: "https://mcp.linear.app/mcp"}

	if err := s.PutCredential(ctx, cred); err != nil {
		t.Fatalf("PutCredential: %v", err)
	}

	if err := s.DeleteCatalog(ctx, user, "linear"); err != nil {
		t.Fatalf("DeleteCatalog: %v", err)
	}

	if err := s.DeleteCatalog(ctx, user, "linear"); err != nil {
		t.Errorf("DeleteCatalog again: %v, want nil", err)
	}

	if _, err := s.GetTool(ctx, user, "linear", "create_issue"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTool(linear.create_issue): err = %v, want ErrNotFound", err)
	}

	if _, err := s.LastSync(ctx, user, "linear"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LastSync(linear): err = %v, want ErrNotFound", err)
	}

	if _, err := s.GetTool(ctx, user, "github", "create_pr"); err != nil {
		t.Errorf("GetTool(github.create_pr): %v", err)
	}

	gotSync, err := s.LastSync(ctx, user, "github")

	if err != nil {
		t.Fatalf("LastSync(github): %v", err)
	}

	if diff := cmp.Diff(store.SyncRecord{At: time.UnixMilli(appliedAt)}, gotSync); diff != "" {
		t.Errorf("LastSync(github) (-want +got):\n%s", diff)
	}

	gotCred, err := s.GetCredential(ctx, user, "linear")

	if err != nil {
		t.Fatalf("GetCredential(linear): %v", err)
	}

	if diff := cmp.Diff(cred, gotCred); diff != "" {
		t.Errorf("credential after DeleteCatalog (-want +got):\n%s", diff)
	}

	// Re-adding after a delete reports every tool as changed again.
	changed := apply(t, s, user, "linear", tool(t, "linear", "create_issue", "Creates.", 1))

	if diff := cmp.Diff([]string{"create_issue"}, changed); diff != "" {
		t.Errorf("changed after delete (-want +got):\n%s", diff)
	}
}

func testToolCountsCountsEachConnectorsCatalog(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)

	got, err := s.ToolCounts(ctx, user)

	if err != nil {
		t.Fatalf("ToolCounts on an empty catalog: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("ToolCounts on an empty catalog = %v, want none", got)
	}

	apply(t, s, user, "linear",
		tool(t, "linear", "create_issue", "Creates.", 1),
		tool(t, "linear", "list_issues", "Lists.", 2))
	apply(t, s, user, "github", tool(t, "github", "create_pr", "Opens.", 3))
	apply(t, s, user, "empty")

	if err := s.DeleteCatalog(ctx, user, "github"); err != nil {
		t.Fatalf("DeleteCatalog: %v", err)
	}

	got, err = s.ToolCounts(ctx, user)

	if err != nil {
		t.Fatalf("ToolCounts: %v", err)
	}

	// A connector with no catalog entries is absent rather than zero.
	if diff := cmp.Diff(map[string]int{"linear": 2}, got); diff != "" {
		t.Errorf("ToolCounts (-want +got):\n%s", diff)
	}

	if got, err := s.ToolCounts(ctx, unknownUser(t)); err != nil || len(got) != 0 {
		t.Errorf("ToolCounts(unknown user) = %v, %v; want none", got, err)
	}
}
