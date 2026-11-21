package clientconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/atomicpages/wayfindr/internal/clientconfig"
)

// tree writes files (path relative to root → content, with {{CWD}}
// replaced by the project directory) under a temp dir and returns Dirs with
// Home at root/home and Cwd at root/work/app, and root. {{CWD}} sits inside
// a JSON or TOML string, so its backslashes (Windows) are escaped. The Dirs
// read project files too.
func tree(t *testing.T, goos string, files map[string]string) (clientconfig.Dirs, string) {
	t.Helper()
	root := t.TempDir()
	cwd := filepath.Join(root, "work", "app")
	quotedCwd := strings.ReplaceAll(cwd, `\`, `\\`)

	for rel, content := range files {
		content = strings.ReplaceAll(content, "{{CWD}}", quotedCwd)
		path := filepath.Join(root, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return clientconfig.Dirs{
		Home:    filepath.Join(root, "home"),
		Cwd:     cwd,
		GOOS:    goos,
		Getenv:  func(string) string { return "" },
		Project: true,
	}, root
}

// Without Dirs.Project, no client reads a file in the project directory: a
// cloned repository chose those servers (wayfindr-awvh). Each one that
// exists is listed as skipped, and the user's own files are still read.
func TestReadSkipsProjectFilesWithoutProject(t *testing.T) {
	const server = `{"mcpServers": {"evil": {"command": "sh"}}, "servers": {"evil": {"command": "sh"}}}`

	for _, tt := range []struct {
		client  clientconfig.Client
		files   map[string]string
		project string
	}{
		{clientconfig.Cursor, map[string]string{"home/.cursor/mcp.json": `{"mcpServers": {"own": {"command": "npx"}}}`, "work/app/.cursor/mcp.json": server}, ".cursor/mcp.json"},
		{clientconfig.VSCode, map[string]string{"home/.config/Code/User/mcp.json": `{"servers": {"own": {"command": "npx"}}}`, "work/app/.vscode/mcp.json": server}, ".vscode/mcp.json"},
		{clientconfig.ClaudeCode, map[string]string{
			"home/.claude.json":                    `{"mcpServers": {"own": {"command": "npx"}}, "projects": {"{{CWD}}": {"enableAllProjectMcpServers": true}}}`,
			"work/app/.mcp.json":                   server,
			"work/app/.claude/settings.local.json": `{"enableAllProjectMcpServers": true}`,
		}, ".mcp.json"},
		{clientconfig.Codex, map[string]string{
			"home/.codex/config.toml":     "[mcp_servers.own]\ncommand = \"npx\"\n\n[projects.\"{{CWD}}\"]\ntrust_level = \"trusted\"\n",
			"work/app/.codex/config.toml": "[mcp_servers.evil]\ncommand = \"sh\"\n",
		}, ".codex/config.toml"},
	} {
		t.Run(string(tt.client), func(t *testing.T) {
			d, _ := tree(t, "linux", tt.files)
			d.Project = false

			f := read(t, tt.client, d)

			var names []string

			for _, s := range f.Servers {
				names = append(names, s.Name)
			}

			if diff := cmp.Diff([]string{"own"}, names); diff != "" {
				t.Errorf("servers (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff([]string{filepath.Join(d.Cwd, filepath.FromSlash(tt.project))}, f.SkippedProject); diff != "" {
				t.Errorf("SkippedProject (-want +got):\n%s", diff)
			}
		})
	}
}

func read(t *testing.T, c clientconfig.Client, d clientconfig.Dirs) clientconfig.Found {
	t.Helper()
	f, err := clientconfig.Read(c, d)

	if err != nil {
		t.Fatalf("Read(%s): %v", c, err)
	}

	return f
}

func lit(s string) clientconfig.Value { return clientconfig.Lit(s) }

func env(name string) clientconfig.Value { return clientconfig.Value{{Env: name}} }

func diffServers(t *testing.T, want, got []clientconfig.Server) {
	t.Helper()

	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("servers (-want +got):\n%s", diff)
	}
}

func TestReadClaudeCode(t *testing.T) {
	claudeJSON := `{
  "numStartups": 12,
  "mcpServers": {
    "github": {"type": "http", "url": "https://api.githubcopilot.com/mcp/",
               "headers": {"Authorization": "Bearer ${GITHUB_TOKEN}"}},
    "fs": {"command": "npx", "args": ["-y", "server-fs", "${HOME}/src"]}
  },
  "projects": {
    "{{CWD}}": {
      "mcpServers": {
        "fs": {"command": "npx", "args": ["-y", "server-fs", "."], "timeout": 90000}
      },
      "disabledMcpServers": ["sentry"],
      "enabledMcpjsonServers": ["linear", "sentry"],
      "disabledMcpjsonServers": ["rejected"]
    },
    "/elsewhere": {"mcpServers": {"other": {"command": "x"}}}
  }
}`
	mcpJSON := `{
  // project servers, checked in
  "mcpServers": {
    "sentry": {"command": "npx", "args": ["@sentry/mcp-server"],
               "env": {"SENTRY_TOKEN": "${SENTRY_TOKEN:-none}"},},
    "linear": {"type": "sse", "url": "https://mcp.linear.app/sse", "oauth": {"clientId": "abc", "callbackPort": 8080}},
    "helper": {"type": "http", "url": "https://x.example/mcp", "headersHelper": "./get-headers.sh"},
    "rejected": {"command": "evil"},
    "unasked": {"command": "maybe", "env": {"K": "Bearer sk-live-1 ${"}},
  },
}`
	d, _ := tree(t, "darwin", map[string]string{
		"home/.claude.json":  claudeJSON,
		"work/app/.mcp.json": mcpJSON,
	})
	user := filepath.Join(d.Home, ".claude.json")
	project := filepath.Join(d.Cwd, ".mcp.json")

	f := read(t, clientconfig.ClaudeCode, d)

	diffServers(t, []clientconfig.Server{
		{Name: "fs", Scope: clientconfig.ScopeUser, Source: user, Command: "npx",
			Args: []clientconfig.Value{lit("-y"), lit("server-fs"), {{Env: "HOME"}, {Lit: "/src"}}}},
		{Name: "github", Scope: clientconfig.ScopeUser, Source: user, Type: "http",
			URL:     lit("https://api.githubcopilot.com/mcp/"),
			Headers: map[string]clientconfig.Value{"Authorization": {{Lit: "Bearer "}, {Env: "GITHUB_TOKEN"}}}},
		{Name: "fs", Scope: clientconfig.ScopeLocal, Source: user, Command: "npx",
			Args: []clientconfig.Value{lit("-y"), lit("server-fs"), lit(".")}, Timeout: 90 * time.Second},
		{Name: "helper", Scope: clientconfig.ScopeProject, Source: project, Type: "http",
			URL: lit("https://x.example/mcp"), Unsupported: []string{"headersHelper"}, Unapproved: true},
		{Name: "linear", Scope: clientconfig.ScopeProject, Source: project, Type: "sse",
			URL: lit("https://mcp.linear.app/sse"), OAuth: true, OAuthClientID: "abc"},
		{Name: "rejected", Scope: clientconfig.ScopeProject, Source: project, Command: "evil", Disabled: true},
		{Name: "sentry", Scope: clientconfig.ScopeProject, Source: project, Command: "npx",
			Args:     []clientconfig.Value{lit("@sentry/mcp-server")},
			Env:      map[string]clientconfig.Value{"SENTRY_TOKEN": {{Env: "SENTRY_TOKEN", Default: "none", HasDefault: true}}},
			Disabled: true},
		// The error names the setting, never its value.
		{Name: "unasked", Scope: clientconfig.ScopeProject, Source: project, Command: "maybe",
			Env:         map[string]clientconfig.Value{"K": nil},
			Unsupported: []string{"env.K (a ${ is not closed)"}, Unapproved: true},
	}, f.Servers)

	if diff := cmp.Diff([]string{user, project}, f.Files); diff != "" {
		t.Errorf("files (-want +got):\n%s", diff)
	}
}

func TestReadClaudeCodePrefersClaudeConfigDir(t *testing.T) {
	d, root := tree(t, "linux", map[string]string{
		"home/.claude.json": `{"mcpServers": {"home": {"command": "a"}}}`,
		"cfg/.claude.json":  `{"mcpServers": {"cfg": {"command": "b"}}}`,
	})
	d.Getenv = func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return filepath.Join(root, "cfg")
		}

		return ""
	}

	f := read(t, clientconfig.ClaudeCode, d)

	diffServers(t, []clientconfig.Server{
		{Name: "cfg", Scope: clientconfig.ScopeUser, Source: filepath.Join(root, "cfg", ".claude.json"), Command: "b"},
	}, f.Servers)
}

func TestReadClaudeDesktop(t *testing.T) {
	entry := `{"mcpServers": {"fs": {"command": "npx", "args": ["${HOME}"], "env": {"K": "v"}, "cwd": "/tmp"}}}`

	for _, tt := range []struct {
		goos, rel string
		env       map[string]string
	}{
		{"darwin", "home/Library/Application Support/Claude/claude_desktop_config.json", nil},
		{"linux", "home/.config/Claude/claude_desktop_config.json", nil},
		{"linux", "xdg/Claude/claude_desktop_config.json", map[string]string{"XDG_CONFIG_HOME": "XDG"}},
		{"windows", "appdata/Claude/claude_desktop_config.json", map[string]string{"APPDATA": "APPDATA"}},
	} {
		t.Run(tt.goos+" "+tt.rel, func(t *testing.T) {
			d, root := tree(t, tt.goos, map[string]string{tt.rel: entry})
			d.Getenv = func(k string) string {
				switch tt.env[k] {
				case "XDG":
					return filepath.Join(root, "xdg")
				case "APPDATA":
					return filepath.Join(root, "appdata")
				}

				return ""
			}

			f := read(t, clientconfig.ClaudeDesktop, d)

			// Claude Desktop expands nothing, and has no cwd.
			diffServers(t, []clientconfig.Server{{
				Name: "fs", Scope: clientconfig.ScopeUser, Source: filepath.Join(root, filepath.FromSlash(tt.rel)),
				Command: "npx", Args: []clientconfig.Value{lit("${HOME}")},
				Env: map[string]clientconfig.Value{"K": lit("v")}, Unsupported: []string{"cwd"},
			}}, f.Servers)
		})
	}
}

func TestReadCursor(t *testing.T) {
	d, _ := tree(t, "darwin", map[string]string{
		"home/.cursor/mcp.json": `{
  "mcpServers": {
    "notes": {"command": "${userHome}/bin/notes", "args": ["--root", "${workspaceFolder}", "// not a comment"]},
    "api": {"url": "https://api.example/mcp", "headers": {"X-Key": "${env:API_KEY}"}, "auth": {"CLIENT_ID": "cid", "scopes": ["read"]}},
    "dotenv": {"command": "srv", "envFile": "${workspaceFolder}/.env"},
    "secretive": {"url": "https://s.example/mcp", "auth": {"CLIENT_ID": "c", "CLIENT_SECRET": "s"}},
    "weird": {"command": "srv", "args": ["${config:foo}"]}, /* trailing comma next */
  },
}`,
		"work/app/.cursor/mcp.json": `{"mcpServers": {"notes": {"type": "stdio", "command": "notes"}}}`,
	})
	user := filepath.Join(d.Home, ".cursor", "mcp.json")
	project := filepath.Join(d.Cwd, ".cursor", "mcp.json")

	f := read(t, clientconfig.Cursor, d)

	diffServers(t, []clientconfig.Server{
		{Name: "api", Scope: clientconfig.ScopeUser, Source: user, URL: lit("https://api.example/mcp"),
			Headers: map[string]clientconfig.Value{"X-Key": env("API_KEY")},
			OAuth:   true, OAuthClientID: "cid", OAuthScopes: []string{"read"}},
		{Name: "dotenv", Scope: clientconfig.ScopeUser, Source: user, Command: "srv", Unsupported: []string{"envFile"}},
		// ${userHome} is replaced as text: the rest keeps the file's slashes.
		{Name: "notes", Scope: clientconfig.ScopeUser, Source: user, Command: d.Home + "/bin/notes",
			Args: []clientconfig.Value{lit("--root"), lit(d.Cwd), lit("// not a comment")}},
		{Name: "secretive", Scope: clientconfig.ScopeUser, Source: user, URL: lit("https://s.example/mcp"),
			OAuth: true, OAuthClientID: "c", Unsupported: []string{"auth.CLIENT_SECRET"}},
		{Name: "weird", Scope: clientconfig.ScopeUser, Source: user, Command: "srv",
			Args: []clientconfig.Value{nil}, Unsupported: []string{"args (${config:…} has no equivalent in a connector)"}},
		{Name: "notes", Scope: clientconfig.ScopeProject, Source: project, Type: "stdio", Command: "notes"},
	}, f.Servers)
}

func TestReadCodex(t *testing.T) {
	d, root := tree(t, "linux", map[string]string{
		"codexhome/config.toml": `
model = "gpt-5"

[projects."{{CWD}}"]
trust_level = "trusted"

[mcp_servers.docs]
command = "npx"
args = ["-y", "docs-mcp"]
env = { DOCS_KEY = "k-123" }
startup_timeout_sec = 20
tool_timeout_sec = 120

[mcp_servers.gh]
url = "https://api.githubcopilot.com/mcp/"
bearer_token_env_var = "GITHUB_TOKEN"
http_headers = { "X-Org" = "acme" }
env_http_headers = { "X-Team" = "TEAM_ID" }

[mcp_servers.off]
command = "x"
enabled = false

[mcp_servers.here]
command = "x"
cwd = "/srv"
enabled_tools = ["a"]
`,
		"work/app/.codex/config.toml": `
[mcp_servers.local]
command = "local-mcp"
`,
	})
	codexHome := filepath.Join(root, "codexhome")
	d.Getenv = func(k string) string {
		if k == "CODEX_HOME" {
			return codexHome
		}

		return ""
	}
	user := filepath.Join(codexHome, "config.toml")

	f := read(t, clientconfig.Codex, d)

	diffServers(t, []clientconfig.Server{
		{Name: "docs", Scope: clientconfig.ScopeUser, Source: user, Command: "npx",
			Args: []clientconfig.Value{lit("-y"), lit("docs-mcp")},
			Env:  map[string]clientconfig.Value{"DOCS_KEY": lit("k-123")}, Timeout: 2 * time.Minute},
		{Name: "gh", Scope: clientconfig.ScopeUser, Source: user, URL: lit("https://api.githubcopilot.com/mcp/"),
			Headers: map[string]clientconfig.Value{
				"Authorization": {{Lit: "Bearer "}, {Env: "GITHUB_TOKEN"}},
				"X-Org":         lit("acme"),
				"X-Team":        env("TEAM_ID"),
			}},
		{Name: "here", Scope: clientconfig.ScopeUser, Source: user, Command: "x", Unsupported: []string{"cwd", "enabled_tools"}},
		{Name: "off", Scope: clientconfig.ScopeUser, Source: user, Command: "x", Disabled: true},
		{Name: "local", Scope: clientconfig.ScopeProject, Source: filepath.Join(d.Cwd, ".codex", "config.toml"), Command: "local-mcp"},
	}, f.Servers)
}

func TestReadCodexIgnoresAnUntrustedProject(t *testing.T) {
	d, _ := tree(t, "linux", map[string]string{
		"home/.codex/config.toml":     "[mcp_servers.a]\ncommand = \"a\"\n",
		"work/app/.codex/config.toml": "[mcp_servers.evil]\ncommand = \"evil\"\n",
	})

	f := read(t, clientconfig.Codex, d)

	diffServers(t, []clientconfig.Server{
		{Name: "a", Scope: clientconfig.ScopeUser, Source: filepath.Join(d.Home, ".codex", "config.toml"), Command: "a"},
	}, f.Servers)
}

func TestReadVSCode(t *testing.T) {
	d, _ := tree(t, "darwin", map[string]string{
		"work/app/.vscode/mcp.json": `{
  "inputs": [
    {"type": "promptString", "id": "api-key", "description": "API key", "password": true},
    {"type": "pickString", "id": "region", "options": ["eu", "us"]}
  ],
  "servers": {
    "perplexity": {"type": "stdio", "command": "npx", "args": ["-y", "server-perplexity"],
                   "env": {"PERPLEXITY_API_KEY": "${input:api-key}"}},
    "regional": {"type": "http", "url": "https://${input:region}.example/mcp"},
    "boxed": {"command": "srv", "sandboxEnabled": true},
    "unboxed": {"command": "srv", "sandboxEnabled": false, "dev": {"watch": "src/**"}}
  }
}`,
		"home/Library/Application Support/Code/User/mcp.json": `{"servers": {"gh": {"type": "http", "url": "https://api.githubcopilot.com/mcp/", "headers": {"Authorization": "Bearer ${env:GH}"}}}}`,
	})
	user := filepath.Join(d.Home, "Library", "Application Support", "Code", "User", "mcp.json")
	project := filepath.Join(d.Cwd, ".vscode", "mcp.json")

	f := read(t, clientconfig.VSCode, d)

	diffServers(t, []clientconfig.Server{
		{Name: "gh", Scope: clientconfig.ScopeUser, Source: user, Type: "http", URL: lit("https://api.githubcopilot.com/mcp/"),
			Headers: map[string]clientconfig.Value{"Authorization": {{Lit: "Bearer "}, {Env: "GH"}}}},
		{Name: "boxed", Scope: clientconfig.ScopeProject, Source: project, Command: "srv", Unsupported: []string{"sandboxEnabled"}},
		{Name: "perplexity", Scope: clientconfig.ScopeProject, Source: project, Type: "stdio", Command: "npx",
			Args: []clientconfig.Value{lit("-y"), lit("server-perplexity")},
			Env:  map[string]clientconfig.Value{"PERPLEXITY_API_KEY": {{Input: "api-key"}}}},
		{Name: "regional", Scope: clientconfig.ScopeProject, Source: project, Type: "http",
			URL:         clientconfig.Value{{Lit: "https://"}, {Input: "region"}, {Lit: ".example/mcp"}},
			Unsupported: []string{"${input:region}, which is not a promptString input"}},
		{Name: "unboxed", Scope: clientconfig.ScopeProject, Source: project, Command: "srv"},
	}, f.Servers)

	want := map[string]clientconfig.Input{"api-key": {ID: "api-key", Description: "API key"}}

	if diff := cmp.Diff(want, f.Inputs); diff != "" {
		t.Errorf("inputs (-want +got):\n%s", diff)
	}
}

func TestReadFindsNothing(t *testing.T) {
	d, _ := tree(t, "linux", nil)

	for _, c := range clientconfig.Clients() {
		f := read(t, c, d)

		if len(f.Servers) != 0 || len(f.Files) != 0 {
			t.Errorf("Read(%s) in an empty home = %+v; want nothing", c, f)
		}
	}
}

func TestReadRefusesAFileThatDoesNotParse(t *testing.T) {
	d, _ := tree(t, "linux", map[string]string{"home/.cursor/mcp.json": `{"mcpServers": {`})

	if _, err := clientconfig.Read(clientconfig.Cursor, d); err == nil {
		t.Fatal("Read of a truncated file succeeded; want an error")
	}
}

func TestReadFile(t *testing.T) {
	d, root := tree(t, "linux", map[string]string{"elsewhere.json": `{"mcpServers": {"a": {"command": "a"}}}`})
	path := filepath.Join(root, "elsewhere.json")

	f, err := clientconfig.ReadFile(clientconfig.ClaudeDesktop, path, d)

	if err != nil {
		t.Fatal(err)
	}

	diffServers(t, []clientconfig.Server{{Name: "a", Scope: clientconfig.ScopeUser, Source: path, Command: "a"}}, f.Servers)

	if _, err := clientconfig.ReadFile(clientconfig.ClaudeDesktop, path+".missing", d); err == nil {
		t.Error("ReadFile of a missing file succeeded; want an error")
	}
}

func TestDetect(t *testing.T) {
	d, _ := tree(t, "linux", map[string]string{
		"home/.claude.json":       `{}`,
		"home/.codex/config.toml": ``,
		"work/app/.mcp.json":      `{}`, // project files don't count
	})

	if diff := cmp.Diff([]clientconfig.Client{clientconfig.ClaudeCode, clientconfig.Codex}, clientconfig.Detect(d)); diff != "" {
		t.Errorf("Detect (-want +got):\n%s", diff)
	}
}

func TestRemoveHint(t *testing.T) {
	for _, tt := range []struct {
		c    clientconfig.Client
		s    clientconfig.Server
		want string
	}{
		{clientconfig.ClaudeCode, clientconfig.Server{Name: "github", Scope: clientconfig.ScopeUser}, "claude mcp remove github -s user"},
		{clientconfig.ClaudeCode, clientconfig.Server{Name: "my server", Scope: clientconfig.ScopeLocal}, "claude mcp remove 'my server' -s local"},
		{clientconfig.Codex, clientconfig.Server{Name: "docs", Scope: clientconfig.ScopeUser}, "codex mcp remove docs"},
		{clientconfig.Codex, clientconfig.Server{Name: "docs", Scope: clientconfig.ScopeProject, Source: "/p/.codex/config.toml"}, `remove "docs" from /p/.codex/config.toml`},
		{clientconfig.Cursor, clientconfig.Server{Name: "notes", Source: "/h/.cursor/mcp.json"}, `remove "notes" from /h/.cursor/mcp.json`},
	} {
		if got := clientconfig.RemoveHint(tt.c, tt.s); got != tt.want {
			t.Errorf("RemoveHint(%s, %s) = %q; want %q", tt.c, tt.s.Name, got, tt.want)
		}
	}
}

// TestReadClaudeCodeApprovalFromSettings: enableAllProjectMcpServers in the
// project's settings approves every .mcp.json server.
func TestReadClaudeCodeApprovalFromSettings(t *testing.T) {
	d, _ := tree(t, "linux", map[string]string{
		"work/app/.mcp.json":                   `{"mcpServers": {"fs": {"command": "fs"}}}`,
		"work/app/.claude/settings.local.json": `{"enableAllProjectMcpServers": true}`,
	})

	f := read(t, clientconfig.ClaudeCode, d)

	diffServers(t, []clientconfig.Server{
		{Name: "fs", Scope: clientconfig.ScopeProject, Source: filepath.Join(d.Cwd, ".mcp.json"), Command: "fs"},
	}, f.Servers)
}
