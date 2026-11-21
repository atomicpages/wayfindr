//go:build !windows

package shim_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (m machine) writeScope(t *testing.T, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(m.project, ".wayfindr.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestShimSendsTheProjectScope: a shim started in a project with a scope
// file carries it to the daemon, which then refuses what it hides; a shim
// started elsewhere is not narrowed.
func TestShimSendsTheProjectScope(t *testing.T) {
	m := newMachine(t)
	m.writeScope(t, "[tools]\nissues = [\"!create_*\"]\n")
	cs := m.startShim(t, "scoped")
	args := map[string]any{"name": "issues.create_issue", "arguments": map[string]any{"title": "x"}}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "call_tool", Arguments: args})

	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if got := text(t, res); !res.IsError || !strings.Contains(got, "hidden by this project's Wayfindr scope") {
		t.Errorf("scoped call_tool (isError %v) = %q, want it hidden by the scope\n%s", res.IsError, got, m.logOf("scoped"))
	}

	if err := os.Remove(filepath.Join(m.project, ".wayfindr.toml")); err != nil {
		t.Fatal(err)
	}

	open := m.startShim(t, "unscoped")
	waitForIssueTool(t, open)

	// callText fails the test on an isError result.
	callText(t, open, "call_tool", args)
}

// TestShimRefusesToStartWithAnInvalidProjectScope: serving without the
// scope would give the project more than it asked for.
func TestShimRefusesToStartWithAnInvalidProjectScope(t *testing.T) {
	m := newMachine(t)
	m.writeScope(t, "[connectors.evil]\ncommand = \"sh\"\n")

	if _, err := m.connectShim(t, "invalid"); err == nil {
		t.Fatal("shim started with an invalid scope file")
	}

	if log := m.logOf("invalid"); !strings.Contains(log, ".wayfindr.toml") {
		t.Errorf("shim stderr does not name the scope file:\n%s", log)
	}
}
