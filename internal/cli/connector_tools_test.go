package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// toolsDaemon serves the admin API with a tool lister that answers list
// for gmail and a 404 for any other connector.
func toolsDaemon(t *testing.T, list admin.ToolList) Host {
	t.Helper()
	host, root := testHost(t)
	writeStatusHost(t, root, serveAdminAPI(t, admin.Options{
		Upstreams: &fakeUpstreams{},
		Store:     &fakeStore{},
		UserID:    "local",
		Config:    func() admin.ConfigState { return admin.ConfigState{} },
		Tools: func(_ context.Context, connector string) (admin.ToolList, error) {
			if connector != "gmail" {
				return admin.ToolList{}, &admin.ToolsError{Kind: admin.KindUnknownConnector, Advice: admin.Advice{Summary: "No connector is called " + connector + "."}}
			}

			return list, nil
		},
		Logger: zerolog.New(zerolog.NewTestWriter(t)),
	}))

	return host
}

func TestConnectorToolsPrintsTheTableAndJSON(t *testing.T) {
	list := admin.ToolList{Connector: "gmail", Tools: []admin.ToolEntry{
		{Name: "create_draft", Class: "write", Exposed: true, RequiredArgs: 2},
		{Name: "get_thread", Class: "read", Exposed: true, RequiredArgs: 1},
		{Name: "send_message", Class: "write"},
	}}
	host := toolsDaemon(t, list)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"connector", "tools", "gmail"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want := `TOOL          CLASS  EXPOSED  REQUIRED ARGS
create_draft  write  yes      2
get_thread    read   yes      1
send_message  write  no       0

2 of 3 tools exposed.
`

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	stdout.Reset()

	if code := RunWith([]string{"connector", "tools", "gmail", "--json"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("--json: exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	var got admin.ToolList

	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a ToolList: %v\n%s", err, stdout.String())
	}

	if diff := cmp.Diff(list, got); diff != "" {
		t.Errorf("--json (-want +got):\n%s", diff)
	}
}

func TestConnectorToolsReportsTheDaemonsRefusal(t *testing.T) {
	host := toolsDaemon(t, admin.ToolList{})
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"connector", "tools", "jira"}, &stdout, &stderr, host); code == 0 {
		t.Fatalf("exit code = 0, want failure; stdout: %s", stdout.String())
	}

	if !strings.Contains(stderr.String(), "No connector is called jira.") {
		t.Errorf("stderr = %q, want the daemon's advice", stderr.String())
	}
}

