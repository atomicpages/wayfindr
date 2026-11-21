package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// TestConnectorRemoveDryRun says what remove would do and changes nothing:
// the file stays, and nothing is revoked or deleted.
func TestConnectorRemoveDryRun(t *testing.T) {
	for _, tt := range []struct {
		name, connector string
		wantPlan        string
	}{
		{
			name:      "a credential and a secret only it uses",
			connector: "gmail",
			wantPlan: "  remove [connectors.gmail] from %s\n" +
				"  delete its OAuth credential, if one is stored\n" +
				"  delete its secret gmail_client_secret, if it is stored\n",
		},
		{
			name:      "a secret another connector uses is kept",
			connector: "slack",
			wantPlan: "  remove [connectors.slack] from %s\n" +
				"  delete its OAuth credential, if one is stored\n" +
				"  keep slack_token: connectors.slack-2 uses it\n",
		},
		{
			name:      "two secrets",
			connector: "github",
			wantPlan: "  remove [connectors.github] from %s\n" +
				"  delete its secret github_authorization, if it is stored\n" +
				"  delete its secret github_org, if it is stored\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{secrets: allSecrets()}
			creds := &fakeCredentials{report: admin.RevokeReport{Found: true}}
			host, path := removeHost(t, st, creds)
			before := readFile(t, path)

			stdout, stderr, code := run(t, host, "connector", "remove", tt.connector, "--dry-run")

			if code != 0 {
				t.Fatalf("exit code = %d; stderr: %s", code, stderr)
			}

			want := "Removing " + tt.connector + " will:\n" + strings.Replace(tt.wantPlan, "%s", path, 1) +
				"Run again without --dry-run to remove " + tt.connector + ".\n"

			if diff := cmp.Diff(want, stdout); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}

			if got := readFile(t, path); got != before {
				t.Errorf("the file changed:\n%s", got)
			}

			if got := creds.revokedConnectors(); len(got) != 0 {
				t.Errorf("revoked %v, want nothing", got)
			}

			if got := st.deletedSecrets(); len(got) != 0 {
				t.Errorf("deleted secrets %v, want none", got)
			}
		})
	}
}

// TestConnectorRemoveDryRunNeedsNoDaemon: the plan comes from the file
// alone, so a dry run works with no daemon to ask.
func TestConnectorRemoveDryRunNeedsNoDaemon(t *testing.T) {
	host, path, before := connectorHost(t, "127.0.0.1:1")

	if err := os.WriteFile(path, []byte(before+removeConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := run(t, host, "connector", "remove", "gmail", "--dry-run")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if !strings.HasSuffix(stdout, "Run again without --dry-run to remove gmail.\n") {
		t.Errorf("stdout = %q, want the dry run's last line", stdout)
	}
}

// TestConnectorRemoveOffATerminalNeedsYes shows the plan, then refuses to
// ask: nothing is removed, revoked, or deleted.
func TestConnectorRemoveOffATerminalNeedsYes(t *testing.T) {
	st := &fakeStore{secrets: allSecrets()}
	creds := &fakeCredentials{report: admin.RevokeReport{Found: true}}
	host, path := removeHost(t, st, creds)
	before := readFile(t, path)

	stdout, stderr, code := run(t, host, "connector", "remove", "gmail")

	if code == 0 {
		t.Fatal("exit code = 0, want non-zero")
	}

	if want := "Error: not a terminal, so nothing was asked or changed: pass --yes to remove it\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}

	if !strings.HasPrefix(stdout, "Removing gmail will:\n") {
		t.Errorf("stdout = %q, want the plan first", stdout)
	}

	if got := readFile(t, path); got != before {
		t.Errorf("the file changed:\n%s", got)
	}

	if got := creds.revokedConnectors(); len(got) != 0 {
		t.Errorf("revoked %v, want nothing", got)
	}

	if got := st.deletedSecrets(); len(got) != 0 {
		t.Errorf("deleted secrets %v, want none", got)
	}
}
