package cli

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// connectorConfig is a config file with comments, for checking that
// `connector add` and `remove` keep the rest of the file as it was.
const connectorConfig = `# My wayfindr config.

[server]
listen = %q   # loopback only

# Linear, via OAuth.
[connectors.linear]
url  = "https://mcp.linear.app/mcp"
auth = { type = "oauth" }
`

// connectorHost is a test host whose config file is connectorConfig with
// listen as the daemon's address. It returns the config file's path and
// its content.
func connectorHost(t *testing.T, listen string) (Host, string, string) {
	t.Helper()
	host, root := testHost(t)
	writeStatusHost(t, root, listen)
	path := filepath.Join(root, "config", "wayfindr", "wayfindr.toml")
	content := strings.Replace(connectorConfig, "%q", `"`+listen+`"`, 1)

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return host, path, content
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))

	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func run(t *testing.T, host Host, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = RunWith(args, &out, &errOut, host)

	return out.String(), errOut.String(), code
}

// TestConnectorAddThenRemoveRoundTrips adds a stdio connector with
// --no-wait while no daemon runs, then removes it: the file comes back as
// it was, byte for byte.
func TestConnectorAddThenRemoveRoundTrips(t *testing.T) {
	host, path, before := connectorHost(t, "127.0.0.1:1")

	stdout, stderr, code := run(t, host, "connector", "add", "fs", "--no-wait", "--env", "DEBUG=1",
		"--", "npx", "-y", "@modelcontextprotocol/server-filesystem", "/src")

	if code != 0 {
		t.Fatalf("add: exit code = %d; stderr: %s", code, stderr)
	}

	wantOut := "Added connector fs.\nIt takes effect on the next `wayfindr start`.\n"

	if diff := cmp.Diff(wantOut, stdout); diff != "" {
		t.Errorf("add stdout (-want +got):\n%s", diff)
	}

	wantFile := before + `
[connectors.fs]
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "/src"]
[connectors.fs.env]
DEBUG = "1"
`

	if diff := cmp.Diff(wantFile, readFile(t, path)); diff != "" {
		t.Errorf("file after add (-want +got):\n%s", diff)
	}

	stdout, stderr, code = run(t, host, "connector", "rm", "fs", "-y")

	if code != 0 {
		t.Fatalf("remove: exit code = %d; stderr: %s", code, stderr)
	}

	if diff := cmp.Diff("Removed fs.\n", stdout); diff != "" {
		t.Errorf("remove stdout (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff(before, readFile(t, path)); diff != "" {
		t.Errorf("file after add and remove (-want +got):\n%s", diff)
	}
}

// TestConnectorAddStoresTheSecretAndReportsTheConnector: --secret-header
// stores the value as <name>_<header> and writes a reference to it; the
// command then waits for the connector and reports it ready.
func TestConnectorAddStoresTheSecretAndReportsTheConnector(t *testing.T) {
	st := &fakeStore{tools: map[string]int{"github": 38}}
	addr := adminDaemonOver(t, &fakeUpstreams{states: []upstream.ConnectorStatus{
		{Connector: "github", State: upstream.StateReady},
	}}, st, admin.ConfigState{Connectors: []string{"github"}})
	host, path, before := connectorHost(t, addr)
	host.Stdin = strings.NewReader("ghp_s3cret\n")

	stdout, stderr, code := run(t, host, "connector", "add", "github",
		"--url", "https://api.githubcopilot.com/mcp/", "--secret-header", "Authorization")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if diff := cmp.Diff(map[string]string{"github_authorization": "ghp_s3cret"}, st.stored()); diff != "" {
		t.Errorf("secrets the daemon stored (-want +got):\n%s", diff)
	}

	wantOut := "Stored secret github_authorization.\nAdded connector github.\ngithub is ready (38 tools).\n"

	if diff := cmp.Diff(wantOut, stdout); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	wantFile := before + `
[connectors.github]
url = "https://api.githubcopilot.com/mcp/"
[connectors.github.headers]
Authorization = "${secret:github_authorization}"
`

	if diff := cmp.Diff(wantFile, readFile(t, path)); diff != "" {
		t.Errorf("file (-want +got):\n%s", diff)
	}

	if strings.Contains(stdout+stderr, "s3cret") {
		t.Error("the output echoes the secret value")
	}
}

// TestConnectorAddWithAManualOAuthClient: --client-id, --client-secret,
// and --authorize-param write the auth table, store the secret, and ask the
// server nothing.
func TestConnectorAddWithAManualOAuthClient(t *testing.T) {
	st := &fakeStore{tools: map[string]int{"gmail": 23}}
	addr := adminDaemonOver(t, &fakeUpstreams{states: []upstream.ConnectorStatus{
		{Connector: "gmail", State: upstream.StateReady},
	}}, st, admin.ConfigState{Connectors: []string{"gmail"}})
	host, path, before := connectorHost(t, addr)
	host.Stdin = strings.NewReader("GOCSPX-s3cret\n")

	stdout, stderr, code := run(t, host, "connector", "add", "gmail",
		"--url", "https://gmailmcp.googleapis.com/mcp/v1", "--client-id", "123.apps.googleusercontent.com",
		"--client-secret", "--authorize-param", "access_type=offline", "--authorize-param", "prompt=consent")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if diff := cmp.Diff(map[string]string{"gmail_client_secret": "GOCSPX-s3cret"}, st.stored()); diff != "" { //nolint:gosec // G101: a test value, not a credential
		t.Errorf("secrets the daemon stored (-want +got):\n%s", diff)
	}

	wantOut := "Stored secret gmail_client_secret.\nAdded connector gmail.\ngmail is ready (23 tools).\n"

	if diff := cmp.Diff(wantOut, stdout); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	wantFile := before + `
[connectors.gmail]
url = "https://gmailmcp.googleapis.com/mcp/v1"
[connectors.gmail.auth]
type = "oauth"
client_id = "123.apps.googleusercontent.com"
client_secret = "${secret:gmail_client_secret}"
[connectors.gmail.auth.authorize_params]
access_type = "offline"
prompt = "consent"
`

	if diff := cmp.Diff(wantFile, readFile(t, path)); diff != "" {
		t.Errorf("file (-want +got):\n%s", diff)
	}

	if strings.Contains(stdout+stderr, "s3cret") {
		t.Error("the output echoes the secret value")
	}
}

// testSigningKey is a PEM block as --signing-key reads it from stdin. The
// key in it is not a real one: config and the CLI never parse it.
const testSigningKey = "-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIA==\n-----END PRIVATE KEY-----" //nolint:gosec // G101: a test value, not a credential

// TestConnectorAddWithAnUnattendedGrant: --grant, --token-auth-method,
// --signing-key, and --assertion-subject write the auth table and its
// subtables, store the PEM key piped on stdin, and ask the server nothing.
func TestConnectorAddWithAnUnattendedGrant(t *testing.T) {
	st := &fakeStore{tools: map[string]int{"svc": 3}}
	addr := adminDaemonOver(t, &fakeUpstreams{states: []upstream.ConnectorStatus{
		{Connector: "svc", State: upstream.StateReady},
	}}, st, admin.ConfigState{Connectors: []string{"svc"}})
	host, path, before := connectorHost(t, addr)
	host.Stdin = strings.NewReader(testSigningKey + "\n")

	stdout, stderr, code := run(t, host, "connector", "add", "svc", "--url", "https://svc.test/mcp", "--client-id", "svc",
		"--grant", "jwt_bearer", "--authorization-server", "https://svc.test", "--token-auth-method", "private_key_jwt",
		"--signing-key", "--assertion-subject", "svc@example.com")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if diff := cmp.Diff(map[string]string{"svc_signing_key": testSigningKey}, st.stored()); diff != "" {
		t.Errorf("secrets the daemon stored (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff("Stored secret svc_signing_key.\nAdded connector svc.\nsvc is ready (3 tools).\n", stdout); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	wantFile := before + `
[connectors.svc]
url = "https://svc.test/mcp"
[connectors.svc.auth]
type = "oauth"
grant = "jwt_bearer"
authorization_server = "https://svc.test"
client_id = "svc"
token_endpoint_auth_method = "private_key_jwt"
[connectors.svc.auth.signing_key]
key = "${secret:svc_signing_key}"
[connectors.svc.auth.assertion]
subject = "svc@example.com"
`

	if diff := cmp.Diff(wantFile, readFile(t, path)); diff != "" {
		t.Errorf("file (-want +got):\n%s", diff)
	}

	if strings.Contains(stdout+stderr, "MC4CAQ") {
		t.Error("the output echoes the signing key")
	}
}

// TestConnectorAddWithAnAssertionFile writes the jwt_bearer grant with a
// workload token file, and needs no secret.
func TestConnectorAddWithAnAssertionFile(t *testing.T) {
	host, path, before := connectorHost(t, "127.0.0.1:1")

	_, stderr, code := run(t, host, "connector", "add", "k8s", "--url", "https://k8s.test/mcp", "--no-wait",
		"--client-id", "svc", "--grant", "jwt_bearer", "--authorization-server", "https://k8s.test", "--assertion-file", "/var/run/secrets/tokens/mcp")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	wantFile := before + `
[connectors.k8s]
url = "https://k8s.test/mcp"
[connectors.k8s.auth]
type = "oauth"
grant = "jwt_bearer"
authorization_server = "https://k8s.test"
client_id = "svc"
[connectors.k8s.auth.assertion]
file = "/var/run/secrets/tokens/mcp"
`

	if diff := cmp.Diff(wantFile, readFile(t, path)); diff != "" {
		t.Errorf("file (-want +got):\n%s", diff)
	}
}

// TestConnectorAddDetectsOAuth: a URL connector with no headers is asked
// whether it uses OAuth, and gets the auth table when it does: when it
// answers with a 401, and when it answers without a token but publishes
// PRM, as Google's MCP servers do.
func TestConnectorAddDetectsOAuth(t *testing.T) {
	tests := []struct {
		name string
		opts []fakeas.Option
	}{
		{name: "401 challenge"},
		{name: "anonymous initialize", opts: []fakeas.Option{fakeas.WithAnonymousSession(), fakeas.WithoutGET()}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := fakeas.Start(t, tt.opts...)
			host, path, before := connectorHost(t, "127.0.0.1:1")

			stdout, stderr, code := run(t, host, "connector", "add", "acme", "--url", as.ResourceURL, "--no-wait")

			if code != 0 {
				t.Fatalf("exit code = %d; stderr: %s", code, stderr)
			}

			if !strings.HasPrefix(stdout, "Checking "+as.ResourceURL+" … it uses OAuth.\n") {
				t.Errorf("stdout = %q, want it to say the server uses OAuth", stdout)
			}

			wantFile := before + "\n[connectors.acme]\nurl = \"" + as.ResourceURL + "\"\n[connectors.acme.auth]\ntype = \"oauth\"\n"

			if diff := cmp.Diff(wantFile, readFile(t, path)); diff != "" {
				t.Errorf("file (-want +got):\n%s", diff)
			}
		})
	}
}

// TestConnectorAddProbeTrustsCAFile: the OAuth probe goes through the HTTP
// client the [tls] table makes, so a server on a private CA is reached
// with --ca-file, and the table is written with the connector; without
// it the probe cannot reach the server.
func TestConnectorAddProbeTrustsCAFile(t *testing.T) {
	as := fakeas.Start(t, fakeas.WithTLS(nil))
	caFile := filepath.Join(t.TempDir(), "ca.pem")

	if err := os.WriteFile(caFile, as.CertificatePEM, 0o600); err != nil {
		t.Fatalf("write ca_file: %v", err)
	}

	t.Run("without ca_file", func(t *testing.T) {
		host, _, _ := connectorHost(t, "127.0.0.1:1")
		stdout, stderr, code := run(t, host, "connector", "add", "acme", "--url", as.ResourceURL, "--no-wait")

		if code != 0 || !strings.HasPrefix(stdout, "Checking "+as.ResourceURL+" … could not reach it.\n") {
			t.Errorf("exit code %d, stdout %q, stderr %q; want the probe to fail to reach the server", code, stdout, stderr)
		}
	})

	t.Run("with ca_file", func(t *testing.T) {
		host, path, before := connectorHost(t, "127.0.0.1:1")
		stdout, stderr, code := run(t, host, "connector", "add", "acme", "--url", as.ResourceURL, "--ca-file", caFile, "--no-wait")

		if code != 0 {
			t.Fatalf("exit code = %d; stderr: %s", code, stderr)
		}

		if !strings.HasPrefix(stdout, "Checking "+as.ResourceURL+" … it uses OAuth.\n") {
			t.Errorf("stdout = %q, want it to say the server uses OAuth", stdout)
		}

		wantFile := before + "\n[connectors.acme]\nurl = \"" + as.ResourceURL + "\"\n[connectors.acme.auth]\ntype = \"oauth\"\n" +
			// strconv.Quote escapes a Windows path's backslashes as TOML does.
			"[connectors.acme.tls]\nca_file = " + strconv.Quote(caFile) + "\n"

		if diff := cmp.Diff(wantFile, readFile(t, path)); diff != "" {
			t.Errorf("file (-want +got):\n%s", diff)
		}
	})
}

// TestConnectorAddNeedingAuthOffATerminal reports the advice and does not
// prompt: stdin is not a terminal.
func TestConnectorAddNeedingAuthOffATerminal(t *testing.T) {
	addr := adminDaemonOver(t, &fakeUpstreams{states: []upstream.ConnectorStatus{
		{Connector: "notion", State: upstream.StateNeedsAuth, Reason: "no credential",
			Kind: upstream.KindNeedsAuth, Case: upstream.CaseNoCredential, Link: "http://127.0.0.1:7777/auth/notion"},
	}}, &fakeStore{}, admin.ConfigState{Connectors: []string{"notion"}})
	host, _, _ := connectorHost(t, addr)

	stdout, stderr, code := run(t, host, "connector", "add", "notion", "--url", "https://mcp.notion.test/mcp", "--oauth")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	want := `Added connector notion.
notion: The connector has not been authorized yet.
  Open the link to authorize it.
  http://127.0.0.1:7777/auth/notion
`

	if diff := cmp.Diff(want, stdout); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

func TestConnectorAddRefuses(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"name taken", []string{"linear", "--url", "https://x.test/mcp", "--no-oauth"},
			"Error: linear already exists; change it with `wayfindr connector edit linear`, or remove it with `wayfindr connector remove linear`\n"},
		{"bad name", []string{"My Server", "--no-wait", "--", "ls"}, "connector name must match"},
		{"neither url nor command", []string{"fs"}, "give --url <url> for an HTTP connector"},
		{"both url and command", []string{"fs", "--url", "https://x.test/mcp", "--", "ls"}, "not both"},
		{"stdio flag on http", []string{"fs", "--url", "https://x.test/mcp", "--env", "A=1"},
			"--env is only for stdio connectors"},
		{"http flag on stdio", []string{"fs", "--oauth", "--", "ls"}, "--oauth is only for HTTP connectors"},
		{"oauth both ways", []string{"fs", "--url", "https://x.test/mcp", "--oauth", "--no-oauth"}, "not both"},
		{"malformed header", []string{"fs", "--url", "https://x.test/mcp", "--header", "X"}, "want K=V"},
		{"client secret without client id", []string{"fs", "--url", "https://x.test/mcp", "--client-secret"},
			"--client-secret needs --client-id"},
		{"client secret with no-oauth", []string{"fs", "--url", "https://x.test/mcp", "--no-oauth", "--client-id", "c", "--client-secret"},
			"which --no-oauth turns off"},
		{"client secret on stdio", []string{"fs", "--client-secret", "--", "ls"}, "--client-secret is only for HTTP connectors"},
		{"authorize param with no-oauth", []string{"fs", "--url", "https://x.test/mcp", "--no-oauth", "--authorize-param", "a=b"},
			"which --no-oauth turns off"},
		{"malformed authorize param", []string{"fs", "--url", "https://x.test/mcp", "--authorize-param", "a"}, "want K=V"},
		{"authorize param overriding the protocol", []string{"fs", "--url", "https://x.test/mcp", "--no-wait", "--authorize-param", "scope=x"},
			"connectors.fs.auth.authorize_params.scope: is set by Wayfindr; use scopes"},
		{"signing key without client id", []string{"fs", "--url", "https://x.test/mcp", "--signing-key"},
			"--signing-key needs --client-id"},
		{"signing key that is not PEM", []string{"fs", "--url", "https://x.test/mcp", "--no-wait", "--client-id", "c", "--signing-key"},
			"the OAuth signing key is not a PEM block: pipe the key file on stdin, as in < key.pem"},
		{"grant with no-oauth", []string{"fs", "--url", "https://x.test/mcp", "--no-oauth", "--grant", "device_code"},
			"which --no-oauth turns off"},
		{"unknown grant", []string{"fs", "--url", "https://x.test/mcp", "--no-wait", "--grant", "password"},
			`connectors.fs.auth.grant: must be "authorization_code", "device_code", "client_credentials", or "jwt_bearer", not "password"`},
		{"unattended grant without a client", []string{"fs", "--url", "https://x.test/mcp", "--no-wait", "--grant", "client_credentials"},
			`connectors.fs.auth.client_id: is required with grant = "client_credentials"`},
		{"a tls method without a client certificate", []string{"fs", "--url", "https://x.test/mcp", "--no-wait", "--client-id", "c", "--token-auth-method", "tls_client_auth"},
			`connectors.fs.auth.token_endpoint_auth_method: token_endpoint_auth_method = "tls_client_auth" requires a client certificate`},
		{"assertion file on stdio", []string{"fs", "--assertion-file", "/t", "--", "ls"}, "--assertion-file is only for HTTP connectors"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host, path, before := connectorHost(t, "127.0.0.1:1")
			host.Stdin = strings.NewReader("not a key\n")

			_, stderr, code := run(t, host, append([]string{"connector", "add"}, tt.args...)...)

			if code == 0 {
				t.Fatal("exit code = 0, want non-zero")
			}

			if !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantErr)
			}

			if got := readFile(t, path); got != before {
				t.Errorf("the file changed:\n%s", got)
			}
		})
	}
}

// TestConnectorAddChecksTheNameBeforeAskingTheServer: a taken name fails
// before the OAuth check sends anything.
func TestConnectorAddChecksTheNameBeforeAskingTheServer(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	host, _, _ := connectorHost(t, "127.0.0.1:1")

	stdout, stderr, code := run(t, host, "connector", "add", "linear", "--url", srv.URL, "--no-wait")

	if code == 0 || !strings.Contains(stderr, "linear already exists; change it with") {
		t.Errorf("exit code %d, stderr %q; want the name-taken error", code, stderr)
	}

	if n := hits.Load(); n != 0 || stdout != "" {
		t.Errorf("the server got %d requests and stdout is %q; want none and nothing", n, stdout)
	}
}

func TestConnectorAddWithoutAConfig(t *testing.T) {
	host, root := testHost(t)

	_, stderr, code := run(t, host, "connector", "add", "fs", "--no-wait", "--", "ls")

	want := "no config at " + filepath.Join(root, "config", "wayfindr", "wayfindr.toml") + ": run `wayfindr init` first"

	if code == 0 || !strings.Contains(stderr, want) {
		t.Errorf("exit code %d, stderr %q; want an error containing %q", code, stderr, want)
	}
}

// removeConfig adds to connectorConfig a gmail with a client secret, a
// slack whose token slack-2 shares, and a github with two header secrets.
const removeConfig = `
[connectors.gmail]
url = "https://gmailmcp.googleapis.com/mcp/v1"
[connectors.gmail.auth]
type = "oauth"
client_id = "123.apps.googleusercontent.com"
client_secret = "${secret:gmail_client_secret}"

[connectors.slack]
url = "https://mcp.slack.test/mcp"
auth = { type = "oauth" }
[connectors.slack.headers]
X-Token = "${secret:slack_token}"

[connectors.slack-2]
url = "https://mcp2.slack.test/mcp"
[connectors.slack-2.headers]
X-Token = "${secret:slack_token}"

[connectors.github]
url = "https://api.githubcopilot.com/mcp/"
headers = { Authorization = "${secret:github_authorization}", X-Org = "${secret:github_org}" }
`

// removeHost is a host whose config is connectorConfig plus removeConfig,
// on a daemon that stores st's secrets and revokes through creds. It
// returns the config file's path.
func removeHost(t *testing.T, st *fakeStore, creds *fakeCredentials) (Host, string) {
	t.Helper()
	addr := adminDaemonOver(t, &fakeUpstreams{}, st, admin.ConfigState{}, func(o *admin.Options) {
		if creds != nil {
			o.Credentials = creds
		}
	})
	host, path, before := connectorHost(t, addr)

	if err := os.WriteFile(path, []byte(before+removeConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	return host, path
}

func allSecrets() map[string]string {
	return map[string]string{
		"gmail_client_secret": "a", "slack_token": "b", "github_authorization": "c", "github_org": "d",
	}
}

func TestConnectorRemove(t *testing.T) {
	t.Run("deletes the credential and the secret", func(t *testing.T) {
		st := &fakeStore{secrets: allSecrets()}
		creds := &fakeCredentials{report: admin.RevokeReport{Found: true}}
		host, path := removeHost(t, st, creds)

		stdout, stderr, code := run(t, host, "connector", "remove", "gmail", "--yes")

		if code != 0 {
			t.Fatalf("exit code = %d; stderr: %s", code, stderr)
		}

		want := "Removed gmail. Deleted its OAuth credential and its secret gmail_client_secret.\n"

		if diff := cmp.Diff(want, stdout); diff != "" {
			t.Errorf("stdout (-want +got):\n%s", diff)
		}

		if diff := cmp.Diff([]string{"gmail"}, creds.revokedConnectors()); diff != "" {
			t.Errorf("revoked (-want +got):\n%s", diff)
		}

		if diff := cmp.Diff([]string{"gmail_client_secret"}, st.deletedSecrets()); diff != "" {
			t.Errorf("deleted secrets (-want +got):\n%s", diff)
		}

		if strings.Contains(readFile(t, path), "gmail") {
			t.Errorf("the file still names gmail:\n%s", readFile(t, path))
		}
	})

	t.Run("keeps and names a shared secret", func(t *testing.T) {
		st := &fakeStore{secrets: allSecrets()}
		host, _ := removeHost(t, st, &fakeCredentials{report: admin.RevokeReport{Found: true}})

		stdout, stderr, code := run(t, host, "connector", "remove", "slack", "--yes")

		if code != 0 {
			t.Fatalf("exit code = %d; stderr: %s", code, stderr)
		}

		want := "Removed slack. Deleted its OAuth credential.\nKept slack_token: connectors.slack-2 uses it.\n"

		if diff := cmp.Diff(want, stdout); diff != "" {
			t.Errorf("stdout (-want +got):\n%s", diff)
		}

		if got := st.deletedSecrets(); len(got) != 0 {
			t.Errorf("deleted secrets %v, want none", got)
		}
	})

	t.Run("nothing stored is nothing deleted", func(t *testing.T) {
		host, _ := removeHost(t, &fakeStore{}, &fakeCredentials{})

		stdout, stderr, code := run(t, host, "connector", "remove", "github", "--yes")

		if code != 0 {
			t.Fatalf("exit code = %d; stderr: %s", code, stderr)
		}

		if diff := cmp.Diff("Removed github.\n", stdout); diff != "" {
			t.Errorf("stdout (-want +got):\n%s", diff)
		}
	})

	t.Run("a failed delete names what is left", func(t *testing.T) {
		st := &fakeStore{secrets: allSecrets(), deleteErr: errors.New("disk full")}
		// No credentials: the daemon answers a revoke with 501.
		host, path := removeHost(t, st, nil)

		stdout, stderr, code := run(t, host, "connector", "remove", "gmail", "--yes")

		if code == 0 {
			t.Fatal("exit code = 0, want non-zero")
		}

		if diff := cmp.Diff("Removed gmail.\n", stdout); diff != "" {
			t.Errorf("stdout (-want +got):\n%s", diff)
		}

		want := "Error: could not delete everything gmail owned: revoking credentials is not available\n" +
			"To finish, run `wayfindr auth logout gmail` and `wayfindr secret remove gmail_client_secret`\n"

		if diff := cmp.Diff(want, stderr); diff != "" {
			t.Errorf("stderr (-want +got):\n%s", diff)
		}

		if strings.Contains(readFile(t, path), "gmail") {
			t.Error("the file still names gmail; the edit comes first")
		}
	})

	t.Run("a daemon that can't start names what is left", func(t *testing.T) {
		host, path, before := connectorHost(t, "127.0.0.1:1")

		if err := os.WriteFile(path, []byte(before+removeConfig), 0o600); err != nil {
			t.Fatal(err)
		}

		stdout, stderr, code := run(t, host, "connector", "remove", "github", "--yes")

		if code == 0 {
			t.Fatal("exit code = 0, want non-zero")
		}

		if diff := cmp.Diff("Removed github.\n", stdout); diff != "" {
			t.Errorf("stdout (-want +got):\n%s", diff)
		}

		if want := "To finish, run `wayfindr secret remove github_authorization` and `wayfindr secret remove github_org`\n"; !strings.HasSuffix(stderr, want) {
			t.Errorf("stderr = %q, want it to end with %q", stderr, want)
		}
	})

	t.Run("not found", func(t *testing.T) {
		host, _, _ := connectorHost(t, "127.0.0.1:1")

		_, stderr, code := run(t, host, "connector", "remove", "fs")

		if want := "Error: no connector is called fs; `wayfindr status` lists them\n"; code == 0 || stderr != want {
			t.Errorf("exit code %d, stderr %q; want %q", code, stderr, want)
		}
	})
}

// Two secret keys that map to one secret name are refused before anything
// is read or written, so the second value can't silently replace the first.
func TestConnectorAddRefusesTwoKeysWithOneSecretName(t *testing.T) {
	long := strings.Repeat("a", 63)

	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "keys that differ only in case",
			args: []string{"connector", "add", "fs", "--no-wait", "--secret-env", "API_KEY", "--secret-env", "api_key", "--", "server"},
			want: "Error: the environment variable API_KEY and the environment variable api_key would both be stored as the secret fs_api_key: " +
				"use keys that differ in more than case and punctuation, or a shorter connector name\n",
		},
		{
			name: "a connector name that fills the 63 bytes",
			args: []string{"connector", "add", long, "--no-wait", "--url", "https://example.com/mcp",
				"--secret-header", "Authorization", "--secret-header", "X-Api-Key"},
			want: "Error: the header Authorization and the header X-Api-Key would both be stored as the secret " + long + ": " +
				"use keys that differ in more than case and punctuation, or a shorter connector name\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host, path, before := connectorHost(t, "127.0.0.1:1")
			host.Stdin = unreadable{t}

			_, stderr, code := run(t, host, tt.args...)

			if code != 1 {
				t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr)
			}

			if diff := cmp.Diff(tt.want, stderr); diff != "" {
				t.Errorf("stderr (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff(before, readFile(t, path)); diff != "" {
				t.Errorf("file changed (-want +got):\n%s", diff)
			}
		})
	}
}

// A new connector's secret name can't be one another connector or
// [embedding] already uses (wayfindr-eadp): google with DRIVE_TOKEN and
// google_drive with TOKEN both make google_drive_token, and storing the
// second value would silently change what the first sends. The add is
// refused before any value is read or stored, and the file is unchanged.
func TestConnectorAddRefusesASecretNameInUse(t *testing.T) {
	const others = `
[connectors.google_drive]
command = "npx"
args = ["drive-mcp"]
env = { TOKEN = "${secret:google_drive_token}" }

[embedding]
base_url = "http://127.0.0.1:11434/v1"
model = "nomic-embed-text"
dimensions = 768
api_key = "${secret:notes_key}"
`

	for _, tt := range []struct{ name, key, secret, user string }{
		{"google", "DRIVE_TOKEN", "google_drive_token", "connectors.google_drive"},
		{"notes", "KEY", "notes_key", "embedding.api_key"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{}
			addr := adminDaemonOver(t, &fakeUpstreams{}, st, admin.ConfigState{})
			host, path, _ := connectorHost(t, addr)
			before := readFile(t, path) + others

			if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
				t.Fatal(err)
			}

			host.Stdin = strings.NewReader("s3cret\n")

			_, stderr, code := run(t, host, "connector", "add", tt.name, "--secret-env", tt.key, "--", "npx", "pkg")

			if code == 0 || !strings.Contains(stderr, tt.secret) || !strings.Contains(stderr, tt.user) {
				t.Errorf("exit %d, stderr %q; want a refusal naming %s and %s", code, stderr, tt.secret, tt.user)
			}

			if got := st.stored(); len(got) != 0 {
				t.Errorf("secrets stored = %v, want none", got)
			}

			if got := readFile(t, path); got != before {
				t.Errorf("the config file changed:\n%s", got)
			}
		})
	}
}
