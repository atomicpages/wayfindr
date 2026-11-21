package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/daemonctl"
	"github.com/atomicpages/wayfindr/internal/httpclient"
	"github.com/atomicpages/wayfindr/internal/newfile"
	"github.com/atomicpages/wayfindr/internal/paths"
	"github.com/atomicpages/wayfindr/internal/redact"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

const (
	// settleInterval is the pause between status polls while `connector
	// add` waits for the new connector.
	settleInterval = 250 * time.Millisecond
	// settleTimeout bounds that wait. The daemon reloads within a second of
	// the write; the rest is the connector's first connect.
	settleTimeout = 30 * time.Second
	// reloadGrace is how long `connector edit` waits for the daemon to
	// restart the connector it changed before it reports the state it
	// finds: the daemon reloads within a second of the write.
	reloadGrace = 5 * time.Second
)

func newConnectorCmd(host Host) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connector",
		Short: "Add, list, edit, import, or remove connectors, or list their tools",
		Args:  cobra.ArbitraryArgs,
		RunE:  runGroup,
	}
	cmd.AddCommand(newConnectorAddCmd(host), newConnectorListCmd(host), newConnectorEditCmd(host),
		newConnectorImportCmd(host), newConnectorRemoveCmd(host), newConnectorToolsCmd(host))

	return cmd
}

// connectorFlags are the flags `connector add` and `connector edit` share.
type connectorFlags struct {
	url           string
	headers       []string
	secretHeaders []string
	transport     string
	oauth         bool
	noOAuth       bool
	clientID      string
	clientSecret  bool
	// grant, authorizationServer, tokenAuthMethod, signingKey,
	// assertionFile, and assertionSubject configure the newer [auth] keys;
	// config validation checks the values.
	grant               string
	authorizationServer string
	tokenAuthMethod     string
	signingKey          bool
	assertionFile       string
	assertionSubject    string
	scopes              []string
	authParams          []string
	env                 []string
	secretEnv           []string
	timeout             time.Duration
	noWait              bool
	// certFile, keyFile, and caFile set the keys of the [tls] table.
	certFile string
	keyFile  string
	caFile   string
}

// configuresOAuth reports whether a flag other than --oauth configures the
// auth table, and so implies it.
func (f connectorFlags) configuresOAuth() bool {
	return f.clientID != "" || f.clientSecret || len(f.scopes) > 0 || len(f.authParams) > 0 ||
		f.grant != "" || f.authorizationServer != "" || f.tokenAuthMethod != "" || f.signingKey ||
		f.assertionFile != "" || f.assertionSubject != ""
}

// httpOnly and stdioOnly name the flags that apply to one kind of
// connector.
func httpOnly() []string {
	return []string{
		"header", "secret-header", "transport", "oauth", "no-oauth", "client-id", "client-secret", "scope", "authorize-param",
		"grant", "authorization-server", "token-auth-method", "signing-key", "assertion-file", "assertion-subject",
		"cert-file", "key-file", "ca-file",
	}
}

func stdioOnly() []string { return []string{"env", "secret-env"} }

