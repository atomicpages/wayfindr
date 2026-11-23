// Package config decodes, defaults, and validates wayfindr.toml (design §4,
// ADR 0006, ADR 0007), and computes what changed between two configs for the
// daemon's reload loop (design §7.8).
//
// Decoding is strict: unknown keys are errors. ${env:NAME} and ${secret:name}
// templates are checked for syntax at load but stored unresolved; callers
// resolve them through a Resolver each time they need the value.
package config

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/atomicpages/wayfindr/internal/paths"
)

// Defaults from design §4 and prd.md.
const (
	DefaultListen           = "127.0.0.1:7777"
	DefaultDriver           = DriverSQLite
	DefaultRefreshInterval  = time.Hour
	DefaultConnectorTimeout = 120 * time.Second
	DefaultEmbeddingTimeout = 2 * time.Second
	DefaultCallRetention    = 30 * 24 * time.Hour
)

// Storage drivers.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// AuthTypeOAuth is the only auth type. API-key auth is a headers table, and
// no auth is an absent auth table (ADR 0006).
const AuthTypeOAuth = "oauth"

// TransportSSE forces the legacy HTTP+SSE transport on a url connector.
const TransportSSE = "sse"

// Config is a decoded, defaulted, and validated wayfindr.toml.
type Config struct {
	Server  Server  `toml:"server"`
	Storage Storage `toml:"storage"`
	// Embedding is nil when the [embedding] table is absent, which means
	// fuzzy-only search.
	Embedding  *Embedding `toml:"embedding"`
	Index      Index      `toml:"index"`
	Calls      Calls      `toml:"calls"`
	Telemetry  Telemetry  `toml:"telemetry"`
	Connectors Connectors `toml:"connectors"`
	// Profiles are named scopes a client picks with wayfindr stdio
	// --profile or /mcp?profile= (ADR 0011). Each only narrows, as a
	// project scope file does.
	Profiles map[string]*Scope `toml:"profiles"`
}

// Server is the [server] table. Changing listen requires a daemon restart;
// public_url is applied by a reload.
type Server struct {
	// Listen is a loopback host:port.
	Listen string `toml:"listen"`
	// PublicURL is the https address a tunnel serves the daemon at, or empty
	// when there is none (ADR 0021). It is informational: the console uses it
	// for setup text and to test the connection, and no request is ever
	// checked against it.
	PublicURL string `toml:"public_url"`
}

// Storage is the [storage] table. Changing it requires a daemon restart.
type Storage struct {
	Driver string `toml:"driver"`
	// DSN is a file path for sqlite and a URL for postgres. A leading ~ in a
	// sqlite path is expanded at load (design §4).
	DSN string `toml:"dsn"`
}

// Embedding is the [embedding] table: an OpenAI-compatible /v1/embeddings
// provider.
type Embedding struct {
	BaseURL    string   `toml:"base_url"`
	Model      string   `toml:"model"`
	Dimensions int      `toml:"dimensions"`
	APIKey     Template `toml:"api_key"`
	// Timeout bounds one query-embedding request.
	Timeout Duration `toml:"timeout"`
}

// Secrets returns the names of the ${secret:…} references in e's api_key,
// sorted and without repeats. A malformed template has none; validation
// reports it.
func (e Embedding) Secrets() []string { return secretNames([]Template{e.APIKey}) }

// Index is the [index] table.
type Index struct {
	// RefreshInterval is how often HTTP connectors are re-synced.
	RefreshInterval Duration `toml:"refresh_interval"`
}

// Calls is the [calls] table: the call log.
type Calls struct {
	// Retention is how long a call stays in the call log. Zero turns the
	// log off: nothing is recorded or pruned.
	Retention Duration `toml:"retention"`
}

// Telemetry is the [telemetry] table: the opt-in usage report (ADR 0020).
// A reload applies it.
type Telemetry struct {
	// Enabled sends the daily usage report. It defaults to false; the
	// DO_NOT_TRACK variable overrides true.
	Enabled bool `toml:"enabled"`
}

// Connectors maps connector name to connector.
type Connectors map[string]Connector

