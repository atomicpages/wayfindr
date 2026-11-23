package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// ValidationError lists every problem found in a config: unknown keys and
// rule violations. The daemon keeps running the last good config when a
// reload fails with one (design §7.8).
type ValidationError struct {
	// What is the kind of file found invalid: "" for the config, or
	// "project scope".
	What   string
	Issues []Issue
}

// Issue is one problem, located by its TOML key path.
type Issue struct {
	// Path is the dotted key, e.g. "connectors.slack.auth.pkce".
	Path    string
	Message string
}

func (i Issue) String() string { return i.Path + ": " + i.Message }

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Issues))

	for i, is := range e.Issues {
		parts[i] = is.String()
	}

	what := e.What

	if what == "" {
		what = "config"
	}

	return "invalid " + what + ": " + strings.Join(parts, "; ")
}

// validator collects issues so one load reports all of them.
type validator struct {
	issues []Issue
	// defined reports whether the file sets the key, as
	// toml.MetaData.IsDefined does. Nil when unknown, which counts as not
	// set.
	defined func(key ...string) bool
}

// set reports whether the file sets the key, rather than a default filling
// it.
func (v *validator) set(key ...string) bool { return v.defined != nil && v.defined(key...) }

func (v *validator) add(path, format string, args ...any) {
	v.issues = append(v.issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
}

func (v *validator) err() error { return v.errFor("") }

// errFor is err for a file of kind what (ValidationError.What).
func (v *validator) errFor(what string) error {
	if len(v.issues) == 0 {
		return nil
	}

	return &ValidationError{What: what, Issues: v.issues}
}

// path renders a key path the way TOML (and MetaData.Undecoded) writes it,
// quoting segments that aren't bare keys, e.g. connectors."a.b".
func path(parts ...string) string { return toml.Key(parts).String() }

func (v *validator) config(c *Config) {
	v.server(c.Server)
	v.publicURL(c.Server.PublicURL)
	v.oauth(c.Server)
	v.storage(c.Storage)

	if c.Embedding != nil {
		v.embedding(*c.Embedding)
	}

	if c.Index.RefreshInterval.Duration <= 0 {
		v.add("index.refresh_interval", "must be positive")
	}

	if c.Calls.Retention.Duration < 0 {
		v.add("calls.retention", "must not be negative")
	}

	for _, name := range sortedKeys(c.Connectors) {
		v.connector(name, c.Connectors[name])
	}

	for _, name := range sortedKeys(c.Profiles) {
		v.profile(name, c.Profiles[name], c.Connectors)
	}
}

// profile checks a [profiles.<name>] table as a scope file is checked, and
// that each connector it names is configured. Unlike a scope file, which a
// teammate may write for connectors this user lacks, a profile is the
// user's own, so an unknown connector is a typo or left over.
func (v *validator) profile(name string, s *Scope, connectors Connectors) {
	if !IsProfileName(name) {
		v.add(path("profiles", name), "%v", ErrProfileName)
	}

	if s == nil {
		return
	}

	v.scope([]string{"profiles", name}, s)

	names := slices.Concat(s.Connectors, sortedKeys(s.Tools))
	slices.Sort(names)

	for _, c := range slices.Compact(names) {
		if _, ok := connectors[c]; !ok && IsConnectorName(c) {
			v.add(path("profiles", name), "connector %q is not configured", c)
		}
	}
}

// server enforces the loopback-only bind (ADR 0003, design §9). Only IP
// literals and "localhost" are accepted, so no DNS answer can widen it.
func (v *validator) server(s Server) {
	host, port, err := net.SplitHostPort(s.Listen)

	if err != nil {
		v.add("server.listen", "must be host:port: %v", err)

		return
	}

	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		v.add("server.listen", "port %q must be 1-65535", port)
	}

	if host == "localhost" {
		return
	}

	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		v.add("server.listen", "host %q must be a loopback address such as 127.0.0.1", host)
	}
}

// publicURL checks server.public_url (ADR 0021): empty, or the absolute https
// address of the tunnel, with at most a "/" path. It is informational, so the
// rules only keep it printable and usable as an origin: no userinfo, query, or
// fragment. Messages never quote the value, which may carry a credential.
func (v *validator) publicURL(s string) {
	const at = "server.public_url"

	if s == "" {
		return
	}

	u, err := url.Parse(s)

	if err != nil {
		v.notURL(at, err)

		return
	}

	switch {
	case u.Scheme != "https" || u.Hostname() == "":
		v.add(at, "must be an absolute https URL such as https://wayfindr.example.com")
	case u.User != nil:
		v.add(at, "must not contain a username or password")
	case u.RawQuery != "" || u.ForceQuery || strings.Contains(s, "?"):
		v.add(at, "must not have a query string")
	case u.Fragment != "" || strings.Contains(s, "#"):
		v.add(at, "must not have a fragment")
	case u.Path != "" && u.Path != "/":
		v.add(at, "must not have a path: the tunnel serves the daemon at its root")
	}
}

