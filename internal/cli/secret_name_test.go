package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// A new secret's name follows the connector-name rule. One the daemon
// already knows, stored or referenced by the config, keeps a name it got
// under the older, wider rule (wayfindr-vw6h).
func TestSecretSetNameRule(t *testing.T) {
	for _, tt := range []struct {
		name    string
		secret  string
		stored  map[string]string
		cfg     admin.ConfigState
		file    string // appended to the config file
		stdin   io.Reader
		want    map[string]string
		wantErr string
	}{
		{
			name:    "a new name with capitals is refused",
			secret:  "GitHubToken",
			wantErr: "Error: secret name \"GitHubToken\" must match [a-z0-9][a-z0-9_-]{0,62}\n",
		},
		{
			name:    "a new name with a dot is refused",
			secret:  "github.token",
			wantErr: "Error: secret name \"github.token\" must match [a-z0-9][a-z0-9_-]{0,62}\n",
		},
		{
			name:   "a stored older-rule name can be set again",
			secret: "Old.Name",
			stored: map[string]string{"Old.Name": "before"},
			want:   map[string]string{"Old.Name": "v"},
		},
		{
			name:   "an older-rule name the config refers to can be set",
			secret: "Old.Name",
			cfg:    admin.ConfigState{SecretUsers: map[string][]string{"Old.Name": {"connectors.github"}}},
			file:   "[connectors.github]\nurl = \"https://example.com/mcp\"\nheaders = { Authorization = \"${secret:Old.Name}\" }\n",
			want:   map[string]string{"Old.Name": "v"},
		},
		{
			name:    "an unknown older-rule name is refused before the value is read",
			secret:  "Old.Name",
			stdin:   unreadable{t},
			wantErr: "Error: secret name \"Old.Name\" must match [a-z0-9][a-z0-9_-]{0,62}\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{secrets: tt.stored}
			host, root := testHost(t)
			writeStatusHost(t, root, adminDaemonOver(t, &fakeUpstreams{}, st, tt.cfg))
			appendConfig(t, root, tt.file)
			host.Stdin = strings.NewReader("v\n")

			if tt.stdin != nil {
				host.Stdin = tt.stdin
			}
			var stdout, stderr bytes.Buffer

			code := RunWith([]string{"secret", "set", tt.secret}, &stdout, &stderr, host)

			if diff := cmp.Diff(tt.wantErr, stderr.String()); diff != "" {
				t.Errorf("stderr (-want +got):\n%s", diff)
			}

			if ok := tt.wantErr == ""; (code == 0) != ok {
				t.Errorf("exit code = %d, want success %t", code, ok)
			}

			if diff := cmp.Diff(tt.want, st.stored()); diff != "" {
				t.Errorf("secrets stored (-want +got):\n%s", diff)
			}
		})
	}
}

// unreadable fails the test if the command reads the secret's value.
type unreadable struct{ t *testing.T }

func (u unreadable) Read([]byte) (int, error) {
	u.t.Error("the value was read before the name was refused")

	return 0, io.EOF
}

// appendConfig appends toml to the test host's config file.
func appendConfig(t *testing.T, root, toml string) {
	t.Helper()

	if toml == "" {
		return
	}

	f, err := os.OpenFile(filepath.Join(root, "config", "wayfindr", "wayfindr.toml"), os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // G304: the config file under the test's temp root

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = f.Close() }()

	if _, err := f.WriteString("\n" + toml); err != nil {
		t.Fatal(err)
	}
}

// A secret stored under an older-rule name is still listed and removed.
func TestSecretWithAnOlderRuleNameIsListedAndRemoved(t *testing.T) {
	st := &fakeStore{secrets: map[string]string{"Old.Name": "v", "github_token": "w"}}
	host, root := testHost(t)
	writeStatusHost(t, root, adminDaemonOver(t, &fakeUpstreams{}, st, admin.ConfigState{}))
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"secret", "list"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("list: exit code = %d; stderr: %s", code, stderr.String())
	}

	const table = `NAME          USED BY
Old.Name      -
github_token  -
`

	if diff := cmp.Diff(table, stdout.String()); diff != "" {
		t.Errorf("list stdout (-want +got):\n%s", diff)
	}

	stdout.Reset()

	if code := RunWith([]string{"secret", "remove", "Old.Name"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("remove: exit code = %d; stderr: %s", code, stderr.String())
	}

	if diff := cmp.Diff(map[string]string{"github_token": "w"}, st.stored()); diff != "" {
		t.Errorf("secrets left (-want +got):\n%s", diff)
	}
}