// Connector is one [connectors.<name>] table. Exactly one of URL (HTTP) or
// Command (stdio) is set.
type Connector struct {
	// Name is the table key, the connector's stable identifier.
	Name string `toml:"-"`

	URL       Template            `toml:"url"`
	Headers   map[string]Template `toml:"headers"`
	Transport string              `toml:"transport"`
	// Auth is nil when the connector has no auth table.
	Auth *Auth `toml:"auth"`
	// TLS is nil when the connector has no tls table: the system roots and
	// no client certificate.
	TLS *TLS `toml:"tls"`

	// Command and each element of Args have a leading ~ expanded at load
	// (design §4).
	Command string              `toml:"command"`
	Args    []Template          `toml:"args"`
	Env     map[string]Template `toml:"env"`

	// Timeout bounds one call_tool on this connector.
	Timeout Duration `toml:"timeout"`

	// Tools chooses which of the connector's tools agents can see and call;
	// nil exposes all of them.
	Tools ToolPolicy `toml:"tools"`
}

// IsStdio reports whether the connector is reached by spawning Command.
func (c Connector) IsStdio() bool { return c.Command != "" }

// UsesOAuth reports whether the connector authorizes through the OAuth
// broker: it has an auth table of type oauth. A connector with an API key in
// its headers, or with no auth at all, does not (ADR 0006).
func (c Connector) UsesOAuth() bool { return c.Auth != nil && c.Auth.Type == AuthTypeOAuth }

// Secrets returns the names of the ${secret:…} references in c, sorted and
// without repeats. A malformed template has none; validation reports it.
func (c Connector) Secrets() []string {
	ts := append([]Template{c.URL}, c.Args...)
	ts = append(ts, slices.Collect(maps.Values(c.Headers))...)
	ts = append(ts, slices.Collect(maps.Values(c.Env))...)

	if c.Auth != nil {
		ts = append(ts, c.Auth.ClientSecret)

		if c.Auth.SigningKey != nil {
			ts = append(ts, c.Auth.SigningKey.Key)
		}

		if c.Auth.Assertion != nil {
			ts = append(ts, c.Auth.Assertion.Value)
		}
	}

	if c.TLS != nil {
		ts = append(ts, c.TLS.Key)
	}

	return secretNames(ts)
}

// SecretRefs returns the names of every ${secret:…} reference in the
// config, sorted and without repeats: each connector's and [embedding]'s.
// A secret not in it is used by nothing the config holds.
func (c *Config) SecretRefs() []string { return slices.Sorted(maps.Keys(c.SecretUsers())) }

// SecretInUse returns what other than the connector refers to the secret
// name, as SecretUsers spells it ("connectors.<name>" or
// "embedding.api_key"), if anything does. A command about to store a new
// value under a name it made up for the connector must not when something
// else uses it: the value would change what that one sends (wayfindr-eadp).
func (c *Config) SecretInUse(name, connector string) (string, bool) {
	for _, u := range c.SecretUsers()[name] {
		if u != "connectors."+connector {
			return u, true
		}
	}

	return "", false
}

// SecretUsers maps the name of every ${secret:…} reference in the config to
// what refers to it, sorted: "connectors.<name>" for a connector and
// "embedding.api_key" for [embedding]'s key. `wayfindr secret list` shows
// them.
func (c *Config) SecretUsers() map[string][]string {
	users := map[string][]string{}

	for name, cn := range c.Connectors {
		for _, s := range cn.Secrets() {
			users[s] = append(users[s], "connectors."+name)
		}
	}

	if c.Embedding != nil {
		for _, s := range c.Embedding.Secrets() {
			users[s] = append(users[s], "embedding.api_key")
		}
	}

	for _, u := range users {
		slices.Sort(u)
	}

	return users
}

// OrphanedSecrets returns the secrets before references and after does
// not, sorted: what an edit of the config left unused. A nil config
// references nothing.
func OrphanedSecrets(before, after *Config) []string {
	if before == nil {
		return nil
	}

	var used []string

	if after != nil {
		used = after.SecretRefs()
	}

	var out []string

	for _, name := range before.SecretRefs() {
		if _, found := slices.BinarySearch(used, name); !found {
			out = append(out, name)
		}
	}

	return out
}

// secretNames returns the names of the ${secret:…} references in ts,
// sorted and without repeats. A malformed template has none; validation
// reports it.
func secretNames(ts []Template) []string {
	var names []string

	for _, t := range ts {
		refs, _ := t.Refs()

		for _, r := range refs {
			if r.Kind == RefSecret {
				names = append(names, r.Name)
			}
		}
	}

	slices.Sort(names)

	return slices.Compact(names)
}