// oauth checks [server.oauth] (ADR 0022, design §7.11). The entries of clients
// are checked whether or not the server is on, so a bad one is found before it
// is switched on. Messages name an entry by its 1-based position and never
// quote it.
func (v *validator) oauth(s Server) {
	const at = "server.oauth.clients"

	if s.OAuth.Enabled && s.PublicURL == "" {
		v.add("server.oauth.enabled", "needs server.public_url")
	}

	if s.OAuth.Enabled && len(s.OAuth.Clients) == 0 {
		v.add(at, "must list at least one client when server.oauth.enabled is true")
	}

	for i, c := range s.OAuth.Clients {
		n := i + 1

		if slices.Contains(s.OAuth.Clients[:i], c) {
			v.add(at, "entry %d repeats an earlier entry", n)
		}

		u, err := url.Parse(c)

		if err != nil {
			v.add(at, "entry %d is not a URL", n)

			continue
		}

		switch {
		case u.Scheme != "https" || u.Hostname() == "":
			v.add(at, "entry %d must be an absolute https URL with a host", n)
		case u.User != nil:
			v.add(at, "entry %d must not contain a username or password", n)
		case u.RawQuery != "" || u.ForceQuery || strings.Contains(c, "?"):
			v.add(at, "entry %d must not have a query string", n)
		case u.Fragment != "" || strings.Contains(c, "#"):
			v.add(at, "entry %d must not have a fragment", n)
		case u.Path == "" || u.Path == "/":
			v.add(at, "entry %d must have a path: a client ID is the URL of its metadata document", n)
		case !canonicalClientID(c, u):
			v.add(at, "entry %d is not in canonical form", n)
		}
	}
}

// canonicalClientID reports whether entry, which parsed as u, is the one
// spelling of its URL a client ID may have: it round-trips through the URL
// parser, its scheme and host are lowercase, and no path segment is "." or
// "..". A client ID is compared as a string against what a client presents,
// so two spellings of one URL must not both be allowed.
func canonicalClientID(entry string, u *url.URL) bool {
	if u.String() != entry || u.Scheme != strings.ToLower(u.Scheme) || u.Host != strings.ToLower(u.Host) {
		return false
	}

	return !slices.ContainsFunc(strings.Split(u.Path, "/"), func(seg string) bool { return seg == "." || seg == ".." })
}

func (v *validator) storage(s Storage) {
	switch s.Driver {
	case DriverSQLite:
	case DriverPostgres:
		if s.DSN == "" {
			v.add("storage.dsn", "is required for the postgres driver")
		}
	default:
		v.add("storage.driver", "must be %q or %q, not %q", DriverSQLite, DriverPostgres, s.Driver)
	}
}

func (v *validator) embedding(e Embedding) {
	if e.BaseURL == "" {
		v.add("embedding.base_url", "is required when [embedding] is present")
	} else {
		v.httpURL("embedding.base_url", e.BaseURL)
	}

	if e.Model == "" {
		v.add("embedding.model", "is required when [embedding] is present")
	}

	if e.Dimensions <= 0 {
		v.add("embedding.dimensions", "must be a positive integer")
	}

	v.template("embedding.api_key", e.APIKey)

	if e.Timeout.Duration <= 0 {
		v.add("embedding.timeout", "must be positive")
	}
}

