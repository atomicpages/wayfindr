package cli

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// editConfig adds to connectorConfig two Google connectors sharing a
// client secret, a github with a secret header, and a stdio connector.
const editConfig = `
# Gmail, with a hand-named secret.
[connectors.gmail]
url = "https://gmailmcp.googleapis.com/mcp/v1"
[connectors.gmail.auth]
type = "oauth"
client_id = "123.apps.googleusercontent.com"
client_secret = "${secret:google_client_secret}"
[connectors.gmail.auth.authorize_params]
access_type = "offline"

[connectors.gdrive]
url = "https://drivemcp.googleapis.com/mcp/v1"
[connectors.gdrive.auth]
type = "oauth"
client_id = "123.apps.googleusercontent.com"
client_secret = "${secret:google_client_secret}"

[connectors.github]
url = "https://api.githubcopilot.com/mcp/"
[connectors.github.headers]
Authorization = "${secret:github_authorization}"

[connectors.fs]
command = "ls"
`

// editHost is a host whose config is connectorConfig plus editConfig, on a
// daemon that stores st's secrets and revokes through creds, where every
// connector is ready with 12 tools. Each connector entered ready after the
// edit, so the command reports it at once. It returns the config file's
// path and content.
func editHost(t *testing.T, st *fakeStore, creds *fakeCredentials) (Host, string, string) {
	t.Helper()
	since := time.Now().Add(time.Hour)
	var states []upstream.ConnectorStatus
	st.tools = map[string]int{}

	for _, c := range []string{"fs", "gdrive", "github", "gmail", "linear"} {
		states = append(states, upstream.ConnectorStatus{Connector: c, State: upstream.StateReady, Since: since})
		st.tools[c] = 12
	}

	addr := adminDaemonOver(t, &fakeUpstreams{states: states}, st, admin.ConfigState{}, func(o *admin.Options) {
		o.Credentials = creds
	})
	host, path, before := connectorHost(t, addr)
	content := before + editConfig

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return host, path, content
}

