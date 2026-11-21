//go:build !windows

package shim_test

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// addProfiles adds profiles to m's config, before any daemon reads it.
func (m machine) addProfiles(t *testing.T, profiles map[string]any) {
	t.Helper()
	var cfg map[string]any

	if _, err := toml.DecodeFile(m.config, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}

	cfg["profiles"] = profiles
	writeConfig(t, m.config, cfg)
}

// TestShimSendsTheProfile: a shim started with --profile names it on every
// request, and the daemon then refuses what the profile hides; a shim
// started without one is not narrowed.
func TestShimSendsTheProfile(t *testing.T) {
	m := newMachine(t)
	m.addProfiles(t, map[string]any{
		"acme": map[string]any{"tools": map[string]any{"issues": []string{"!create_*"}}},
	})
	cs, err := m.connectShimWith(t, "profiled", nil, nil, "--profile", "acme")

	if err != nil {
		t.Fatalf("%v\n%s", err, m.logOf("profiled"))
	}

	args := map[string]any{"name": "issues.create_issue", "arguments": map[string]any{"title": "x"}}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "call_tool", Arguments: args})

	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if got := text(t, res); !res.IsError || !strings.Contains(got, "hidden by the Wayfindr profile acme") {
		t.Errorf("profiled call_tool (isError %v) = %q, want it hidden by the profile\n%s", res.IsError, got, m.logOf("profiled"))
	}

	open := m.startShim(t, "unprofiled")
	waitForIssueTool(t, open)

	// callText fails the test on an isError result.
	callText(t, open, "call_tool", args)
}

// TestShimRefusesToStartWithAnInvalidProfileName: the daemon would refuse
// every request, so the shim says so once, at start.
func TestShimRefusesToStartWithAnInvalidProfileName(t *testing.T) {
	m := newMachine(t)

	if _, err := m.connectShimWith(t, "invalid", nil, nil, "--profile", "Acme Co"); err == nil {
		t.Fatal("shim started with an invalid profile name")
	}

	if log := m.logOf("invalid"); !strings.Contains(log, `profile "Acme Co"`) {
		t.Errorf("shim stderr does not name the profile:\n%s", log)
	}
}
