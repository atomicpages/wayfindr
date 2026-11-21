package store_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/store"
)

// planTool builds a valid catalog entry for connector "linear".
func planTool(t *testing.T, name string, hash byte) store.Tool {
	t.Helper()
	def := fmt.Sprintf(`{"name":%q,"inputSchema":{"type":"object"}}`, name)
	tl, err := store.NewTool("linear", json.RawMessage(def), []byte{hash})

	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}

	return tl
}

func TestPlanCatalogClassifiesEachToolByItsStoredContentHash(t *testing.T) {
	existing := map[string][]byte{"same": {1}, "edited": {2}, "removed": {3}, "gone": {4}}
	same, edited := planTool(t, "same", 1), planTool(t, "edited", 9)
	added, alsoAdded := planTool(t, "zeta", 5), planTool(t, "added", 6)

	got, err := store.PlanCatalog("linear", existing, []store.Tool{alsoAdded, same, added, edited})

	if err != nil {
		t.Fatalf("PlanCatalog: %v", err)
	}

	want := store.CatalogPlan{
		Insert:  []store.Tool{alsoAdded, added},
		Update:  []store.Tool{edited},
		Touch:   []store.Tool{same},
		Delete:  []string{"gone", "removed"},
		Changed: []string{"added", "edited", "zeta"},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("PlanCatalog (-want +got):\n%s", diff)
	}
}

func TestPlanCatalogOfAnEmptyListDeletesEveryStoredTool(t *testing.T) {
	got, err := store.PlanCatalog("linear", map[string][]byte{"b": {1}, "a": {2}}, nil)

	if err != nil {
		t.Fatalf("PlanCatalog: %v", err)
	}

	if diff := cmp.Diff(store.CatalogPlan{Delete: []string{"a", "b"}}, got); diff != "" {
		t.Errorf("PlanCatalog (-want +got):\n%s", diff)
	}
}

func TestPlanCatalogRejectsAListItCannotStore(t *testing.T) {
	tests := []struct {
		name  string
		tools []store.Tool
	}{
		{"duplicate name", []store.Tool{planTool(t, "new", 2), planTool(t, "new", 3)}},
		{"invalid definition", []store.Tool{
			planTool(t, "new", 2),
			{Connector: "linear", Name: "broken", Definition: json.RawMessage(`{`), ContentHash: []byte{4}},
		}},
		{"definition without a name", []store.Tool{
			{Connector: "linear", Name: "", Definition: json.RawMessage(`{"description":"x"}`), ContentHash: []byte{4}},
		}},
		{"name disagrees with definition", []store.Tool{
			func() store.Tool { tl := planTool(t, "new", 2); tl.Name = "other"; return tl }(),
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.PlanCatalog("linear", map[string][]byte{"keep": {1}}, tt.tools)

			if err == nil {
				t.Fatalf("PlanCatalog = %+v, want an error", got)
			}

			if diff := cmp.Diff(store.CatalogPlan{}, got); diff != "" {
				t.Errorf("plan with the error (-want +got):\n%s", diff)
			}
		})
	}
}
