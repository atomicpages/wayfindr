package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
)

// editGmail has a comment inside a connector's table, an auth subtable, an
// inline auth table, a multi-line value, and connectors after the one a
// test edits.
const editGmail = `# Wayfindr configuration.

# Gmail, via Google.
[connectors.gmail]
url = "https://gmail.test/mcp"   # the MCP endpoint
# sign-in is below
timeout = "30s"

[connectors.gmail.auth]
type = "oauth"
client_id = "abc"
client_secret = "${secret:gmail_client_secret}"
scopes = ["a", "b"]

# Linear, via OAuth.
[connectors.linear]
url  = "https://mcp.linear.app/mcp"
auth = { type = "oauth" }   # the quick-start form

[connectors.fs]
command = "npx"
args    = [
  "-y",
  "server-filesystem",
]
`

func TestEditConnector(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		connector string
		edit      config.Edit
		// old and new are the text the edit replaces: want is src with
		// old replaced by new.
		old, new string
	}{
		{
			name:      "set a scalar, keeping its comment",
			src:       editGmail,
			connector: "gmail",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"url"}, Value: config.Template("https://gmail.test/v2")}}},
			old:       `url = "https://gmail.test/mcp"   # the MCP endpoint`,
			new:       `url = "https://gmail.test/v2"   # the MCP endpoint`,
		},
		{
			name:      "set a duration",
			src:       editGmail,
			connector: "gmail",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"timeout"}, Value: config.Duration{Duration: time.Minute}}}},
			old:       `timeout = "30s"`,
			new:       `timeout = "1m0s"`,
		},
		{
			name:      "replace a list",
			src:       editGmail,
			connector: "gmail",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"auth", "scopes"}, Value: []string{"c"}}}},
			old:       `scopes = ["a", "b"]`,
			new:       `scopes = ["c"]`,
		},
		{
			name:      "add a key at the end of its table",
			src:       editGmail,
			connector: "gmail",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"auth", "redirect_uri"}, Value: "http://127.0.0.1:7777/oauth/callback"}}},
			old:       "scopes = [\"a\", \"b\"]\n",
			new:       "scopes = [\"a\", \"b\"]\nredirect_uri = \"http://127.0.0.1:7777/oauth/callback\"\n",
		},
		{
			name:      "add a key to the connector's own table",
			src:       editGmail,
			connector: "gmail",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"transport"}, Value: "sse"}}},
			old:       "timeout = \"30s\"\n",
			new:       "timeout = \"30s\"\ntransport = \"sse\"\n",
		},
		{
			name:      "create a subtable after the connector's last table",
			src:       editGmail,
			connector: "gmail",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"headers", "X-Team"}, Value: "core"}}},
			old:       "scopes = [\"a\", \"b\"]\n",
			new:       "scopes = [\"a\", \"b\"]\n\n[connectors.gmail.headers]\nX-Team = \"core\"\n",
		},
		{
			name:      "create a nested subtable",
			src:       editGmail,
			connector: "gmail",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"auth", "authorize_params", "prompt"}, Value: "consent"}}},
			old:       "scopes = [\"a\", \"b\"]\n",
			new:       "scopes = [\"a\", \"b\"]\n\n[connectors.gmail.auth.authorize_params]\nprompt = \"consent\"\n",
		},
		{
			name:      "unset a key, keeping the comment above it",
			src:       editGmail,
			connector: "gmail",
			edit:      config.Edit{Unset: []config.Key{{"timeout"}}},
			old:       "# sign-in is below\ntimeout = \"30s\"\n",
			new:       "# sign-in is below\n",
		},
		{
			name:      "unset the auth table",
			src:       editGmail,
			connector: "gmail",
			edit:      config.Edit{Unset: []config.Key{{"auth"}}},
			old:       "[connectors.gmail.auth]\ntype = \"oauth\"\nclient_id = \"abc\"\nclient_secret = \"${secret:gmail_client_secret}\"\nscopes = [\"a\", \"b\"]\n\n",
			new:       "",
		},
		{
			name:      "set a key in an inline table",
			src:       editGmail,
			connector: "linear",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"auth", "client_id"}, Value: "xyz"}}},
			old:       `auth = { type = "oauth" }   # the quick-start form`,
			new:       `auth = { type = "oauth", client_id = "xyz" }   # the quick-start form`,
		},
		{
			name:      "add a table inside an inline table",
			src:       editGmail,
			connector: "linear",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"auth", "authorize_params", "prompt"}, Value: "consent"}}},
			old:       `auth = { type = "oauth" }`,
			new:       `auth = { type = "oauth", authorize_params = { prompt = "consent" } }`,
		},
		{
			name: "edit an inline table keeping its key order",
			src: "[connectors.linear]\nurl = \"https://mcp.linear.app/mcp\"\n" +
				"auth = { type = \"oauth\", scopes = [\"x\"], client_id = \"a\" }\n",
			connector: "linear",
			edit: config.Edit{
				Set:   []config.Set{{Key: config.Key{"auth", "scopes"}, Value: []string{"y", "z"}}},
				Unset: []config.Key{{"auth", "client_id"}},
			},
			old: `auth = { type = "oauth", scopes = ["x"], client_id = "a" }`,
			new: `auth = { type = "oauth", scopes = ["y", "z"] }`,
		},
		{
			name:      "unset an inline auth",
			src:       editGmail,
			connector: "linear",
			edit:      config.Edit{Unset: []config.Key{{"auth"}}},
			old:       "auth = { type = \"oauth\" }   # the quick-start form\n",
			new:       "",
		},
		{
			name:      "a multi-line value not being changed",
			src:       editGmail,
			connector: "fs",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"env", "DEBUG"}, Value: "1"}}},
			old:       "  \"server-filesystem\",\n]\n",
			new:       "  \"server-filesystem\",\n]\n\n[connectors.fs.env]\nDEBUG = \"1\"\n",
		},
		{
			name:      "add auth to a connector without it",
			src:       "[connectors.notes]\nurl = \"https://notes.test/mcp\"\n\n[server]\nlisten = \"127.0.0.1:7777\"\n",
			connector: "notes",
			edit: config.Edit{Set: []config.Set{
				{Key: config.Key{"auth", "authorize_params", "prompt"}, Value: "consent"},
				{Key: config.Key{"auth", "type"}, Value: config.AuthTypeOAuth},
				{Key: config.Key{"auth", "client_id"}, Value: "x"},
			}},
			old: "url = \"https://notes.test/mcp\"\n",
			new: "url = \"https://notes.test/mcp\"\n\n[connectors.notes.auth]\ntype = \"oauth\"\nclient_id = \"x\"\n" +
				"\n[connectors.notes.auth.authorize_params]\nprompt = \"consent\"\n",
		},
		{
			name:      "a file without a final line break",
			src:       "[connectors.notes]\n  url = \"https://notes.test/mcp\"",
			connector: "notes",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"timeout"}, Value: "10s"}}},
			old:       `url = "https://notes.test/mcp"`,
			new:       "url = \"https://notes.test/mcp\"\n  timeout = \"10s\"\n",
		},
		{
			name:      "a quoted header key",
			src:       editGmail,
			connector: "gmail",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"headers", "X.Team"}, Value: "core"}}},
			old:       "scopes = [\"a\", \"b\"]\n",
			new:       "scopes = [\"a\", \"b\"]\n\n[connectors.gmail.headers]\n\"X.Team\" = \"core\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if strings.Count(tt.src, tt.old) != 1 {
				t.Fatalf("old %q is not in src exactly once", tt.old)
			}

			got, err := config.EditConnector([]byte(tt.src), tt.connector, tt.edit, options())

			if err != nil {
				t.Fatalf("EditConnector: %v", err)
			}

			want := strings.Replace(tt.src, tt.old, tt.new, 1)

			if diff := cmp.Diff(want, string(got.File)); diff != "" {
				t.Errorf("file (-want +got):\n%s", diff)
			}

			if !got.Changed {
				t.Error("Changed = false, want true")
			}

			if _, ok := got.After.Connectors[tt.connector]; !ok {
				t.Errorf("After has no connector %s", tt.connector)
			}
		})
	}
}

