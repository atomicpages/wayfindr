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
