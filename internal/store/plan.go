package store

import (
	"bytes"
	"fmt"
	"sort"
)

// CatalogPlan is what a successful sync writes to make a connector's stored
// catalog its new tool list. Every list is sorted by name.
type CatalogPlan struct {
	// Insert is the tools not stored yet.
	Insert []Tool
	// Update is the stored tools whose content hash changed: their embedded
	// text did, so their embedding is stale and must be cleared.
	Update []Tool
	// Touch is the stored tools whose content hash is the same. Their
	// definition is still rewritten, since parts that are not embedded
	// (outputSchema, annotations) may have changed, and call_tool validates
	// against it.
	Touch []Tool
	// Delete is the stored tool names not in the new list.
	Delete []string
	// Changed is the names in Insert and Update: what ApplySync reports.
	Changed []string
}

// PlanCatalog plans the write that makes tools connector's complete catalog,
// given existing, the stored catalog's content hash by tool name. Every
// driver's ApplySync executes the plan, so they agree on what changed.
//
// It rejects a list it cannot store: a definition NewTool cannot parse
// (the parsed fields are derived from it on every read), a tool whose Name
// is not its definition's name, or two tools with one name.
func PlanCatalog(connector string, existing map[string][]byte, tools []Tool) (CatalogPlan, error) {
	seen := make(map[string]bool, len(tools))

	for _, t := range tools {
		parsed, err := NewTool(connector, t.Definition, t.ContentHash)

		if err != nil {
			return CatalogPlan{}, fmt.Errorf("tool %q: %w", t.Name, err)
		}

		if parsed.Name != t.Name {
			return CatalogPlan{}, fmt.Errorf("tool %q: definition name is %q", t.Name, parsed.Name)
		}

		if seen[t.Name] {
			return CatalogPlan{}, fmt.Errorf("duplicate tool %q", t.Name)
		}

		seen[t.Name] = true
	}

	sorted := append([]Tool(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var p CatalogPlan

	for _, t := range sorted {
		old, ok := existing[t.Name]

		switch {
		case !ok:
			p.Insert = append(p.Insert, t)
			p.Changed = append(p.Changed, t.Name)
		case !bytes.Equal(old, t.ContentHash):
			p.Update = append(p.Update, t)
			p.Changed = append(p.Changed, t.Name)
		default:
			p.Touch = append(p.Touch, t)
		}
	}

	for name := range existing {
		if !seen[name] {
			p.Delete = append(p.Delete, name)
		}
	}

	sort.Strings(p.Delete)

	return p, nil
}