// TestEditConnectorResult: Before and After are the configs the file
// held, so a caller can tell what the edit changed.
func TestEditConnectorResult(t *testing.T) {
	got, err := config.EditConnector([]byte(editGmail), "linear", config.Edit{Set: []config.Set{
		{Key: config.Key{"auth", "client_id"}, Value: "xyz"},
		{Key: config.Key{"auth", "authorize_params", "prompt"}, Value: "consent"},
	}}, options())

	if err != nil {
		t.Fatalf("EditConnector: %v", err)
	}

	before, after := got.Before.Connectors["linear"], got.After.Connectors["linear"]

	if before.Auth.ClientID != "" || len(before.Auth.AuthorizeParams) != 0 {
		t.Errorf("Before auth = %+v, want the file's", before.Auth)
	}

	if diff := cmp.Diff(map[string]string{"prompt": "consent"}, after.Auth.AuthorizeParams); diff != "" || after.Auth.ClientID != "xyz" {
		t.Errorf("After auth = %+v, want client_id xyz and prompt=consent (%s)", after.Auth, diff)
	}

	if diff := cmp.Diff(got.Before.Connectors["gmail"], got.After.Connectors["gmail"]); diff != "" {
		t.Errorf("gmail changed (-before +after):\n%s", diff)
	}
}

