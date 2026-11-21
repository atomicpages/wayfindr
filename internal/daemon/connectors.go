package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/httpclient"
	"github.com/atomicpages/wayfindr/internal/newfile"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// probeTimeout bounds the OAuth probe of a new URL connector, as the CLI's.
const probeTimeout = 10 * time.Second

// connectorAdder adds a connector to the daemon's own config file for the
// admin API's POST /admin/connectors (ADR 0017, design §7.8). It does what
// `wayfindr connector add` does, in the daemon: append the table with
// config.AppendConnector, probe for OAuth, store the secrets, write the
// file with newfile.Replace, and then reload it at once rather than wait
// for the watcher. One add runs at a time.
type connectorAdder struct {
	// file is the config file the daemon loaded, and opts its load options.
	file string
	opts config.Options
	// putSecret seals and stores a secret (the resolver's).
	putSecret func(ctx context.Context, name, value string) error
	// resolver resolves a tls key of the connector being probed.
	resolver config.Resolver
	// reload reloads the config file (Daemon.reloadFile). The daemon is
	// built after the admin API, so New sets it last.
	reload func(ctx context.Context)
	status func() []upstream.ConnectorStatus
	// awaitSync waits for a ready connector's first sync.
	awaitSync func(ctx context.Context, connector string)
	log       zerolog.Logger

	mu sync.Mutex
}

// add serves admin.Options.AddConnector.
func (a *connectorAdder) add(ctx context.Context, spec admin.ConnectorSpec) (admin.ConnectorReport, error) {
	c, secrets, err := newConnector(spec)

	if err != nil {
		return admin.ConnectorReport{}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	src, err := os.ReadFile(a.file)

	if err != nil {
		return admin.ConnectorReport{}, fmt.Errorf("read config file: %w", err)
	}

	// The table is checked against the file before any network call, so a
	// taken or invalid name fails first.
	next, err := config.AppendConnector(src, c, a.opts)

	if err != nil {
		return admin.ConnectorReport{}, appendError(spec.Name, err)
	}

	// A secret name another connector or [embedding] uses is refused: its
	// value would change what that one sends (wayfindr-eadp).
	cfg, err := config.Parse(src, a.opts)

	if err != nil {
		return admin.ConnectorReport{}, appendError(spec.Name, err)
	}

	for _, s := range secrets {
		if user, ok := cfg.SecretInUse(s.name, c.Name); ok {
			return admin.ConnectorReport{}, invalid(fmt.Sprintf("The secret %s is already used by %s; use another name for the connector or the key.", s.name, user))
		}
	}

	rep := admin.ConnectorReport{OAuth: admin.OAuthNotChecked}

	if detectOAuth(spec, c) {
		var found *config.NewAuth

		if found, rep.OAuth = a.probe(ctx, c); found != nil {
			c.Auth = found

			if next, err = config.AppendConnector(src, c, a.opts); err != nil {
				return admin.ConnectorReport{}, appendError(spec.Name, err)
			}
		}
	}

	// Stored before the write, so the first connect finds them.
	for _, s := range secrets {
		if err := a.putSecret(ctx, s.name, s.value); err != nil {
			return admin.ConnectorReport{}, fmt.Errorf("store secret %s: %w", s.name, err)
		}

		rep.Secrets = append(rep.Secrets, s.name)
	}

	if err := newfile.Replace(a.file, src, next, a.file); err != nil {
		if errors.Is(err, newfile.ErrChanged) {
			return admin.ConnectorReport{}, &admin.AddError{Status: http.StatusConflict, Kind: string(config.KindConflict), Case: "config_changed",
				Advice: admin.Advice{Summary: "The config file changed while " + spec.Name + " was being added. Try again." + kept(rep.Secrets)}}
		}

		return admin.ConnectorReport{}, fmt.Errorf("write config file: %w", err)
	}

	a.log.Info().Str("connector", spec.Name).Str("oauth", rep.OAuth).Int("secrets", len(rep.Secrets)).Msg("connector added to the config file")
	a.reload(ctx)
	i := slices.IndexFunc(a.status(), func(s upstream.ConnectorStatus) bool { return s.Connector == spec.Name })

	if i < 0 {
		// The reload failed; the status says why (config_error).
		return admin.ConnectorReport{}, fmt.Errorf("connector %s is in the file but the reload did not apply it", spec.Name)
	}

	rep.Status = a.status()[i]

	if rep.Status.State == upstream.StateReady {
		a.awaitSync(ctx, spec.Name)
		rep.Status = a.statusOf(spec.Name, rep.Status)
	}

	return rep, nil
}

// statusOf is connector's current status, or fallback when it is gone.
func (a *connectorAdder) statusOf(connector string, fallback upstream.ConnectorStatus) upstream.ConnectorStatus {
	for _, s := range a.status() {
		if s.Connector == connector {
			return s
		}
	}

	return fallback
}

// probe asks the server of c, the connector being added, without
// credentials, whether it uses OAuth, as `wayfindr connector add` does,
// through the HTTP client its tls table makes. It returns the auth table
// to write (nil for none) and how the probe went.
func (a *connectorAdder) probe(ctx context.Context, c config.NewConnector) (*config.NewAuth, string) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	client, err := httpclient.For(ctx, config.Connector{Name: c.Name, URL: c.URL, TLS: c.TLS}, a.resolver)

	if err != nil {
		a.log.Debug().Err(err).Msg("oauth probe client")

		return nil, admin.OAuthUnreachable
	}

	r, err := auth.Probe(ctx, client, string(c.URL))

	if err != nil {
		a.log.Debug().Err(err).Msg("oauth probe failed")

		return nil, admin.OAuthUnreachable
	}

	switch r.Outcome {
	case auth.ProbeOAuth:
		return &config.NewAuth{Type: config.AuthTypeOAuth}, admin.OAuthDetected
	case auth.ProbeOpen:
		return nil, admin.OAuthOpen
	case auth.ProbeUnknown:
		return nil, admin.OAuthUnclear
	}

	return nil, admin.OAuthUnclear
}