// registerConnectorFlags adds the flags add and edit share to cmd, with
// edit's wording when edit is true. --no-wait is add's alone.
func registerConnectorFlags(cmd *cobra.Command, f *connectorFlags, edit bool) {
	say := func(add, ed string) string {
		if edit {
			return ed
		}

		return add
	}
	fl := cmd.Flags()
	fl.StringVar(&f.url, "url", "", say("the upstream URL of an HTTP connector", "set the upstream URL (HTTP)"))
	fl.StringArrayVar(&f.headers, "header", nil, say("a header `K=V` to send (HTTP; repeatable)",
		"set the header `K=V`, keeping the others (HTTP; repeatable)"))
	fl.StringArrayVar(&f.secretHeaders, "secret-header", nil, say("a header `K` whose value is prompted for and stored as a secret (HTTP; repeatable)",
		"prompt for the header `K`'s value and replace its stored secret (HTTP; repeatable)"))
	fl.StringVar(&f.transport, "transport", "", "\"sse\" to force the legacy HTTP+SSE transport (HTTP)")
	fl.BoolVar(&f.oauth, "oauth", false, say("write an OAuth auth table without asking the server (HTTP)",
		"add an OAuth auth table (HTTP)"))
	fl.BoolVar(&f.noOAuth, "no-oauth", false, say("write no auth table without asking the server (HTTP)",
		"remove the auth table and delete the stored OAuth credential (HTTP)"))
	fl.StringVar(&f.clientID, "client-id", "", say("a pre-registered OAuth client ID; implies --oauth (HTTP)",
		"set the OAuth client ID (HTTP)"))
	fl.BoolVar(&f.clientSecret, "client-secret", false, say("prompt for the OAuth client's secret and store it as a secret; needs --client-id (HTTP)",
		"prompt for the OAuth client's secret and replace the stored one (HTTP)"))
	fl.StringVar(&f.grant, "grant", "", say("how the connector gets a token, the `grant`: authorization_code (the default), device_code, client_credentials, or jwt_bearer; implies --oauth (HTTP)",
		"set how the connector gets a token, the `grant`: authorization_code, device_code, client_credentials, or jwt_bearer; client_credentials and jwt_bearer delete the stored OAuth credential (HTTP)"))
	fl.StringVar(&f.authorizationServer, "authorization-server", "", say("the authorization server's issuer `URL`, used instead of the one the server names; client_credentials and jwt_bearer need it; implies --oauth (HTTP)",
		"set the authorization server's issuer `URL`, used instead of the one the server names (HTTP)"))
	fl.StringVar(&f.tokenAuthMethod, "token-auth-method", "", say("the token_endpoint_auth_method, a `method` such as private_key_jwt; implies --oauth (HTTP)",
		"set the token_endpoint_auth_method, a `method` such as private_key_jwt (HTTP)"))
	fl.BoolVar(&f.signingKey, "signing-key", false, say("read the OAuth client's PEM private key from stdin and store it as a secret; needs --client-id (HTTP)",
		"read the OAuth client's PEM private key from stdin and replace the stored one (HTTP)"))
	fl.StringVar(&f.assertionFile, "assertion-file", "", say("a `file` holding the jwt_bearer grant's workload token; implies --oauth (HTTP)",
		"set the `file` holding the jwt_bearer grant's workload token (HTTP)"))
	fl.StringVar(&f.assertionSubject, "assertion-subject", "", say("the `subject` of a jwt_bearer assertion Wayfindr signs with the signing key; implies --oauth (HTTP)",
		"set the `subject` of a jwt_bearer assertion Wayfindr signs with the signing key (HTTP)"))
	fl.StringArrayVar(&f.scopes, "scope", nil, say("an OAuth `scope` to request; implies --oauth (HTTP; repeatable)",
		"an OAuth `scope` to request, replacing the scopes (HTTP; repeatable)"))
	fl.StringArrayVar(&f.authParams, "authorize-param", nil, say("an extra authorization request parameter `K=V`, such as access_type=offline; implies --oauth (HTTP; repeatable)",
		"set the authorization request parameter `K=V`, keeping the others (HTTP; repeatable)"))
	fl.StringArrayVar(&f.env, "env", nil, say("an environment variable `K=V` (stdio; repeatable)",
		"set the environment variable `K=V`, keeping the others (stdio; repeatable)"))
	fl.StringArrayVar(&f.secretEnv, "secret-env", nil, say("an environment variable `K` whose value is prompted for and stored as a secret (stdio; repeatable)",
		"prompt for the environment variable `K`'s value and replace its stored secret (stdio; repeatable)"))
	fl.DurationVar(&f.timeout, "timeout", 0, say("the connector's timeout for one tool call, such as 5m (default 2m)",
		"set the connector's timeout for one tool call, such as 5m"))

	fl.StringVar(&f.certFile, "cert-file", "", say("a PEM `file` holding the client certificate to present, in the [tls] table; needs --key-file (HTTP)",
		"set the PEM `file` holding the client certificate to present, in the [tls] table (HTTP)"))
	fl.StringVar(&f.keyFile, "key-file", "", say("the PEM `file` holding --cert-file's private key (HTTP)",
		"set the PEM `file` holding the client certificate's private key, in the [tls] table (HTTP)"))
	fl.StringVar(&f.caFile, "ca-file", "", say("a PEM `file` of CA certificates to trust besides the system's, in the [tls] table (HTTP)",
		"set the PEM `file` of CA certificates to trust besides the system's, in the [tls] table (HTTP)"))

	if !edit {
		fl.BoolVar(&f.noWait, "no-wait", false, "only write the file; don't start the daemon or wait for the connector")
	}
}

