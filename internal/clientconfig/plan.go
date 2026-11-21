package clientconfig

import (
	"fmt"
	"maps"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/atomicpages/wayfindr/internal/config"
)

// Options are what Map needs to know beyond the servers.
type Options struct {
	// Existing are the connectors already in the config file.
	Existing config.Connectors
	// Listen is the daemon's listen address, so an entry pointing at
	// Wayfindr itself is skipped.
	Listen string
	// LookupEnv reads the environment of the shell running the import,
	// which is the one the client expands ${VAR} from. Nil means none is
	// set.
	LookupEnv func(key string) (string, bool)
	// PlainEnv names environment variables kept as plain text instead of
	// being stored as secrets.
	PlainEnv []string
	// Secrets are the secret names the config already refers to besides
	// Existing's, such as [embedding]'s api_key, so a new secret is never
	// stored over one of them (wayfindr-eadp).
	Secrets []string
}

// Plan is what an import would do.
type Plan struct {
	// Connectors are the connectors to add, sorted by name.
	Connectors []Planned
	// Skips are the entries not imported, sorted by name.
	Skips []Skip
}

// Planned is one connector to add.
type Planned struct {
	// From is the entry it comes from.
	From      Server
	Connector config.NewConnector
	// Secrets are the secrets its table refers to, to store before the
	// table is written.
	Secrets []Secret
	// Notes say what changed on the way, one line each.
	Notes []string
}

// Secret is a secret to store. Value holds literal text and VS Code
// inputs, which are prompted for; it never holds an environment variable.
type Secret struct {
	Name  string
	Label string // what it holds, such as "header Authorization"
	Value Value
}

// Skip is an entry that is not imported, and why.
type Skip struct {
	From   Server
	Reason string
}

// Map turns servers read from a client into the connectors to add. Where a
// name is in more than one scope, the narrowest wins. Literal values in env
// and headers become secrets, as do ${VAR} values the importing shell sets;
// ${VAR} the shell does not set, and every ${VAR} in args and url, become
// ${env:VAR}.
func Map(servers []Server, opts Options) Plan {
	var plan Plan
	taken := map[string]string{} // connector name → the entry that took it
	// used are the secret names taken, by existing connectors or by this
	// import, so no secret is stored over another connector's.
	used := map[string]bool{}

	for _, c := range opts.Existing {
		for _, name := range c.Secrets() {
			used[name] = true
		}
	}

	for _, name := range opts.Secrets {
		used[name] = true
	}

	for _, s := range narrowest(servers) {
		p, reason := mapServer(s, opts, taken, used)

		if reason != "" {
			plan.Skips = append(plan.Skips, Skip{From: s, Reason: reason})

			continue
		}

		taken[p.Connector.Name] = s.Name
		plan.Connectors = append(plan.Connectors, p)
	}

	return plan
}

// narrowest returns servers sorted by name, keeping for each connector
// name only the entries in the narrowest scope that has it. Entries in that
// scope that become the same connector are all kept, for Map to report.
func narrowest(servers []Server) []Server {
	best := map[string]int{}

	for _, s := range servers {
		name := ConnectorName(s.Name)
		best[name] = max(best[name], s.Scope.rank())
	}

	var out []Server

	for _, s := range servers {
		if s.Scope.rank() == best[ConnectorName(s.Name)] {
			out = append(out, s)
		}
	}

	slices.SortStableFunc(out, func(a, b Server) int { return strings.Compare(a.Name, b.Name) })

	return out
}

// mapServer maps one entry, or says why it is skipped.
func mapServer(s Server, opts Options, taken map[string]string, used map[string]bool) (Planned, string) {
	if s.Disabled {
		return Planned{}, "it is turned off in the client"
	}

	if s.Unapproved {
		return Planned{}, "it comes from the project and the client has not approved it"
	}

	if len(s.Unsupported) > 0 {
		return Planned{}, fmt.Sprintf("it sets %s, which a connector can't express", strings.Join(s.Unsupported, ", "))
	}

	name := ConnectorName(s.Name)

	if !config.IsConnectorName(name) {
		return Planned{}, fmt.Sprintf("%q can't be made a connector name ([a-z0-9][a-z0-9_-]{0,62})", s.Name)
	}

	if isSelf(s, opts.Listen) {
		return Planned{}, "this is Wayfindr itself"
	}

	if _, ok := opts.Existing[name]; ok {
		return Planned{}, fmt.Sprintf("a connector called %s already exists", name)
	}

	if other, ok := taken[name]; ok {
		return Planned{}, fmt.Sprintf("%s also becomes connector %s", other, name)
	}

	m := mapper{conn: name, opts: opts, used: used, p: Planned{From: s, Connector: config.NewConnector{Name: name}}}

	if reason := m.transport(s); reason != "" {
		return Planned{}, reason
	}

	if err := m.fields(s); err != nil {
		return Planned{}, err.Error()
	}

	if bin := filepath.Base(s.Command); bin == "npx" || bin == "uvx" || bin == "bunx" {
		m.note(fmt.Sprintf("%s downloads the server on every start", bin))
	}

	if u, err := url.Parse(string(m.p.Connector.URL)); err == nil && u.RawQuery != "" {
		m.note("the url's query is written as is; if it holds a key, move the url into a secret")
	}

	// The names are final only now: commit them.
	for _, sec := range m.p.Secrets {
		used[sec.Name] = true
	}

	return m.p, ""
}

