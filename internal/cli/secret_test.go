package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// secretDaemon serves a daemon on which github and linear wait on secret
// tok; setting it leaves github needing auth and linear ready. Values the
// daemon stores land in the returned store as they were sent.
func secretDaemon(t *testing.T) (string, *fakeStore) {
	t.Helper()
	st := &fakeStore{}
	addr := adminDaemonOver(t, &fakeUpstreams{
		retried: map[string][]upstream.ConnectorStatus{"tok": {
			{Connector: "github", State: upstream.StateNeedsAuth, Reason: "no credential",
				Kind: upstream.KindNeedsAuth, Case: upstream.CaseNoCredential, Link: "http://127.0.0.1:7777/auth/github"},
			{Connector: "linear", State: upstream.StateReady},
		}},
	}, st, admin.ConfigState{Connectors: []string{"github", "linear"}})

	return addr, st
}

func TestSecretSetSendsPipedStdinAndReportsTheRetriedConnectors(t *testing.T) {
	for _, tt := range []struct {
		name, stdin, want string
	}{
		{"one trailing newline is trimmed", "s3cret\n", "s3cret"},
		{"a CRLF is trimmed", "s3cret\r\n", "s3cret"},
		{"only one newline is trimmed", "s3cret\n\n", "s3cret\n"},
		{"no newline", "s3cret", "s3cret"},
		{"inner spaces are kept", " s3 cret \n", " s3 cret "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			addr, st := secretDaemon(t)
			host, root := testHost(t)
			writeStatusHost(t, root, addr)
			host.Stdin = strings.NewReader(tt.stdin)
			var stdout, stderr bytes.Buffer

			code := RunWith([]string{"secret", "set", "tok"}, &stdout, &stderr, host)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			if diff := cmp.Diff(map[string]string{"tok": tt.want}, st.stored()); diff != "" {
				t.Errorf("secrets the daemon stored (-want +got):\n%s", diff)
			}

			want := `Secret tok set.
github: The connector has not been authorized yet.
  Open the link to authorize it.
  http://127.0.0.1:7777/auth/github
linear is ready.
`

			if diff := cmp.Diff(want, stdout.String()); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}

			if strings.Contains(stdout.String()+stderr.String(), "s3") {
				t.Errorf("the output echoes the value")
			}
		})
	}
}

func TestSecretSetWithNoConnectorWaitingSaysWhenItTakesEffect(t *testing.T) {
	host, root := testHost(t)
	writeStatusHost(t, root, adminDaemonOver(t, &fakeUpstreams{}, &fakeStore{}, admin.ConfigState{}))
	host.Stdin = strings.NewReader("v\n")
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"secret", "set", "tok"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want := "Secret tok set. Connectors that use it pick it up on their next connect.\n"

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

func TestSecretSetRefusesBeforeCallingTheDaemon(t *testing.T) {
	for _, tt := range []struct {
		name, secret, stdin, wantErr string
	}{
		{"empty value", "tok", "", "empty"},
		{"only a newline", "tok", "\n", "empty"},
		{"name outside the grammar", "tok!", "v\n", `secret name "tok!" must match [a-z0-9][a-z0-9_-]{0,62}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			addr, st := secretDaemon(t)
			host, root := testHost(t)
			writeStatusHost(t, root, addr)
			host.Stdin = strings.NewReader(tt.stdin)
			var stdout, stderr bytes.Buffer

			code := RunWith([]string{"secret", "set", tt.secret}, &stdout, &stderr, host)

			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero; stdout: %s", stdout.String())
			}

			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}

			if got := st.stored(); len(got) != 0 {
				t.Errorf("the daemon stored %d secrets, want none", len(got))
			}
		})
	}
}

// The daemon reads at most 64 KiB of PUT /admin/secrets/{name} (design
// §7.7), and the value travels inside {"value":"…"}: 12 bytes of wrapper,
// plus whatever JSON escaping adds. The CLI refuses a value whose body
// would be over the limit before it sends anything, so the user sees why
// rather than the daemon's 413.
func TestSecretSetRefusesAValueWhoseBodyIsOverTheDaemonsLimit(t *testing.T) {
	for _, tt := range []struct {
		name, value string
	}{
		{"one byte over once wrapped", strings.Repeat("a", 65536-12+1)},
		{"under the limit until escaped", strings.Repeat(`"`, 40000)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			addr, st := secretDaemon(t)
			host, root := testHost(t)
			writeStatusHost(t, root, addr)
			host.Stdin = strings.NewReader(tt.value + "\n")
			var stdout, stderr bytes.Buffer

			code := RunWith([]string{"secret", "set", "tok"}, &stdout, &stderr, host)

			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero; stdout: %s", stdout.String())
			}

			// Refused by the CLI, which names the limit, not by the daemon's 413.
			if msg := stderr.String(); !strings.Contains(msg, "65536") || strings.Contains(msg, "413") {
				t.Errorf("stderr = %q, want the CLI's refusal naming the 65536-byte limit", msg)
			}

			if got := st.stored(); len(got) != 0 {
				t.Errorf("the daemon stored %d secrets, want none", len(got))
			}
		})
	}
}