func TestConnectorEdit(t *testing.T) {
	for _, tt := range []struct {
		name  string
		args  []string
		stdin string
		// edit turns the file before into the one wanted, by replacing
		// old with new.
		old, new   string
		wantOut    string
		wantStored map[string]string
		wantGone   []string
		wantRevoke []string
	}{
		{
			name:    "a new map key leaves the rest of the file as it was",
			args:    []string{"gmail", "--authorize-param", "prompt=consent"},
			old:     "access_type = \"offline\"\n",
			new:     "access_type = \"offline\"\nprompt = \"consent\"\n",
			wantOut: "Updated gmail: auth.authorize_params.prompt.\nThis applies at the next sign-in.\ngmail is ready (12 tools).\n",
		},
		{
			name:    "unset, written as in the auth table",
			args:    []string{"gmail", "--unset", "authorize_params.access_type"},
			old:     "[connectors.gmail.auth.authorize_params]\naccess_type = \"offline\"\n",
			new:     "[connectors.gmail.auth.authorize_params]\n",
			wantOut: "Updated gmail: auth.authorize_params.access_type.\nThis applies at the next sign-in.\ngmail is ready (12 tools).\n",
		},
		{
			name:    "an inline auth table stays inline",
			args:    []string{"linear", "--scope", "read", "--scope", "write"},
			old:     `auth = { type = "oauth" }`,
			new:     `auth = { type = "oauth", scopes = ["read", "write"] }`,
			wantOut: "Updated linear: auth.scopes.\nThis applies at the next sign-in.\nlinear is ready (12 tools).\n",
		},
		{
			name:    "a tls key creates the tls table",
			args:    []string{"linear", "--ca-file", "/corp/ca.pem"},
			old:     `auth = { type = "oauth" }` + "\n",
			new:     `auth = { type = "oauth" }` + "\n\n[connectors.linear.tls]\nca_file = \"/corp/ca.pem\"\n",
			wantOut: "Updated linear: tls.ca_file.\nlinear is ready (12 tools).\n",
		},
		{
			name:  "a client secret needs the client_id the edit sets",
			args:  []string{"linear", "--client-id", "abc", "--client-secret"},
			stdin: "s3cret\n",
			old:   `auth = { type = "oauth" }`,
			new:   `auth = { type = "oauth", client_id = "abc", client_secret = "${secret:linear_client_secret}" }`,
			wantOut: "Stored secret linear_client_secret.\n" +
				"Updated linear: auth.client_id, auth.client_secret.\nlinear is ready (12 tools).\n",
			wantStored: map[string]string{"linear_client_secret": "s3cret"},
		},
		{
			name:  "a shared client secret is stored under its name, for both",
			args:  []string{"gmail", "--client-secret"},
			stdin: "s3cret\n",
			wantOut: "google_client_secret is also used by gdrive; it changes for both.\n" +
				"Stored secret google_client_secret.\ngmail is ready (12 tools).\n",
			wantStored: map[string]string{"google_client_secret": "s3cret"},
		},
		{
			name:     "a secret the edit leaves unused is deleted",
			args:     []string{"github", "--header", "Authorization=Bearer-x"},
			old:      `Authorization = "${secret:github_authorization}"`,
			new:      `Authorization = "Bearer-x"`,
			wantOut:  "Updated github: headers.Authorization.\nDeleted its secret github_authorization.\ngithub is ready (12 tools).\n",
			wantGone: []string{"github_authorization"},
		},
		{
			name: "dropping the client_id deletes the manual client's credential",
			args: []string{"gmail", "--unset", "auth.client_id", "--unset", "auth.client_secret"},
			old: "client_id = \"123.apps.googleusercontent.com\"\nclient_secret = \"${secret:google_client_secret}\"\n" +
				"[connectors.gmail.auth.authorize_params]",
			new:        "[connectors.gmail.auth.authorize_params]",
			wantOut:    "Updated gmail: auth.client_id, auth.client_secret.\nDeleted its OAuth credential.\ngmail is ready (12 tools).\n",
			wantRevoke: []string{"gmail"},
		},
		{
			name:       "an unattended grant deletes the stored credential",
			args:       []string{"gdrive", "--grant", "client_credentials", "--authorization-server", "https://accounts.google.com"},
			old:        "client_secret = \"${secret:google_client_secret}\"\n\n[connectors.github]",
			new:        "client_secret = \"${secret:google_client_secret}\"\ngrant = \"client_credentials\"\nauthorization_server = \"https://accounts.google.com\"\n\n[connectors.github]",
			wantOut:    "Updated gdrive: auth.grant, auth.authorization_server.\nDeleted its OAuth credential.\ngdrive is ready (12 tools).\n",
			wantRevoke: []string{"gdrive"},
		},
		{
			name:    "an auth flow's grant keeps the credential",
			args:    []string{"linear", "--grant", "device_code"},
			old:     `auth = { type = "oauth" }`,
			new:     `auth = { type = "oauth", grant = "device_code" }`,
			wantOut: "Updated linear: auth.grant.\nlinear is ready (12 tools).\n",
		},
		{
			name:  "a signing key is read from stdin and stored",
			args:  []string{"gdrive", "--signing-key", "--token-auth-method", "private_key_jwt"},
			stdin: testSigningKey + "\n",
			old:   "client_secret = \"${secret:google_client_secret}\"\n\n[connectors.github]",
			new: "client_secret = \"${secret:google_client_secret}\"\ntoken_endpoint_auth_method = \"private_key_jwt\"\n" +
				"\n[connectors.gdrive.auth.signing_key]\nkey = \"${secret:gdrive_signing_key}\"\n\n[connectors.github]",
			wantOut: "Stored secret gdrive_signing_key.\n" +
				"Updated gdrive: auth.token_endpoint_auth_method, auth.signing_key.key.\ngdrive is ready (12 tools).\n",
			wantStored: map[string]string{"gdrive_signing_key": testSigningKey},
		},
		{
			name: "an assertion file",
			args: []string{"linear", "--client-id", "svc", "--grant", "jwt_bearer", "--authorization-server", "https://linear.app",
				"--assertion-file", "/t"},
			old: `auth = { type = "oauth" }`,
			new: `auth = { type = "oauth", client_id = "svc", grant = "jwt_bearer", authorization_server = "https://linear.app", ` +
				`assertion = { file = "/t" } }`,
			wantOut: "Updated linear: auth.client_id, auth.grant, auth.authorization_server, auth.assertion.file.\n" +
				"Deleted its OAuth credential.\nlinear is ready (12 tools).\n",
			wantRevoke: []string{"linear"},
		},
		{
			name:       "unset auth removes the table and deletes the credential",
			args:       []string{"linear", "--unset", "auth"},
			old:        "auth = { type = \"oauth\" }\n",
			new:        "",
			wantOut:    "Updated linear: auth.\nDeleted its OAuth credential.\nlinear is ready (12 tools).\n",
			wantRevoke: []string{"linear"},
		},
		{
			name: "unset auth keeps a client secret another connector uses",
			args: []string{"gmail", "--unset", "auth"},
			old: "[connectors.gmail.auth]\ntype = \"oauth\"\nclient_id = \"123.apps.googleusercontent.com\"\n" +
				"client_secret = \"${secret:google_client_secret}\"\n[connectors.gmail.auth.authorize_params]\naccess_type = \"offline\"\n\n",
			new:        "",
			wantOut:    "Updated gmail: auth.\nDeleted its OAuth credential.\ngmail is ready (12 tools).\n",
			wantRevoke: []string{"gmail"},
		},
		{
			// Hidden on edit in favour of --unset auth, but it still works.
			name:       "no-oauth deletes the credential",
			args:       []string{"linear", "--no-oauth"},
			old:        "auth = { type = \"oauth\" }\n",
			new:        "",
			wantOut:    "Updated linear: auth.\nDeleted its OAuth credential.\nlinear is ready (12 tools).\n",
			wantRevoke: []string{"linear"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{secrets: map[string]string{"github_authorization": "x", "google_client_secret": "y"}}
			creds := &fakeCredentials{report: admin.RevokeReport{Found: true}}
			host, path, before := editHost(t, st, creds)
			host.Stdin = strings.NewReader(tt.stdin)

			stdout, stderr, code := run(t, host, append([]string{"connector", "edit"}, tt.args...)...)

			if code != 0 {
				t.Fatalf("exit code = %d; stderr: %s", code, stderr)
			}

			if diff := cmp.Diff(tt.wantOut, stdout); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}

			want := before

			if tt.old != "" {
				if !strings.Contains(before, tt.old) {
					t.Fatalf("the config has no %q", tt.old)
				}

				want = strings.Replace(before, tt.old, tt.new, 1)
			}

			if diff := cmp.Diff(want, readFile(t, path)); diff != "" {
				t.Errorf("file (-want +got):\n%s", diff)
			}

			for k, v := range tt.wantStored {
				if got := st.stored()[k]; got != v {
					t.Errorf("secret %s = %q, want %q", k, got, v)
				}
			}

			if diff := cmp.Diff(tt.wantGone, st.deletedSecrets()); diff != "" {
				t.Errorf("deleted secrets (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff(tt.wantRevoke, creds.revokedConnectors()); diff != "" {
				t.Errorf("revoked (-want +got):\n%s", diff)
			}

			if strings.Contains(stdout+stderr, "s3cret") {
				t.Error("the output echoes the secret value")
			}
		})
	}
}