// ConnectorName is the connector an entry called name becomes: lowercased,
// with each character outside [a-z0-9_-] replaced by "-". The result may
// still not be a valid connector name.
func ConnectorName(name string) string {
	return strings.Map(func(r rune) rune {
		if ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') || r == '_' || r == '-' {
			return r
		}

		return '-'
	}, strings.ToLower(name))
}

// isSelf reports whether s is Wayfindr's own entry: the stdio shim, or a
// URL on the daemon's listen address.
func isSelf(s Server, listen string) bool {
	if s.Command != "" {
		bin := strings.TrimSuffix(filepath.Base(s.Command), ".exe")
		first := ""

		if len(s.Args) > 0 {
			first, _ = s.Args[0].Literal()
		}

		return bin == "wayfindr" && first == "stdio"
	}

	raw, ok := s.URL.Literal()

	if !ok || listen == "" {
		return false
	}

	u, err := url.Parse(raw)

	if err != nil {
		return false
	}

	lhost, lport, err := net.SplitHostPort(listen)

	if err != nil || u.Port() != lport {
		return false
	}

	return u.Hostname() == lhost || (loopback(u.Hostname()) && loopback(lhost))
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// mapper builds one Planned.
type mapper struct {
	conn string
	opts Options
	used map[string]bool // secret names taken outside this connector
	p    Planned
}

func (m *mapper) note(n string) {
	if !slices.Contains(m.p.Notes, n) {
		m.p.Notes = append(m.p.Notes, n)
	}
}

// transport checks that the entry is either stdio or HTTP and sets the
// connector's transport.
func (m *mapper) transport(s Server) string {
	hasURL := len(s.URL) > 0

	switch {
	case s.Command != "" && hasURL:
		return "it has both a command and a url"
	case s.Command == "" && !hasURL:
		return "it has neither a command nor a url"
	case s.Command != "":
		if s.Type != "" && s.Type != "stdio" {
			return fmt.Sprintf("it has a command but type %q", s.Type)
		}

		return ""
	}

	switch s.Type {
	case "", "http", "streamable-http", "streamableHttp":
	case "sse":
		m.p.Connector.Transport = config.TransportSSE
	default:
		return fmt.Sprintf("type %q is not one a connector supports", s.Type)
	}

	return ""
}

// fields maps command, args, url, env and headers.
func (m *mapper) fields(s Server) error {
	c := &m.p.Connector

	c.Command = s.Command

	if s.OAuth {
		c.Auth = &config.NewAuth{Type: config.AuthTypeOAuth, ClientID: s.OAuthClientID, Scopes: s.OAuthScopes}
	}

	if s.Timeout > 0 {
		c.Timeout = &config.Duration{Duration: s.Timeout}
	}

	for i, a := range s.Args {
		t, err := m.template(a, fmt.Sprintf("argument %d", i+1), false)

		if err != nil {
			return err
		}

		c.Args = append(c.Args, t)
	}

	if len(s.URL) > 0 {
		t, err := m.template(s.URL, "url", false)

		if err != nil {
			return err
		}

		c.URL = t
	}

	var err error

	if c.Env, err = m.values(s.Env, "environment variable", true); err != nil {
		return err
	}

	c.Headers, err = m.values(s.Headers, "header", false)

	return err
}

// template writes v as a config template: literal text as is, a variable
// as ${env:VAR}, and an input as a secret. With secretLits, literal text
// other than an auth scheme such as "Bearer " is stored as a secret too.
func (m *mapper) template(v Value, what string, secretLits bool) (config.Template, error) {
	var b strings.Builder

	for _, p := range v {
		if secretLits && p.Env == "" && p.Input == "" && !authScheme(p.Lit) {
			name := m.secret(secretKey(what), what, Value{p})
			b.WriteString(config.Ref{Kind: config.RefSecret, Name: name}.String())

			continue
		}

		switch {
		case p.Env != "":
			b.WriteString(config.Ref{Kind: config.RefEnv, Name: p.Env}.String())
			m.note(fmt.Sprintf("the daemon's environment must set %s", p.Env))

			if p.HasDefault {
				m.note(fmt.Sprintf("the default of %s is dropped", p.Env))
			}
		case p.Input != "":
			name := m.secret(p.Input, "input "+p.Input, Value{p})
			b.WriteString(config.Ref{Kind: config.RefSecret, Name: name}.String())
		default:
			if strings.Contains(p.Lit, "${") {
				return "", fmt.Errorf("its %s has a literal ${, which a connector can't write", what)
			}

			b.WriteString(p.Lit)
		}
	}

	return config.Template(b.String()), nil
}

// values maps env or headers. A value whose variables the importing shell
// sets (or that has none) is stored as a secret, unless it is a --plain-env
// variable; otherwise it is written as a template.
func (m *mapper) values(vs map[string]Value, kind string, env bool) (map[string]config.Template, error) {
	if len(vs) == 0 {
		return nil, nil
	}

	out := make(map[string]config.Template, len(vs))

	for _, k := range slices.Sorted(maps.Keys(vs)) {
		v := vs[k]
		plain := env && slices.Contains(m.opts.PlainEnv, k)
		resolved, ok := m.resolve(v)

		if !ok {
			for _, p := range v {
				if p.Env != "" {
					if _, set := m.lookupEnv(p.Env); !set && !p.HasDefault {
						m.note(fmt.Sprintf("%s is not set in this shell", p.Env))
					}
				}
			}

			t, err := m.template(v, kind+" "+k, !plain)

			if err != nil {
				return nil, err
			}

			out[k] = t

			continue
		}

		for _, p := range v {
			if p.Env != "" {
				if plain {
					m.note(fmt.Sprintf("%s %s holds the value of %s from this shell", kind, k, p.Env))
				} else {
					m.note(fmt.Sprintf("%s %s stores the value of %s from this shell", kind, k, p.Env))
				}
			}
		}

		text, literal := resolved.Literal()

		switch {
		case literal && (plain || text == ""):
			if strings.Contains(text, "${") {
				return nil, fmt.Errorf("its %s %s has a literal ${, which a connector can't write", kind, k)
			}

			out[k] = config.Template(text)
		default:
			name := m.secret(k, kind+" "+k, resolved)
			out[k] = config.Template(config.Ref{Kind: config.RefSecret, Name: name}.String())
		}
	}

	return out, nil
}

// resolve replaces each variable in v with its value in the importing
// shell, or its default. ok is false if one is set nowhere.
func (m *mapper) resolve(v Value) (Value, bool) {
	var out Value

	for _, p := range v {
		if p.Input != "" {
			out = append(out, p)

			continue
		}

		if p.Env == "" {
			out = appendLit(out, p.Lit)

			continue
		}

		val, set := m.lookupEnv(p.Env)

		switch {
		case set:
		case p.HasDefault:
			val = p.Default
		default:
			return nil, false
		}

		out = appendLit(out, val)
	}

	return out, true
}

// appendLit adds literal text to v, joining it to a literal before it.
func appendLit(v Value, s string) Value {
	switch {
	case s == "":
		return v
	case len(v) > 0 && v[len(v)-1].Env == "" && v[len(v)-1].Input == "":
		v[len(v)-1].Lit += s

		return v
	}

	return append(v, Part{Lit: s})
}

func (m *mapper) lookupEnv(k string) (string, bool) {
	if m.opts.LookupEnv == nil {
		return "", false
	}

	return m.opts.LookupEnv(k)
}

// secret adds a secret named for the connector and key, unique among the
// secrets of existing connectors and of this import, and returns its name.
// An input used twice in a connector is one secret.
func (m *mapper) secret(key, label string, v Value) string {
	name := config.SecretNameFor(m.conn, key)

	for i := 2; ; i++ {
		idx := slices.IndexFunc(m.p.Secrets, func(s Secret) bool { return s.Name == name })

		if idx >= 0 && m.p.Secrets[idx].Label == label && slices.Equal(m.p.Secrets[idx].Value, v) {
			return name
		}

		if idx < 0 && !m.used[name] {
			break
		}

		name = config.NumberedSecretNameFor(m.conn, key, i)
	}

	m.p.Secrets = append(m.p.Secrets, Secret{Name: name, Label: label, Value: v})

	return name
}

// secretKey is the key part of a secret's name for what a value is, such as
// "header Authorization": the last word.
func secretKey(what string) string {
	if i := strings.LastIndexByte(what, ' '); i >= 0 {
		return what[i+1:]
	}

	return what
}

// authScheme reports whether s is only an HTTP auth scheme and a space, such
// as "Bearer ", which needs no protecting.
func authScheme(s string) bool {
	scheme, rest, ok := strings.Cut(s, " ")

	return ok && rest == "" && slices.Contains([]string{"Bearer", "Basic", "Token", "Bot"}, scheme)
}

// Inputs returns the ids of the VS Code inputs the plan's secrets need, in
// order, each once.
func (p Plan) Inputs() []string {
	var ids []string

	for _, pc := range p.Connectors {
		for _, s := range pc.Secrets {
			for _, part := range s.Value {
				if part.Input != "" && !slices.Contains(ids, part.Input) {
					ids = append(ids, part.Input)
				}
			}
		}
	}

	return ids
}

// Resolve returns the secret's value, with each input's value from inputs.
func (s Secret) Resolve(inputs map[string]string) string {
	var b strings.Builder

	for _, part := range s.Value {
		if part.Input != "" {
			b.WriteString(inputs[part.Input])
		} else {
			b.WriteString(part.Lit)
		}
	}

	return b.String()
}
