package clientconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Formats, as documented on 2026-11-09:
//
//   - Claude Code: https://code.claude.com/docs/en/mcp — scopes local >
//     project > user; ${VAR} and ${VAR:-default}; no type means stdio.
//     Where CLAUDE_CONFIG_DIR puts .claude.json is not documented; it is
//     looked for there first.
//   - Claude Desktop: https://modelcontextprotocol.io/docs/develop/connect-local-servers
//     — command, args and env only; nothing is expanded.
//   - Cursor: https://cursor.com/docs/context/mcp — project over global;
//     ${env:NAME} and the predefined ${userHome}-style variables.
//   - Codex: https://learn.chatgpt.com/docs/config-file/config-reference —
//     [mcp_servers.<name>] in $CODEX_HOME/config.toml; the project's
//     .codex/config.toml only when that config trusts the project; nothing
//     is expanded.
//   - VS Code: https://code.visualstudio.com/docs/copilot/reference/mcp-configuration
//     — servers and inputs; workspace over user; JSONC.

// Dirs are where a client's files are looked for.
type Dirs struct {
	Home string
	// Cwd is the project directory: the current directory.
	Cwd  string
	GOOS string
	// Getenv reads APPDATA, XDG_CONFIG_HOME, CLAUDE_CONFIG_DIR and
	// CODEX_HOME. Nil means none is set.
	Getenv func(key string) string
	// Project reads the client's files in the project directory too
	// (ScopeProject): Cursor's and VS Code's, Claude Code's .mcp.json, and a
	// trusted Codex project's config. They come with the repository, so
	// whoever wrote it chose their servers; without Project they are only
	// listed in Found.SkippedProject (wayfindr-awvh).
	Project bool
}

// project reports whether d reads the project file at path. Without
// Project it doesn't, and an existing file is listed in f.SkippedProject.
func (d Dirs) project(f *Found, path string) bool {
	if d.Project {
		return true
	}

	if _, err := os.Stat(path); err == nil {
		f.SkippedProject = append(f.SkippedProject, path)
	}

	return false
}

func (d Dirs) env(k string) string {
	if d.Getenv == nil {
		return ""
	}

	return d.Getenv(k)
}

// Found is what Read found in a client's files.
type Found struct {
	Servers []Server
	// Files are the files that were read, in the order read.
	Files []string
	// SkippedProject are the project files that exist but were not read,
	// because Dirs.Project was not set.
	SkippedProject []string
	// Inputs are VS Code's promptString inputs, by id.
	Inputs map[string]Input
}

// Input is a VS Code input that prompts for a string.
type Input struct {
	ID          string
	Description string
}

// Read reads every config file of client c that exists. A file that is
// missing is not an error; one that doesn't parse is.
func Read(c Client, d Dirs) (Found, error) {
	var f Found

	switch c {
	case ClaudeCode:
		return f, readClaudeCode(&f, d)
	case ClaudeDesktop:
		return f, readJSON(&f, ClaudeDesktop, claudeDesktopFile(d), ScopeUser, d)
	case Cursor:
		if err := readJSON(&f, Cursor, filepath.Join(d.Home, ".cursor", "mcp.json"), ScopeUser, d); err != nil {
			return f, err
		}

		if p := filepath.Join(d.Cwd, ".cursor", "mcp.json"); d.project(&f, p) {
			return f, readJSON(&f, Cursor, p, ScopeProject, d)
		}

		return f, nil
	case Codex:
		return f, readCodex(&f, d)
	case VSCode:
		if err := readJSON(&f, VSCode, vscodeUserFile(d), ScopeUser, d); err != nil {
			return f, err
		}

		if p := filepath.Join(d.Cwd, ".vscode", "mcp.json"); d.project(&f, p) {
			return f, readJSON(&f, VSCode, p, ScopeProject, d)
		}

		return f, nil
	}

	return f, fmt.Errorf("unknown client %q", c)
}

