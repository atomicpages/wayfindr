package config

import (
	pathpkg "path"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolPolicy is a connector's tools list: which of its tools agents can see
// and call (GLOSSARY: Tool policy). Each entry is a class (@read, @write), a
// glob on the upstream tool name, or a !glob that hides the tools it
// matches. Nil or empty exposes every tool.
type ToolPolicy []string

// ToolClass is what a tool's annotations say it does (GLOSSARY: Tool class).
type ToolClass string

// Tool classes, from the upstream's annotations.
const (
	// ClassRead is a tool whose readOnlyHint is true.
	ClassRead ToolClass = "read"
	// ClassWrite is a tool that is not read-only but whose destructiveHint
	// is explicitly false.
	ClassWrite ToolClass = "write"
	// ClassDestructive is every other tool. MCP's defaults (readOnlyHint
	// false, destructiveHint true) put a tool without annotations here.
	ClassDestructive ToolClass = "destructive"
)

// The class entries of a ToolPolicy.
const (
	entryRead  = "@read"
	entryWrite = "@write"
)

// ClassOf returns the class of a tool with annotations a, which may be nil.
func ClassOf(a *mcp.ToolAnnotations) ToolClass {
	switch {
	case a == nil:
		return ClassDestructive
	case a.ReadOnlyHint:
		return ClassRead
	case a.DestructiveHint != nil && !*a.DestructiveHint:
		return ClassWrite
	default:
		return ClassDestructive
	}
}

// Exposes reports whether the policy lets agents see and call the tool
// named name (the upstream's name, not the qualified one) of class class.
// A !glob that matches hides the tool whatever else matches. A policy with
// no other entries exposes every other tool; otherwise the tool must match
// a glob or be in a listed class (@write includes @read). The entries must
// have passed validation; a malformed glob matches nothing.
func (p ToolPolicy) Exposes(name string, class ToolClass) bool {
	positive, matched := false, false

	for _, e := range p {
		if glob, ok := strings.CutPrefix(e, "!"); ok {
			if globMatch(glob, name) {
				return false
			}

			continue
		}

		positive = true

		switch e {
		case entryRead:
			matched = matched || class == ClassRead
		case entryWrite:
			matched = matched || class == ClassRead || class == ClassWrite
		default:
			matched = matched || globMatch(e, name)
		}
	}

	return !positive || matched
}

// HidesName reports whether the policy hides the tool named name whatever
// its class: a !glob matches it, or the policy names tools and none of its
// globs match while it lists no class. The gateway uses it when it has no
// annotations to class the tool by.
func (p ToolPolicy) HidesName(name string) bool {
	positive, matched := false, false

	for _, e := range p {
		if glob, ok := strings.CutPrefix(e, "!"); ok {
			if globMatch(glob, name) {
				return true
			}

			continue
		}

		positive = true
		matched = matched || e == entryRead || e == entryWrite || globMatch(e, name)
	}

	return positive && !matched
}

func globMatch(pattern, name string) bool {
	ok, err := pathpkg.Match(pattern, name)

	return err == nil && ok
}

// tools checks each entry of a tools list: a known class, or a well-formed
// glob, optionally negated.
func (v *validator) tools(at string, p ToolPolicy) {
	for i, e := range p {
		where := at + "[" + strconv.Itoa(i) + "]"
		glob, negated := strings.CutPrefix(e, "!")

		switch {
		case strings.HasPrefix(glob, "@"):
			switch {
			case glob != entryRead && glob != entryWrite:
				v.add(where, "%q is not a class; use %q or %q", glob, entryRead, entryWrite)
			case negated:
				v.add(where, "%q: a class cannot be hidden with !; hide tools by name", e)
			}
		case strings.TrimSpace(glob) == "":
			v.add(where, "must name a tool, a glob, or a class (%q, %q)", entryRead, entryWrite)
		default:
			if err := checkGlob(glob); err != nil {
				v.add(where, "%q is not a valid glob: %v", e, err)
			}
		}
	}
}

// checkGlob reports why glob is not a well-formed glob, of a tools list or
// an approval entry, or nil.
func checkGlob(glob string) error {
	// path.Match checks the whole pattern only when the name doesn't cut it
	// short; an empty name never does.
	_, err := pathpkg.Match(glob, "")

	return err
}