// The largest value that fits: its body is exactly 64 KiB.
func TestSecretSetSendsTheLargestValueThatFits(t *testing.T) {
	value := strings.Repeat("a", 65536-12)
	addr, st := secretDaemon(t)
	host, root := testHost(t)
	writeStatusHost(t, root, addr)
	host.Stdin = strings.NewReader(value + "\n")
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"secret", "set", "tok"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if got := st.stored()["tok"]; got != value {
		t.Errorf("the daemon stored %d bytes, want the %d-byte value", len(got), len(value))
	}
}

func TestSecretSetWhenTheDaemonIsNotRunningSuggestsStartingIt(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatal(err)
	}

	addr := ln.Addr().String()

	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	host, root := testHost(t)
	writeHostFiles(t, root, addr)
	host.Stdin = strings.NewReader("v\n")
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"secret", "set", "tok"}, &stdout, &stderr, host)

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero; stdout: %s", stdout.String())
	}

	if msg := stderr.String(); !strings.Contains(msg, "daemon is not running") || !strings.Contains(msg, "`wayfindr start`") {
		t.Errorf("stderr = %q, want the not-running message", msg)
	}
}

func TestSecretRemove(t *testing.T) {
	// rm is remove's alias, kept for scripts from before the rename.
	for _, verb := range []string{"remove", "rm"} {
		t.Run(verb+" deletes the secret", func(t *testing.T) {
			st := &fakeStore{secrets: map[string]string{"tok": "v", "other": "w"}}
			host, root := testHost(t)
			writeStatusHost(t, root, adminDaemonOver(t, &fakeUpstreams{}, st, admin.ConfigState{}))
			var stdout, stderr bytes.Buffer

			code := RunWith([]string{"secret", verb, "tok"}, &stdout, &stderr, host)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			if diff := cmp.Diff("Deleted secret tok.\n", stdout.String()); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff(map[string]string{"other": "w"}, st.stored()); diff != "" {
				t.Errorf("secrets left (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("one that isn't stored is the daemon's error", func(t *testing.T) {
		host, root := testHost(t)
		writeStatusHost(t, root, adminDaemonOver(t, &fakeUpstreams{}, &fakeStore{}, admin.ConfigState{}))
		var stdout, stderr bytes.Buffer

		code := RunWith([]string{"secret", "remove", "tok"}, &stdout, &stderr, host)

		if code == 0 {
			t.Fatal("exit code = 0, want non-zero")
		}

		if diff := cmp.Diff("Error: \"tok\" has no stored secret\n", stderr.String()); diff != "" {
			t.Errorf("stderr (-want +got):\n%s", diff)
		}
	})
}

// secretListDaemon serves a daemon storing three secrets: two that the
// running config refers to and one nothing does.
func secretListDaemon(t *testing.T) string {
	t.Helper()

	return adminDaemonOver(t, &fakeUpstreams{},
		&fakeStore{secrets: map[string]string{"github_token": "a", "embed_key": "b", "old_key": "c"}},
		admin.ConfigState{SecretUsers: map[string][]string{
			"embed_key":    {"connectors.fs", "embedding.api_key"},
			"github_token": {"connectors.github"},
			"not_stored":   {"connectors.linear"},
		}})
}

func TestSecretList(t *testing.T) {
	const table = `NAME          USED BY
embed_key     connectors.fs, embedding.api_key
github_token  connectors.github
old_key       -
`

	// ls is list's alias.
	for _, verb := range []string{"list", "ls"} {
		t.Run(verb+" prints names and users, never values", func(t *testing.T) {
			host, root := testHost(t)
			writeStatusHost(t, root, secretListDaemon(t))
			var stdout, stderr bytes.Buffer

			code := RunWith([]string{"secret", verb}, &stdout, &stderr, host)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			if diff := cmp.Diff(table, stdout.String()); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("json prints the daemon's response", func(t *testing.T) {
		host, root := testHost(t)
		writeStatusHost(t, root, secretListDaemon(t))
		var stdout, stderr bytes.Buffer

		code := RunWith([]string{"secret", "list", "--json"}, &stdout, &stderr, host)

		if code != 0 {
			t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
		}

		var got admin.SecretList

		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
			t.Fatalf("stdout is not a SecretList: %v\n%s", err, stdout.String())
		}

		want := admin.SecretList{Secrets: []admin.Secret{
			{Name: "embed_key", UsedBy: []string{"connectors.fs", "embedding.api_key"}},
			{Name: "github_token", UsedBy: []string{"connectors.github"}},
			{Name: "old_key", UsedBy: []string{}},
		}}

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("--json (-want +got):\n%s", diff)
		}
	})

	t.Run("none stored", func(t *testing.T) {
		host, root := testHost(t)
		writeStatusHost(t, root, adminDaemonOver(t, &fakeUpstreams{}, &fakeStore{}, admin.ConfigState{}))
		var stdout, stderr bytes.Buffer

		code := RunWith([]string{"secret", "list"}, &stdout, &stderr, host)

		if code != 0 {
			t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
		}

		if diff := cmp.Diff("No secrets are stored.\n", stdout.String()); diff != "" {
			t.Errorf("stdout (-want +got):\n%s", diff)
		}
	})
}