// Auth is a [connectors.<name>.auth] table (ADR 0007).
type Auth struct {
	Type string `toml:"type"`
	// ClientID, when set, makes this a manual connector.
	ClientID              string   `toml:"client_id"`
	ClientSecret          Template `toml:"client_secret"`
	Scopes                []string `toml:"scopes"`
	AuthorizationServer   string   `toml:"authorization_server"`
	AuthorizationEndpoint string   `toml:"authorization_endpoint"`
	TokenEndpoint         string   `toml:"token_endpoint"`
	ClientMetadataURL     string   `toml:"client_metadata_url"`
	// RedirectURI is empty for the default loopback callback.
	RedirectURI       string `toml:"redirect_uri"`
	PKCE              bool   `toml:"pkce"`
	ValidateIssuer    bool   `toml:"validate_issuer"`
	ResourceIndicator bool   `toml:"resource_indicator"`
	// AuthorizeParams are extra query parameters for the authorization
	// request, such as Google's access_type = "offline". Validation refuses
	// the parameters Wayfindr sets itself.
	AuthorizeParams map[string]string `toml:"authorize_params"`

	// Grant is how the connector gets a token. Parse sets
	// GrantAuthorizationCode when the key is absent; the zero value means
	// the same.
	Grant Grant `toml:"grant"`
	// TokenEndpointAuthMethod is how Wayfindr authenticates as the OAuth
	// client at the token endpoint. Empty chooses one from the credential
	// held and the authorization server's metadata.
	TokenEndpointAuthMethod ClientAuthMethod `toml:"token_endpoint_auth_method"`
	// DPoP decides whether tokens are bound to a DPoP key. Parse sets
	// DPoPAuto when the key is absent; the zero value means the same.
	DPoP DPoPMode `toml:"dpop"`
	// DeviceAuthorizationEndpoint overrides the device authorization
	// endpoint from the authorization server's metadata (RFC 8628).
	DeviceAuthorizationEndpoint string `toml:"device_authorization_endpoint"`
	// SigningKey is nil when the connector has no signing_key table.
	SigningKey *SigningKey `toml:"signing_key"`
	// Assertion is nil when the connector has no assertion table. Only the
	// jwt_bearer grant takes one.
	Assertion *Assertion `toml:"assertion"`
}

// Grant is how a connector gets a token: an auth flow, which the user takes
// part in, or an unattended grant, which has no user (GLOSSARY: Grant). A
// refresh renews a grant's token and is not a grant.
type Grant string

// The grants a connector can use.
const (
	// GrantAuthorizationCode is the browser auth flow, and the default.
	GrantAuthorizationCode Grant = "authorization_code"
	// GrantDeviceCode is the auth flow where the user enters a user code
	// at a verification URI on any device (RFC 8628).
	GrantDeviceCode Grant = "device_code"
	// GrantClientCredentials is the OAuth client's own access.
	GrantClientCredentials Grant = "client_credentials"
	// GrantJWTBearer presents an assertion that stands in for the user or
	// workload (RFC 7523).
	GrantJWTBearer Grant = "jwt_bearer"
)

// Grants returns every grant, in the order the docs list them.
func Grants() []Grant {
	return []Grant{GrantAuthorizationCode, GrantDeviceCode, GrantClientCredentials, GrantJWTBearer}
}

// Unattended reports whether g is an unattended grant: one with no user,
// whose token Wayfindr gets itself whenever a call needs one and never
// stores. It is the one place that classifies a grant.
func (g Grant) Unattended() bool {
	switch g {
	case GrantClientCredentials, GrantJWTBearer:
		return true
	case GrantAuthorizationCode, GrantDeviceCode:
		return false
	}

	return false
}

// ClientAuthMethod is a token_endpoint_auth_method: how Wayfindr proves at
// the token endpoint that it is the OAuth client (RFC 7591, RFC 8705).
type ClientAuthMethod string

// The client authentication methods.
const (
	AuthMethodNone                    ClientAuthMethod = "none"
	AuthMethodClientSecretBasic       ClientAuthMethod = "client_secret_basic"
	AuthMethodClientSecretPost        ClientAuthMethod = "client_secret_post"
	AuthMethodClientSecretJWT         ClientAuthMethod = "client_secret_jwt"
	AuthMethodPrivateKeyJWT           ClientAuthMethod = "private_key_jwt"
	AuthMethodTLSClientAuth           ClientAuthMethod = "tls_client_auth"
	AuthMethodSelfSignedTLSClientAuth ClientAuthMethod = "self_signed_tls_client_auth"
)

