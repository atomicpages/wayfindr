package clientconfig

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// syntax turns one config string of a client into a Value. A reference the
// client would expand but a connector can't express is an error, and the
// reader reports the setting as unsupported.
type syntax func(s string) (Value, error)

// literalSyntax is for clients that expand nothing: Claude Desktop and Codex.
func literalSyntax(s string) (Value, error) { return Lit(s), nil }

// shellSyntax is Claude Code's: ${VAR} and ${VAR:-default}.
func shellSyntax(s string) (Value, error) {
	return parseRefs(s, func(body string) (Part, error) {
		name, def, hasDef := strings.Cut(body, ":-")

		if !envName(name) {
			return Part{}, fmt.Errorf("%s is not ${VAR} or ${VAR:-default}", describeRef(body))
		}

		return Part{Env: name, Default: def, HasDefault: hasDef}, nil
	})
}

// editorVars are the values of the predefined variables of VS Code and
// Cursor.
type editorVars struct {
	home      string
	workspace string // the folder the client has open: the current directory
	inputs    bool   // whether ${input:id} is allowed (VS Code)
}

// editorSyntax is VS Code's and Cursor's: ${env:NAME}, ${input:id} (VS
// Code), and the predefined ${userHome}, ${workspaceFolder},
// ${workspaceFolderBasename}, ${pathSeparator} and ${/}.
func (ev editorVars) syntax(s string) (Value, error) {
	return parseRefs(s, func(body string) (Part, error) {
		if name, ok := strings.CutPrefix(body, "env:"); ok && envName(name) {
			return Part{Env: name}, nil
		}

		if id, ok := strings.CutPrefix(body, "input:"); ok && ev.inputs && id != "" {
			return Part{Input: id}, nil
		}

		switch body {
		case "userHome":
			return Part{Lit: ev.home}, nil
		case "workspaceFolder":
			return Part{Lit: ev.workspace}, nil
		case "workspaceFolderBasename":
			return Part{Lit: filepath.Base(ev.workspace)}, nil
		case "pathSeparator", "/":
			return Part{Lit: string(filepath.Separator)}, nil
		}

		return Part{}, fmt.Errorf("%s has no equivalent in a connector", describeRef(body))
	})
}

// parseRefs splits s at each ${…}, which ref turns into a Part. A "$" not
// followed by "{" is literal.
func parseRefs(s string, ref func(body string) (Part, error)) (Value, error) {
	var v Value
	var lit strings.Builder

	for s != "" {
		i := strings.Index(s, "${")

		if i < 0 {
			lit.WriteString(s)

			break
		}

		lit.WriteString(s[:i])
		end := strings.IndexByte(s[i:], '}')

		if end < 0 {
			return nil, errors.New("a ${ is not closed")
		}

		p, err := ref(s[i+2 : i+end])

		if err != nil {
			return nil, err
		}

		if p.Env == "" && p.Input == "" {
			lit.WriteString(p.Lit)
		} else {
			if lit.Len() > 0 {
				v = append(v, Part{Lit: lit.String()})
				lit.Reset()
			}

			v = append(v, p)
		}

		s = s[i+end+1:]
	}

	if lit.Len() > 0 {
		v = append(v, Part{Lit: lit.String()})
	}

	return v, nil
}

// envName reports whether s is a name ${env:…} accepts:
// [A-Za-z_][A-Za-z0-9_]*.
func envName(s string) bool {
	if s == "" {
		return false
	}

	for i, c := range s {
		letter := c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')

		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}

	return true
}

// describeRef names the reference ${body} for an error. The body may hold
// a credential, so only a word of letters is shown: ${config:…} for
// ${config:x}, ${foo} for ${foo}, and "a reference" for anything else.
func describeRef(body string) string {
	word, _, hasColon := strings.Cut(body, ":")

	if word == "" || len(word) > 30 || strings.Trim(word, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return "a reference"
	}

	if hasColon {
		return "${" + word + ":…}"
	}

	return "${" + word + "}"
}
