package config

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/atomicpages/wayfindr/internal/redact"
)

// Template is a config string that may contain ${env:NAME} and
// ${secret:name} references (design §4). It holds the text exactly as
// written; Load checks its syntax but never resolves it, so a changed
// environment variable or `wayfindr secret set` takes effect on the next
// Resolve without a reload.
//
// A "$" not followed by "{" is literal. There is no escape for a literal
// "${".
type Template string

// RefKind is the source a reference reads from.
type RefKind string

// Reference kinds.
const (
	RefEnv    RefKind = "env"
	RefSecret RefKind = "secret"
)

// Ref is one ${kind:name} reference inside a Template.
type Ref struct {
	Kind RefKind
	Name string
}

// String renders the reference as it is written in config, e.g.
// "${secret:slack_client_secret}". It never contains the value.
func (r Ref) String() string { return "${" + string(r.Kind) + ":" + r.Name + "}" }

// Resolver supplies the value of a reference. Implementations return an
// error wrapping ErrNotFound when the variable or secret does not exist, and
// ErrUnreadable when a secret exists but can't be read.
type Resolver interface {
	Resolve(ctx context.Context, ref Ref) (string, error)
}

// ResolverFunc adapts a function to Resolver.
type ResolverFunc func(ctx context.Context, ref Ref) (string, error)

// Resolve calls f.
func (f ResolverFunc) Resolve(ctx context.Context, ref Ref) (string, error) { return f(ctx, ref) }

// ErrNotFound is what a Resolver wraps when a referenced environment
// variable or secret does not exist.
var ErrNotFound = errors.New("reference not found")

// ErrUnreadable is what a Resolver wraps when a referenced secret exists
// but can't be read: it was sealed under another master key, tampered
// with, or stored in a format this build doesn't know. Retrying can't
// help; storing the secret again fixes it.
var ErrUnreadable = errors.New("reference unreadable")

// ResolveError reports which reference could not be resolved, so the caller
// can name it in the connector's auth_error reason (design §4).
type ResolveError struct {
	Ref Ref
	Err error
}

func (e *ResolveError) Error() string { return fmt.Sprintf("resolve %s: %v", e.Ref, e.Err) }

// Unwrap returns the resolver's error.
func (e *ResolveError) Unwrap() error { return e.Err }

// TemplateError is a syntax error in a Template.
type TemplateError struct {
	// Offset is the byte offset of the offending "${".
	Offset int
	Reason string
}

func (e *TemplateError) Error() string {
	return fmt.Sprintf("bad reference at offset %d: %s", e.Offset, e.Reason)
}

// Refs returns the references in t, in order. It fails with a
// *TemplateError if t is malformed.
func (t Template) Refs() ([]Ref, error) {
	segs, err := t.parse()

	if err != nil {
		return nil, err
	}

	var refs []Ref

	for _, sg := range segs {
		if sg.isRef {
			refs = append(refs, sg.ref)
		}
	}

	return refs, nil
}

// Resolve returns t with every reference replaced by its value from r.
// Values are inserted verbatim and are not themselves expanded. A malformed
// template fails with a *TemplateError before r is called; a resolver
// failure is returned as a *ResolveError naming the reference.
func (t Template) Resolve(ctx context.Context, r Resolver) (string, error) {
	s, _, err := t.ResolveRefs(ctx, r)

	return s, err
}

// Resolved is one reference a template was resolved with, and its value.
type Resolved struct {
	Ref   Ref
	Value string
}

// ResolveRefs is [Template.Resolve] that also returns each reference it
// resolved and the value it inserted, for [Mask]: text that may quote the
// result, such as an error with the resolved url, must not show the values
// (wayfindr-wt2v).
func (t Template) ResolveRefs(ctx context.Context, r Resolver) (string, []Resolved, error) {
	segs, err := t.parse()

	if err != nil {
		return "", nil, err
	}

	var b strings.Builder
	var used []Resolved

	for _, sg := range segs {
		if !sg.isRef {
			b.WriteString(sg.lit)
			continue
		}

		v, err := r.Resolve(ctx, sg.ref)

		if err != nil {
			return "", nil, &ResolveError{Ref: sg.ref, Err: err}
		}

		b.WriteString(v)
		used = append(used, Resolved{Ref: sg.ref, Value: v})
	}

	return b.String(), used, nil
}

// Mask returns the [redact.Values] that replaces each resolved value with
// its reference, so "https://h/s/k-123/mcp" reads "https://h/s/${secret:zap}/mcp".
func Mask(resolved []Resolved) redact.Values {
	pairs := make([]string, 0, 2*len(resolved))

	for _, rv := range resolved {
		pairs = append(pairs, rv.Value, rv.Ref.String())
	}

	return redact.NewValues(pairs...)
}

