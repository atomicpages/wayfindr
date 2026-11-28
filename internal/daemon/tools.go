package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// toolLister lists a connector's tools live without recording the list's
// cache hint, so a look from the admin API doesn't move the poller's
// schedule. *upstream.Manager satisfies it.
type toolLister interface {
	PeekTools(ctx context.Context, connector string) ([]*mcp.Tool, error)
}

// adminTools adapts the manager's PeekTools to admin.Options.Tools: each
// tool the upstream serves now, with its class, its definition, and whether
// the connector's running tools list exposes it. An upstream failure becomes an
// *admin.ToolsError with upstream's advice.
func adminTools(l toolLister, policy func(connector string) config.ToolPolicy) func(context.Context, string) (admin.ToolList, error) {
	return func(ctx context.Context, connector string) (admin.ToolList, error) {
		listed, err := l.PeekTools(ctx, connector)

		if e, ok := upstream.AsError(err); ok {
			a := upstream.Advise(e)

			return admin.ToolList{}, &admin.ToolsError{
				Kind:   string(e.Kind),
				Case:   string(e.Case),
				Advice: admin.Advice{Summary: a.Summary, Action: a.Action, Link: a.Link},
			}
		}

		if err != nil {
			return admin.ToolList{}, err
		}

		p := policy(connector)
		list := admin.ToolList{Connector: connector, Tools: make([]admin.ToolEntry, 0, len(listed))}

		for _, lt := range listed {
			if lt == nil {
				continue
			}

			def, err := json.Marshal(lt)

			if err != nil {
				return admin.ToolList{}, fmt.Errorf("marshal tool %q: %w", lt.Name, err)
			}

			class := config.ClassOf(lt.Annotations)
			list.Tools = append(list.Tools, admin.ToolEntry{
				Name: lt.Name, Class: string(class), Exposed: p.Exposes(lt.Name, class),
				RequiredArgs: requiredArgs(def), Definition: def,
			})
		}

		slices.SortFunc(list.Tools, func(a, b admin.ToolEntry) int { return strings.Compare(a.Name, b.Name) })

		return list, nil
	}
}

// requiredArgs counts the distinct names a tool definition's
// inputSchema.required lists; 0 when it lists none or doesn't decode.
func requiredArgs(definition json.RawMessage) int {
	var d struct {
		InputSchema struct {
			Required []string `json:"required"`
		} `json:"inputSchema"`
	}

	if json.Unmarshal(definition, &d) != nil {
		return 0
	}

	seen := make(map[string]struct{}, len(d.InputSchema.Required))

	for _, r := range d.InputSchema.Required {
		seen[r] = struct{}{}
	}

	return len(seen)
}