// TestEditConnectorNoChange: an edit that sets every key to the value it
// has leaves the file alone and says so.
func TestEditConnectorNoChange(t *testing.T) {
	for _, edit := range []config.Edit{
		{Set: []config.Set{{Key: config.Key{"url"}, Value: "https://gmail.test/mcp"}}},
		{Set: []config.Set{
			{Key: config.Key{"auth", "scopes"}, Value: []string{"a", "b"}},
			{Key: config.Key{"auth", "client_id"}, Value: "abc"},
		}},
	} {
		got, err := config.EditConnector([]byte(editGmail), "gmail", edit, options())

		if err != nil {
			t.Fatalf("EditConnector: %v", err)
		}

		if got.Changed || string(got.File) != editGmail || got.After != got.Before {
			t.Errorf("EditConnector(%+v) = changed %t, want the file unchanged", edit, got.Changed)
		}
	}
}

func TestEditConnectorRefuses(t *testing.T) {
	const dotted = "[connectors.notes]\nurl = \"https://notes.test/mcp\"\nauth.type = \"oauth\"\n"

	tests := []struct {
		name      string
		src       string
		connector string
		edit      config.Edit
		want      config.Case
	}{
		{
			name:      "no such connector",
			src:       editGmail,
			connector: "notes",
			edit:      config.Edit{Unset: []config.Key{{"timeout"}}},
			want:      config.CaseEditNotFound,
		},
		{
			name:      "nothing to change",
			src:       editGmail,
			connector: "gmail",
			want:      config.CaseNothingToChange,
		},
		{
			name:      "set and unset one key",
			src:       editGmail,
			connector: "gmail",
			edit: config.Edit{
				Set:   []config.Set{{Key: config.Key{"timeout"}, Value: "1s"}},
				Unset: []config.Key{{"timeout"}},
			},
			want: config.CaseKeyTwice,
		},
		{
			name:      "unset a table and set a key in it",
			src:       editGmail,
			connector: "gmail",
			edit: config.Edit{
				Set:   []config.Set{{Key: config.Key{"auth", "type"}, Value: "oauth"}},
				Unset: []config.Key{{"auth"}},
			},
			want: config.CaseKeyTwice,
		},
		{
			name:      "unset a missing key",
			src:       editGmail,
			connector: "gmail",
			edit:      config.Edit{Unset: []config.Key{{"auth", "redirect_uri"}}},
			want:      config.CaseKeyNotSet,
		},
		{
			name:      "unset a missing table",
			src:       editGmail,
			connector: "gmail",
			edit:      config.Edit{Unset: []config.Key{{"headers"}}},
			want:      config.CaseKeyNotSet,
		},
		{
			name:      "unset a missing key of an inline table",
			src:       editGmail,
			connector: "linear",
			edit:      config.Edit{Unset: []config.Key{{"auth", "scopes"}}},
			want:      config.CaseKeyNotSet,
		},
		{
			name:      "dotted keys",
			src:       dotted,
			connector: "notes",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"auth", "client_id"}, Value: "x"}}},
			want:      config.CaseDottedKeys,
		},
		{
			name:      "a multi-line value being changed",
			src:       editGmail,
			connector: "fs",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"args"}, Value: []string{"-y"}}}},
			want:      config.CaseMultiline,
		},
		{
			name:      "a multi-line inline table being changed",
			src:       "[connectors.notes]\nurl = \"https://notes.test/mcp\"\nheaders = { A = \"\"\"\nx\"\"\" }\n",
			connector: "notes",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"headers", "B"}, Value: "y"}}},
			want:      config.CaseMultiline,
		},
		{
			name:      "an inline connector",
			src:       "[connectors]\nfs = { command = \"ls\" }\n",
			connector: "fs",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"command"}, Value: "cat"}}},
			want:      config.CaseNotTables,
		},
		{
			name:      "a dotted connector",
			src:       "[connectors]\nfs.command = \"ls\"\n",
			connector: "fs",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"command"}, Value: "cat"}}},
			want:      config.CaseNotTables,
		},
		{
			name:      "a value where a table is",
			src:       editGmail,
			connector: "gmail",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"auth"}, Value: "oauth"}}},
			want:      config.CaseUnparsable,
		},
		{
			name:      "a header inside a multi-line string",
			src:       "[connectors.fs]\ncommand = \"ls\"\nargs = [\"\"\"\n[connectors.other]\n\"\"\"]\n",
			connector: "fs",
			edit:      config.Edit{Set: []config.Set{{Key: config.Key{"env", "A"}, Value: "1"}}},
			want:      config.CaseReadsDifferently,
		},
		{
			name:      "file does not parse",
			src:       editGmail + "[server\n",
			connector: "gmail",
			edit:      config.Edit{Unset: []config.Key{{"timeout"}}},
			want:      config.CaseUnparsable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.EditConnector([]byte(tt.src), tt.connector, tt.edit, options())
			checkEditCase(t, err, tt.want)
		})
	}
}