// detectOAuth reports whether an add asks the server about OAuth: a URL
// connector with auth left to detection, no headers, and a URL without a
// reference, as the CLI decides.
func detectOAuth(spec admin.ConnectorSpec, c config.NewConnector) bool {
	if c.URL == "" || c.Auth != nil || len(c.Headers) > 0 {
		return false
	}

	if spec.Auth != "" && spec.Auth != admin.AuthDetect {
		return false
	}

	refs, err := c.URL.Refs()

	return err == nil && len(refs) == 0
}

// secret is a value an add stores as a secret.
type secret struct{ name, value string }

// newConnector builds the table spec describes and the secrets it refers
// to, refusing a spec that mixes the two kinds of connector or names a key
// twice. The config's own validation (AppendConnector) checks the rest.
func newConnector(spec admin.ConnectorSpec) (config.NewConnector, []secret, error) {
	name := strings.TrimSpace(spec.Name)
	c := config.NewConnector{Name: name, URL: config.Template(strings.TrimSpace(spec.URL)), Transport: spec.Transport, Command: strings.TrimSpace(spec.Command)}
	isURL, isCommand := c.URL != "", c.Command != ""
	urlOnly := spec.Transport != "" || len(spec.Headers) > 0 || len(spec.SecretHeaders) > 0 || spec.ClientID != "" ||
		spec.ClientSecret != "" || len(spec.Scopes) > 0 || (spec.Auth != "" && spec.Auth != admin.AuthDetect && spec.Auth != admin.AuthNone)
	commandOnly := len(spec.Args) > 0 || len(spec.Env) > 0 || len(spec.SecretEnv) > 0

	switch {
	case name == "":
		return c, nil, invalid("Give the connector a name.")
	case isURL && isCommand:
		return c, nil, invalid("Give the connector a URL or a command, not both.")
	case !isURL && !isCommand:
		return c, nil, invalid("Give the connector's URL, or the command that runs it.")
	case isURL && commandOnly:
		return c, nil, invalid("Arguments and environment variables are for a command connector, not a URL one.")
	case isCommand && urlOnly:
		return c, nil, invalid("Headers, a transport, and OAuth settings are for a URL connector, not a command one.")
	}

	switch spec.Auth {
	case "", admin.AuthDetect, admin.AuthOAuth, admin.AuthNone:
	default:
		return c, nil, invalid(fmt.Sprintf("Authentication %.32q is not one of detect, oauth, or none.", spec.Auth))
	}

	if spec.Timeout != "" {
		d, err := time.ParseDuration(spec.Timeout)

		if err != nil || d <= 0 {
			return c, nil, invalid(fmt.Sprintf("The timeout %.32q is not a duration such as 30s or 2m.", spec.Timeout))
		}

		c.Timeout = &config.Duration{Duration: d}
	}

	for _, a := range spec.Args {
		c.Args = append(c.Args, config.Template(a))
	}

	var secrets []secret
	names := map[string]string{}
	// addSecret stores value under the secret name for key and returns the
	// reference the table writes. Two keys whose secret names collide, such
	// as X-Key and x_key, are refused.
	addSecret := func(key, value string) (config.Template, error) {
		if value == "" {
			return "", invalid(fmt.Sprintf("The secret value for %.64s is empty.", key))
		}

		sn := config.SecretNameFor(name, key)

		if other, dup := names[sn]; dup {
			return "", invalid(fmt.Sprintf("%.64s and %.64s would share the secret %s; rename one.", other, key, sn))
		}

		names[sn] = key
		secrets = append(secrets, secret{name: sn, value: value})

		return config.Template(config.Ref{Kind: config.RefSecret, Name: sn}.String()), nil
	}

	var err error

	if c.Headers, err = merge("header", spec.Headers, spec.SecretHeaders, addSecret); err != nil {
		return c, nil, err
	}

	if c.Env, err = merge("environment variable", spec.Env, spec.SecretEnv, addSecret); err != nil {
		return c, nil, err
	}

	if spec.Auth == admin.AuthOAuth || spec.ClientID != "" || spec.ClientSecret != "" || len(spec.Scopes) > 0 {
		c.Auth = &config.NewAuth{Type: config.AuthTypeOAuth, ClientID: strings.TrimSpace(spec.ClientID), Scopes: spec.Scopes}

		if spec.ClientSecret != "" {
			if spec.ClientID == "" {
				return c, nil, invalid("A client secret needs its client ID.")
			}

			if c.Auth.ClientSecret, err = addSecret("client_secret", spec.ClientSecret); err != nil {
				return c, nil, err
			}
		}
	}

	return c, secrets, nil
}

