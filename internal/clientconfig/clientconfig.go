// Package clientconfig reads the MCP servers other clients are set up with
// (Claude Code, Claude Desktop, Cursor, Codex, VS Code) and maps them to
// connectors, for `wayfindr connector import`. Reading is local file I/O;
// mapping is pure. Neither touches the client's files or the daemon.
package clientconfig

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Client names a client whose config can be imported. Its value is the
// argument of `wayfindr connector import`.
type Client string

// The clients that can be imported.
const (
	ClaudeCode    Client = "claude-code"
	ClaudeDesktop Client = "claude-desktop"
	Cursor        Client = "cursor"
	Codex         Client = "codex"
	VSCode        Client = "vscode"
)

// Clients returns every client, in the order help text lists them.
func Clients() []Client {
	return []Client{ClaudeCode, ClaudeDesktop, Cursor, Codex, VSCode}
}

// ParseClient returns the client named s.
func ParseClient(s string) (Client, error) {
	if slices.Contains(Clients(), Client(s)) {
		return Client(s), nil
	}

	names := make([]string, 0, len(Clients()))

	for _, c := range Clients() {
		names = append(names, string(c))
	}

	return "", fmt.Errorf("unknown client %q: want one of %s", s, strings.Join(names, ", "))
}

// Scope is where in a client's config a server is defined. A narrower
// scope wins when the same name is in two, as in the clients themselves.
type Scope string

// Scopes, broadest first.
const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
	ScopeLocal   Scope = "local"
)

// rank orders scopes from broadest to narrowest.
func (s Scope) rank() int {
	switch s {
	case ScopeUser:
		return 1
	case ScopeProject:
		return 2
	case ScopeLocal:
		return 3
	}

	return 0
}

// Server is one MCP server entry as a client defines it, with the client's
// variable syntax already parsed into Values.
type Server struct {
	// Name is the entry's key in the client's config, as written there.
	Name   string
	Scope  Scope
	Source string // the file it was read from

	Command string
	Args    []Value
	Env     map[string]Value

	URL     Value
	Type    string // "stdio", "http", "streamable-http", "sse", or ""
	Headers map[string]Value

	// OAuth is set when the entry has an OAuth table, with the client ID
	// and scopes it gives, if any.
	OAuth         bool
	OAuthClientID string
	OAuthScopes   []string
	// Timeout bounds one tool call; zero means the default.
	Timeout time.Duration

	// Disabled is set when the client has the entry turned off.
	Disabled bool
	// Unapproved is set for a project entry the client has not been told
	// to trust (Claude Code's .mcp.json approval).
	Unapproved bool
	// Unsupported names the entry's settings that a connector cannot
	// express, sorted. An entry with any is not imported.
	Unsupported []string
}

// Value is a config string as a sequence of literal text and references.
type Value []Part

// Part is one piece of a Value: literal text, an environment variable, or
// a VS Code input. Exactly one of Lit, Env and Input is meaningful: Env if
// it is set, else Input if it is set, else Lit.
type Part struct {
	Lit string

	// Env names an environment variable the client reads from the shell
	// it runs in. HasDefault reports a ${VAR:-default}.
	Env        string
	Default    string
	HasDefault bool

	// Input is the id of a VS Code input the client prompts for.
	Input string
}

// Lit returns s as a Value of literal text only.
func Lit(s string) Value {
	if s == "" {
		return nil
	}

	return Value{{Lit: s}}
}

// Literal returns v's text and true if v has no references.
func (v Value) Literal() (string, bool) {
	var b strings.Builder

	for _, p := range v {
		if p.Env != "" || p.Input != "" {
			return "", false
		}

		b.WriteString(p.Lit)
	}

	return b.String(), true
}

// String renders v for display, references in the shell's ${VAR} form and
// inputs as ${input:id}. It may contain a literal credential.
func (v Value) String() string {
	var b strings.Builder

	for _, p := range v {
		switch {
		case p.Env != "" && p.HasDefault:
			b.WriteString("${" + p.Env + ":-" + p.Default + "}")
		case p.Env != "":
			b.WriteString("${" + p.Env + "}")
		case p.Input != "":
			b.WriteString("${input:" + p.Input + "}")
		default:
			b.WriteString(p.Lit)
		}
	}

	return b.String()
}
