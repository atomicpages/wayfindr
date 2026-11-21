package daemon_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// A connector's tools list keeps the tools it hides out of the catalog;
// status counts them, GET /admin/tools (`wayfindr connector tools`) lists
// every tool live with its class, and a reload that changes the list
// re-syncs the connector under it.
func TestToolsListHidesToolsAndAReloadAppliesIt(t *testing.T) {
	h := newHost(t)
	files := stdioConnector(t, fakeupstream.V20251125, fakeupstream.WithTools(
		&mcp.Tool{Name: "read_file", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		&mcp.Tool{Name: "write_file"},
	))
	addr := freeAddr(t)
	files["tools"] = []string{"@read"}
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": addr},
		"connectors": map[string]any{"files": files},
	})
	r.waitStatus(t, "files synced with 1 hidden", toolCounts("files", 1, 1))
	c := admin.NewClient("http://"+r.addr, "", r.client)

	list, err := c.Tools(t.Context(), "files")

	if err != nil {
		t.Fatalf("Tools: %v", err)
	}

	want := admin.ToolList{Connector: "files", Tools: []admin.ToolEntry{
		{Name: "read_file", Class: "read", Exposed: true},
		{Name: "write_file", Class: "destructive"},
	}}

	if diff := cmp.Diff(want, list, cmpopts.IgnoreFields(admin.ToolEntry{}, "Definition")); diff != "" {
		t.Errorf("listing (-want +got):\n%s", diff)
	}

	for _, e := range list.Tools {
		var def struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		}

		if err := json.Unmarshal(e.Definition, &def); err != nil || def.Name != e.Name || def.InputSchema == nil {
			t.Errorf("%s definition = %s (%v), want its name and inputSchema", e.Name, e.Definition, err)
		}
	}

	if _, err := c.Tools(t.Context(), "jira"); statusOf(err) != http.StatusNotFound {
		t.Errorf("Tools(jira) error = %v, want 404", err)
	}

	delete(files, "tools")
	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": addr},
		"connectors": map[string]any{"files": files},
	})
	r.waitStatus(t, "files re-synced with none hidden", toolCounts("files", 2, 0))
}

// toolCounts waits for name to be ready with tools catalog entries and
// hidden hidden tools.
func toolCounts(name string, tools, hidden int) func(admin.Status) bool {
	return func(s admin.Status) bool {
		c, ok := connector(s, name)

		return ok && c.State == upstream.StateReady && c.LastSync != nil && c.Tools == tools && c.HiddenTools == hidden
	}
}