// merge is the table of plain values and secret references for one map
// key, such as headers: nil when both are empty.
func merge(kind string, plain, secret map[string]string, addSecret func(key, value string) (config.Template, error)) (map[string]config.Template, error) {
	if len(plain)+len(secret) == 0 {
		return nil, nil
	}

	out := make(map[string]config.Template, len(plain)+len(secret))

	for k, v := range plain {
		if strings.TrimSpace(k) == "" {
			return nil, invalid("Every " + kind + " needs a name.")
		}

		out[k] = config.Template(v)
	}

	for _, k := range slices.Sorted(maps.Keys(secret)) {
		if strings.TrimSpace(k) == "" {
			return nil, invalid("Every secret " + kind + " needs a name.")
		}

		if _, dup := out[k]; dup {
			return nil, invalid(fmt.Sprintf("The %s %.64s is given twice.", kind, k))
		}

		ref, err := addSecret(k, secret[k])

		if err != nil {
			return nil, err
		}

		out[k] = ref
	}

	return out, nil
}

// invalid is a 400 refusal with msg.
func invalid(msg string) error {
	return &admin.AddError{Status: http.StatusBadRequest, Kind: string(config.KindInvalid), Case: "invalid_spec", Advice: admin.Advice{Summary: msg}}
}

// appendError words a refused AppendConnector for the console: the config's
// own validation issues, or the editor's Kind and Case. config.Advise is
// worded for the CLI, so only its summary is kept here.
func appendError(name string, err error) error {
	if ve := (*config.ValidationError)(nil); errors.As(err, &ve) {
		var issues []string

		for _, is := range ve.Issues {
			path := strings.TrimPrefix(is.Path, "connectors."+name+".")
			issues = append(issues, path+": "+is.Message)
		}

		return &admin.AddError{Status: http.StatusBadRequest, Kind: string(config.KindInvalid), Case: "invalid_connector",
			Advice: admin.Advice{Summary: "The connector is not valid: " + strings.Join(issues, "; ") + "."}}
	}

	ce, ok := config.AsError(err)

	if !ok {
		return err
	}

	ae := &admin.AddError{Status: http.StatusConflict, Kind: string(ce.Kind), Case: string(ce.Case)}

	switch ce.Kind {
	case config.KindConflict:
		if ce.Case == config.CaseConnectorExists {
			ae.Advice.Summary = fmt.Sprintf("A connector named %q is already configured. Pick another name.", name)
		} else {
			ae.Advice.Summary = sentence(config.Advise(ce).Summary)
		}
	case config.KindUnsafe:
		ae.Advice.Summary = "The config file can't be changed safely: " + strings.TrimPrefix(config.Advise(ce).Summary, "could not change it safely: ") +
			". Add the connector to the file by hand."
	case config.KindInvalid:
		ae.Status = http.StatusBadRequest
		ae.Advice.Summary = sentence(config.Advise(ce).Summary)
	}

	return ae
}

// sentence capitalizes s and ends it with a period.
func sentence(s string) string {
	if s == "" {
		return s
	}

	r, n := utf8.DecodeRuneInString(s)

	return string(unicode.ToUpper(r)) + s[n:] + "."
}

// kept is the note that the secrets stored before a failed write stay.
func kept(secrets []string) string {
	if len(secrets) == 0 {
		return ""
	}

	return " The secrets " + strings.Join(secrets, ", ") + " were stored and are kept."
}
