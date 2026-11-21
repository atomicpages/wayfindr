package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/BurntSushi/toml"
)

// scopeWhat names a scope in its ValidationError.
const scopeWhat = "project scope"

// ScopeFile is the name of a project scope file (GLOSSARY: Project scope).
const ScopeFile = ".wayfindr.toml"

// ScopeHeader is the HTTP header the stdio shim carries a project scope in,
// as the JSON [Scope.Header] writes, on every request to the daemon (ADR
// 0010).
const ScopeHeader = "Wayfindr-Scope"

// ProfileHeader is the HTTP header that names the profile a request is
// scoped by (ADR 0011): the stdio shim's --profile, or /mcp?profile=, which
// the gateway copies into it.
const ProfileHeader = "Wayfindr-Profile"

// IsProfileName reports whether s is a valid profile name. It follows the
// connector-name rule, [a-z0-9][a-z0-9_-]{0,62}, so a name is safe to
// print and to send in a header.
func IsProfileName(s string) bool { return IsConnectorName(s) }

// ErrProfileName is why a profile name is invalid. The config's validation
// and the stdio shim's --profile both report it, so the rule reads the same.
var ErrProfileName = errors.New("profile name must match " + NameGrammar)

// Scope is a project scope: what one project's agents may use of what the
// config exposes (ADR 0010). It only narrows. It names no connector settings,
// so a scope file in a cloned repository can hide tools but never add a
// command, a URL, or a tool the config hides.
//
// A nil *Scope is no scope: it allows every connector and tool.
type Scope struct {
	// Connectors lists the connectors the project may use. Nil allows every
	// connector; an empty list allows none. Not omitempty: dropping an empty
	// list from the header would widen it to every connector.
	Connectors []string `toml:"connectors" json:"connectors"`
	// Tools narrows connectors' tools: a tool must pass both the config's
	// tools list and this one. A connector absent here keeps the config's.
	Tools map[string]ToolPolicy `toml:"tools" json:"tools,omitempty"`
}

// Allows reports whether the scope lets agents use connector.
func (s *Scope) Allows(connector string) bool {
	if s == nil || s.Connectors == nil {
		return true
	}

	for _, c := range s.Connectors {
		if c == connector {
			return true
		}
	}

	return false
}

// Exposes reports whether the scope lets agents see and call connector's
// tool named name of class class. The config's own policy is checked
// separately; a tool must pass both.
func (s *Scope) Exposes(connector, name string, class ToolClass) bool {
	if !s.Allows(connector) {
		return false
	}

	return s == nil || s.Tools[connector].Exposes(name, class)
}

// HidesName reports whether the scope hides connector's tool named name
// whatever its class, as [ToolPolicy.HidesName] does.
func (s *Scope) HidesName(connector, name string) bool {
	if !s.Allows(connector) {
		return true
	}

	return s != nil && s.Tools[connector].HidesName(name)
}

// ParseScope decodes and validates a scope file's TOML. Unknown keys and rule
// violations are reported together as a *ValidationError.
func ParseScope(data []byte) (*Scope, error) {
	var s Scope
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&s)

	if err != nil {
		return nil, fmt.Errorf("decode project scope: %w", err)
	}

	var v validator

	for _, k := range md.Undecoded() {
		v.add(k.String(), "unknown key")
	}

	v.scope(nil, &s)

	if err := v.errFor(scopeWhat); err != nil {
		return nil, err
	}

	return &s, nil
}

// LoadScope finds the project scope that applies in dir: the nearest
// ScopeFile in dir or a parent, stopping before home, so a file in the home
// directory never applies to every project. It returns the parsed scope and
// the file's path, or nil and "" when there is none. A file that can't be
// read or doesn't validate is an error, never ignored, since ignoring it
// would widen what the project gets. An empty home walks to the root.
func LoadScope(dir, home string) (*Scope, string, error) {
	dir, err := filepath.Abs(dir)

	if err != nil {
		return nil, "", fmt.Errorf("project scope: %w", err)
	}

	if home != "" {
		if home, err = filepath.Abs(home); err != nil {
			return nil, "", fmt.Errorf("project scope: %w", err)
		}
	}

	for d := dir; d != home; {
		file := filepath.Join(d, ScopeFile)
		data, err := os.ReadFile(file) //nolint:gosec // the user's own project file, read only to narrow access

		switch {
		case err == nil:
			s, err := ParseScope(data)

			if err != nil {
				return nil, "", fmt.Errorf("%s: %w", file, err)
			}

			return s, file, nil
		case !errors.Is(err, fs.ErrNotExist):
			return nil, "", fmt.Errorf("project scope: %w", err)
		}

		parent := filepath.Dir(d)

		if parent == d {
			break
		}

		d = parent
	}

	return nil, "", nil
}

// Header encodes the scope for ScopeHeader: compact JSON with every non-ASCII
// character escaped, so the value is a valid header field. A nil scope
// encodes as "", which means no scope.
func (s *Scope) Header() (string, error) {
	if s == nil {
		return "", nil
	}

	b, err := json.Marshal(s)

	if err != nil {
		return "", fmt.Errorf("encode scope: %w", err)
	}

	var out strings.Builder

	for _, r := range string(b) {
		switch {
		case r < 0x80:
			out.WriteRune(r)
		case r <= 0xFFFF:
			fmt.Fprintf(&out, `\u%04x`, r)
		default:
			// Outside the BMP, JSON needs a UTF-16 surrogate pair.
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&out, `\u%04x\u%04x`, hi, lo)
		}
	}

	return out.String(), nil
}

