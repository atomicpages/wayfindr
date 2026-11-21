package config_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
)

// editBase is a real-looking config: comments, blank lines, a subtable,
// and connectors around the ones the tests edit.
const editBase = `# Wayfindr configuration.

[server]
listen = "127.0.0.1:7777"   # loopback only

# Linear, via OAuth.
[connectors.linear]
url  = "https://mcp.linear.app/mcp"
auth = { type = "oauth" }

[connectors.fs]
command = "npx"
args    = ["-y", "server-filesystem"]

[connectors.fs.env]
DEBUG = "1"

# GitHub, with a stored token.
[connectors.github]
url = "https://api.githubcopilot.com/mcp/"
headers = { Authorization = "${secret:github_authorization}" }
`

func timeout(d time.Duration) *config.Duration { return &config.Duration{Duration: d} }

func TestAppendConnector(t *testing.T) {
	tests := []struct {
		name string
		src  string
		c    config.NewConnector
		want string
	}{
		{
			name: "stdio after a commented file",
			src:  editBase,
			c: config.NewConnector{
				Name:    "notes",
				Command: "npx",
				Args:    []config.Template{"-y", "notes server"},
				Env:     map[string]config.Template{"TOKEN": "${secret:notes_token}"},
			},
			want: editBase + `
[connectors.notes]
command = "npx"
args = ["-y", "notes server"]
[connectors.notes.env]
TOKEN = "${secret:notes_token}"
`,
		},
		{
			name: "http with every key add sets",
			src:  "[server]\nlisten = \"127.0.0.1:7777\"\n",
			c: config.NewConnector{
				Name:      "acme",
				URL:       `https://mcp.acme.test/"sse"`,
				Headers:   map[string]config.Template{"X-Team": "core"},
				Transport: config.TransportSSE,
				Auth: &config.NewAuth{ //nolint:gosec // G101: a reference, not a credential
					Type: config.AuthTypeOAuth, ClientID: "abc", ClientSecret: "${secret:acme_client_secret}",
					Scopes: []string{"read"}, AuthorizeParams: map[string]string{"access_type": "offline"},
				},
				Timeout: timeout(90 * time.Second),
			},
			want: "[server]\nlisten = \"127.0.0.1:7777\"\n" + `
[connectors.acme]
url = "https://mcp.acme.test/\"sse\""
transport = "sse"
timeout = "1m30s"
[connectors.acme.headers]
X-Team = "core"
[connectors.acme.auth]
type = "oauth"
client_id = "abc"
client_secret = "${secret:acme_client_secret}"
scopes = ["read"]
[connectors.acme.auth.authorize_params]
access_type = "offline"
`,
		},
		{
			name: "an unattended grant with its signing key and assertion",
			src:  "",
			c: config.NewConnector{
				Name: "svc",
				URL:  "https://svc.test/mcp",
				Auth: &config.NewAuth{
					Type: config.AuthTypeOAuth, Grant: config.GrantJWTBearer, ClientID: "svc",
					AuthorizationServer:     "https://as.example",
					TokenEndpointAuthMethod: config.AuthMethodPrivateKeyJWT,
					SigningKey:              &config.SigningKey{Key: "${secret:svc_signing_key}"},
					Assertion:               &config.Assertion{Subject: "svc@example.com"},
				},
			},
			want: `[connectors.svc]
url = "https://svc.test/mcp"
[connectors.svc.auth]
type = "oauth"
grant = "jwt_bearer"
authorization_server = "https://as.example"
client_id = "svc"
token_endpoint_auth_method = "private_key_jwt"
[connectors.svc.auth.signing_key]
key = "${secret:svc_signing_key}"
[connectors.svc.auth.assertion]
subject = "svc@example.com"
`,
		},
		{
			name: "file without a final newline",
			src:  "[server]\nlisten = \"127.0.0.1:7777\"",
			c:    config.NewConnector{Name: "fs", Command: "ls"},
			want: "[server]\nlisten = \"127.0.0.1:7777\"\n\n[connectors.fs]\ncommand = \"ls\"\n",
		},
		{
			name: "empty file",
			src:  "",
			c:    config.NewConnector{Name: "fs", Command: "ls"},
			want: "[connectors.fs]\ncommand = \"ls\"\n",
		},
		{
			name: "next to dotted keys under [connectors]",
			src:  "[connectors]\nfs.command = \"ls\"\n",
			c:    config.NewConnector{Name: "gh", URL: "https://gh.test/mcp"},
			want: "[connectors]\nfs.command = \"ls\"\n\n[connectors.gh]\nurl = \"https://gh.test/mcp\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.AppendConnector([]byte(tt.src), tt.c, options())

			if err != nil {
				t.Fatalf("AppendConnector: %v", err)
			}

			if diff := cmp.Diff(tt.want, string(got)); diff != "" {
				t.Errorf("file (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAppendConnectorKeepsOAuthDefaults: an added OAuth connector gets
// pkce, validate_issuer and resource_indicator on, as an absent key does
// (ADR 0007), not the false a zero bool would write, and the default grant
// and dpop mode.
func TestAppendConnectorKeepsOAuthDefaults(t *testing.T) {
	out, err := config.AppendConnector([]byte(editBase), config.NewConnector{
		Name: "notion",
		URL:  "https://mcp.notion.com/mcp",
		Auth: &config.NewAuth{Type: config.AuthTypeOAuth},
	}, options())

	if err != nil {
		t.Fatalf("AppendConnector: %v", err)
	}

	got := mustParse(t, string(out)).Connectors["notion"]
	want := config.Connector{
		Name: "notion",
		URL:  "https://mcp.notion.com/mcp",
		Auth: &config.Auth{
			Type: "oauth", PKCE: true, ValidateIssuer: true, ResourceIndicator: true,
			Grant: config.GrantAuthorizationCode, DPoP: config.DPoPAuto,
		},
		Timeout: dur(config.DefaultConnectorTimeout),
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("connector (-want +got):\n%s", diff)
	}
}

func TestAppendConnectorRefuses(t *testing.T) {
	tests := []struct {
		name string
		src  string
		c    config.NewConnector
		want config.Case
	}{
		{
			name: "name taken",
			src:  editBase,
			c:    config.NewConnector{Name: "fs", Command: "ls"},
			want: config.CaseConnectorExists,
		},
		{
			name: "file does not parse",
			src:  "[server\n",
			c:    config.NewConnector{Name: "fs", Command: "ls"},
			want: config.CaseUnparsable,
		},
		{
			name: "inline connectors table",
			src:  "connectors = { fs = { command = \"ls\" } }\n",
			c:    config.NewConnector{Name: "gh", URL: "https://gh.test/mcp"},
			want: config.CaseInlineConnectors,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.AppendConnector([]byte(tt.src), tt.c, options())
			checkEditCase(t, err, tt.want)
		})
	}
}

// checkEditCase fails t unless err is a config *Error of Case want, with
// advice to show.
func checkEditCase(t *testing.T, err error, want config.Case) {
	t.Helper()
	e, ok := config.AsError(err)

	if !ok {
		t.Fatalf("err = %v, want a config *Error of Case %s", err, want)
	}

	if e.Case != want {
		t.Errorf("Case = %s, want %s (err: %v)", e.Case, want, err)
	}

	if a := config.Advise(e); a.Summary == "" || a.Action == "" {
		t.Errorf("Advise(%s) = %+v, want a summary and an action", e.Case, a)
	}
}

// TestAppendConnectorBadName: the name is checked by the same rule as a
// hand-written table.
func TestAppendConnectorBadName(t *testing.T) {
	_, err := config.AppendConnector([]byte(editBase), config.NewConnector{Name: "My Server", Command: "ls"}, options())

	var ve *config.ValidationError

	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a *ValidationError", err)
	}
}

func TestRemoveConnector(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		connector string
		want      string
	}{
		{
			name:      "with its comment, keeping the next one's",
			src:       editBase,
			connector: "linear",
			want: `# Wayfindr configuration.

[server]
listen = "127.0.0.1:7777"   # loopback only

[connectors.fs]
command = "npx"
args    = ["-y", "server-filesystem"]

[connectors.fs.env]
DEBUG = "1"

# GitHub, with a stored token.
[connectors.github]
url = "https://api.githubcopilot.com/mcp/"
headers = { Authorization = "${secret:github_authorization}" }
`,
		},
		{
			name:      "with its subtable",
			src:       editBase,
			connector: "fs",
			want: `# Wayfindr configuration.

[server]
listen = "127.0.0.1:7777"   # loopback only

# Linear, via OAuth.
[connectors.linear]
url  = "https://mcp.linear.app/mcp"
auth = { type = "oauth" }

# GitHub, with a stored token.
[connectors.github]
url = "https://api.githubcopilot.com/mcp/"
headers = { Authorization = "${secret:github_authorization}" }
`,
		},
		{
			name:      "last in the file",
			src:       editBase,
			connector: "github",
			want: `# Wayfindr configuration.

[server]
listen = "127.0.0.1:7777"   # loopback only

# Linear, via OAuth.
[connectors.linear]
url  = "https://mcp.linear.app/mcp"
auth = { type = "oauth" }

[connectors.fs]
command = "npx"
args    = ["-y", "server-filesystem"]

[connectors.fs.env]
DEBUG = "1"
`,
		},
		{
			name:      "subtable apart from its table",
			src:       "[connectors.fs]\ncommand = \"ls\"\n\n[server]\nlisten = \"127.0.0.1:7777\"\n\n[connectors.fs.env]\nA = \"1\"\n",
			connector: "fs",
			want:      "[server]\nlisten = \"127.0.0.1:7777\"\n",
		},
		{
			name:      "quoted header key",
			src:       "[server]\nlisten = \"127.0.0.1:7777\"\n\n[ connectors . \"fs\" ]\ncommand = \"ls\"\n",
			connector: "fs",
			want:      "[server]\nlisten = \"127.0.0.1:7777\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := config.RemoveConnector([]byte(tt.src), tt.connector, options())

			if err != nil {
				t.Fatalf("RemoveConnector: %v", err)
			}

			if diff := cmp.Diff(tt.want, string(got)); diff != "" {
				t.Errorf("file (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRemoveConnectorRefuses(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		connector string
		want      config.Case
	}{
		{
			name:      "not found",
			src:       editBase,
			connector: "notes",
			want:      config.CaseConnectorNotFound,
		},
		{
			name:      "file does not parse",
			src:       editBase + "[server\n",
			connector: "fs",
			want:      config.CaseUnparsable,
		},
		{
			name:      "dotted keys",
			src:       "[connectors]\nfs.command = \"ls\"\n",
			connector: "fs",
			want:      config.CaseNotTables,
		},
		{
			name:      "inline table",
			src:       "connectors = { fs = { command = \"ls\" } }\n",
			connector: "fs",
			want:      config.CaseNotTables,
		},
		{
			name:      "also defined by dotted keys",
			src:       "[connectors]\nfs.command = \"ls\"\n\n[connectors.fs.env]\nA = \"1\"\n",
			connector: "fs",
			want:      config.CaseDefinedElsewhere,
		},
		{
			name:      "header inside a multi-line string",
			src:       "[connectors.fs]\ncommand = \"ls\"\nargs = [\"\"\"\n[connectors.other]\n\"\"\"]\n",
			connector: "fs",
			want:      config.CaseUnparsable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := config.RemoveConnector([]byte(tt.src), tt.connector, options())
			checkEditCase(t, err, tt.want)
		})
	}
}

// TestAddRemoveRoundTrip: removing what AppendConnector added gives back
// the file it started from, byte for byte.
func TestAddRemoveRoundTrip(t *testing.T) {
	for _, src := range []string{editBase, "", "[server]\nlisten = \"127.0.0.1:7777\"\n\n"} {
		added, err := config.AppendConnector([]byte(src), config.NewConnector{
			Name:    "notes",
			URL:     "https://notes.test/mcp",
			Headers: map[string]config.Template{"X-Notes-Team": "${secret:notes_team}"},
			Auth:    &config.NewAuth{Type: config.AuthTypeOAuth, Scopes: []string{"a", "b"}},
		}, options())

		if err != nil {
			t.Fatalf("AppendConnector: %v", err)
		}

		got, removed, err := config.RemoveConnector(added, "notes", options())

		if err != nil {
			t.Fatalf("RemoveConnector: %v", err)
		}

		if diff := cmp.Diff(src, string(got)); diff != "" {
			t.Errorf("round trip (-want +got):\n%s", diff)
		}

		if removed.Name != "notes" || !removed.UsesOAuth() {
			t.Errorf("removed = %+v, want notes with OAuth", removed)
		}
	}
}