// TestConnectorEditChangingNothing leaves the file alone and needs no
// daemon.
func TestConnectorEditChangingNothing(t *testing.T) {
	host, path, before := connectorHost(t, "127.0.0.1:1")
	content := before + editConfig

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := run(t, host, "connector", "edit", "gmail", "--client-id", "123.apps.googleusercontent.com", "--oauth")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if diff := cmp.Diff("gmail is already set that way.\n", stdout); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	if got := readFile(t, path); got != content {
		t.Errorf("the file changed:\n%s", got)
	}
}

func TestConnectorEditRefuses(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"unknown name", []string{"nope", "--timeout", "5m"},
			"Error: no connector is called nope; `wayfindr connector add` creates one\n"},
		{"no flags", []string{"gmail"}, "Error: nothing to change: give the flags to set or --unset\n"},
		{"url on a stdio connector", []string{"fs", "--url", "https://x.test/mcp"},
			"Error: edit can't turn a stdio connector into an HTTP one; remove it and add it again\n"},
		{"oauth on a stdio connector", []string{"fs", "--oauth"}, "Error: --oauth is only for HTTP connectors\n"},
		{"env on an HTTP connector", []string{"gmail", "--env", "A=1"}, "Error: --env is only for stdio connectors\n"},
		{"unset of a key that isn't there", []string{"gmail", "--unset", "auth.scopes"},
			"Error: gmail has no auth.scopes; leave it out, or check its spelling\n"},
		{"set and unset", []string{"gmail", "--client-id", "x", "--unset", "auth.client_id"},
			"Error: auth.client_id is both set and unset\n"},
		{"a client secret without a client_id", []string{"linear", "--client-secret"},
			"Error: --client-secret needs a client_id, and linear has none: give --client-id too; a secret belongs to a client you registered\n"},
		{"a malformed key", []string{"gmail", "--unset", "auth..scopes"},
			"Error: --unset \"auth..scopes\": want a dotted key such as auth.scopes or headers.X-Team\n"},
		{"the config's own rules", []string{"gmail", "--unset", "auth.client_id"}, "client_secret: requires client_id"},
		{"a signing key without a client_id", []string{"linear", "--signing-key"},
			"Error: --signing-key needs a client_id, and linear has none: give --client-id too; a signing key belongs to a client you registered\n"},
		{"an unattended grant with authorize_params", []string{"gmail", "--grant", "client_credentials"},
			`connectors.gmail.auth.authorize_params: is not allowed with grant = "client_credentials", which has no user`},
		{"an unknown token auth method", []string{"gmail", "--token-auth-method", "magic"},
			`connectors.gmail.auth.token_endpoint_auth_method: must be "none"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host, path, before := connectorHost(t, "127.0.0.1:1")
			content := before + editConfig

			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			stdout, stderr, code := run(t, host, append([]string{"connector", "edit"}, tt.args...)...)

			if code == 0 {
				t.Fatal("exit code = 0, want non-zero")
			}

			if !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantErr)
			}

			if stdout != "" {
				t.Errorf("stdout = %q, want nothing: it fails before any output", stdout)
			}

			if got := readFile(t, path); got != content {
				t.Errorf("the file changed:\n%s", got)
			}
		})
	}
}

// TestConnectorAddWithAStaleDaemon: a daemon of another version that
// rejects the new config gets the restart hint; one of this version
// doesn't.
func TestConnectorAddWithAStaleDaemon(t *testing.T) {
	const reject = "invalid config: connectors.fs: unknown key"
	hint := func(daemon string) string {
		return "The running daemon is " + daemon + ", not this command's " + version +
			". Restart it with `wayfindr stop`, then `wayfindr start`; fs is saved in the config file and loads then."
	}

	for _, tt := range []struct {
		name     string
		version  string
		wantHint string
	}{
		{"another version", "v0.9.0", hint("v0.9.0")},
		{"from before Status had a version", "", hint("an older version")},
		{"same", version, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The first status, before the write, has no config error.
			var calls atomic.Int32
			cfg := func() admin.ConfigState {
				if calls.Add(1) == 1 {
					return admin.ConfigState{}
				}

				return admin.ConfigState{ConfigError: reject}
			}
			addr := adminDaemonOver(t, &fakeUpstreams{}, &fakeStore{}, admin.ConfigState{}, func(o *admin.Options) {
				o.Version = tt.version
				o.Config = cfg
			})
			host, _, _ := connectorHost(t, addr)

			_, stderr, code := run(t, host, "connector", "add", "fs", "--", "ls")

			if code == 0 {
				t.Fatal("exit code = 0, want non-zero")
			}

			if !strings.Contains(stderr, reject) {
				t.Errorf("stderr = %q, want the daemon's config error", stderr)
			}

			if got := strings.Contains(stderr, "The running daemon is"); got != (tt.wantHint != "") {
				t.Errorf("stderr = %q; has a restart hint = %v, want %v", stderr, got, tt.wantHint != "")
			}

			if tt.wantHint != "" && !strings.Contains(stderr, tt.wantHint) {
				t.Errorf("stderr = %q, want the hint %q", stderr, tt.wantHint)
			}
		})
	}
}

// TestConnectorEditHidesNoOAuth: --unset auth replaces --no-oauth on edit,
// so edit's help leaves it out; add's help, where it means "don't ask the
// server", keeps it.
func TestConnectorEditHidesNoOAuth(t *testing.T) {
	host, _ := testHost(t)

	edit, _, code := run(t, host, "connector", "edit", "--help")

	if code != 0 {
		t.Fatalf("edit --help: exit code = %d", code)
	}

	if strings.Contains(edit, "--no-oauth") {
		t.Errorf("edit --help shows --no-oauth:\n%s", edit)
	}

	if !strings.Contains(edit, "auth removes the auth table and deletes the stored OAuth credential") {
		t.Errorf("edit --help doesn't say what --unset auth does:\n%s", edit)
	}

	add, _, code := run(t, host, "connector", "add", "--help")

	if code != 0 {
		t.Fatalf("add --help: exit code = %d", code)
	}

	if !strings.Contains(add, "--no-oauth") {
		t.Errorf("add --help doesn't show --no-oauth:\n%s", add)
	}
}

// An edit that would make up a secret name another connector uses is
// refused before any value is read (wayfindr-eadp): git with the header
// Hub_Token makes git_hub_token, which git_hub sends. Re-setting a secret
// the connector already refers to, shared or not, stays allowed (see the
// shared client secret case of TestConnectorEdit).
func TestConnectorEditRefusesASecretNameInUse(t *testing.T) {
	st := &fakeStore{}
	addr := adminDaemonOver(t, &fakeUpstreams{}, st, admin.ConfigState{})
	host, path, before := connectorHost(t, addr)
	content := before + `
[connectors.git]
url = "https://git.test/mcp"

[connectors.git_hub]
url = "https://hub.test/mcp"
headers = { Token = "${secret:git_hub_token}" }
`

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	host.Stdin = strings.NewReader("s3cret\n")

	_, stderr, code := run(t, host, "connector", "edit", "git", "--secret-header", "Hub_Token")

	if code == 0 || !strings.Contains(stderr, "git_hub_token") || !strings.Contains(stderr, "connectors.git_hub") {
		t.Errorf("exit %d, stderr %q; want a refusal naming git_hub_token and connectors.git_hub", code, stderr)
	}

	if got := st.stored(); len(got) != 0 {
		t.Errorf("secrets stored = %v, want none", got)
	}

	if got := readFile(t, path); got != content {
		t.Errorf("the config file changed:\n%s", got)
	}
}
