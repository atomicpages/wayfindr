package config

import (
	pathpkg "path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// entryDestructive is an approval rule's other class entry, beside
// @write. There is no @read: gating reads isn't offered.
const entryDestructive = "@destructive"

// approvalWhat names an approval rule in its ValidationError.
const approvalWhat = "approval rule"

// ApprovalRule is a client's approval entries, parsed: which of its calls
// wait for the owner's approval before they run (GLOSSARY: Approval rule).
// The zero value gates nothing.
type ApprovalRule struct {
	gate   []approvalEntry
	exempt []approvalEntry
}

// approvalEntry is one parsed entry: a class, or a connector and a glob on
// its upstream tool names.
type approvalEntry struct {
	class     ToolClass
	connector string
	glob      string
}

func (e approvalEntry) matches(connector, tool string, class ToolClass) bool {
	if e.class != "" {
		return e.class == class
	}

	return e.connector == connector && approvalGlobMatch(e.glob, tool)
}

// ParseApproval parses a client's approval entries. Each is a class
// (@write or @destructive), <connector>.<glob> on the upstream's tool
// name, or !<connector>.<glob>, which exempts the tools it matches. Unlike
// a tools list, @write does not include @read: each class gates itself
// only. Every bad entry is reported together as a *ValidationError. Nil or
// empty entries gate nothing.
//
// On error the returned rule is the zero rule, which gates nothing. A
// caller must never use it then: entries that don't parse fail closed, so
// the caller treats every call from that client as gated (ADR 0024).
func ParseApproval(entries []string) (ApprovalRule, error) {
	var (
		r        ApprovalRule
		v        validator
		positive bool
	)

	for i, e := range entries {
		where := "approval[" + strconv.Itoa(i) + "]"
		rest, negated := strings.CutPrefix(e, "!")
		positive = positive || !negated

		// White space at an end, or a control character, would leave an
		// entry that reads right and gates nothing.
		if untidy(rest) {
			v.add(where, "%q has white space at an end or a control character", e)

			continue
		}

		if strings.HasPrefix(rest, "@") {
			switch {
			case negated:
				v.add(where, "%q: a class cannot be exempted with !; exempt tools by name", e)
			case rest == entryWrite:
				r.gate = append(r.gate, approvalEntry{class: ClassWrite})
			case rest == entryDestructive:
				r.gate = append(r.gate, approvalEntry{class: ClassDestructive})
			case rest == entryRead:
				v.add(where, "%q: reads cannot be gated; use %q or %q", e, entryWrite, entryDestructive)
			default:
				v.add(where, "%q is not a class; use %q or %q", e, entryWrite, entryDestructive)
			}

			continue
		}

		entry, ok := parseApprovalName(&v, where, e, rest)

		if !ok {
			continue
		}

		if negated {
			r.exempt = append(r.exempt, entry)
		} else {
			r.gate = append(r.gate, entry)
		}
	}

	if !positive && len(r.exempt) > 0 {
		v.add("approval", "only ! exemptions, which gate nothing: an exemption needs a class or tool entry to exempt from")
	}

	if err := v.errFor(approvalWhat); err != nil {
		return ApprovalRule{}, err
	}

	return r, nil
}

// untidy reports whether s has white space at either end or holds a
// control character.
func untidy(s string) bool {
	return s != strings.TrimSpace(s) || strings.ContainsFunc(s, unicode.IsControl)
}

// parseApprovalName parses rest, the <connector>.<glob> of entry e, adding
// an issue at where when it is malformed. Connector names hold no dot, so
// the first one ends the connector.
func parseApprovalName(v *validator, where, e, rest string) (approvalEntry, bool) {
	connector, glob, found := strings.Cut(rest, ".")

	switch {
	case !found || connector == "":
		v.add(where, "%q must be a class (%q, %q) or <connector>.<glob>", e, entryWrite, entryDestructive)
	case !IsConnectorName(connector):
		v.add(where, "%q: %q is not a connector name; it must match %s", e, connector, NameGrammar)
	case glob == "":
		v.add(where, "%q must name a tool or a glob after %q", e, connector+".")
	case untidy(glob):
		v.add(where, "%q has white space after %q", e, connector+".")
	default:
		if err := checkGlob(glob); err != nil {
			v.add(where, "%q is not a valid glob: %v", e, err)

			return approvalEntry{}, false
		}

		return approvalEntry{connector: connector, glob: glob}, true
	}

	return approvalEntry{}, false
}

// Connectors returns the connectors r's name entries (! entries included)
// name, sorted and without repeats; nil when it has none. A client's
// rule may name only the connectors the client can use: an entry for any
// other would gate nothing.
func (r ApprovalRule) Connectors() []string {
	var names []string

	for _, e := range slices.Concat(r.gate, r.exempt) {
		if e.connector != "" {
			names = append(names, e.connector)
		}
	}

	slices.Sort(names)

	return slices.Compact(names)
}

// Gates reports whether a call to connector's tool (the upstream's name,
// not the qualified one) of class class waits for the owner's approval:
// some entry matches it and no ! entry does. A class other than read or
// write, the empty one included, counts as destructive, as a tool without
// annotations does.
func (r ApprovalRule) Gates(connector, tool string, class ToolClass) bool {
	if class != ClassRead && class != ClassWrite {
		class = ClassDestructive
	}

	for _, e := range r.exempt {
		if e.matches(connector, tool, class) {
			return false
		}
	}

	for _, e := range r.gate {
		if e.matches(connector, tool, class) {
			return true
		}
	}

	return false
}

// approvalGlobMatch is globMatch for an approval entry, except that * and ?
// also match '/'. path.Match stops them at '/', so slack.* would miss a tool
// named admin/delete and leave it ungated. A tools list keeps globMatch,
// where that miss hides the tool: it fails closed. Character classes and
// escapes are path.Match's, which already match '/'. The glob must have
// passed checkGlob; a malformed one matches nothing.
func approvalGlobMatch(glob, name string) bool {
	var (
		g, n int
		// star is where the glob resumes after its last *, or -1; retry is
		// where in name that * next tries to stop.
		star, retry = -1, 0
	)

	for n < len(name) {
		if g < len(glob) && glob[g] == '*' {
			g++
			star, retry = g, n

			continue
		}

		if g < len(glob) {
			if gw, nw, ok := matchOne(glob[g:], name[n:]); ok {
				g, n = g+gw, n+nw

				continue
			}
		}

		if star < 0 {
			return false
		}

		_, w := utf8.DecodeRuneInString(name[retry:])
		retry += w
		g, n = star, retry
	}

	for g < len(glob) && glob[g] == '*' {
		g++
	}

	return g == len(glob)
}

// matchOne matches the glob's first element, which is not *, against the
// first rune of name, which is not empty. It returns the bytes each used.
func matchOne(glob, name string) (gw, nw int, ok bool) {
	r, nw := utf8.DecodeRuneInString(name)

	switch glob[0] {
	case '?':
		return 1, nw, true
	case '[':
		end := classEnd(glob)

		if end < 0 {
			return 0, 0, false
		}

		matched, err := pathpkg.Match(glob[:end+1], string(r))

		return end + 1, nw, err == nil && matched
	case '\\':
		if len(glob) < 2 {
			return 0, 0, false
		}

		lit, lw := utf8.DecodeRuneInString(glob[1:])

		return 1 + lw, nw, lit == r
	default:
		lit, lw := utf8.DecodeRuneInString(glob)

		return lw, nw, lit == r
	}
}

// classEnd is the index of the ']' that closes the character class glob
// starts with, or -1. In a well-formed glob the first unescaped ']' closes
// it: path.Match refuses one as a range's bound.
func classEnd(glob string) int {
	for i := 1; i < len(glob); i++ {
		switch glob[i] {
		case '\\':
			i++
		case ']':
			return i
		}
	}

	return -1
}