// ParseScopeHeader decodes and validates a ScopeHeader value, as ParseScope
// does a file. "" is no scope and returns nil. Unknown fields are an error:
// a field the daemon doesn't know might be a restriction it would drop.
func ParseScopeHeader(v string) (*Scope, error) {
	if v == "" {
		return nil, nil
	}

	var s Scope
	dec := json.NewDecoder(strings.NewReader(v))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decode scope header: %w", err)
	}

	if dec.More() {
		return nil, errors.New("decode scope header: trailing data")
	}

	var val validator

	val.scope(nil, &s)

	if err := val.errFor(scopeWhat); err != nil {
		return nil, err
	}

	return &s, nil
}

// scope checks connector names and each tools list. A tools list for a
// connector the allowlist leaves out is a contradiction in the file. prefix
// is the key path of the scope's table: nil for a scope file, or
// profiles.<name> in the config.
func (v *validator) scope(prefix []string, s *Scope) {
	for i, c := range s.Connectors {
		if !IsConnectorName(c) {
			v.add(path(append(slices.Clone(prefix), "connectors")...)+"["+strconv.Itoa(i)+"]", "%q is not a connector name", c)
		}
	}

	for _, name := range sortedKeys(s.Tools) {
		at := path(append(slices.Clone(prefix), "tools", name)...)

		switch {
		case !IsConnectorName(name):
			v.add(at, "%q is not a connector name", name)
		case !s.Allows(name):
			v.add(at, "connector %q is not in connectors", name)
		}

		v.tools(at, s.Tools[name])
	}
}

// Source names what narrows a client and hides a tool from it: its profile
// or its project scope file.
type Source int

const (
	// SourceNone hides nothing.
	SourceNone Source = iota
	// SourceProfile is the config's profile the client names (ADR 0011).
	SourceProfile
	// SourceFile is the project's scope file (ADR 0010).
	SourceFile
)

// Narrowing is what narrows one client: the connectors its client token
// allows, the config's profile it names, and its project scope file, all
// ANDed. A connector must pass all three, and a tool the profile, the file,
// and the config's own tools lists, which are checked separately.
//
// The token is the outer limit. The server sets it from the token the
// client authenticated with, so the client can't choose or widen it; the
// profile and the file, which the client picks, only narrow within it. A
// token limits connectors only, never tools, so it hides no tool by name or
// class and never shows up as a Source: to a client whose token leaves a
// connector out, that connector is not configured at all.
//
// The profile and the file are kept apart rather than merged because two
// tools lists can't be folded into one, and because a refusal says which
// one hid the tool. A nil Token and nil scopes narrow nothing, so the zero
// Narrowing narrows nothing.
type Narrowing struct {
	// Token lists the connectors the client token allows. Nil allows every
	// connector, as the default token does; a non-nil empty list allows
	// none.
	Token []string
	// ProfileName is the profile's name; "" for none.
	ProfileName string
	Profile     *Scope
	File        *Scope
}

// TokenAllows reports whether the client token allows connector.
func (n Narrowing) TokenAllows(connector string) bool {
	return n.Token == nil || slices.Contains(n.Token, connector)
}

// Allows reports whether the token, the profile, and the file all allow
// connector.
func (n Narrowing) Allows(connector string) bool {
	return n.TokenAllows(connector) && n.Profile.Allows(connector) && n.File.Allows(connector)
}

// Allowlist is the connectors the token, the profile, and the file all
// allow: in the profile's order, else the token's, else the file's. Nil
// means every connector; a non-nil empty list means none.
func (n Narrowing) Allowlist() []string {
	var profile, file []string

	if n.Profile != nil {
		profile = n.Profile.Connectors
	}

	if n.File != nil {
		file = n.File.Connectors
	}

	var out []string

	for _, l := range [][]string{profile, n.Token, file} {
		switch {
		case l == nil:
		case out == nil:
			out = l
		default:
			// Cloned first, so the profile's or the token's own list is
			// never reordered or cut. The clone of a non-nil list is
			// non-nil, so an empty intersection still means none.
			out = slices.DeleteFunc(slices.Clone(out), func(c string) bool { return !slices.Contains(l, c) })
		}
	}

	return out
}

// HidesName is what hides connector's tool called name whatever its class,
// as [Scope.HidesName] decides: the profile before the file, or SourceNone.
func (n Narrowing) HidesName(connector, name string) Source {
	switch {
	case n.Profile.HidesName(connector, name):
		return SourceProfile
	case n.File.HidesName(connector, name):
		return SourceFile
	default:
		return SourceNone
	}
}

// Hides is what hides connector's tool called name of class: the profile
// before the file, or SourceNone when both expose it.
func (n Narrowing) Hides(connector, name string, class ToolClass) Source {
	switch {
	case !n.Profile.Exposes(connector, name, class):
		return SourceProfile
	case !n.File.Exposes(connector, name, class):
		return SourceFile
	default:
		return SourceNone
	}
}