// ReadFile reads one file in client c's format, whatever its name. Its
// servers are in the user scope.
func ReadFile(c Client, path string, d Dirs) (Found, error) {
	var f Found
	var err error

	if c == Codex {
		err = readCodexFile(&f, path, ScopeUser)
	} else {
		err = readJSON(&f, c, path, ScopeUser, d)
	}

	if err == nil && len(f.Files) == 0 {
		err = fmt.Errorf("read %s: %w", path, fs.ErrNotExist)
	}

	return f, err
}

// Detect returns the clients that have a user-level config file, for
// `wayfindr init` to suggest importing from.
func Detect(d Dirs) []Client {
	var found []Client

	for _, c := range Clients() {
		for _, p := range userFiles(c, d) {
			if _, err := os.Stat(p); err == nil {
				found = append(found, c)

				break
			}
		}
	}

	return found
}

// userFiles are the user-level files of c.
func userFiles(c Client, d Dirs) []string {
	switch c {
	case ClaudeCode:
		return claudeJSONFiles(d)
	case ClaudeDesktop:
		return []string{claudeDesktopFile(d)}
	case Cursor:
		return []string{filepath.Join(d.Home, ".cursor", "mcp.json")}
	case Codex:
		return []string{filepath.Join(codexHome(d), "config.toml")}
	case VSCode:
		return []string{vscodeUserFile(d)}
	}

	return nil
}

func claudeJSONFiles(d Dirs) []string {
	files := []string{filepath.Join(d.Home, ".claude.json")}

	if dir := d.env("CLAUDE_CONFIG_DIR"); dir != "" {
		files = append([]string{filepath.Join(dir, ".claude.json")}, files...)
	}

	return files
}

// appConfigDir is where desktop apps keep their settings: Application
// Support on macOS, %APPDATA% on Windows, $XDG_CONFIG_HOME elsewhere.
func appConfigDir(d Dirs) string {
	switch d.GOOS {
	case "darwin":
		return filepath.Join(d.Home, "Library", "Application Support")
	case "windows":
		if a := d.env("APPDATA"); a != "" {
			return a
		}

		return filepath.Join(d.Home, "AppData", "Roaming")
	}

	if x := d.env("XDG_CONFIG_HOME"); x != "" {
		return x
	}

	return filepath.Join(d.Home, ".config")
}

func claudeDesktopFile(d Dirs) string {
	return filepath.Join(appConfigDir(d), "Claude", "claude_desktop_config.json")
}

func vscodeUserFile(d Dirs) string {
	return filepath.Join(appConfigDir(d), "Code", "User", "mcp.json")
}

func codexHome(d Dirs) string {
	if h := d.env("CODEX_HOME"); h != "" {
		return h
	}

	return filepath.Join(d.Home, ".codex")
}

// readFile reads path, reporting false if it does not exist.
func readFile(f *Found, path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the user's own client config

	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}

	f.Files = append(f.Files, path)

	return b, true, nil
}

// readClaudeCode reads ~/.claude.json (user and local scopes) and
// ./.mcp.json (project scope). Claude Code runs a .mcp.json server only once
// the user approves it, so one that is not approved is marked Unapproved,
// and one that was rejected Disabled.
func readClaudeCode(f *Found, d Dirs) error {
	// disabled are the servers the project has turned off, in any scope.
	var disabled []string
	var approval mcpjsonApproval

	for _, path := range claudeJSONFiles(d) {
		b, ok, err := readFile(f, path)

		if err != nil {
			return err
		}

		if !ok {
			continue
		}

		var doc struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
			Projects   map[string]struct {
				MCPServers         map[string]json.RawMessage `json:"mcpServers"`
				DisabledMCPServers []string                   `json:"disabledMcpServers"`
				mcpjsonApproval
			} `json:"projects"`
		}

		if err := json.Unmarshal(stripJSONC(b), &doc); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}

		project := doc.Projects[d.Cwd]
		disabled = project.DisabledMCPServers
		approval = approval.merge(project.mcpjsonApproval)
		f.Servers = append(f.Servers, jsonServers(ClaudeCode, doc.MCPServers, ScopeUser, path, d)...)
		f.Servers = append(f.Servers, jsonServers(ClaudeCode, project.MCPServers, ScopeLocal, path, d)...)

		break
	}

	before := len(f.Servers)

	if p := filepath.Join(d.Cwd, ".mcp.json"); d.project(f, p) {
		if err := readJSON(f, ClaudeCode, p, ScopeProject, d); err != nil {
			return err
		}
	}

	if len(f.Servers) > before {
		approval = approval.merge(claudeSettingsApproval(d))
	}

	for i := range f.Servers {
		s := &f.Servers[i]
		s.Disabled = s.Disabled || slices.Contains(disabled, s.Name)

		if i >= before {
			switch {
			case slices.Contains(approval.Disabled, s.Name):
				s.Disabled = true
			case !approval.EnableAll && !slices.Contains(approval.Enabled, s.Name):
				s.Unapproved = true
			}
		}
	}

	return nil
}

