package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// listConfig adds to connectorConfig's linear a stdio connector, an HTTP
// one with a secret header, one with a plain header over SSE, and one whose
// URL has a query.
const listConfig = `
[connectors.fs]
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "/src"]

[connectors.github]
url = "https://api.githubcopilot.com/mcp/"
headers = { Authorization = "Bearer ${secret:github_token}" }

[connectors.docs]
url = "https://docs.example.com/sse"
transport = "sse"
headers = { X-Team = "web" }

[connectors.search]
url = "https://search.example.com/mcp?key=hunter2"
`

// listHost is a host whose config is connectorConfig plus extra, with no
// daemon: list reads the file only.
func listHost(t *testing.T, extra string) (Host, string) {
	t.Helper()
	host, path, before := connectorHost(t, "127.0.0.1:1")

	if err := os.WriteFile(path, []byte(before+extra), 0o600); err != nil {
		t.Fatal(err)
	}

	return host, path
}

func TestConnectorList(t *testing.T) {
	host, _ := listHost(t, listConfig)

	for _, args := range [][]string{{"connector", "list"}, {"connector", "ls"}} {
		stdout, stderr, code := run(t, host, args...)

		if code != 0 {
			t.Fatalf("%v: exit code = %d; stderr: %s", args, code, stderr)
		}

		want := "" +
			"NAME    KIND   TARGET                                               AUTH\n" +
			"docs    http   https://docs.example.com/sse (sse)                   -\n" +
			"fs      stdio  npx -y @modelcontextprotocol/server-filesystem /src  -\n" +
			"github  http   https://api.githubcopilot.com/mcp/                   header (secret)\n" +
			"linear  http   https://mcp.linear.app/mcp                           oauth\n" +
			"search  http   https://search.example.com/mcp?REDACTED              -\n"

		if diff := cmp.Diff(want, stdout); diff != "" {
			t.Errorf("%v: stdout (-want +got):\n%s", args, diff)
		}
	}
}

func TestConnectorListJSON(t *testing.T) {
	host, _ := listHost(t, listConfig)

	stdout, stderr, code := run(t, host, "connector", "list", "--json")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	want := `{
  "connectors": [
    {
      "name": "docs",
      "kind": "http",
      "target": "https://docs.example.com/sse (sse)",
      "auth": "none"
    },
    {
      "name": "fs",
      "kind": "stdio",
      "target": "npx -y @modelcontextprotocol/server-filesystem /src",
      "auth": "none"
    },
    {
      "name": "github",
      "kind": "http",
      "target": "https://api.githubcopilot.com/mcp/",
      "auth": "secret_header"
    },
    {
      "name": "linear",
      "kind": "http",
      "target": "https://mcp.linear.app/mcp",
      "auth": "oauth"
    },
    {
      "name": "search",
      "kind": "http",
      "target": "https://search.example.com/mcp?REDACTED",
      "auth": "none"
    }
  ]
}
`

	if diff := cmp.Diff(want, stdout); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

func TestConnectorListEmpty(t *testing.T) {
	host, path, _ := connectorHost(t, "127.0.0.1:1")

	if err := os.WriteFile(path, []byte("[server]\nlisten = \"127.0.0.1:1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := run(t, host, "connector", "list")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if diff := cmp.Diff("No connectors are configured.\n", stdout); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	stdout, _, _ = run(t, host, "connector", "list", "--json")

	if diff := cmp.Diff("{\n  \"connectors\": []\n}\n", stdout); diff != "" {
		t.Errorf("--json stdout (-want +got):\n%s", diff)
	}
}

func TestConnectorListInvalidConfig(t *testing.T) {
	host, path := listHost(t, "\n[connectors.bad]\nurl = \"https://x.test/mcp\"\ncolour = \"red\"\n")

	stdout, stderr, code := run(t, host, "connector", "list")

	if code == 0 {
		t.Fatal("exit code = 0, want non-zero")
	}

	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}

	for _, want := range []string{"connectors.bad.colour: unknown key", "Fix the config file " + path + "."} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
}