// Tool names come from the upstream, so the table replaces an escape
// sequence or a bidi override in one with U+FFFD rather than let the
// terminal obey it. --json keeps the name as the upstream sent it.
func TestConnectorToolsReplacesControlCharactersInNames(t *testing.T) {
	list := admin.ToolList{Connector: "gmail", Tools: []admin.ToolEntry{
		{Name: "red\x1b[31m", Class: "read", Exposed: true},
		{Name: "read\u202eetirw", Class: "read"},
	}}
	host := toolsDaemon(t, list)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"connector", "tools", "gmail"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want := "TOOL        CLASS  EXPOSED  REQUIRED ARGS\n" +
		"red\uFFFD[31m    read   yes      0\n" +
		"read\uFFFDetirw  read   no       0\n" +
		"\n1 of 2 tools exposed.\n"

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	stdout.Reset()

	if code := RunWith([]string{"connector", "tools", "gmail", "--json"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("--json: exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	var got admin.ToolList

	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a ToolList: %v\n%s", err, stdout.String())
	}

	if diff := cmp.Diff(list, got); diff != "" {
		t.Errorf("--json (-want +got):\n%s", diff)
	}
}

func TestConnectorToolsDescribesOneTool(t *testing.T) {
	saveIssue := json.RawMessage(`{"name":"save_issue","inputSchema":{"type":"object","properties":{` +
		`"assignee":{"type":["string","null"],"description":"User ID, name,\nor \"me\". Null to remove"},` +
		`"labels":{"type":"array","items":{"type":"string"},"description":"Label names."},` +
		`"slaType":{"anyOf":[{"type":"string","enum":["all","onlyBusinessDays"]},{"type":"null"}],"description":"SLA day counting"},` +
		`"state":{"type":"string","enum":["open","closed"],"description":"The state."},` +
		`"team":{"type":"string","description":"Team name or ID (required when creating)"}}}}`)
	createIssue := json.RawMessage(`{"name":"create_issue","inputSchema":{"type":"object","properties":{` +
		`"team":{"type":"string"},"title":{"type":"string","description":"Issue title"},"meta":{}},"required":["team","title"]}}`)
	list := admin.ToolList{Connector: "gmail", Tools: []admin.ToolEntry{
		{Name: "save_issue", Class: "write", Exposed: true, Definition: saveIssue},
		{Name: "create_issue", Class: "write", Definition: createIssue, RequiredArgs: 2},
		{Name: "ping", Class: "read", Exposed: true, Definition: json.RawMessage(`{"name":"ping","inputSchema":{"type":"object"}}`)},
		{Name: "bare", Class: "read", Exposed: true, Definition: json.RawMessage(`{"name":"bare"}`)},
	}}
	host := toolsDaemon(t, list)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"nothing required", []string{"save_issue"}, `gmail.save_issue (write)
No argument is required by the schema; its descriptions may still say what a call needs.

  NAME      TYPE         DESCRIPTION
  assignee  string|null  User ID, name, or "me". Null to remove
  labels    string[]     Label names.
  slaType   string|null  SLA day counting. One of "all", "onlyBusinessDays".
  state     string       The state. One of "open", "closed".
  team      string       Team name or ID (required when creating)
`},
		{"required, hidden", []string{"create_issue"}, `gmail.create_issue (write, hidden by the tools list)

  NAME   TYPE    REQUIRED  DESCRIPTION
  team   string  required
  title  string  required  Issue title
  meta   any
`},
		{"no arguments", []string{"ping"}, "gmail.ping (read)\nIt takes no arguments.\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			if code := RunWith(append([]string{"connector", "tools", "gmail"}, tt.args...), &stdout, &stderr, host); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			if diff := cmp.Diff(tt.want, stdout.String()); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}

			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want nothing", stderr.String())
			}
		})
	}

	for _, tt := range []struct{ tool, want string }{
		{"create_issue", `{"type":"object","properties":{"team":{"type":"string"},"title":{"type":"string","description":"Issue title"},"meta":{}},"required":["team","title"]}`},
		{"bare", `{}`},
	} {
		t.Run(tt.tool+" --json is the inputSchema", func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			if code := RunWith([]string{"connector", "tools", "gmail", tt.tool, "--json"}, &stdout, &stderr, host); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			var got, want any

			if err := json.Unmarshal([]byte(tt.want), &want); err != nil {
				t.Fatalf("the test's want is not JSON: %v", err)
			}

			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("--json (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("unknown tool", func(t *testing.T) {
		var stdout, stderr bytes.Buffer

		if code := RunWith([]string{"connector", "tools", "gmail", "nope"}, &stdout, &stderr, host); code == 0 ||
			!strings.Contains(stderr.String(), "gmail serves no tool called nope: list them with `wayfindr connector tools gmail`") {
			t.Errorf("exit code = %d, stderr = %q; want the unknown tool error", code, stderr.String())
		}
	})
}

// TestArgTableWraps: argTable's wrapping, a pure function's decision table
// (CODING_STANDARDS: tested directly because the terminal width can't be
// set through the command): descriptions wrap under their column at the
// given width, and stay one line at 0 or when the column would be under
// minDescriptionWidth.
func TestArgTableWraps(t *testing.T) {
	fields := []admin.SchemaField{
		{Name: "description", Types: []string{"string"}, Description: "Content as Markdown. Do not escape the string, use literal newlines and special characters."},
		{Name: "id", Types: []string{"string"}},
	}

	tests := []struct {
		name  string
		width int
		want  string
	}{
		{"60 columns", 60, `  NAME         TYPE    DESCRIPTION
  description  string  Content as Markdown. Do not escape
                       the string, use literal newlines and
                       special characters.
  id           string
`},
		{"no width", 0, `  NAME         TYPE    DESCRIPTION
  description  string  Content as Markdown. Do not escape the string, use literal newlines and special characters.
  id           string
`},
		{"too narrow", 40, `  NAME         TYPE    DESCRIPTION
  description  string  Content as Markdown. Do not escape the string, use literal newlines and special characters.
  id           string
`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, argTable(fields, tt.width)); diff != "" {
				t.Errorf("argTable (-want +got):\n%s", diff)
			}
		})
	}
}