// connector checks the name, the fields each transport allows, templates,
// and auth.
func (v *validator) connector(name string, c Connector) {
	p := func(keys ...string) string { return path(append([]string{"connectors", name}, keys...)...) }

	if !IsConnectorName(name) {
		v.add(p(), "connector name must match "+NameGrammar)
	}

	switch {
	case c.URL == "" && c.Command == "":
		v.add(p(), "exactly one of url or command is required")
	case c.URL != "" && c.Command != "":
		v.add(p(), "exactly one of url or command is allowed, not both")
	case c.Command != "":
		if len(c.Headers) > 0 {
			v.add(p("headers"), "is allowed only on url connectors")
		}

		if c.Transport != "" {
			v.add(p("transport"), "is allowed only on url connectors")
		}

		if c.Auth != nil {
			v.add(p("auth"), "is allowed only on url connectors")
		}

		if c.TLS != nil {
			v.add(p("tls"), "is allowed only on url connectors")
		}
	default:
		if len(c.Args) > 0 {
			v.add(p("args"), "is allowed only on command (stdio) connectors")
		}

		if len(c.Env) > 0 {
			v.add(p("env"), "is allowed only on command (stdio) connectors")
		}

		if c.Transport != "" && c.Transport != TransportSSE {
			v.add(p("transport"), "must be %q or absent, not %q", TransportSSE, c.Transport)
		}
	}

	// A url with references can only be checked once resolved.
	if v.template(p("url"), c.URL) && c.URL != "" {
		if refs, _ := c.URL.Refs(); len(refs) == 0 {
			v.httpURL(p("url"), string(c.URL))
		}
	}

	for i, a := range c.Args {
		v.template(p("args")+"["+strconv.Itoa(i)+"]", a)
	}

	for _, k := range sortedKeys(c.Env) {
		v.template(p("env", k), c.Env[k])
	}

	for _, k := range sortedKeys(c.Headers) {
		v.template(p("headers", k), c.Headers[k])

		// The header injector runs inside the auth hook, so this header
		// would replace the OAuth token on every request.
		if c.Auth != nil && strings.EqualFold(k, "Authorization") {
			v.add(p("headers", k), "must not be set with an auth table: Wayfindr sends the OAuth token in Authorization")
		}
	}

	if c.Timeout.Duration <= 0 {
		v.add(p("timeout"), "must be positive")
	}

	v.tools(p("tools"), c.Tools)

	if c.Auth != nil && c.Command == "" {
		v.auth(authKeys{
			path: func(keys ...string) string { return p(append([]string{"auth"}, keys...)...) },
			set:  func(key string) bool { return v.set("connectors", name, "auth", key) },
		}, *c.Auth, c.TLS)
	}

	if c.TLS != nil && c.Command == "" {
		v.tls(func(keys ...string) string { return p(append([]string{"tls"}, keys...)...) }, *c.TLS)
	}
}

// tls checks a tls table: it must set something, and a client certificate
// comes with exactly one key. p renders the path of a tls key, or of the
// table itself with no keys. The files are not read here: they are read
// when the connector connects, and again whenever they change.
func (v *validator) tls(p func(keys ...string) string, t TLS) {
	if t == (TLS{}) {
		v.add(p(), "must set cert_file (with key_file or key), ca_file, or both")

		return
	}

	v.template(p("key"), t.Key)

	switch {
	case t.CertFile != "" && t.KeyFile == "" && t.Key == "":
		v.add(p("cert_file"), "requires key_file or key")
	case t.KeyFile != "" && t.Key != "":
		v.add(p(), "exactly one of key_file or key is allowed, not both")
	}

	if t.CertFile == "" {
		if t.KeyFile != "" {
			v.add(p("key_file"), "requires cert_file")
		}

		if t.Key != "" {
			v.add(p("key"), "requires cert_file")
		}
	}
}

// ReservedAuthorizeParam reports whether key is an authorization request
// parameter Wayfindr sets itself, which authorize_params can't set, and
// says what decides it instead. Validation refuses such keys, and auth
// skips them, so they can't reach the authorization server either way.
func ReservedAuthorizeParam(key string) (setting string, reserved bool) {
	switch key {
	case "response_type":
		return "it is always code", true
	case "client_id":
		return "use client_id", true
	case "redirect_uri":
		return "use redirect_uri", true
	case "state":
		return "it is generated for each authorization", true
	case "scope":
		return "use scopes", true
	case "code_challenge", "code_challenge_method":
		return "use pkce", true
	case "resource":
		return "use resource_indicator", true
	case "dpop_jkt":
		return "use dpop", true
	}

	return "", false
}

// template reports a syntax error in t and returns whether t is well formed.
func (v *validator) template(at string, t Template) bool {
	if _, err := t.Refs(); err != nil {
		v.add(at, "%v", err)

		return false
	}

	return true
}

// notURL records that the value at at did not parse as a URL. A *url.Error
// quotes the whole URL, and its query may hold a key (design §9), so only
// its cause is named. A text redactor would miss a URL whose scheme is what's
// broken.
func (v *validator) notURL(at string, err error) {
	var uerr *url.Error

	if errors.As(err, &uerr) {
		v.add(at, "is not a URL: %v", uerr.Err)

		return
	}

	v.add(at, "is not a URL")
}

func (v *validator) httpURL(at, s string) {
	u, err := url.Parse(s)

	if err != nil {
		v.notURL(at, err)

		return
	}

	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		v.add(at, "must be an absolute http or https URL")
	}
}

// NameGrammar is the connector-name rule. Profile, client token, and new
// secret names follow it too, so each is safe to print, to put in a header
// or a URL path, and to type.
const NameGrammar = "[a-z0-9][a-z0-9_-]{0,62}"

// maxNameLen is the longest name NameGrammar allows, in bytes.
const maxNameLen = 63

// IsConnectorName reports whether s is a valid connector name
// (NameGrammar).
func IsConnectorName(s string) bool {
	if s == "" || len(s) > maxNameLen {
		return false
	}

	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '_' || c == '-') && i > 0:
		default:
			return false
		}
	}

	return true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}
