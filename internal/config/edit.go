package config

import (
	"bytes"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// NewConnector is a connector table as AppendConnector writes it: the keys
// `wayfindr connector add` and import set. A zero field is left out of the
// file, so the key takes its default. It is not Connector because Connector
// has no omitempty: encoding one would write pkce, validate_issuer and
// resource_indicator as false, turning off what an absent key defaults to.
type NewConnector struct {
	Name string `toml:"-"`

	URL       Template            `toml:"url,omitempty"`
	Headers   map[string]Template `toml:"headers,omitempty"`
	Transport string              `toml:"transport,omitempty"`
	Auth      *NewAuth            `toml:"auth,omitempty"`
	TLS       *TLS                `toml:"tls,omitempty"`

	Command string              `toml:"command,omitempty"`
	Args    []Template          `toml:"args,omitempty"`
	Env     map[string]Template `toml:"env,omitempty"`

	// Timeout is nil for the default. A pointer, because the encoder does
	// not leave out a zero struct.
	Timeout *Duration `toml:"timeout,omitempty"`
}

// NewAuth is the auth table of a NewConnector.
type NewAuth struct {
	Type                    string            `toml:"type"`
	Grant                   Grant             `toml:"grant,omitempty"`
	AuthorizationServer     string            `toml:"authorization_server,omitempty"`
	ClientID                string            `toml:"client_id,omitempty"`
	ClientSecret            Template          `toml:"client_secret,omitempty"`
	TokenEndpointAuthMethod ClientAuthMethod  `toml:"token_endpoint_auth_method,omitempty"`
	Scopes                  []string          `toml:"scopes,omitempty"`
	AuthorizeParams         map[string]string `toml:"authorize_params,omitempty"`
	SigningKey              *SigningKey       `toml:"signing_key,omitempty"`
	Assertion               *Assertion        `toml:"assertion,omitempty"`
}

// AppendConnector returns src with c's table appended after one blank line.
// Everything before it is kept byte for byte. It fails with the
// *ValidationError of c's table if that is invalid on its own, and
// otherwise with an [*Error]: CaseConnectorExists if src has a connector by
// that name, or a KindUnsafe Case if src doesn't parse, its connectors are
// an inline table, or the result would differ from src in anything but the
// new connector.
func AppendConnector(src []byte, c NewConnector, opts Options) ([]byte, error) {
	before, err := Parse(src, opts)

	if err != nil {
		return nil, &Error{Kind: KindUnsafe, Case: CaseUnparsable, Connector: c.Name, Err: err}
	}

	table, err := renderConnector(c)

	if err != nil {
		return nil, err
	}

	alone, err := Parse(table, opts)

	if err != nil {
		return nil, err
	}

	if _, ok := before.Connectors[c.Name]; ok {
		return nil, &Error{Kind: KindConflict, Case: CaseConnectorExists, Connector: c.Name}
	}

	// The decoder lets a [connectors.<name>] header extend an inline
	// connectors = { … }, but TOML forbids it, so other tools would reject
	// the file. Inline tables are one line, before the first header.
	if inlineConnectors(src) {
		return nil, &Error{Kind: KindUnsafe, Case: CaseInlineConnectors, Connector: c.Name}
	}

	out := bytes.Clone(src)

	if len(out) > 0 {
		if out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}

		out = append(out, '\n')
	}

	out = append(out, table...)
	added, ok, err := onlyChanged(before, out, c.Name, opts)

	if err != nil {
		return nil, err
	}

	if !ok || !connectorEqual(added, alone.Connectors[c.Name]) {
		return nil, &Error{Kind: KindUnsafe, Case: CaseReadsDifferently, Connector: c.Name}
	}

	return out, nil
}

