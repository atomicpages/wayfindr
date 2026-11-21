package clientconfig_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/atomicpages/wayfindr/internal/clientconfig"
	"github.com/atomicpages/wayfindr/internal/config"
)

type tmap = map[string]config.Template

func shellEnv(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := vars[k]

		return v, ok
	}
}

// TestMapConnector covers the mapping table: each case is one entry that
// is imported, and the connector, secrets and notes it becomes.
func TestMapConnector(t *testing.T) {
	shell := shellEnv(map[string]string{"GITHUB_TOKEN": "ghp_1", "LOG": "debug", "EMPTY": ""})

	for _, tt := range []struct {
		name     string
		in       clientconfig.Server
		plainEnv []string
		want     config.NewConnector
		secrets  []clientconfig.Secret
		notes    []string
	}{
		{
			name: "stdio with a literal env value becomes a secret",
			in: clientconfig.Server{Name: "sentry", Command: "sentry-mcp", Args: []clientconfig.Value{lit("--fast")},
				Env: map[string]clientconfig.Value{"SENTRY_TOKEN": lit("sntrys_1")}},
			want: config.NewConnector{Name: "sentry", Command: "sentry-mcp", Args: []config.Template{"--fast"},
				Env: tmap{"SENTRY_TOKEN": "${secret:sentry_sentry_token}"}},
			secrets: []clientconfig.Secret{{Name: "sentry_sentry_token", Label: "environment variable SENTRY_TOKEN", Value: lit("sntrys_1")}},
		},
		{
			name:     "--plain-env keeps a literal",
			in:       clientconfig.Server{Name: "fs", Command: "fs", Env: map[string]clientconfig.Value{"DEBUG": lit("1"), "LEVEL": env("LOG")}},
			plainEnv: []string{"DEBUG", "LEVEL"},
			want:     config.NewConnector{Name: "fs", Command: "fs", Env: tmap{"DEBUG": "1", "LEVEL": "debug"}},
			notes:    []string{"environment variable LEVEL holds the value of LOG from this shell"},
		},
		{
			name:  "an empty value stays empty",
			in:    clientconfig.Server{Name: "fs", Command: "fs", Env: map[string]clientconfig.Value{"A": nil, "B": env("EMPTY")}},
			want:  config.NewConnector{Name: "fs", Command: "fs", Env: tmap{"A": "", "B": ""}},
			notes: []string{"environment variable B stores the value of EMPTY from this shell"},
		},
		{
			name: "a header with a variable this shell sets stores its value",
			in: clientconfig.Server{Name: "github", Type: "http", URL: lit("https://api.githubcopilot.com/mcp/"),
				Headers: map[string]clientconfig.Value{"Authorization": {{Lit: "Bearer "}, {Env: "GITHUB_TOKEN"}}}},
			want: config.NewConnector{Name: "github", URL: "https://api.githubcopilot.com/mcp/",
				Headers: tmap{"Authorization": "${secret:github_authorization}"}},
			secrets: []clientconfig.Secret{{Name: "github_authorization", Label: "header Authorization", Value: lit("Bearer ghp_1")}},
			notes:   []string{"header Authorization stores the value of GITHUB_TOKEN from this shell"},
		},
		{
			name: "a variable this shell doesn't set becomes ${env:…}; literal text beside it a secret",
			in: clientconfig.Server{Name: "api", URL: lit("https://api.example/mcp"),
				Headers: map[string]clientconfig.Value{
					"Authorization": {{Lit: "Bearer "}, {Env: "API_KEY"}},
					"X-Key":         {{Lit: "sk-1;u="}, {Env: "USER_ID"}},
				}},
			want: config.NewConnector{Name: "api", URL: "https://api.example/mcp", Headers: tmap{
				"Authorization": "Bearer ${env:API_KEY}",
				"X-Key":         "${secret:api_x-key}${env:USER_ID}",
			}},
			secrets: []clientconfig.Secret{{Name: "api_x-key", Label: "header X-Key", Value: lit("sk-1;u=")}},
			notes: []string{
				"API_KEY is not set in this shell", "the daemon's environment must set API_KEY",
				"USER_ID is not set in this shell", "the daemon's environment must set USER_ID",
			},
		},
		{
			name: "an unset variable with a default stores the default",
			in: clientconfig.Server{Name: "s", Command: "s",
				Env: map[string]clientconfig.Value{"REGION": {{Env: "REGION", Default: "eu", HasDefault: true}}}},
			want:    config.NewConnector{Name: "s", Command: "s", Env: tmap{"REGION": "${secret:s_region}"}},
			secrets: []clientconfig.Secret{{Name: "s_region", Label: "environment variable REGION", Value: lit("eu")}},
			notes:   []string{"environment variable REGION stores the value of REGION from this shell"},
		},
		{
			name: "args and url always read the daemon's environment",
			in: clientconfig.Server{Name: "fs", Command: "fs",
				Args: []clientconfig.Value{{{Env: "HOME"}, {Lit: "/src"}}, {{Env: "LOG", Default: "info", HasDefault: true}}}},
			want: config.NewConnector{Name: "fs", Command: "fs", Args: []config.Template{"${env:HOME}/src", "${env:LOG}"}},
			notes: []string{
				"the daemon's environment must set HOME", "the daemon's environment must set LOG",
				"the default of LOG is dropped",
			},
		},
		{
			name: "a VS Code input becomes a secret, in env or url",
			in: clientconfig.Server{Name: "p", Type: "http", URL: clientconfig.Value{{Lit: "https://x.example/"}, {Input: "tenant"}},
				Headers: map[string]clientconfig.Value{"Authorization": {{Lit: "Bearer "}, {Input: "key"}}}},
			want: config.NewConnector{Name: "p", URL: "https://x.example/${secret:p_tenant}",
				Headers: tmap{"Authorization": "${secret:p_authorization}"}},
			secrets: []clientconfig.Secret{
				{Name: "p_tenant", Label: "input tenant", Value: clientconfig.Value{{Input: "tenant"}}},
				{Name: "p_authorization", Label: "header Authorization", Value: clientconfig.Value{{Lit: "Bearer "}, {Input: "key"}}},
			},
		},
		{
			name: "secret names stay unique",
			in: clientconfig.Server{Name: "s", URL: lit("https://s.example/mcp"),
				Headers: map[string]clientconfig.Value{"Token": lit("a"), "token": lit("b")}},
			want: config.NewConnector{Name: "s", URL: "https://s.example/mcp",
				Headers: tmap{"Token": "${secret:s_token}", "token": "${secret:s_token_2}"}},
			secrets: []clientconfig.Secret{
				{Name: "s_token", Label: "header Token", Value: lit("a")},
				{Name: "s_token_2", Label: "header token", Value: lit("b")},
			},
		},
		{
			name: "sse sets the transport",
			in:   clientconfig.Server{Name: "old", Type: "sse", URL: lit("https://old.example/sse")},
			want: config.NewConnector{Name: "old", URL: "https://old.example/sse", Transport: config.TransportSSE},
		},
		{
			name: "streamable-http and no type are plain url connectors",
			in:   clientconfig.Server{Name: "new", Type: "streamable-http", URL: lit("https://new.example/mcp")},
			want: config.NewConnector{Name: "new", URL: "https://new.example/mcp"},
		},
		{
			name: "oauth and timeout carry over",
			in: clientconfig.Server{Name: "linear", URL: lit("https://mcp.linear.app/mcp"),
				OAuth: true, OAuthClientID: "cid", OAuthScopes: []string{"read"}, Timeout: 90 * time.Second},
			want: config.NewConnector{Name: "linear", URL: "https://mcp.linear.app/mcp",
				Auth:    &config.NewAuth{Type: config.AuthTypeOAuth, ClientID: "cid", Scopes: []string{"read"}},
				Timeout: &config.Duration{Duration: 90 * time.Second}},
		},
		{
			name:  "the name is normalized; npx is noted",
			in:    clientconfig.Server{Name: "My Server.v2", Command: "npx", Args: []clientconfig.Value{lit("-y"), lit("srv")}},
			want:  config.NewConnector{Name: "my-server-v2", Command: "npx", Args: []config.Template{"-y", "srv"}},
			notes: []string{"npx downloads the server on every start"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan := clientconfig.Map([]clientconfig.Server{tt.in}, clientconfig.Options{
				LookupEnv: shell, PlainEnv: tt.plainEnv, Listen: config.DefaultListen,
			})

			if len(plan.Skips) != 0 || len(plan.Connectors) != 1 {
				t.Fatalf("Map = %+v; want one connector", plan)
			}

			got := plan.Connectors[0]
			opts := cmpopts.EquateEmpty()

			if diff := cmp.Diff(tt.want, got.Connector, opts); diff != "" {
				t.Errorf("connector (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff(tt.secrets, got.Secrets, opts); diff != "" {
				t.Errorf("secrets (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff(tt.notes, got.Notes, opts, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
				t.Errorf("notes (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMapSkips(t *testing.T) {
	existing := config.Connectors{"notes": {Name: "notes"}}

	for _, tt := range []struct {
		name   string
		in     clientconfig.Server
		reason string
	}{
		{"turned off", clientconfig.Server{Name: "a", Command: "a", Disabled: true}, "it is turned off in the client"},
		{"not approved", clientconfig.Server{Name: "a", Command: "a", Unapproved: true}, "it comes from the project and the client has not approved it"},
		{"unsupported", clientconfig.Server{Name: "a", Command: "a", Unsupported: []string{"cwd", "envFile"}}, "it sets cwd, envFile, which a connector can't express"},
		{"bad name", clientconfig.Server{Name: "_hidden", Command: "a"}, `"_hidden" can't be made a connector name ([a-z0-9][a-z0-9_-]{0,62})`},
		{"existing", clientconfig.Server{Name: "Notes", Command: "a"}, "a connector called notes already exists"},
		{"the shim", clientconfig.Server{Name: "wayfindr", Command: "/usr/local/bin/wayfindr", Args: []clientconfig.Value{lit("stdio")}}, "this is Wayfindr itself"},
		{"the daemon", clientconfig.Server{Name: "wf", URL: lit("http://localhost:7777/mcp")}, "this is Wayfindr itself"},
		{"both", clientconfig.Server{Name: "a", Command: "a", URL: lit("https://a.example")}, "it has both a command and a url"},
		{"neither", clientconfig.Server{Name: "a"}, "it has neither a command nor a url"},
		{"command typed http", clientconfig.Server{Name: "a", Command: "a", Type: "http"}, `it has a command but type "http"`},
		{"websocket", clientconfig.Server{Name: "a", Type: "ws", URL: lit("wss://a.example")}, `type "ws" is not one a connector supports`},
		{"literal ${ in an arg", clientconfig.Server{Name: "a", Command: "a", Args: []clientconfig.Value{lit("${HOME}")}}, "its argument 1 has a literal ${, which a connector can't write"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan := clientconfig.Map([]clientconfig.Server{tt.in}, clientconfig.Options{Existing: existing, Listen: config.DefaultListen})

			if len(plan.Connectors) != 0 || len(plan.Skips) != 1 {
				t.Fatalf("Map = %+v; want one skip", plan)
			}

			if got := plan.Skips[0].Reason; got != tt.reason {
				t.Errorf("reason = %q; want %q", got, tt.reason)
			}
		})
	}
}

// TestMapSecretNamesNeverCollide: a_b's C and a's B_C would both be
// a_b_c, and an existing connector already uses s_token. Each secret gets
// its own name, so no import overwrites another connector's credential.
func TestMapSecretNamesNeverCollide(t *testing.T) {
	servers := []clientconfig.Server{
		{Name: "a", Command: "a", Env: map[string]clientconfig.Value{"B_C": lit("one")}},
		{Name: "a_b", Command: "b", Env: map[string]clientconfig.Value{"C": lit("two")}},
		{Name: "s", URL: lit("https://s.example/mcp"), Headers: map[string]clientconfig.Value{"Token": lit("three")}},
	}
	existing := config.Connectors{"old": {Name: "old", Headers: map[string]config.Template{"Token": "${secret:s_token}"}}}

	plan := clientconfig.Map(servers, clientconfig.Options{Existing: existing})
	got := map[string]string{}

	for _, p := range plan.Connectors {
		for _, sec := range p.Secrets {
			got[sec.Name] = sec.Resolve(nil)
		}
	}

	if diff := cmp.Diff(map[string]string{"a_b_c": "one", "a_b_c_2": "two", "s_token_2": "three"}, got); diff != "" {
		t.Errorf("secrets (-want +got):\n%s", diff)
	}
}

// A secret the config refers to outside any connector, such as
// [embedding]'s api_key, is never stored over either (wayfindr-eadp).
func TestMapSecretNamesAvoidTheConfigsOtherSecrets(t *testing.T) {
	servers := []clientconfig.Server{{Name: "notes", Command: "notes", Env: map[string]clientconfig.Value{"KEY": lit("v")}}}

	plan := clientconfig.Map(servers, clientconfig.Options{Secrets: []string{"notes_key"}})

	if len(plan.Connectors) != 1 || len(plan.Connectors[0].Secrets) != 1 || plan.Connectors[0].Secrets[0].Name != "notes_key_2" {
		t.Errorf("plan = %+v, want notes's secret stored as notes_key_2", plan.Connectors)
	}
}

// A long connector name's secrets are cut to 63 bytes, and a clash still
// gets a numbered name of a valid length.
func TestMapLongConnectorSecretNamesStayValid(t *testing.T) {
	long := strings.Repeat("c", 60)
	servers := []clientconfig.Server{
		{Name: long, URL: lit("https://s.example/mcp"), Headers: map[string]clientconfig.Value{"Token": lit("a"), "token": lit("b")}},
	}

	plan := clientconfig.Map(servers, clientconfig.Options{})
	got := map[string]string{}

	for _, p := range plan.Connectors {
		for _, sec := range p.Secrets {
			if !config.IsSecretName(sec.Name) {
				t.Errorf("secret %q is not a valid secret name", sec.Name)
			}

			got[sec.Name] = sec.Resolve(nil)
		}
	}

	if diff := cmp.Diff(map[string]string{long + "_to": "a", long + "__2": "b"}, got); diff != "" {
		t.Errorf("secrets (-want +got):\n%s", diff)
	}
}

func TestMapScopesAndCollisions(t *testing.T) {
	servers := []clientconfig.Server{
		{Name: "fs", Scope: clientconfig.ScopeUser, Command: "user-fs"},
		{Name: "fs", Scope: clientconfig.ScopeLocal, Command: "local-fs"},
		{Name: "fs", Scope: clientconfig.ScopeProject, Command: "project-fs"},
		{Name: "GitHub", Scope: clientconfig.ScopeUser, URL: lit("https://a.example")},
		{Name: "github", Scope: clientconfig.ScopeUser, URL: lit("https://b.example")},
		// Narrower scope wins even when the names differ in case.
		{Name: "Notes", Scope: clientconfig.ScopeUser, Command: "user-notes"},
		{Name: "notes", Scope: clientconfig.ScopeProject, Command: "project-notes"},
	}

	plan := clientconfig.Map(servers, clientconfig.Options{})
	var names []string

	for _, p := range plan.Connectors {
		names = append(names, p.Connector.Name+"="+p.Connector.Command+string(p.Connector.URL))
	}

	if diff := cmp.Diff([]string{"github=https://a.example", "fs=local-fs", "notes=project-notes"}, names); diff != "" {
		t.Errorf("connectors (-want +got):\n%s", diff)
	}

	if len(plan.Skips) != 1 || plan.Skips[0].From.Name != "github" || plan.Skips[0].Reason != "GitHub also becomes connector github" {
		t.Errorf("skips = %+v; want github skipped for GitHub", plan.Skips)
	}
}
