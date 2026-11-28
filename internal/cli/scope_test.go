package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// scopeDaemon serves the admin API with gmail and linear configured, and
// list as gmail's tools; the scope file in the working directory is body.
func scopeDaemon(t *testing.T, list admin.ToolList, body string) Host {
	t.Helper()

	return profileDaemon(t, list, body, nil)
}

// profileDaemon is scopeDaemon whose running config has profiles. An empty
// body writes no scope file.
func profileDaemon(t *testing.T, list admin.ToolList, body string, profiles map[string]*config.Scope) Host {
	t.Helper()
	host, root := testHost(t)

	if body != "" {
		if err := os.WriteFile(filepath.Join(root, ".wayfindr.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeStatusHost(t, root, serveAdminAPI(t, admin.Options{
		Upstreams: &fakeUpstreams{states: []upstream.ConnectorStatus{
			{Connector: "gmail", State: upstream.StateReady},
			{Connector: "linear", State: upstream.StateReady},
		}},
		Store:  &fakeStore{},
		UserID: "local",
		Config: func() admin.ConfigState { return admin.ConfigState{Profiles: profiles} },
		Tools: func(context.Context, string) (admin.ToolList, error) {
			return list, nil
		},
		Logger: zerolog.New(zerolog.NewTestWriter(t)),
	}))

	return host
}

func TestScopeShowsWhatTheProjectGets(t *testing.T) {
	host := scopeDaemon(t, admin.ToolList{}, "connectors = [\"gmail\", \"jira\"]\n[tools]\ngmail = [\"@read\", \"a\\u202eb\"]\n")
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"scope"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	got := stdout.String()
	_, rest, _ := strings.Cut(got, "\n\n")
	want := "CONNECTOR  IN SCOPE  SCOPE TOOLS\n" +
		"gmail      yes       @read, a�b\n" +
		"linear     no        -\n" +
		"\nWarning: the scope names jira, which is not configured; it has no effect.\n"

	if !strings.HasPrefix(got, "Project scope: ") || !strings.Contains(got, ".wayfindr.toml") {
		t.Errorf("stdout = %q, want it to name the scope file", got)
	}

	if diff := cmp.Diff(want, rest); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

func TestScopeWithAConnectorANDsTheToolsLists(t *testing.T) {
	list := admin.ToolList{Connector: "gmail", Tools: []admin.ToolEntry{
		{Name: "create_draft", Class: "write", Exposed: true},
		{Name: "get_thread", Class: "read", Exposed: true},
		{Name: "send_message", Class: "write"},
	}}
	host := scopeDaemon(t, list, "[tools]\ngmail = [\"@read\", \"send_message\"]\n")
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"scope", "gmail"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	_, rest, _ := strings.Cut(stdout.String(), "\n\n")
	want := `TOOL          CLASS  EXPOSED  REQUIRED ARGS
create_draft  write  no       0
get_thread    read   yes      0
send_message  write  no       0

1 of 3 tools exposed.
`

	if diff := cmp.Diff(want, rest); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

func TestScopeReportsAnInvalidFile(t *testing.T) {
	host := scopeDaemon(t, admin.ToolList{}, "url = \"http://x\"\n")
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"scope"}, &stdout, &stderr, host); code == 0 {
		t.Fatalf("exit code = 0, want failure; stdout: %s", stdout.String())
	}

	if !strings.Contains(stderr.String(), "url: unknown key") {
		t.Errorf("stderr = %q, want the validation issue", stderr.String())
	}
}

func TestScopeWithoutAFile(t *testing.T) {
	host, _ := testHost(t)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"scope"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "No project scope applies") {
		t.Errorf("stdout = %q, want no scope", stdout.String())
	}
}

// acme is the profile the --profile tests name: gmail only, read tools.
func acme() map[string]*config.Scope {
	return map[string]*config.Scope{
		"acme": {Connectors: []string{"gmail"}, Tools: map[string]config.ToolPolicy{"gmail": {"@read"}}},
		"all":  {},
	}
}

func TestScopeWithAProfile(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		want       string
	}{
		{
			name: "without a scope file",
			want: "Profile: acme\n\n" +
				"CONNECTOR  IN SCOPE  PROFILE TOOLS\n" +
				"gmail      yes       @read\n" +
				"linear     no        -\n",
		},
		{
			// Each tools column is its own scope's say; IN SCOPE needs both.
			name: "ANDed with a scope file",
			body: "connectors = [\"linear\", \"jira\"]\n[tools]\nlinear = [\"!delete_*\"]\n",
			want: "CONNECTOR  IN SCOPE  PROFILE TOOLS  SCOPE TOOLS\n" +
				"gmail      no        @read          -\n" +
				"linear     no        -              !delete_*\n" +
				"\nWarning: the scope names jira, which is not configured; it has no effect.\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host := profileDaemon(t, admin.ToolList{}, tt.body, acme())
			var stdout, stderr bytes.Buffer

			if code := RunWith([]string{"scope", "--profile", "acme"}, &stdout, &stderr, host); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			got := stdout.String()

			if tt.body != "" {
				head, rest, _ := strings.Cut(got, "\n\n")

				if !strings.HasPrefix(head, "Profile: acme\nProject scope: ") || !strings.HasSuffix(head, ".wayfindr.toml") {
					t.Errorf("header = %q, want the profile, then the scope file", head)
				}

				got = rest
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}
		})
	}
}

func TestScopeWithAProfileAndAConnectorANDsAllThreeToolsLists(t *testing.T) {
	list := admin.ToolList{Connector: "gmail", Tools: []admin.ToolEntry{
		{Name: "create_draft", Class: "write", Exposed: true},
		{Name: "get_thread", Class: "read", Exposed: true},
		{Name: "list_labels", Class: "read", Exposed: true},
		{Name: "send_message", Class: "write"},
	}}
	// The profile allows only reads; the file then hides get_thread.
	host := profileDaemon(t, list, "[tools]\ngmail = [\"!get_thread\"]\n", acme())
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"scope", "gmail", "--profile", "acme"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	head, rest, _ := strings.Cut(stdout.String(), "\n\n")
	want := `TOOL          CLASS  EXPOSED  REQUIRED ARGS
create_draft  write  no       0
get_thread    read   no       0
list_labels   read   yes      0
send_message  write  no       0

1 of 4 tools exposed.
`

	if !strings.HasPrefix(head, "Profile: acme\nProject scope: ") {
		t.Errorf("header = %q, want the profile, then the scope file", head)
	}

	if diff := cmp.Diff(want, rest); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

func TestScopeWithAnUnknownProfile(t *testing.T) {
	for _, tt := range []struct {
		name     string
		profiles map[string]*config.Scope
		want     string
	}{
		{"names the defined ones", acme(), "it defines acme, all"},
		{"says none are defined", nil, "defines no profiles"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host := profileDaemon(t, admin.ToolList{}, "", tt.profiles)
			var stdout, stderr bytes.Buffer

			if code := RunWith([]string{"scope", "--profile", "acme2"}, &stdout, &stderr, host); code == 0 {
				t.Fatalf("exit code = 0, want failure; stdout: %s", stdout.String())
			}

			if got := stderr.String(); !strings.Contains(got, `profile "acme2"`) || !strings.Contains(got, tt.want) {
				t.Errorf("stderr = %q, want it to name acme2 and contain %q", got, tt.want)
			}
		})
	}
}