// ClientAuthMethods returns every client authentication method.
func ClientAuthMethods() []ClientAuthMethod {
	return []ClientAuthMethod{
		AuthMethodNone, AuthMethodClientSecretBasic, AuthMethodClientSecretPost, AuthMethodClientSecretJWT,
		AuthMethodPrivateKeyJWT, AuthMethodTLSClientAuth, AuthMethodSelfSignedTLSClientAuth,
	}
}

// DPoPMode decides whether a connector's tokens are bound to a DPoP key
// (RFC 9449).
type DPoPMode string

// The DPoP modes.
const (
	// DPoPAuto uses DPoP when the resource asks for it, and is the default.
	DPoPAuto DPoPMode = "auto"
	// DPoPAlways uses DPoP for every grant.
	DPoPAlways DPoPMode = "always"
	// DPoPNever sends bearer tokens only.
	DPoPNever DPoPMode = "never"
)

// DPoPModes returns every DPoP mode.
func DPoPModes() []DPoPMode { return []DPoPMode{DPoPAuto, DPoPAlways, DPoPNever} }

// SigningKey is a [connectors.<name>.auth.signing_key] table: the private
// key Wayfindr signs JWTs with, for private_key_jwt client authentication
// and self-signed assertions. Its keys are omitempty so `connector add` can
// write the table without empty keys, which would read as set.
type SigningKey struct {
	// Key is the PEM private key (EC P-256/P-384, RSA, or Ed25519), usually
	// a ${secret:…} reference.
	Key Template `toml:"key,omitempty"`
	// KID is the key ID put in each JWT's header. Empty sends none.
	KID string `toml:"kid,omitempty"`
	// Alg is the JWS algorithm, one of SigningAlgs. Empty chooses one from
	// the key and the authorization server's metadata.
	Alg string `toml:"alg,omitempty"`
	// Audience is what each JWT's aud claim names. Parse sets
	// AudienceIssuer when the key is absent; the zero value means the same.
	Audience Audience `toml:"audience,omitempty"`
}

// SigningAlgs returns the JWS algorithms a signing key may name.
func SigningAlgs() []string { return []string{"ES256", "ES384", "RS256", "PS256", "EdDSA"} }

// Audience is what a signed JWT's aud claim names.
type Audience string

// The audiences.
const (
	// AudienceIssuer is the authorization server's issuer, and the default
	// (RFC 7523 as updated).
	AudienceIssuer Audience = "issuer"
	// AudienceTokenEndpoint is the token endpoint's URL, which some
	// providers (Okta, Entra) still require.
	AudienceTokenEndpoint Audience = "token_endpoint"
)

// Audiences returns every audience.
func Audiences() []Audience { return []Audience{AudienceIssuer, AudienceTokenEndpoint} }

// Assertion is a [connectors.<name>.auth.assertion] table: where the jwt_bearer
// grant's assertion comes from. Exactly one key is set. Its keys are
// omitempty, as SigningKey's are.
type Assertion struct {
	// File holds a workload token, such as one Kubernetes projects, read
	// again for every grant. A leading ~ is expanded at load (design §4).
	File string `toml:"file,omitempty"`
	// Value is the assertion itself, usually an ${env:…} reference,
	// resolved for every grant.
	Value Template `toml:"value,omitempty"`
	// Subject makes Wayfindr sign the assertion itself with the signing
	// key, with this sub claim.
	Subject string `toml:"subject,omitempty"`
}

// TLS is a [connectors.<name>.tls] table: the TLS identity and trust a url
// connector uses on its MCP server's connections and its authorization
// server's. Every field is optional on its own, but the table must set
// something: a client certificate (CertFile with exactly one of KeyFile or
// Key), a private CA (CAFile), or both. Its keys are omitempty, as
// SigningKey's are, so `connector add` writes only the keys it sets.
type TLS struct {
	// CertFile is a PEM file holding the client certificate, then any
	// intermediates. A leading ~ is expanded at load (design §4). The file
	// is read when the connection is made, not at load, and read again
	// when it changes, so a certificate rotated on disk is used from the
	// next handshake.
	CertFile string `toml:"cert_file,omitempty"`
	// KeyFile is a PEM file holding CertFile's private key. A leading ~ is
	// expanded at load.
	KeyFile string `toml:"key_file,omitempty"`
	// Key is CertFile's private key as PEM, usually a ${secret:…}
	// reference. It is resolved each time the connector's HTTP client is
	// built.
	Key Template `toml:"key,omitempty"`
	// CAFile is a PEM file of CA certificates trusted in addition to the
	// system roots. A leading ~ is expanded at load. It is read when the
	// connector's HTTP client is built, not again when it changes on disk:
	// a changed table, or a restart, reads it anew.
	CAFile string `toml:"ca_file,omitempty"`
}

