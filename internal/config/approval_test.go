package config_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
)

// TestApprovalRuleGates covers the approval entry rule: a call is gated
// when some entry matches it and no ! entry does. Unlike a tools list,
// each class gates itself only, and * and ? match across '/'.
func TestApprovalRuleGates(t *testing.T) {
	type call struct {
		connector, tool string
		class           config.ToolClass
	}

	tests := []struct {
		name    string
		entries []string
		call    call
		want    bool
	}{
		{"nil gates nothing", nil, call{"slack", "delete_channel", config.ClassDestructive}, false},
		{"empty gates nothing", []string{}, call{"slack", "post_message", config.ClassWrite}, false},
		{"@write gates a write tool", []string{"@write"}, call{"slack", "post_message", config.ClassWrite}, true},
		{"@write does not gate a read tool", []string{"@write"}, call{"slack", "list_channels", config.ClassRead}, false},
		{"@write does not gate a destructive tool", []string{"@write"}, call{"slack", "delete_channel", config.ClassDestructive}, false},
		{"@destructive gates a destructive tool", []string{"@destructive"}, call{"slack", "delete_channel", config.ClassDestructive}, true},
		{"@destructive does not gate a write tool", []string{"@destructive"}, call{"slack", "post_message", config.ClassWrite}, false},
		{"@destructive does not gate a read tool", []string{"@destructive"}, call{"slack", "list_channels", config.ClassRead}, false},
		{"@destructive gates an empty class", []string{"@destructive"}, call{"slack", "x", ""}, true},
		{"@destructive gates an unknown class", []string{"@destructive"}, call{"slack", "x", "admin"}, true},
		{"@write does not gate an unknown class", []string{"@write"}, call{"slack", "x", "admin"}, false},
		{"both classes gate both", []string{"@write", "@destructive"}, call{"github", "merge_pr", config.ClassWrite}, true},
		{"name glob gates a read tool", []string{"linear.run_*"}, call{"linear", "run_query", config.ClassRead}, true},
		{"name glob is per connector", []string{"linear.run_*"}, call{"github", "run_query", config.ClassRead}, false},
		{"name glob is anchored", []string{"linear.run"}, call{"linear", "run_query", config.ClassRead}, false},
		{"exact name", []string{"slack.post_message"}, call{"slack", "post_message", config.ClassWrite}, true},
		{"glob over a dotted tool name", []string{"fs.file.*"}, call{"fs", "file.write", config.ClassWrite}, true},
		{"* matches across /", []string{"slack.*"}, call{"slack", "admin/delete", config.ClassDestructive}, true},
		{"* after / matches", []string{"slack.admin/*"}, call{"slack", "admin/delete", config.ClassDestructive}, true},
		{"* matches several /", []string{"slack.*_delete"}, call{"slack", "a/b/c_delete", config.ClassDestructive}, true},
		{"? matches /", []string{"slack.admin?delete"}, call{"slack", "admin/delete", config.ClassDestructive}, true},
		{"negated class matches /", []string{"slack.admin[^a]delete"}, call{"slack", "admin/delete", config.ClassDestructive}, true},
		{"class matches a letter", []string{"slack.[dp]*"}, call{"slack", "post_message", config.ClassWrite}, true},
		{"class misses a letter", []string{"slack.[dp]*"}, call{"slack", "list_channels", config.ClassRead}, false},
		{"escaped * is literal", []string{`slack.post\*`}, call{"slack", "post_message", config.ClassWrite}, false},
		{"escaped * matches *", []string{`slack.post\*`}, call{"slack", "post*", config.ClassWrite}, true},
		{"glob is anchored at its end", []string{"slack.*_message"}, call{"slack", "post_message_now", config.ClassWrite}, false},
		{"! exempts a name", []string{"slack.*", "!slack.list_*"}, call{"slack", "list_channels", config.ClassRead}, false},
		{"! leaves the rest gated", []string{"slack.*", "!slack.list_*"}, call{"slack", "post_message", config.ClassWrite}, true},
		{"! exempts across /", []string{"slack.*", "!slack.admin/*"}, call{"slack", "admin/list", config.ClassRead}, false},
		{"! exempts from a class", []string{"@write", "!slack.post_message"}, call{"slack", "post_message", config.ClassWrite}, false},
		{"! exemption is per connector", []string{"@write", "!slack.post_message"}, call{"teams", "post_message", config.ClassWrite}, true},
		{"! wins in any order", []string{"!slack.post_*", "@write"}, call{"slack", "post_message", config.ClassWrite}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := config.ParseApproval(tt.entries)

			if err != nil {
				t.Fatalf("ParseApproval(%q): %v", tt.entries, err)
			}

			if got := r.Gates(tt.call.connector, tt.call.tool, tt.call.class); got != tt.want {
				t.Errorf("Gates(%q, %q, %q) = %v, want %v", tt.call.connector, tt.call.tool, tt.call.class, got, tt.want)
			}
		})
	}
}

