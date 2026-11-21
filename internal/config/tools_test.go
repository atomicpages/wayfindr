package config_test

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
)

// TestClassOf follows the MCP annotation defaults: readOnlyHint false and
// destructiveHint true when absent.
func TestClassOf(t *testing.T) {
	no, yes := false, true
	tests := []struct {
		name string
		a    *mcp.ToolAnnotations
		want config.ToolClass
	}{
		{"no annotations", nil, config.ClassDestructive},
		{"empty annotations", &mcp.ToolAnnotations{}, config.ClassDestructive},
		{"read-only", &mcp.ToolAnnotations{ReadOnlyHint: true}, config.ClassRead},
		{"read-only wins over destructive", &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &yes}, config.ClassRead},
		{"not destructive", &mcp.ToolAnnotations{DestructiveHint: &no}, config.ClassWrite},
		{"destructive", &mcp.ToolAnnotations{DestructiveHint: &yes}, config.ClassDestructive},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := config.ClassOf(tt.a); got != tt.want {
				t.Errorf("ClassOf = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestToolPolicy covers each branch of the rule in the tool policy plan:
// a !glob hides whatever else matches; a list with no other entries
// exposes the rest; otherwise a tool must match a glob or a listed class,
// and @write includes @read. HidesName answers the same question without
// a class, so it must agree with Exposes wherever the class can't change
// the outcome, and hide nothing that some class would expose.
func TestToolPolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy config.ToolPolicy
		tool   string
		class  config.ToolClass
		want   bool
		// hidesName is HidesName's answer for tool.
		hidesName bool
	}{
		{"nil exposes all", nil, "delete_repo", config.ClassDestructive, true, false},
		{"exclusion only: unmatched exposed", config.ToolPolicy{"!delete_*"}, "send", config.ClassDestructive, true, false},
		{"exclusion only: matched hidden", config.ToolPolicy{"!delete_*"}, "delete_repo", config.ClassRead, false, true},
		{"@read exposes a read tool", config.ToolPolicy{"@read"}, "get", config.ClassRead, true, false},
		{"@read hides a write tool", config.ToolPolicy{"@read"}, "create", config.ClassWrite, false, false},
		{"@read hides an unannotated tool", config.ToolPolicy{"@read"}, "x", config.ClassDestructive, false, false},
		{"@write exposes a read tool", config.ToolPolicy{"@write"}, "get", config.ClassRead, true, false},
		{"@write exposes a write tool", config.ToolPolicy{"@write"}, "create", config.ClassWrite, true, false},
		{"@write hides a destructive tool", config.ToolPolicy{"@write"}, "drop", config.ClassDestructive, false, false},
		{"@read plus a name exposes the name", config.ToolPolicy{"@read", "create_draft"}, "create_draft", config.ClassWrite, true, false},
		{"@read plus a name hides other writes", config.ToolPolicy{"@read", "create_draft"}, "send_message", config.ClassWrite, false, false},
		{"names only: listed", config.ToolPolicy{"get_issue", "list_*"}, "list_issues", config.ClassDestructive, true, false},
		{"names only: unlisted read tool hidden", config.ToolPolicy{"get_issue"}, "list_issues", config.ClassRead, false, true},
		{"exclusion beats class", config.ToolPolicy{"@write", "!merge_*"}, "merge_pr", config.ClassWrite, false, true},
		{"exclusion beats name, in any order", config.ToolPolicy{"!send_*", "send_draft"}, "send_draft", config.ClassWrite, false, true},
		{"glob is anchored", config.ToolPolicy{"get"}, "get_issue", config.ClassRead, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.Exposes(tt.tool, tt.class); got != tt.want {
				t.Errorf("Exposes(%q, %q) = %v, want %v", tt.tool, tt.class, got, tt.want)
			}

			if got := tt.policy.HidesName(tt.tool); got != tt.hidesName {
				t.Errorf("HidesName(%q) = %v, want %v", tt.tool, got, tt.hidesName)
			}
		})
	}
}
