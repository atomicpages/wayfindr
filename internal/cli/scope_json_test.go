package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// runScopeJSON runs scope with args and --json and returns the decoded
// object, checking that a scope file it names is the one in dir's place.
func runScopeJSON(t *testing.T, host Host, args ...string) map[string]any {
	t.Helper()
	var stdout, stderr bytes.Buffer

	if code := RunWith(append([]string{"scope", "--json"}, args...), &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	var got map[string]any

	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, stdout.String())
	}

	dir, _ := got["dir"].(string)

	if dir == "" {
		t.Errorf("dir = %v, want the working directory", got["dir"])
	}

	if file, ok := got["scope_file"]; ok && file != filepath.Join(dir, ".wayfindr.toml") {
		t.Errorf("scope_file = %v, want .wayfindr.toml in %s", file, dir)
	}

	return got
}

// withoutPaths drops dir and scope_file, which the tests check apart, and
// records whether scope_file was present.
func withoutPaths(got map[string]any) (map[string]any, bool) {
	_, hasFile := got["scope_file"]
	delete(got, "dir")
	delete(got, "scope_file")

	return got, hasFile
}

func TestScopeJSON(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		args       []string
		wantFile   bool
		want       string
	}{
		{
			name:     "a scope file",
			body:     "connectors = [\"gmail\", \"jira\"]\n[tools]\ngmail = [\"@read\", \"a\\u202eb\"]\n",
			wantFile: true,
			// Tools lists keep their entries as sent; the table makes them
			// printable.
			want: `{
				"connectors": [
					{"name": "gmail", "in_scope": true, "in_scope_file": true, "scope_tools": ["@read", "a\u202eb"]},
					{"name": "linear", "in_scope": false, "in_scope_file": false}
				],
				"unconfigured": ["jira"]
			}`,
		},
		{
			name: "a profile",
			args: []string{"--profile", "acme"},
			want: `{
				"profile": "acme",
				"connectors": [
					{"name": "gmail", "in_scope": true, "in_profile": true, "profile_tools": ["@read"]},
					{"name": "linear", "in_scope": false, "in_profile": false}
				],
				"unconfigured": []
			}`,
		},
		{
			name:     "a profile ANDed with a scope file",
			body:     "connectors = [\"linear\", \"jira\"]\n[tools]\nlinear = [\"!delete_*\"]\n",
			args:     []string{"--profile", "acme"},
			wantFile: true,
			want: `{
				"profile": "acme",
				"connectors": [
					{"name": "gmail", "in_scope": false, "in_profile": true, "profile_tools": ["@read"], "in_scope_file": false},
					{"name": "linear", "in_scope": false, "in_profile": false, "in_scope_file": true, "scope_tools": ["!delete_*"]}
				],
				"unconfigured": ["jira"]
			}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host := profileDaemon(t, admin.ToolList{}, tt.body, acme())
			got, hasFile := withoutPaths(runScopeJSON(t, host, tt.args...))

			if hasFile != tt.wantFile {
				t.Errorf("scope_file present = %v, want %v", hasFile, tt.wantFile)
			}

			if diff := cmp.Diff(decodeJSON(t, tt.want), got); diff != "" {
				t.Errorf("scope (-want +got):\n%s", diff)
			}
		})
	}
}

func TestScopeJSONWithAConnector(t *testing.T) {
	list := admin.ToolList{Connector: "gmail", Tools: []admin.ToolEntry{
		{Name: "create_draft", Class: "write", Exposed: true},
		{Name: "get_thread", Class: "read", Exposed: true},
		{Name: "list_labels", Class: "read", Exposed: true},
		{Name: "send_message", Class: "write"},
	}}
	// The profile allows only reads; the file then hides get_thread.
	host := profileDaemon(t, list, "[tools]\ngmail = [\"!get_thread\"]\n", acme())
	got, hasFile := withoutPaths(runScopeJSON(t, host, "gmail", "--profile", "acme"))
	want := `{
		"profile": "acme",
		"connector": "gmail",
		"tools": [
			{"name": "create_draft", "class": "write", "exposed": false, "required_args": 0},
			{"name": "get_thread", "class": "read", "exposed": false, "required_args": 0},
			{"name": "list_labels", "class": "read", "exposed": true, "required_args": 0},
			{"name": "send_message", "class": "write", "exposed": false, "required_args": 0}
		]
	}`

	if !hasFile {
		t.Error("scope_file is absent, want the scope file")
	}

	if diff := cmp.Diff(decodeJSON(t, want), got); diff != "" {
		t.Errorf("scope (-want +got):\n%s", diff)
	}
}

// Without a scope file or --profile nothing narrows, and the daemon isn't
// asked, so there is nothing to list.
func TestScopeJSONWithoutAFile(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"no connector", nil, `{"connectors": [], "unconfigured": []}`},
		{"a connector", []string{"gmail"}, `{"connector": "gmail", "tools": []}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host, root := testHost(t)
			got := runScopeJSON(t, host, tt.args...)

			if got["dir"] != root {
				t.Errorf("dir = %v, want %s", got["dir"], root)
			}

			got, hasFile := withoutPaths(got)

			if hasFile {
				t.Error("scope_file is present, want it absent")
			}

			if diff := cmp.Diff(decodeJSON(t, tt.want), got); diff != "" {
				t.Errorf("scope (-want +got):\n%s", diff)
			}
		})
	}
}

func decodeJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any

	if err := json.NewDecoder(strings.NewReader(s)).Decode(&v); err != nil {
		t.Fatalf("bad want JSON: %v", err)
	}

	return v
}