// TestApprovalRuleZeroValue: a rule that was never parsed gates nothing.
func TestApprovalRuleZeroValue(t *testing.T) {
	var r config.ApprovalRule

	if r.Gates("slack", "delete_channel", config.ClassDestructive) {
		t.Error("zero ApprovalRule gates a call")
	}
}

// TestParseApprovalRefuses checks that each bad entry is reported at its
// index, with the entry named in the message, and that good entries beside
// it don't hide the bad ones.
func TestParseApprovalRefuses(t *testing.T) {
	tests := []struct {
		name  string
		entry string
	}{
		{"@read", "@read"},
		{"unknown class", "@writes"},
		{"exempted class", "!@write"},
		{"exempted @read", "!@read"},
		{"no connector", "post_message"},
		{"empty connector", ".post_message"},
		{"empty glob", "slack."},
		{"blank glob", "slack. "},
		{"bad connector name", "Slack.post_message"},
		{"bad glob", "slack.list_["},
		{"bad exempt glob", "!slack.list_["},
		{"empty", ""},
		{"bare !", "!"},
		{"trailing space", "slack.post_message "},
		{"space after the dot", "slack. post_message"},
		{"leading space", " slack.post_message"},
		{"space before the dot", "slack .post_message"},
		{"trailing newline", "slack.x\n"},
		{"trailing carriage return", "slack.x\r"},
		{"tab inside", "slack.post\tmessage"},
		{"C1 control", "slack.post\u0085"},
		{"class with a trailing space", "@write "},
		{"exempt with a trailing space", "!slack.post_message "},
		{"space after !", "! slack.post_message"},
		{"exempt with a space after the dot", "!slack. post_message"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.ParseApproval([]string{"@write", tt.entry})

			if diff := cmp.Diff([]string{"approval[1]"}, issuePaths(t, err)); diff != "" {
				t.Errorf("issue paths (-want +got):\n%s", diff)
			}

			if tt.entry != "" && !strings.Contains(err.Error(), strconv.Quote(tt.entry)) {
				t.Errorf("error %q does not name the entry %q", err, tt.entry)
			}
		})
	}
}

// TestParseApprovalRefusesExemptionsOnly: entries that only exempt gate
// nothing, so they are refused as a whole.
func TestParseApprovalRefusesExemptionsOnly(t *testing.T) {
	_, err := config.ParseApproval([]string{"!slack.post_*", "!linear.delete_*"})

	if diff := cmp.Diff([]string{"approval"}, issuePaths(t, err)); diff != "" {
		t.Errorf("issue paths (-want +got):\n%s", diff)
	}

	if !strings.Contains(err.Error(), "class or tool entry to exempt from") {
		t.Errorf("error %q does not say what an exemption needs", err)
	}
}

// TestParseApprovalReportsAll: one parse reports every bad entry.
func TestParseApprovalReportsAll(t *testing.T) {
	_, err := config.ParseApproval([]string{"@read", "slack.post_message", "x.[", "!@destructive"})

	if diff := cmp.Diff([]string{"approval[0]", "approval[2]", "approval[3]"}, issuePaths(t, err)); diff != "" {
		t.Errorf("issue paths (-want +got):\n%s", diff)
	}
}

// issuePaths is the path of each issue of err, which must be a
// *config.ValidationError.
func issuePaths(t *testing.T, err error) []string {
	t.Helper()

	var ve *config.ValidationError

	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a *ValidationError", err)
	}

	paths := make([]string, 0, len(ve.Issues))

	for _, is := range ve.Issues {
		paths = append(paths, is.Path)
	}

	return paths
}

// TestApprovalRuleConnectors covers the connectors a rule names, which
// token create and grant approval check against the client's own.
func TestApprovalRuleConnectors(t *testing.T) {
	r, err := config.ParseApproval([]string{"@write", "slack.post_*", "!linear.list_*", "slack.delete_*"})

	if err != nil {
		t.Fatalf("ParseApproval: %v", err)
	}

	if diff := cmp.Diff([]string{"linear", "slack"}, r.Connectors()); diff != "" {
		t.Errorf("Connectors() (-want +got):\n%s", diff)
	}

	r, err = config.ParseApproval([]string{"@destructive"})

	if err != nil {
		t.Fatalf("ParseApproval: %v", err)
	}

	if got := r.Connectors(); got != nil {
		t.Errorf("Connectors() = %v, want nil", got)
	}
}