// mcpjsonApproval is what Claude Code records about a project's .mcp.json
// servers, in ~/.claude.json and in its settings files.
type mcpjsonApproval struct {
	EnableAll bool     `json:"enableAllProjectMcpServers"`
	Enabled   []string `json:"enabledMcpjsonServers"`
	Disabled  []string `json:"disabledMcpjsonServers"`
}

func (a mcpjsonApproval) merge(b mcpjsonApproval) mcpjsonApproval {
	return mcpjsonApproval{
		EnableAll: a.EnableAll || b.EnableAll,
		Enabled:   append(slices.Clone(a.Enabled), b.Enabled...),
		Disabled:  append(slices.Clone(a.Disabled), b.Disabled...),
	}
}

// claudeSettingsApproval reads the approval keys from Claude Code's user,
// project and local settings files. A file that is missing or doesn't parse
// approves nothing.
func claudeSettingsApproval(d Dirs) mcpjsonApproval {
	userDir := filepath.Join(d.Home, ".claude")

	if dir := d.env("CLAUDE_CONFIG_DIR"); dir != "" {
		userDir = dir
	}

	var a mcpjsonApproval

	for _, path := range []string{
		filepath.Join(userDir, "settings.json"),
		filepath.Join(d.Cwd, ".claude", "settings.json"),
		filepath.Join(d.Cwd, ".claude", "settings.local.json"),
	} {
		b, err := os.ReadFile(path) //nolint:gosec // the user's own client settings

		if err != nil {
			continue
		}

		var one mcpjsonApproval

		if json.Unmarshal(stripJSONC(b), &one) == nil {
			a = a.merge(one)
		}
	}

	return a
}