// onlyChanged parses out and requires it to equal before in everything but
// connector name, which it returns as out has it; ok is false when out has
// no such connector.
func onlyChanged(before *Config, out []byte, name string, opts Options) (c Connector, ok bool, err error) {
	after, err := Parse(out, opts)

	if err != nil {
		return Connector{}, false, &Error{Kind: KindUnsafe, Case: CaseUnparsable, Connector: name, Err: err}
	}

	c, ok = after.Connectors[name]
	delete(after.Connectors, name)
	want := *before
	want.Connectors = maps.Clone(before.Connectors)
	delete(want.Connectors, name)

	if !reflect.DeepEqual(&want, after) {
		return Connector{}, false, &Error{Kind: KindUnsafe, Case: CaseChangesOthers, Connector: name}
	}

	return c, ok, nil
}

// renderConnector encodes c as a [connectors.<name>] table and its
// subtables, ending in a newline. It decodes the table back and requires
// c, so a key the encoder dropped or changed can't reach the file.
func renderConnector(c NewConnector) ([]byte, error) {
	var b bytes.Buffer
	enc := toml.NewEncoder(&b)
	enc.Indent = ""

	if err := enc.Encode(map[string]map[string]NewConnector{"connectors": {c.Name: c}}); err != nil {
		return nil, fmt.Errorf("encode connector %s: %w", c.Name, err)
	}

	var back map[string]map[string]NewConnector

	if _, err := toml.Decode(b.String(), &back); err != nil {
		return nil, fmt.Errorf("encode connector %s: decode it back: %w", c.Name, err)
	}

	got := back["connectors"][c.Name]
	got.Name = c.Name

	if !newConnectorEqual(got, c) {
		return nil, fmt.Errorf("encode connector %s: the table does not decode back to it", c.Name)
	}

	// The encoder opens with the parent table's header, which src may
	// already define; the connector's own header implies it.
	out, ok := bytes.CutPrefix(b.Bytes(), []byte("[connectors]\n"))

	if !ok {
		return nil, fmt.Errorf("encode connector %s: unexpected output", c.Name)
	}

	return out, nil
}

// newConnectorEqual reports whether a and b are the same table. Like
// connectorEqual, it treats a nil map or slice as empty, which omitempty
// makes the same in the file.
func newConnectorEqual(a, b NewConnector) bool {
	return a.Name == b.Name &&
		a.URL == b.URL &&
		maps.Equal(a.Headers, b.Headers) &&
		a.Transport == b.Transport &&
		newAuthEqual(a.Auth, b.Auth) &&
		a.Command == b.Command &&
		slices.Equal(a.Args, b.Args) &&
		maps.Equal(a.Env, b.Env) &&
		(a.Timeout == nil) == (b.Timeout == nil) &&
		(a.Timeout == nil || *a.Timeout == *b.Timeout)
}

func newAuthEqual(a, b *NewAuth) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.Type == b.Type && a.Grant == b.Grant && a.ClientID == b.ClientID && a.ClientSecret == b.ClientSecret &&
		a.TokenEndpointAuthMethod == b.TokenEndpointAuthMethod &&
		slices.Equal(a.Scopes, b.Scopes) && maps.Equal(a.AuthorizeParams, b.AuthorizeParams) &&
		ptrEqual(a.SigningKey, b.SigningKey) && ptrEqual(a.Assertion, b.Assertion)
}