// checkOAuthFlags refuses OAuth flags that contradict each other.
func checkOAuthFlags(f connectorFlags) error {
	switch {
	case f.oauth && f.noOAuth:
		return errors.New("give --oauth or --no-oauth, not both")
	case f.noOAuth && f.configuresOAuth():
		return errors.New("--client-id, --client-secret, --scope, --authorize-param, --grant, --authorization-server, " +
			"--token-auth-method, --signing-key, --assertion-file, and --assertion-subject configure OAuth, which --no-oauth turns off")
	}

	return nil
}

func newConnectorAddCmd(host Host) *cobra.Command {
	var f connectorFlags
	cmd := &cobra.Command{
		Use:   "add <name> (--url <url> | -- <command> [args...])",
		Short: "Add a connector to the config file",
		Long: `Add a [connectors.<name>] table to the config file, keeping the rest of the
file as it is. With --url it is an HTTP connector; otherwise the command after
-- makes a stdio connector. A name that exists is refused: change that
connector with "wayfindr connector edit".

For an HTTP connector with no headers, the server is asked whether it uses
OAuth, and the auth table is written if it does; --oauth and --no-oauth decide
instead. --client-id, --client-secret, --scope, and --authorize-param write the
auth table too, as do --token-auth-method, --signing-key, --grant,
--assertion-file, and --assertion-subject. --secret-header, --secret-env, and --client-secret
prompt for a value, store it as the secret <name>_<key>, and write a
${secret:…} reference to it; --signing-key reads a PEM private key from stdin
the same way and stores it as <name>_signing_key.

Then the daemon is started if it is not running, and the command waits for the
connector's first connect and reports it, offering to authorize it when it
needs OAuth. --no-wait only writes the file.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var command []string

			switch dash := cmd.ArgsLenAtDash(); {
			case dash < 0 && len(args) > 1:
				return errors.New("give the server's command after --, as in `wayfindr connector add fs -- npx -y server`")
			case dash > 1:
				return errors.New("give only the connector's name before --")
			case dash == 1:
				command = args[1:]
			}

			if err := checkAddFlags(cmd, f, command); err != nil {
				return err
			}

			return runConnectorAdd(cmd, host, args[0], command, f)
		},
	}

	registerConnectorFlags(cmd, &f, false)

	return cmd
}

// checkAddFlags refuses flags that don't fit the kind of connector.
func checkAddFlags(cmd *cobra.Command, f connectorFlags, command []string) error {
	isHTTP := f.url != ""

	switch {
	case !isHTTP && len(command) == 0:
		return errors.New("give --url <url> for an HTTP connector, or the server's command after -- for a stdio one")
	case isHTTP && len(command) > 0:
		return errors.New("give --url or a command after --, not both")
	}

	if err := checkOAuthFlags(f); err != nil {
		return err
	}

	wrong, kind := stdioOnly(), "stdio connectors (a command after --)"

	if !isHTTP {
		wrong, kind = httpOnly(), "HTTP connectors (--url)"
	}

	for _, name := range wrong {
		if cmd.Flags().Changed(name) {
			return fmt.Errorf("--%s is only for %s", name, kind)
		}
	}

	if f.clientSecret && f.clientID == "" {
		return errors.New("--client-secret needs --client-id: a secret belongs to a client you registered")
	}

	if f.signingKey && f.clientID == "" {
		return errors.New("--signing-key needs --client-id: a signing key belongs to a client you registered")
	}

	if (f.certFile == "") != (f.keyFile == "") {
		return errors.New("give --cert-file and --key-file together: a client certificate comes with its key")
	}

	return nil
}

// pendingSecret is a secret `connector add` or `edit` prompts for and
// stores.
type pendingSecret struct {
	name  string // the secret's name
	label string // what the prompt calls it, such as "header Authorization"
	value string
	// pem is set for a value that must be a PEM block, such as a signing
	// key, which can't be typed at a no-echo prompt: one line ends it.
	pem bool
}

// newConnector builds the table `connector add` writes from its flags, and
// the secrets it refers to. It leaves Auth nil unless the flags decide it.
func newConnector(name string, command []string, f connectorFlags) (config.NewConnector, []pendingSecret, error) {
	c := config.NewConnector{Name: name, URL: config.Template(f.url), Transport: f.transport}

	if len(command) > 0 {
		c.Command = command[0]

		for _, a := range command[1:] {
			c.Args = append(c.Args, config.Template(a))
		}
	}

	if f.timeout != 0 {
		c.Timeout = &config.Duration{Duration: f.timeout}
	}

	if f.certFile != "" || f.caFile != "" {
		c.TLS = &config.TLS{CertFile: f.certFile, KeyFile: f.keyFile, CAFile: f.caFile}
	}

	var secrets []pendingSecret
	var err error

	if f.oauth || f.configuresOAuth() {
		c.Auth = &config.NewAuth{
			Type: config.AuthTypeOAuth, ClientID: f.clientID, Scopes: f.scopes,
			Grant: config.Grant(f.grant), AuthorizationServer: f.authorizationServer,
			TokenEndpointAuthMethod: config.ClientAuthMethod(f.tokenAuthMethod),
		}

		if f.assertionFile != "" || f.assertionSubject != "" {
			c.Auth.Assertion = &config.Assertion{File: f.assertionFile, Subject: f.assertionSubject}
		}

		if c.Auth.AuthorizeParams, err = plainKeyValues("authorize-param", f.authParams); err != nil {
			return config.NewConnector{}, nil, err
		}

		if f.clientSecret {
			sn := config.SecretNameFor(name, "client_secret")
			c.Auth.ClientSecret = secretRef(sn)
			secrets = append(secrets, pendingSecret{name: sn, label: "OAuth client secret"})
		}

		if f.signingKey {
			sn := config.SecretNameFor(name, "signing_key")
			c.Auth.SigningKey = &config.SigningKey{Key: secretRef(sn)}
			secrets = append(secrets, signingKeySecret(sn))
		}
	}

	if c.Headers, err = keyValues("header", f.headers); err != nil {
		return config.NewConnector{}, nil, err
	}

	if c.Env, err = keyValues("env", f.env); err != nil {
		return config.NewConnector{}, nil, err
	}

	for _, s := range []struct {
		flag, kind string
		keys       []string
		into       *map[string]config.Template
	}{
		{"secret-header", "header", f.secretHeaders, &c.Headers},
		{"secret-env", "environment variable", f.secretEnv, &c.Env},
	} {
		for _, k := range s.keys {
			if _, dup := (*s.into)[k]; dup || k == "" {
				return config.NewConnector{}, nil, fmt.Errorf("--%s %q: give each key once, and not empty", s.flag, k)
			}

			sn := config.SecretNameFor(name, k)

			if *s.into == nil {
				*s.into = map[string]config.Template{}
			}

			(*s.into)[k] = secretRef(sn)
			secrets = append(secrets, pendingSecret{name: sn, label: s.kind + " " + k})
		}
	}

	if err := distinctSecretNames(secrets); err != nil {
		return config.NewConnector{}, nil, err
	}

	return c, secrets, nil
}

// distinctSecretNames refuses two pending secrets that would be stored under
// one name, so the second value never silently replaces the first. Keys that
// differ only in case or in characters a secret name can't hold, or any keys
// of a connector whose name fills the 63 bytes, map to the same name.
func distinctSecretNames(secrets []pendingSecret) error {
	seen := make(map[string]string, len(secrets))

	for _, s := range secrets {
		if other, dup := seen[s.name]; dup {
			return fmt.Errorf("the %s and the %s would both be stored as the secret %s: "+
				"use keys that differ in more than case and punctuation, or a shorter connector name",
				other, s.label, s.name)
		}

		seen[s.name] = s.label
	}

	return nil
}

// refuseTakenSecrets refuses a pending secret whose name the config already
// uses for something other than connector (wayfindr-eadp): the name is made
// from the connector and key, so google with DRIVE_TOKEN and google_drive
// with TOKEN both make google_drive_token, and storing the value would
// silently change what the other one sends. keep are the names the
// connector already refers to, which an edit may give a new value.
func refuseTakenSecrets(cfg *config.Config, connector string, secrets []pendingSecret, keep []string) error {
	for _, s := range secrets {
		if slices.Contains(keep, s.name) {
			continue
		}

		if user, ok := cfg.SecretInUse(s.name, connector); ok {
			return fmt.Errorf("the %s would be stored as the secret %s, which %s already uses: "+
				"use another connector name or key", s.label, s.name, user)
		}
	}

	return nil
}

// signingKeySecret is the pending secret of a signing key stored as name.
func signingKeySecret(name string) pendingSecret {
	return pendingSecret{name: name, label: "OAuth signing key", pem: true}
}

// secretRef is the template that references the secret name.
func secretRef(name string) config.Template {
	return config.Template(config.Ref{Kind: config.RefSecret, Name: name}.String())
}

// plainKeyValues parses repeated K=V flags whose values are not templates.
func plainKeyValues(flag string, kvs []string) (map[string]string, error) {
	m, err := keyValues(flag, kvs)

	if m == nil {
		return nil, err
	}

	out := make(map[string]string, len(m))

	for k, v := range m {
		out[k] = string(v)
	}

	return out, nil
}

// keyValues parses repeated K=V flags.
func keyValues(flag string, kvs []string) (map[string]config.Template, error) {
	if len(kvs) == 0 {
		return nil, nil
	}

	m := make(map[string]config.Template, len(kvs))

	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")

		if !ok || k == "" {
			return nil, fmt.Errorf("--%s %q: want K=V", flag, kv)
		}

		if _, dup := m[k]; dup {
			return nil, fmt.Errorf("--%s %q: %s is given twice", flag, kv, k)
		}

		m[k] = config.Template(v)
	}

	return m, nil
}

func runConnectorAdd(cmd *cobra.Command, host Host, name string, command []string, f connectorFlags) error {
	ctx := cmd.Context()
	in, out := cmd.InOrStdin(), cmd.OutOrStdout()
	p, src, err := readConfigFile(host)

	if err != nil {
		return err
	}

	c, secrets, err := newConnector(name, command, f)

	if err != nil {
		return err
	}

	// A taken name, a secret name in use, or a file that can't be edited
	// fails before the server is asked anything.
	opts := configOptions(host, p)
	next, err := config.AppendConnector(src, c, opts)

	if err != nil {
		return editError(err)
	}

	cfg, err := config.Parse(src, opts)

	if err != nil {
		return editError(&config.Error{Kind: config.KindUnsafe, Case: config.CaseUnparsable, Err: err})
	}

	if err := refuseTakenSecrets(cfg, name, secrets, nil); err != nil {
		return err
	}

	if c.URL != "" && c.Auth == nil && !f.noOAuth && len(c.Headers) == 0 {
		if c.Auth, err = detectOAuth(ctx, out, c, addOAuthAdvice(name)); err != nil {
			return err
		}

		if c.Auth != nil {
			if next, err = config.AppendConnector(src, c, opts); err != nil {
				return editError(err)
			}
		}
	}

	if err := readSecrets(in, cmd.ErrOrStderr(), secrets); err != nil {
		return err
	}

	var d *adminDaemon
	var prevConfigErr string

	if len(secrets) > 0 || !f.noWait {
		if d, prevConfigErr, err = startDaemon(ctx, host, p); err != nil {
			return err
		}
	}

	if err := putSecrets(ctx, out, d, secrets); err != nil {
		return err
	}

	if err := newfile.Replace(p.ConfigFile, src, next, p.ConfigFile); err != nil {
		return storedNote(editError(err), secrets)
	}

	if _, err := fmt.Fprintf(out, "Added connector %s.\n", name); err != nil {
		return err
	}

	if f.noWait {
		return writeTakesEffect(ctx, host, out, p)
	}

	return reportConnector(ctx, cmd, host, d, name, reportOpts{prevConfigErr: prevConfigErr})
}

// readSecrets reads each secret's value from in, prompting on prompt. More
// than one can only be read on a terminal.
func readSecrets(in io.Reader, prompt io.Writer, secrets []pendingSecret) error {
	if len(secrets) > 1 {
		if _, ok := terminal(in); !ok {
			return errors.New("more than one secret value can't be piped: run this on a terminal, or set the others with `wayfindr secret set` and use --header or --env")
		}
	}

	for i := range secrets {
		v, err := readSecret(in, prompt, secrets[i].label)

		if err != nil {
			return err
		}

		if secrets[i].pem {
			if b, _ := pem.Decode([]byte(v)); b == nil {
				return fmt.Errorf("the %s is not a PEM block: pipe the key file on stdin, as in < key.pem", secrets[i].label)
			}
		}

		secrets[i].value = v
	}

	return nil
}

// putSecrets stores each secret in the daemon and says so.
func putSecrets(ctx context.Context, out io.Writer, d *adminDaemon, secrets []pendingSecret) error {
	for _, s := range secrets {
		if err := putSecret(ctx, d, s); err != nil {
			return err
		}

		if _, err := fmt.Fprintf(out, "Stored secret %s.\n", s.name); err != nil {
			return err
		}
	}

	return nil
}

// storedNote adds to err, a failed write of the config file, that the
// secrets stored before it are kept.
func storedNote(err error, secrets []pendingSecret) error {
	if len(secrets) == 0 {
		return err
	}

	stored := make([]string, len(secrets))

	for i, s := range secrets {
		stored[i] = s.name
	}

	return fmt.Errorf("%w (the secrets %s were stored and are kept)", err, strings.Join(stored, ", "))
}

// readConfigFile reads the config file the daemon loads.
func readConfigFile(host Host) (paths.Paths, []byte, error) {
	p, err := paths.Resolve(host.Env)

	if err != nil {
		return paths.Paths{}, nil, err
	}

	src, err := os.ReadFile(p.ConfigFile)

	if errors.Is(err, fs.ErrNotExist) {
		return paths.Paths{}, nil, fmt.Errorf("no config at %s: run `wayfindr init` first", p.ConfigFile)
	}

	if err != nil {
		return paths.Paths{}, nil, fmt.Errorf("read config: %w", err)
	}

	return p, src, nil
}

// configOptions are what the daemon parses the config file with.
func configOptions(host Host, p paths.Paths) config.Options {
	return config.Options{DataDir: p.DataDir, HomeDir: host.Env.HomeDir}
}

// oauthAdvice is what detectOAuth tells the user to do if a connector it
// adds without OAuth does need it: one line each for a URL it didn't check
// and for an unclear answer. Each command has its own way.
type oauthAdvice struct {
	unchecked string
	unclear   string
}

// addOAuthAdvice is connector add's: edit's --oauth, once the connector is
// added.
func addOAuthAdvice(name string) oauthAdvice {
	fix := "run `wayfindr connector edit " + name + " --oauth`."

	return oauthAdvice{
		unchecked: "If it does, " + fix,
		unclear:   "If it needs OAuth, " + fix,
	}
}

// detectOAuth asks the server of c, the connector being added, without
// credentials, whether it uses OAuth, through the HTTP client its tls
// table makes, and says what it found. It returns the auth table to write:
// nil for none, including when the answer is unclear.
func detectOAuth(ctx context.Context, w io.Writer, c config.NewConnector, advice oauthAdvice) (*config.NewAuth, error) {
	url := c.URL
	u := string(url)

	if refs, err := url.Refs(); err != nil || len(refs) > 0 {
		_, err := fmt.Fprintf(w, "Not checking whether %s uses OAuth: its URL has a reference. %s\n", redact.URL(u), advice.unchecked)

		return nil, err
	}

	if _, err := fmt.Fprintf(w, "Checking %s … ", redact.URL(u)); err != nil {
		return nil, err
	}

	unclear := "Adding it without OAuth. " + advice.unclear + "\n"
	// The table's files only: the CLI resolves no ${secret:…} key.
	client, err := httpclient.For(ctx, config.Connector{Name: c.Name, URL: c.URL, TLS: c.TLS}, noSecrets())

	if err != nil {
		return nil, fmt.Errorf("connector %s: %w", c.Name, err)
	}

	r, err := auth.Probe(ctx, client, u)

	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Msg("oauth probe failed")
		_, err := fmt.Fprint(w, "could not reach it.\n"+unclear)

		return nil, err
	}

	switch r.Outcome {
	case auth.ProbeOAuth:
		_, err = fmt.Fprintln(w, "it uses OAuth.")

		return &config.NewAuth{Type: config.AuthTypeOAuth}, err
	case auth.ProbeOpen:
		_, err = fmt.Fprintln(w, "it needs no auth.")
	case auth.ProbeUnknown:
		_, err = fmt.Fprintf(w, "it answered HTTP %d.\n%s", r.Status, unclear)
	}

	return nil, err
}

// noSecrets resolves no reference: the daemon holds the secrets, not the
// CLI.
func noSecrets() config.Resolver {
	return config.ResolverFunc(func(_ context.Context, r config.Ref) (string, error) {
		return "", fmt.Errorf("%s: %w", r, config.ErrNotFound)
	})
}

// startDaemon starts the daemon unless one answers, as the shim does, and
// returns it with its current config error, so a wait can tell a new one
// from it.
func startDaemon(ctx context.Context, host Host, p paths.Paths) (*adminDaemon, string, error) {
	target, err := daemonTarget(ctx, p)

	if err != nil {
		return nil, "", err
	}

	if _, err := daemonctl.Ensure(ctx, target, daemonctl.StartOptions{}); err != nil {
		return nil, "", err
	}

	d, err := findDaemon(ctx, host)

	if err != nil {
		return nil, "", err
	}

	sctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	s, err := d.Status(sctx)

	if err != nil {
		return nil, "", d.explain(err)
	}

	return d, s.ConfigError, nil
}

func putSecret(ctx context.Context, d *adminDaemon, s pendingSecret) error {
	ctx, cancel := context.WithTimeout(ctx, secretSetTimeout)
	defer cancel()

	if _, err := d.PutSecret(ctx, s.name, s.value); err != nil {
		return fmt.Errorf("store secret %s: %w", s.name, d.explain(err))
	}

	return nil
}

// writeTakesEffect says when a file written with --no-wait takes effect.
func writeTakesEffect(ctx context.Context, host Host, w io.Writer, p paths.Paths) error {
	msg := "It takes effect on the next `wayfindr start`.\n"

	if listen, err := config.ListenAddress(p.ConfigFile); err == nil && daemonAnswers(ctx, host, listen) {
		msg = "The daemon is loading it; `wayfindr status` shows how it went.\n"
	}

	_, err := fmt.Fprint(w, msg)

	return err
}

// reportOpts shape how reportConnector waits and what it offers.
type reportOpts struct {
	// prevConfigErr is the daemon's config error before the write, so the
	// wait can tell a new one from it.
	prevConfigErr string
	// changedAt, when not zero, is when the file was written with a change
	// the daemon restarts the connector for: the wait looks for a state
	// entered since, for up to reloadGrace, rather than the one the
	// connector had before the reload.
	changedAt time.Time
	// offerAuth offers to authorize the connector even when it is not in
	// needs_auth: an edit changed what its next sign-in asks for.
	offerAuth bool
}

// reportConnector waits for the daemon to load the connector and reports
// how its connect went, offering on a terminal to authorize a connector
// that needs it.
func reportConnector(ctx context.Context, cmd *cobra.Command, host Host, d *adminDaemon, name string, o reportOpts) error {
	out := cmd.OutOrStdout()
	settled, err := waitConnector(ctx, d, name, o)

	if err != nil {
		return d.explain(err)
	}

	if settled.ConfigError != "" {
		msg := "the daemon could not load the new config and keeps running its last good one:\n  " + settled.ConfigError

		if dv, stale := staleDaemon(ctx, d); stale {
			msg += fmt.Sprintf("\nThe running daemon is %s, not this command's %s. Restart it with `wayfindr stop`, then `wayfindr start`; "+
				"%s is saved in the config file and loads then.", dv, version, name)
		}

		return errors.New(msg)
	}

	if settled.TimedOut {
		_, err := fmt.Fprintf(out, "%s is still starting; `wayfindr status` shows how it goes.\n", name)

		return err
	}

	c := settled.Connector
	var b bytes.Buffer
	renderSettled(&b, c)

	if _, err := out.Write(b.Bytes()); err != nil {
		return err
	}

	if c.State != upstream.StateNeedsAuth && !o.offerAuth {
		return nil
	}

	if !confirm(cmd.InOrStdin(), out, "Authorize now? [Y/n] ", true) {
		return nil
	}

	return runAuth(ctx, host, out, cmd.ErrOrStderr(), name)
}

// waitConnector waits until the daemon reports the connector past
// starting, or a new config error. With o.changedAt set, a state the
// connector entered before then is taken only once reloadGrace has passed:
// the daemon may not have reloaded yet, or the change may not restart it.
func waitConnector(ctx context.Context, d *adminDaemon, name string, o reportOpts) (admin.Settled, error) {
	grace := time.Now().Add(reloadGrace)

	for {
		s, err := d.WaitConnector(ctx, name, o.prevConfigErr, settleInterval, settleTimeout)

		if err != nil || o.changedAt.IsZero() || s.ConfigError != "" || s.TimedOut ||
			!s.Connector.Since.Before(o.changedAt) || time.Now().After(grace) {
			return s, err
		}

		select {
		case <-ctx.Done():
			return admin.Settled{}, ctx.Err()
		case <-time.After(settleInterval):
		}
	}
}

// staleDaemon reports whether the running daemon is another build than
// this command, which is what an unknown key in a config the command wrote
// points to. A daemon from before Status had a version reports none.
func staleDaemon(ctx context.Context, d *adminDaemon) (daemonVersion string, stale bool) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	s, err := d.Status(ctx)

	if err != nil || s.Version == version {
		return "", false
	}

	if s.Version == "" {
		return "an older version", true // from before Status had a version
	}

	return s.Version, true
}

// confirm asks question on a terminal and reports whether the answer is
// yes. An empty answer takes defaultYes, which the question's [Y/n] or [y/N]
// shows. Off a terminal it asks nothing and says no.
func confirm(in io.Reader, out io.Writer, question string, defaultYes bool) bool {
	if _, ok := terminal(in); !ok {
		return false
	}

	if _, err := fmt.Fprint(out, question); err != nil {
		return false
	}

	line, err := bufio.NewReader(in).ReadString('\n')

	if err != nil && line == "" {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return defaultYes
	case "y", "yes":
		return true
	default:
		return false
	}
}

// confirmChange asks question on a terminal before a command changes
// anything, unless yes (its --yes) is set, and reports whether to go on.
// Off a terminal without --yes it asks nothing and fails, saying that
// --yes would do: todo ends that sentence, as in "remove it". A no says
// so and changes nothing.
func confirmChange(in io.Reader, out io.Writer, yes bool, question string, defaultYes bool, todo string) (bool, error) {
	if yes {
		return true, nil
	}

	if _, ok := terminal(in); !ok {
		return false, errors.New("not a terminal, so nothing was asked or changed: pass --yes to " + todo)
	}

	if !confirm(in, out, question, defaultYes) {
		_, err := fmt.Fprintln(out, "Nothing changed.")

		return false, err
	}

	return true, nil
}

// editError says why an edit of the config file was refused, in the words
// of config.Advise. The file's path is left out: the user knows which file
// they configured, and `wayfindr status` names it.
func editError(err error) error {
	if e, ok := config.AsError(err); ok {
		a := config.Advise(e)

		return fmt.Errorf("%s; %s", a.Summary, a.Action)
	}

	if errors.Is(err, newfile.ErrChanged) {
		return errors.New("the config file changed while the command ran; run the command again, or edit the file by hand")
	}

	return err
}

// joinAnd joins items as a sentence does: "a", "a and b", "a, b, and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
	}
}