// readJSON reads one JSON (or JSONC) file of a client whose servers are an
// object keyed by name: mcpServers, or servers for VS Code.
func readJSON(f *Found, c Client, path string, scope Scope, d Dirs) error {
	b, ok, err := readFile(f, path)

	if err != nil || !ok {
		return err
	}

	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
		Servers    map[string]json.RawMessage `json:"servers"`
		Inputs     []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"inputs"`
	}

	if err := json.Unmarshal(stripJSONC(b), &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	entries := doc.MCPServers

	if c == VSCode {
		entries = doc.Servers
	}

	servers := jsonServers(c, entries, scope, path, d)

	if c == VSCode {
		prompts := map[string]bool{}

		for _, in := range doc.Inputs {
			if in.Type != "promptString" {
				continue
			}

			prompts[in.ID] = true

			if f.Inputs == nil {
				f.Inputs = map[string]Input{}
			}

			f.Inputs[in.ID] = Input{ID: in.ID, Description: in.Description}
		}

		for i := range servers {
			for _, id := range inputsOf(servers[i]) {
				if !prompts[id] {
					servers[i].Unsupported = append(servers[i].Unsupported, fmt.Sprintf("${input:%s}, which is not a promptString input", id))
				}
			}

			slices.Sort(servers[i].Unsupported)
		}
	}

	f.Servers = append(f.Servers, servers...)

	return nil
}

// inputsOf returns the ids of the inputs s refers to.
func inputsOf(s Server) []string {
	var ids []string
	vals := append(append([]Value{s.URL}, s.Args...), slices.Collect(maps.Values(s.Env))...)
	vals = append(vals, slices.Collect(maps.Values(s.Headers))...)

	for _, v := range vals {
		for _, p := range v {
			if p.Input != "" && !slices.Contains(ids, p.Input) {
				ids = append(ids, p.Input)
			}
		}
	}

	slices.Sort(ids)

	return ids
}

// jsonKeys are the keys an entry of client c may have beyond command, args,
// env, url, type, headers and disabled: true when the key is ignored
// because it only affects the client, false when it can't be expressed.
// Keys not listed can't be expressed either.
func jsonKeys(c Client) map[string]bool {
	switch c {
	case ClaudeCode:
		return map[string]bool{"oauth": true, "timeout": true, "alwaysLoad": true, "headersHelper": false}
	case Cursor:
		return map[string]bool{"auth": true, "envFile": false}
	case VSCode:
		return map[string]bool{
			"oauth": true, "dev": true, "gallery": true, "version": true,
			"cwd": false, "envFile": false, "sandbox": false, "sandboxEnabled": false,
		}
	case ClaudeDesktop, Codex:
	}

	return map[string]bool{}
}

// clientSyntax is how client c writes variables.
func clientSyntax(c Client, d Dirs) syntax {
	switch c {
	case ClaudeCode:
		return shellSyntax
	case Cursor:
		return editorVars{home: d.Home, workspace: d.Cwd}.syntax
	case VSCode:
		return editorVars{home: d.Home, workspace: d.Cwd, inputs: true}.syntax
	case ClaudeDesktop, Codex:
	}

	return literalSyntax
}

// jsonServers parses the entries of one JSON file, sorted by name.
func jsonServers(c Client, entries map[string]json.RawMessage, scope Scope, path string, d Dirs) []Server {
	var out []Server

	for _, name := range slices.Sorted(maps.Keys(entries)) {
		s := Server{Name: name, Scope: scope, Source: path}
		var raw map[string]json.RawMessage

		if err := json.Unmarshal(entries[name], &raw); err != nil {
			s.Unsupported = []string{"an entry that is not an object"}
			out = append(out, s)

			continue
		}

		e := entryParser{s: &s, syn: clientSyntax(c, d)}
		known := jsonKeys(c)

		for _, k := range slices.Sorted(maps.Keys(raw)) {
			v := raw[k]

			switch k {
			case "command":
				e.command(k, decode[string](&e, k, v))
			case "args":
				e.args(k, decode[[]string](&e, k, v))
			case "env":
				s.Env = e.values(k, decode[map[string]string](&e, k, v))
			case "headers":
				s.Headers = e.values(k, decode[map[string]string](&e, k, v))
			case "url":
				s.URL = e.value(k, decode[string](&e, k, v))
			case "type":
				s.Type = decode[string](&e, k, v)
			case "disabled":
				s.Disabled = decode[bool](&e, k, v)
			case "timeout":
				if _, ok := known[k]; !ok {
					e.unsupported(k)
				} else if ms := decode[float64](&e, k, v); ms >= 1000 {
					// Claude Code ignores a timeout under a second.
					s.Timeout = time.Duration(ms) * time.Millisecond
				}
			case "oauth", "auth":
				if _, ok := known[k]; ok {
					e.oauth(k, v)
				} else {
					e.unsupported(k)
				}
			case "sandboxEnabled":
				// Turning the sandbox off is fine; dropping it is not.
				if decode[bool](&e, k, v) {
					e.unsupported(k)
				}
			default:
				if !known[k] {
					e.unsupported(k)
				}
			}
		}

		slices.Sort(s.Unsupported)
		out = append(out, s)
	}

	return out
}

// entryParser collects one entry's fields and what can't be expressed.
type entryParser struct {
	s   *Server
	syn syntax
}

func (e *entryParser) unsupported(what string) {
	if !slices.Contains(e.s.Unsupported, what) {
		e.s.Unsupported = append(e.s.Unsupported, what)
	}
}

// decode unmarshals v, noting key as unsupported when it has the wrong type.
func decode[T any](e *entryParser, key string, v json.RawMessage) T {
	var t T

	if err := json.Unmarshal(v, &t); err != nil {
		e.unsupported(key + " (malformed)")
	}

	return t
}

func (e *entryParser) value(key, s string) Value {
	v, err := e.syn(s)

	if err != nil {
		e.unsupported(fmt.Sprintf("%s (%v)", key, err))
	}

	return v
}

func (e *entryParser) values(key string, m map[string]string) map[string]Value {
	if len(m) == 0 {
		return nil
	}

	out := make(map[string]Value, len(m))

	for k, s := range m {
		out[k] = e.value(key+"."+k, s)
	}

	return out
}

func (e *entryParser) command(key, s string) {
	v := e.value(key, s)

	if text, ok := v.Literal(); ok {
		e.s.Command = text
	} else {
		e.unsupported("command (a variable in it)")
	}
}

func (e *entryParser) args(key string, args []string) {
	for _, a := range args {
		e.s.Args = append(e.s.Args, e.value(key, a))
	}
}

// oauth reads Claude Code's and VS Code's oauth table and Cursor's auth
// table: a client ID and scopes are kept; a client secret or metadata URL
// can't be expressed; a callback port only matters to the client.
func (e *entryParser) oauth(key string, v json.RawMessage) {
	var o map[string]json.RawMessage

	if err := json.Unmarshal(v, &o); err != nil {
		e.unsupported(key + " (malformed)")

		return
	}

	e.s.OAuth = true

	for _, k := range slices.Sorted(maps.Keys(o)) {
		switch k {
		case "clientId", "CLIENT_ID":
			id, ok := e.value(key+"."+k, decode[string](e, key+"."+k, o[k])).Literal()

			if !ok {
				e.unsupported(key + "." + k + " (a variable in it)")
			}

			e.s.OAuthClientID = id
		case "scopes":
			e.s.OAuthScopes = decode[[]string](e, key+"."+k, o[k])
		case "callbackPort":
		default:
			e.unsupported(key + "." + k)
		}
	}
}

// readCodex reads $CODEX_HOME/config.toml, and the project's
// .codex/config.toml when that config trusts the project.
func readCodex(f *Found, d Dirs) error {
	path := filepath.Join(codexHome(d), "config.toml")
	doc, err := codexDoc(f, path)

	if err != nil {
		return err
	}

	codexServers(f, doc, ScopeUser, path)

	if doc.Projects[d.Cwd].TrustLevel != "trusted" {
		return nil
	}

	if p := filepath.Join(d.Cwd, ".codex", "config.toml"); d.project(f, p) {
		return readCodexFile(f, p, ScopeProject)
	}

	return nil
}

func readCodexFile(f *Found, path string, scope Scope) error {
	doc, err := codexDoc(f, path)

	if err != nil {
		return err
	}

	codexServers(f, doc, scope, path)

	return nil
}

type codexConfig struct {
	MCPServers map[string]map[string]any `toml:"mcp_servers"`
	Projects   map[string]struct {
		TrustLevel string `toml:"trust_level"`
	} `toml:"projects"`
}

func codexDoc(f *Found, path string) (codexConfig, error) {
	var doc codexConfig
	b, ok, err := readFile(f, path)

	if err != nil || !ok {
		return doc, err
	}

	if _, err := toml.NewDecoder(bytes.NewReader(b)).Decode(&doc); err != nil {
		return doc, fmt.Errorf("parse %s: %w", path, err)
	}

	return doc, nil
}

// codexIgnored are Codex keys that only affect Codex itself.
func codexIgnored() []string {
	return []string{
		"startup_timeout_sec", "startup_timeout_ms", "required", "startup_readiness",
		"default_tools_approval_mode", "tools", "supports_parallel_tool_calls",
		"tool_input_schema_max_bytes", "omit_tools_from",
	}
}

func codexServers(f *Found, doc codexConfig, scope Scope, path string) {
	for _, name := range slices.Sorted(maps.Keys(doc.MCPServers)) {
		s := Server{Name: name, Scope: scope, Source: path}
		e := entryParser{s: &s, syn: literalSyntax}
		raw := doc.MCPServers[name]

		for _, k := range slices.Sorted(maps.Keys(raw)) {
			v := raw[k]

			switch k {
			case "command":
				s.Command = tomlAs[string](&e, k, v)
			case "args":
				for _, a := range tomlStrings(&e, k, v) {
					s.Args = append(s.Args, Lit(a))
				}
			case "env", "http_headers":
				m := map[string]Value{}

				for hk, hv := range tomlStringMap(&e, k, v) {
					m[hk] = Lit(hv)
				}

				if k == "env" {
					s.Env = m
				} else {
					s.Headers = mergeValues(s.Headers, m)
				}
			case "env_http_headers":
				m := map[string]Value{}

				for hk, name := range tomlStringMap(&e, k, v) {
					m[hk] = Value{{Env: name}}
				}

				s.Headers = mergeValues(s.Headers, m)
			case "bearer_token_env_var":
				s.Headers = mergeValues(s.Headers, map[string]Value{
					"Authorization": {{Lit: "Bearer "}, {Env: tomlAs[string](&e, k, v)}},
				})
			case "url":
				s.URL = Lit(tomlAs[string](&e, k, v))
			case "enabled":
				s.Disabled = !tomlAs[bool](&e, k, v)
			case "tool_timeout_sec":
				switch n := v.(type) {
				case int64:
					s.Timeout = time.Duration(n) * time.Second
				case float64:
					s.Timeout = time.Duration(n * float64(time.Second))
				default:
					e.unsupported(k + " (malformed)")
				}
			case "auth":
				if a := tomlAs[string](&e, k, v); a != "oauth" {
					e.unsupported(fmt.Sprintf("auth = %q", a))
				}
			case "scopes":
				s.OAuth = true
				s.OAuthScopes = tomlStrings(&e, k, v)
			case "oauth":
				codexOAuth(&e, v)
			default:
				if !slices.Contains(codexIgnored(), k) {
					e.unsupported(k)
				}
			}
		}

		slices.Sort(s.Unsupported)
		f.Servers = append(f.Servers, s)
	}
}

func codexOAuth(e *entryParser, v any) {
	o, ok := v.(map[string]any)

	if !ok {
		e.unsupported("oauth (malformed)")

		return
	}

	e.s.OAuth = true

	for _, k := range slices.Sorted(maps.Keys(o)) {
		switch k {
		case "client_id":
			e.s.OAuthClientID = tomlAs[string](e, "oauth."+k, o[k])
		case "callback_port", "callback_url":
		default:
			e.unsupported("oauth." + k)
		}
	}
}

// mergeValues adds b's entries to a; a key in both keeps a's.
func mergeValues(a, b map[string]Value) map[string]Value {
	if a == nil {
		a = map[string]Value{}
	}

	for k, v := range b {
		if _, ok := a[k]; !ok {
			a[k] = v
		}
	}

	return a
}

func tomlAs[T any](e *entryParser, key string, v any) T {
	t, ok := v.(T)

	if !ok {
		e.unsupported(key + " (malformed)")
	}

	return t
}

func tomlStrings(e *entryParser, key string, v any) []string {
	list, ok := v.([]any)

	if !ok {
		e.unsupported(key + " (malformed)")

		return nil
	}

	out := make([]string, 0, len(list))

	for _, x := range list {
		out = append(out, tomlAs[string](e, key, x))
	}

	return out
}

func tomlStringMap(e *entryParser, key string, v any) map[string]string {
	m, ok := v.(map[string]any)

	if !ok {
		e.unsupported(key + " (malformed)")

		return nil
	}

	out := make(map[string]string, len(m))

	for k, x := range m {
		out[k] = tomlAs[string](e, key+"."+k, x)
	}

	return out
}

// RemoveHint says how to remove s from client c once it is imported.
func RemoveHint(c Client, s Server) string {
	switch {
	case c == ClaudeCode:
		return fmt.Sprintf("claude mcp remove %s -s %s", shellWord(s.Name), s.Scope)
	case c == Codex && s.Scope == ScopeUser:
		return "codex mcp remove " + shellWord(s.Name)
	}

	return fmt.Sprintf("remove %q from %s", s.Name, s.Source)
}

// shellWord quotes s for a POSIX shell if it needs it.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-.") == "" {
		return s
	}

	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