// RemoveConnector returns src without the [connectors.<name>] table, its
// subtables, and the comment lines directly above each, and the connector
// it removed. Everything else is kept byte for byte. It fails with an
// [*Error]: CaseConnectorNotFound if src has no such connector, or a
// KindUnsafe Case if src doesn't parse, the connector is not written as
// tables the edit can find (a quoted or dotted key, an inline table), or
// the result would differ from src in anything but the removed connector.
func RemoveConnector(src []byte, name string, opts Options) ([]byte, Connector, error) {
	before, err := Parse(src, opts)

	if err != nil {
		return nil, Connector{}, &Error{Kind: KindUnsafe, Case: CaseUnparsable, Connector: name, Err: err}
	}

	removed, ok := before.Connectors[name]

	if !ok {
		return nil, Connector{}, &Error{Kind: KindConflict, Case: CaseConnectorNotFound, Connector: name}
	}

	lines := bytes.SplitAfter(src, []byte("\n"))
	segs := tableSpans(lines)
	drop := make([]bool, len(lines))
	found := false
	last := 0 // end of the last dropped span

	for _, s := range segs {
		if !isConnectorTable(s.key, name) {
			continue
		}

		found = true
		last = s.end

		for i := s.start; i < s.end; i++ {
			drop[i] = true
		}
	}

	if !found {
		return nil, Connector{}, &Error{Kind: KindUnsafe, Case: CaseNotTables, Connector: name}
	}

	// A table at the end of the file goes with the blank line that set it
	// off, the one AppendConnector adds.
	if last == len(lines) {
		i := len(lines) - 1

		for i > 0 && drop[i-1] {
			i--
		}

		if i > 0 && isBlank(lines[i-1]) {
			drop[i-1] = true
		}
	}

	var out []byte

	for i, l := range lines {
		if !drop[i] {
			out = append(out, l...)
		}
	}

	_, left, err := onlyChanged(before, out, name, opts)

	if err != nil {
		return nil, Connector{}, err
	}

	if left {
		return nil, Connector{}, &Error{Kind: KindUnsafe, Case: CaseDefinedElsewhere, Connector: name}
	}

	return out, removed, nil
}

// tableSpan is a table header's span of lines, [start, end): the comment
// lines directly above the header, the header, and its body up to the
// next span.
type tableSpan struct {
	start, end int
	key        []string
}

// tableSpans splits lines at table headers. Lines before the first header
// are in no span. A line inside a multi-line string or array that looks
// like a header splits wrongly; the callers' parse check refuses the
// result.
func tableSpans(lines [][]byte) []tableSpan {
	var segs []tableSpan

	for i, l := range lines {
		key, ok := headerKey(string(l))

		if !ok {
			continue
		}

		start := i

		for start > 0 && isComment(lines[start-1]) {
			start--
		}

		if n := len(segs); n > 0 {
			segs[n-1].end = start
		}

		segs = append(segs, tableSpan{start: start, end: len(lines), key: key})
	}

	return segs
}

// headerKey reads the key of a table or array-of-tables header line. ok is
// false for a line that is not a header. A header whose key can't be read
// has a nil key. Escapes in quoted parts are not decoded: no connector
// name needs them.
func headerKey(line string) (key []string, ok bool) {
	s := strings.TrimSpace(line)

	if !strings.HasPrefix(s, "[") {
		return nil, false
	}

	s = strings.TrimPrefix(strings.TrimPrefix(s, "["), "[")

	var part strings.Builder

	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\'':
			end := strings.IndexByte(s[i+1:], c)

			if end < 0 {
				return nil, true
			}

			part.WriteString(s[i+1 : i+1+end])
			i += end + 1
		case '.':
			key = append(key, strings.TrimSpace(part.String()))
			part.Reset()
		case ']':
			return append(key, strings.TrimSpace(part.String())), true
		default:
			part.WriteByte(c)
		}
	}

	return nil, true
}

// inlineConnectors reports whether a line before the first table header
// assigns the connectors key itself.
func inlineConnectors(src []byte) bool {
	for l := range bytes.Lines(src) {
		if _, ok := headerKey(string(l)); ok {
			return false
		}

		key, _, ok := bytes.Cut(l, []byte("="))

		if ok && strings.Trim(string(bytes.TrimSpace(key)), `"'`) == "connectors" {
			return true
		}
	}

	return false
}

// isConnectorTable reports whether key is connectors.<name> or a subtable
// of it.
func isConnectorTable(key []string, name string) bool {
	return len(key) >= 2 && key[0] == "connectors" && key[1] == name
}

func isComment(l []byte) bool { return bytes.HasPrefix(bytes.TrimSpace(l), []byte("#")) }

func isBlank(l []byte) bool { return len(bytes.TrimSpace(l)) == 0 }