// TestEditConnectorDottedElsewhere: dotted keys the edit doesn't touch
// don't stop it.
func TestEditConnectorDottedElsewhere(t *testing.T) {
	src := "[connectors.notes]\nurl = \"https://notes.test/mcp\"\nauth.type = \"oauth\"\n"
	got, err := config.EditConnector([]byte(src), "notes", config.Edit{
		Set: []config.Set{{Key: config.Key{"timeout"}, Value: "5s"}},
	}, options())

	if err != nil {
		t.Fatalf("EditConnector: %v", err)
	}

	if want := src + "timeout = \"5s\"\n"; string(got.File) != want {
		t.Errorf("file = %q, want %q", got.File, want)
	}
}

// TestEditConnectorValidation: an edit the config's rules refuse fails
// with the config's own error, naming the rule.
func TestEditConnectorValidation(t *testing.T) {
	tests := []struct {
		name string
		edit config.Edit
		path string
	}{
		{"client_secret needs client_id", config.Edit{Unset: []config.Key{{"auth", "client_id"}}}, "connectors.gmail.auth.client_secret"},
		{"url is required", config.Edit{Unset: []config.Key{{"url"}}}, "connectors.gmail"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.EditConnector([]byte(editGmail), "gmail", tt.edit, options())

			var ve *config.ValidationError

			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want a *ValidationError", err)
			}

			if _, ok := config.AsError(err); ok {
				t.Errorf("err = %v, want no config *Error", err)
			}

			if !strings.Contains(err.Error(), tt.path) {
				t.Errorf("err = %v, want it to name %s", err, tt.path)
			}
		})
	}
}

func TestEditAdvice(t *testing.T) {
	tests := []struct {
		err  config.Error
		want config.Advice
	}{
		{
			config.Error{Case: config.CaseConnectorExists, Connector: "gmail"},
			config.Advice{
				Summary: "gmail already exists",
				Action:  "change it with `wayfindr connector edit gmail`, or remove it with `wayfindr connector remove gmail`",
			},
		},
		{
			config.Error{Case: config.CaseEditNotFound, Connector: "gmail"},
			config.Advice{Summary: "no connector is called gmail", Action: "`wayfindr connector add` creates one"},
		},
		{
			config.Error{Case: config.CaseConnectorNotFound, Connector: "gmail"},
			config.Advice{Summary: "no connector is called gmail", Action: "`wayfindr status` lists them"},
		},
		{
			config.Error{Case: config.CaseKeyNotSet, Connector: "gmail", Key: "auth.scopes"},
			config.Advice{Summary: "gmail has no auth.scopes", Action: "leave it out, or check its spelling"},
		},
	}

	for _, tt := range tests {
		if diff := cmp.Diff(tt.want, config.Advise(&tt.err)); diff != "" {
			t.Errorf("Advise(%s) (-want +got):\n%s", tt.err.Case, diff)
		}
	}
}