// HasClientCert reports whether t presents a client certificate.
func (t TLS) HasClientCert() bool { return t.CertFile != "" }

// IsManual reports whether the connector uses a pre-registered client.
func (a Auth) IsManual() bool { return a.ClientID != "" }

// Duration is a time.Duration written in TOML as a Go duration string
// ("2s", "1h").
type Duration struct{ time.Duration }

// UnmarshalText parses a Go duration string.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))

	if err != nil {
		return fmt.Errorf("duration must be a Go duration string such as \"30s\": %w", err)
	}

	d.Duration = v

	return nil
}

// MarshalText writes the duration as a Go duration string.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Options is what Load and Parse need from the host.
type Options struct {
	// DataDir is the data directory (internal/paths), used for the default
	// sqlite DSN.
	DataDir string
	// HomeDir returns the user's home directory, used to expand a leading ~
	// (design §4). It is called only when a value needs expanding. Nil
	// means no home directory is known.
	HomeDir func() (string, error)
}

// Load reads the config file at path and returns it decoded, defaulted,
// expanded, and validated.
func Load(ctx context.Context, path string, opts Options) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}

	data, err := os.ReadFile(path) //nolint:gosec // the path is the user's own config file

	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	c, err := Parse(data, opts)

	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}

	return c, nil
}

// Parse decodes TOML bytes, applies defaults, expands a leading ~ in paths,
// and validates the result, so validation sees the expanded paths. Unknown
// keys, paths that can't be expanded, and rule violations are reported
// together as a *ValidationError; TOML syntax and type errors are returned
// as decoded.
func Parse(data []byte, opts Options) (*Config, error) {
	var c Config
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&c)

	if err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	// The validator can tell a key the file sets from one a default
	// filled, for rules about keys that must not be written at all.
	v := validator{defined: md.IsDefined}

	for _, k := range md.Undecoded() {
		v.add(k.String(), "unknown key")
	}

	applyDefaults(&c, md, opts.DataDir)
	expandHome(&c, &v, opts.HomeDir)
	v.config(&c)

	if err := v.err(); err != nil {
		return nil, err
	}

	return &c, nil
}

// applyDefaults fills keys the file leaves out. Booleans that default to true
// are checked with md.IsDefined, since a decoded false can't be told apart
// from an absent key otherwise.
func applyDefaults(c *Config, md toml.MetaData, dataDir string) {
	if c.Server.Listen == "" {
		c.Server.Listen = DefaultListen
	}

	if c.Storage.Driver == "" {
		c.Storage.Driver = DefaultDriver
	}

	if c.Storage.DSN == "" && c.Storage.Driver == DriverSQLite {
		c.Storage.DSN = paths.Paths{DataDir: dataDir}.DBFile()
	}

	if c.Embedding != nil && !md.IsDefined("embedding", "timeout") {
		c.Embedding.Timeout.Duration = DefaultEmbeddingTimeout
	}

	if !md.IsDefined("index", "refresh_interval") {
		c.Index.RefreshInterval.Duration = DefaultRefreshInterval
	}

	if !md.IsDefined("calls", "retention") {
		c.Calls.Retention.Duration = DefaultCallRetention
	}

	if c.Connectors == nil {
		c.Connectors = Connectors{}
	}

	for name, cn := range c.Connectors {
		cn.Name = name

		if !md.IsDefined("connectors", name, "timeout") {
			cn.Timeout.Duration = DefaultConnectorTimeout
		}

		if cn.Auth != nil {
			for key, field := range map[string]*bool{
				"pkce":               &cn.Auth.PKCE,
				"validate_issuer":    &cn.Auth.ValidateIssuer,
				"resource_indicator": &cn.Auth.ResourceIndicator,
			} {
				if !md.IsDefined("connectors", name, "auth", key) {
					*field = true
				}
			}

			// An explicit "" is left for validation to refuse.
			if !md.IsDefined("connectors", name, "auth", "grant") {
				cn.Auth.Grant = GrantAuthorizationCode
			}

			if !md.IsDefined("connectors", name, "auth", "dpop") {
				cn.Auth.DPoP = DPoPAuto
			}

			if cn.Auth.SigningKey != nil && !md.IsDefined("connectors", name, "auth", "signing_key", "audience") {
				cn.Auth.SigningKey.Audience = AudienceIssuer
			}
		}

		c.Connectors[name] = cn
	}
}
