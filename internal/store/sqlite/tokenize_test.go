package sqlite

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// These tests call tokenize and foldPlural directly. They are pure decision
// tables and the readable spec of the design §7.5 rules, so they are their
// own seam (CODING_STANDARDS.md, Tests); Store.Search tests cover how the
// rules affect ranking.

// The tokenizer rules are design §7.5.
func TestTokenize(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"createIssue", []string{"create", "issue"}},
		{"create_issue", []string{"create", "issue"}},
		{"create-issue", []string{"create", "issue"}},
		{"linear.create_issue", []string{"linear", "create", "issue"}},
		{"API-post-search", []string{"api", "post", "search"}},
		{"HTTPServerError", []string{"http", "server", "error"}},
		{"getV2Users", []string{"get", "users", "user"}},
		// A plural is kept and followed by its folded form.
		{"list_invoices", []string{"list", "invoices", "invoice"}},
		{"Lists QUERIES and their status", []string{"lists", "list", "queries", "query", "and", "their", "status"}},
		{"sha256 base64url", []string{"sha", "256", "base", "64", "url"}},
		{"Read a file, then a b c.", []string{"read", "file", "then"}},
		{"  ", nil},
		{"Ünïcode Straße", []string{"ünïcode", "straße"}},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, tokenize(tt.in)); diff != "" {
				t.Errorf("tokenize(%q) (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

// The plural folding rules are design §7.5. Non-plurals that end in s must
// come through unchanged.
func TestFoldPlural(t *testing.T) {
	tests := []struct{ in, want string }{
		// ies → y
		{"queries", "query"},
		{"entities", "entity"},
		{"repositories", "repository"},
		// sses → ss
		{"classes", "class"},
		{"processes", "process"},
		{"addresses", "address"},
		// xes, ches, shes → drop es
		{"indexes", "index"},
		{"branches", "branch"},
		{"searches", "search"},
		{"pushes", "push"},
		// s → drop
		{"invoices", "invoice"},
		{"subscriptions", "subscription"},
		{"commits", "commit"},
		{"messages", "message"},
		{"dsns", "dsn"},
		{"schemas", "schema"},
		{"users", "user"},
		// not plurals: ss, us, is endings
		{"class", "class"},
		{"process", "process"},
		{"access", "access"},
		{"status", "status"},
		{"previous", "previous"},
		{"analysis", "analysis"},
		{"this", "this"},
		// too short to fold
		{"has", "has"},
		{"ids", "ids"},
		// ies on a 4-rune word is left alone
		{"lies", "lies"},
		{"ties", "ties"},
		{"is", "is"},
		// already singular
		{"query", "query"},
		{"issue", "issue"},
		{"search", "search"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := foldPlural(tt.in); got != tt.want {
				t.Errorf("foldPlural(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