// segment is either literal text or one reference.
type segment struct {
	lit   string
	ref   Ref
	isRef bool
}

func (t Template) parse() ([]segment, error) {
	var segs []segment
	s := string(t)
	base := 0

	for {
		i := strings.Index(s, "${")

		if i < 0 {
			if s != "" {
				segs = append(segs, segment{lit: s})
			}

			return segs, nil
		}

		if i > 0 {
			segs = append(segs, segment{lit: s[:i]})
		}

		off := base + i
		end := strings.IndexByte(s[i:], '}')

		if end < 0 {
			return nil, &TemplateError{Offset: off, Reason: "missing closing \"}\""}
		}

		r, err := parseRef(s[i+2 : i+end])

		if err != nil {
			return nil, &TemplateError{Offset: off, Reason: err.Error()}
		}

		segs = append(segs, segment{ref: r, isRef: true})
		s = s[i+end+1:]
		base = off + end + 1
	}
}

func parseRef(body string) (Ref, error) {
	kind, name, ok := strings.Cut(body, ":")

	if !ok {
		return Ref{}, fmt.Errorf("%q is not ${env:NAME} or ${secret:name}", "${"+body+"}")
	}

	switch RefKind(kind) {
	case RefEnv:
		if !isEnvName(name) {
			return Ref{}, fmt.Errorf("env name %q must match [A-Za-z_][A-Za-z0-9_]*", name)
		}
	case RefSecret:
		if !IsSecretRefName(name) {
			return Ref{}, fmt.Errorf("secret name %q must match %s", name, SecretRefGrammar)
		}
	default:
		return Ref{}, fmt.Errorf("unknown reference kind %q (want env or secret)", kind)
	}

	return Ref{Kind: RefKind(kind), Name: name}, nil
}

func isEnvName(s string) bool {
	if s == "" {
		return false
	}

	for i, c := range s {
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}

	return true
}

// SecretNameGrammar is the rule a new secret's name follows (design §4):
// the connector-name rule, so a secret name is lowercase and at most 63
// bytes like every other name Wayfindr keeps.
const SecretNameGrammar = NameGrammar

// IsSecretName reports whether s may name a new secret (SecretNameGrammar).
func IsSecretName(s string) bool { return IsConnectorName(s) }

// SecretRefGrammar is the wider rule secret names followed before they
// took the connector-name rule (wayfindr-vw6h). A ${secret:…} reference
// may still use it, so configs written then still load, and a secret
// already known by such a name can still be set (CheckSecretName).
const SecretRefGrammar = "[A-Za-z0-9][A-Za-z0-9_.-]*" //nolint:gosec // G101: a grammar, not a credential

// IsSecretRefName reports whether s matches SecretRefGrammar. Every name
// IsSecretName accepts matches it too.
func IsSecretRefName(s string) bool {
	if s == "" {
		return false
	}

	for i, c := range s {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '_' || c == '.' || c == '-') && i > 0:
		default:
			return false
		}
	}

	return true
}

// CheckSecretName returns why a secret can't be set as name, or nil. A new
// secret's name follows SecretNameGrammar. A known secret, one that is
// stored or that the config refers to, may keep a name it got under
// SecretRefGrammar, so setting it again keeps working. The error names
// only SecretNameGrammar: that is the rule to pick a name by.
func CheckSecretName(name string, known bool) error {
	if IsSecretName(name) || known && IsSecretRefName(name) {
		return nil
	}

	// %.64q bounds the echoed name.
	return fmt.Errorf("secret name %.64q must match %s", name, SecretNameGrammar)
}

// SecretNameFor is the secret `wayfindr connector add` stores the value of
// a connector's header or environment variable key under:
// <connector>_<key>, lowercased, with each rune a secret name can't hold
// replaced by _, and cut to the 63 bytes SecretNameGrammar allows. A valid
// connector name keeps the result a valid secret name.
func SecretNameFor(connector, key string) string {
	return secretNameFor(connector, key, "")
}

// NumberedSecretNameFor is SecretNameFor(connector, key) followed by _<n>,
// for when that name is taken. The <connector>_<key> part is cut shorter
// when it must be, so the result is still a valid secret name.
func NumberedSecretNameFor(connector, key string, n int) string {
	return secretNameFor(connector, key, "_"+strconv.Itoa(n))
}

func secretNameFor(connector, key, suffix string) string {
	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, strings.ToLower(connector+"_"+key))

	// Every rune is ASCII by now, so the cut splits none.
	if limit := maxNameLen - len(suffix); len(base) > limit {
		base = base[:limit]
	}

	return base + suffix
}