// TestReason: the reason is the advice's summary without the CLI's
// "could not change it safely: " lead-in, which only an unsafe edit has.
func TestReason(t *testing.T) {
	tests := []struct {
		err  config.Error
		want string
	}{
		{config.Error{Case: config.CaseUnparsable}, "it does not parse, or would not after the edit"},
		{config.Error{Case: config.CaseServerNotTable}, "server is not written as a [server] table"},
		{config.Error{Case: config.CaseOAuthNotTable}, "server.oauth is not written as a [server.oauth] table"},
		{config.Error{Case: config.CaseOAuthNotTable, Key: "server.oauth"}, "server.oauth is not written as a [server.oauth] table"},
		{config.Error{Case: config.CaseOAuthNotTable, Key: "server"}, "server is not written as a [server] table"},
		{config.Error{Case: config.CaseEditNotFound, Connector: "gmail"}, "no connector is called gmail"},
	}

	for _, tt := range tests {
		t.Run(string(tt.err.Case), func(t *testing.T) {
			if got := config.Reason(&tt.err); got != tt.want {
				t.Errorf("Reason(%s) = %q, want %q", tt.err.Case, got, tt.want)
			}
		})
	}
}

// TestEditConnectorKeyNotSetNames: the error names the key as TOML writes
// it.
func TestEditConnectorKeyNotSetNames(t *testing.T) {
	_, err := config.EditConnector([]byte(editGmail), "gmail", config.Edit{
		Unset: []config.Key{{"auth", "authorize_params", "prompt"}},
	}, options())
	e, ok := config.AsError(err)

	if !ok || e.Case != config.CaseKeyNotSet || e.Key != "auth.authorize_params.prompt" {
		t.Fatalf("err = %v, want key_not_set for auth.authorize_params.prompt", err)
	}
}

func TestSecretRefs(t *testing.T) {
	const src = `[embedding]
base_url = "https://embed.test/v1"
model = "m"
dimensions = 8
api_key = "${secret:embed_key}"

[connectors.gmail]
url = "https://gmail.test/mcp"
auth = { type = "oauth", client_id = "a", client_secret = "${secret:google_client_secret}" }

[connectors.gdrive]
url = "https://gdrive.test/mcp"
auth = { type = "oauth", client_id = "b", client_secret = "${secret:google_client_secret}" }

[connectors.github]
url = "https://github.test/mcp"
headers = { Authorization = "Bearer ${secret:github_token}", X-User = "${env:USER}" }

[connectors.fs]
command = "npx"
env = { TOKEN = "${secret:embed_key}" }
`
	c, err := config.Parse([]byte(src), options())

	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := []string{"embed_key", "github_token", "google_client_secret"}

	if diff := cmp.Diff(want, c.SecretRefs()); diff != "" {
		t.Errorf("SecretRefs (-want +got):\n%s", diff)
	}

	wantUsers := map[string][]string{
		"embed_key":            {"connectors.fs", "embedding.api_key"},
		"github_token":         {"connectors.github"},
		"google_client_secret": {"connectors.gdrive", "connectors.gmail"},
	}

	if diff := cmp.Diff(wantUsers, c.SecretUsers()); diff != "" {
		t.Errorf("SecretUsers (-want +got):\n%s", diff)
	}

	// SecretInUse names a user other than the connector (wayfindr-eadp).
	for _, tt := range []struct {
		secret, connector, want string
	}{
		{"github_token", "github", ""},
		{"github_token", "git", "connectors.github"},
		{"embed_key", "fs", "embedding.api_key"},
		{"google_client_secret", "gmail", "connectors.gdrive"},
		{"nobody_uses_this", "x", ""},
	} {
		if got, ok := c.SecretInUse(tt.secret, tt.connector); got != tt.want || ok != (tt.want != "") {
			t.Errorf("SecretInUse(%q, %q) = %q, %v; want %q", tt.secret, tt.connector, got, ok, tt.want)
		}
	}

	tests := []struct {
		name   string
		remove []string
		want   []string
	}{
		{"a shared secret stays", []string{"gmail"}, nil},
		{"the last user goes", []string{"gmail", "gdrive"}, []string{"google_client_secret"}},
		{"embedding still uses it", []string{"fs", "github"}, []string{"github_token"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := []byte(src)

			for _, name := range tt.remove {
				if out, _, err = config.RemoveConnector(out, name, options()); err != nil {
					t.Fatalf("RemoveConnector %s: %v", name, err)
				}
			}

			after, err := config.Parse(out, options())

			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if diff := cmp.Diff(tt.want, config.OrphanedSecrets(c, after)); diff != "" {
				t.Errorf("OrphanedSecrets (-want +got):\n%s", diff)
			}
		})
	}
}
