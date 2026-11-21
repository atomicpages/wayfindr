package cli

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/newfile"
)

func newConnectorEditCmd(host Host) *cobra.Command {
	var f connectorFlags
	var unset []string
	cmd := &cobra.Command{
		Use:   "edit <name> [flags]",
		Short: "Change keys of a connector in the config file",
		Long: `Change keys of the [connectors.<name>] table in the config file. Each flag
changes its key and nothing else: the table's other keys, their order, its
comments, and the rest of the file stay as they are. The flags are add's, for
the kind of connector the table already is; --unset removes a key, given as a
dotted path in the table such as auth.scopes or headers.X-Team (or
headers."X.Team" for a key with a dot).

--client-secret, --secret-header, and --secret-env prompt for a new value and
store it under the secret the key already references, or as <name>_<key>;
--signing-key reads a PEM private key from stdin the same way, storing it under
the secret signing_key.key references, or as <name>_signing_key. A secret the
edit leaves unreferenced is deleted. --unset auth removes the whole auth table
and deletes the stored OAuth credential, as does --grant client_credentials or
jwt_bearer, whose tokens are never stored.

Then the daemon is started if it is not running, and the command waits for the
connector to reconnect and reports it, offering to authorize it when it needs
OAuth or the edit changed what its next sign-in asks for.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConnectorEdit(cmd, host, args[0], f, unset)
		},
	}

	registerConnectorFlags(cmd, &f, true)
	cmd.Flags().StringArrayVar(&unset, "unset", nil,
		"remove the `key`, a dotted path in the connector's table such as auth.scopes or headers.X-Team; "+
			"auth removes the auth table and deletes the stored OAuth credential (repeatable)")
	// --unset auth does what --no-oauth did on edit, where the flag read as
	// add's "don't ask the server". It keeps working for scripts that use it.
	_ = cmd.Flags().MarkHidden("no-oauth") // registerConnectorFlags added it, so this can't fail

	return cmd
}

func runConnectorEdit(cmd *cobra.Command, host Host, name string, f connectorFlags, unset []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	if !anyChanged(cmd, slices.Concat(httpOnly(), stdioOnly(), []string{"url", "timeout", "unset"})) {
		return errors.New("nothing to change: give the flags to set or --unset")
	}

	p, src, err := readConfigFile(host)

	if err != nil {
		return err
	}

	opts := configOptions(host, p)
	cfg, err := config.Parse(src, opts)

	if err != nil {
		return editError(&config.Error{Kind: config.KindUnsafe, Case: config.CaseUnparsable, Connector: name, Err: err})
	}

	cur, ok := cfg.Connectors[name]

	if !ok {
		return editError(&config.Error{Kind: config.KindConflict, Case: config.CaseEditNotFound, Connector: name})
	}

	if err := checkEditFlags(cmd, f, cur); err != nil {
		return err
	}

	e, secrets, err := connectorEdit(cmd, cur, f, unset)

	if err != nil {
		return err
	}

	if err := refuseTakenSecrets(cfg, name, secrets, cur.Secrets()); err != nil {
		return err
	}

	// The edit is checked, the config's own validation included, before
	// anything is asked or stored. A secret flag that keeps the reference
	// the table has changes only the secret.
	ed := config.Edited{File: src, Before: cfg, After: cfg}

	if len(e.Set) > 0 || len(e.Unset) > 0 {
		if ed, err = config.EditConnector(src, name, e, opts); err != nil {
			return editError(err)
		}
	}

	if !ed.Changed && len(secrets) == 0 {
		_, err := fmt.Fprintf(out, "%s is already set that way.\n", name)

		return err
	}

	for _, s := range secrets {
		if users := secretUsers(ed.After, s.name, name, func(c string) string { return c }); len(users) > 0 {
			all := "both"

			if len(users) > 1 {
				all = "all of them"
			}

			if _, err := fmt.Fprintf(out, "%s is also used by %s; it changes for %s.\n", s.name, joinAnd(users), all); err != nil {
				return err
			}
		}
	}

	if err := readSecrets(cmd.InOrStdin(), cmd.ErrOrStderr(), secrets); err != nil {
		return err
	}

	d, prevConfigErr, err := startDaemon(ctx, host, p)

	if err != nil {
		return err
	}

	if err := putSecrets(ctx, out, d, secrets); err != nil {
		return err
	}

	after := ed.After.Connectors[name]
	o := reportOpts{prevConfigErr: prevConfigErr}
	var changed []config.Key

	if ed.Changed {
		changed = changedKeys(src, name, e, opts)
		writtenAt := time.Now()

		if err := newfile.Replace(p.ConfigFile, src, ed.File, p.ConfigFile); err != nil {
			return storedNote(editError(err), secrets)
		}

		keys := make([]string, len(changed))

		for i, k := range changed {
			keys[i] = k.String()
		}

		if _, err := fmt.Fprintf(out, "Updated %s: %s.\n", name, strings.Join(keys, ", ")); err != nil {
			return err
		}

		if ch := config.Diff(ed.Before, ed.After); slices.Contains(ch.Changed, name) || slices.Contains(ch.URLChanged, name) {
			o.changedAt = writtenAt
		}
	}

	// Dropping [auth] leaves a token nothing uses. Dropping client_id makes
	// the connector discover its client, and the daemon doesn't check a
	// discovered connector's grant against its client (auth's
	// clientChanged), so the manual client's token goes here instead.
	// An unattended grant never reads a stored credential, so switching to
	// one leaves the auth flow's token behind too.
	droppedClient := cur.UsesOAuth() && cur.Auth.ClientID != "" && after.UsesOAuth() && after.Auth.ClientID == ""
	toUnattended := cur.UsesOAuth() && !cur.Auth.Grant.Unattended() && after.UsesOAuth() && after.Auth.Grant.Unattended()
	revoke := cur.UsesOAuth() && !after.UsesOAuth() || droppedClient || toUnattended
	c := cleanUp(ctx, d, nil, name, revoke, config.OrphanedSecrets(ed.Before, ed.After))

	if s := c.deleted(); s != "" {
		if _, err := fmt.Fprintln(out, s); err != nil {
			return err
		}
	}

	if err := c.error("everything the edit left unused"); err != nil {
		return err
	}

	// An unattended grant has no sign-in to offer.
	if after.UsesOAuth() && !after.Auth.Grant.Unattended() {
		nextSignIn := slices.ContainsFunc(changed, func(k config.Key) bool {
			return hasKeyPrefix(k, config.Key{"auth", "scopes"}) || hasKeyPrefix(k, config.Key{"auth", "authorize_params"}) ||
				hasKeyPrefix(k, config.Key{"auth", "redirect_uri"})
		})

		if nextSignIn {
			if _, err := fmt.Fprintln(out, "This applies at the next sign-in."); err != nil {
				return err
			}
		}

		newClient := f.clientSecret || slices.ContainsFunc(changed, func(k config.Key) bool {
			return hasKeyPrefix(k, config.Key{"auth", "client_id"})
		})
		o.offerAuth = nextSignIn || newClient
	}

	return reportConnector(ctx, cmd, host, d, name, o)
}

// anyChanged reports whether any of the named flags was given.
func anyChanged(cmd *cobra.Command, names []string) bool {
	return slices.ContainsFunc(names, cmd.Flags().Changed)
}

// checkEditFlags refuses flags that contradict each other or don't fit
// the kind of connector cur already is.
func checkEditFlags(cmd *cobra.Command, f connectorFlags, cur config.Connector) error {
	if err := checkOAuthFlags(f); err != nil {
		return err
	}

	if cur.IsStdio() && cmd.Flags().Changed("url") {
		return errors.New("edit can't turn a stdio connector into an HTTP one; remove it and add it again")
	}

	wrong, kind := stdioOnly(), "stdio connectors"

	if cur.IsStdio() {
		wrong, kind = httpOnly(), "HTTP connectors"
	}

	for _, flag := range wrong {
		if cmd.Flags().Changed(flag) {
			return fmt.Errorf("--%s is only for %s", flag, kind)
		}
	}

	return nil
}

// connectorEdit builds the edit of cur's table from the flags, and the
// secrets to prompt for.
func connectorEdit(cmd *cobra.Command, cur config.Connector, f connectorFlags, unset []string) (config.Edit, []pendingSecret, error) {
	var e config.Edit

	for _, s := range unset {
		k, err := parseKey(s)

		if err != nil {
			return config.Edit{}, nil, err
		}

		e.Unset = append(e.Unset, k)
	}

	set := func(v any, key ...string) { e.Set = append(e.Set, config.Set{Key: key, Value: v}) }
	changed := cmd.Flags().Changed

	if changed("url") {
		set(config.Template(f.url), "url")
	}

	if changed("transport") {
		set(f.transport, "transport")
	}

	if changed("timeout") {
		set(config.Duration{Duration: f.timeout}, "timeout")
	}

	headers, err := keyValues("header", f.headers)

	if err != nil {
		return config.Edit{}, nil, err
	}

	env, err := keyValues("env", f.env)

	if err != nil {
		return config.Edit{}, nil, err
	}

	for _, m := range []struct {
		table string
		kvs   map[string]config.Template
	}{{"headers", headers}, {"env", env}} {
		for _, k := range sortedKeys(m.kvs) {
			set(m.kvs[k], m.table, k)
		}
	}

	if (f.oauth || f.configuresOAuth()) && cur.Auth == nil {
		set(config.AuthTypeOAuth, "auth", "type")
	}

	if changed("client-id") {
		set(f.clientID, "auth", "client_id")
	}

	for _, s := range []struct {
		flag, val string
		key       []string
	}{
		{"grant", f.grant, []string{"auth", "grant"}},
		{"authorization-server", f.authorizationServer, []string{"auth", "authorization_server"}},
		{"token-auth-method", f.tokenAuthMethod, []string{"auth", "token_endpoint_auth_method"}},
		{"assertion-file", f.assertionFile, []string{"auth", "assertion", "file"}},
		{"assertion-subject", f.assertionSubject, []string{"auth", "assertion", "subject"}},
		{"cert-file", f.certFile, []string{"tls", "cert_file"}},
		{"key-file", f.keyFile, []string{"tls", "key_file"}},
		{"ca-file", f.caFile, []string{"tls", "ca_file"}},
	} {
		if changed(s.flag) {
			set(s.val, s.key...)
		}
	}

	if len(f.scopes) > 0 {
		set(f.scopes, "auth", "scopes")
	}

	params, err := plainKeyValues("authorize-param", f.authParams)

	if err != nil {
		return config.Edit{}, nil, err
	}

	for _, k := range sortedKeys(params) {
		set(params[k], "auth", "authorize_params", k)
	}

	if f.noOAuth {
		e.Unset = append(e.Unset, config.Key{"auth"})
	}

	// Keys a secret flag sets, even when it keeps the reference the table
	// has: none may also be unset.
	secretKeys := []config.Key{}
	var secrets []pendingSecret

	// addSecret stores the secret key references, or a new one named for
	// the connector and suffix.
	addSecret := func(key config.Key, have config.Template, suffix string, pending pendingSecret) {
		secretKeys = append(secretKeys, key)
		sn, ok := secretName(have)

		if !ok {
			sn = config.SecretNameFor(cur.Name, suffix)
			e.Set = append(e.Set, config.Set{Key: key, Value: secretRef(sn)})
		}

		if !slices.ContainsFunc(secrets, func(s pendingSecret) bool { return s.name == sn }) {
			pending.name = sn
			secrets = append(secrets, pending)
		}
	}

	if f.clientSecret {
		if err := needsClientID(cmd, cur, f, e.Unset, "client-secret", "a secret"); err != nil {
			return config.Edit{}, nil, err
		}

		var have config.Template

		if cur.Auth != nil {
			have = cur.Auth.ClientSecret
		}

		addSecret(config.Key{"auth", "client_secret"}, have, "client_secret", pendingSecret{label: "OAuth client secret"})
	}

	if f.signingKey {
		if err := needsClientID(cmd, cur, f, e.Unset, "signing-key", "a signing key"); err != nil {
			return config.Edit{}, nil, err
		}

		var have config.Template

		if cur.Auth != nil && cur.Auth.SigningKey != nil {
			have = cur.Auth.SigningKey.Key
		}

		addSecret(config.Key{"auth", "signing_key", "key"}, have, "signing_key", signingKeySecret(""))
	}

	for _, s := range []struct {
		flag, table, kind string
		keys              []string
		plain             map[string]config.Template
		have              map[string]config.Template
	}{
		{"secret-header", "headers", "header", f.secretHeaders, headers, cur.Headers},
		{"secret-env", "env", "environment variable", f.secretEnv, env, cur.Env},
	} {
		for i, k := range s.keys {
			_, plain := s.plain[k]

			if k == "" || plain || slices.Contains(s.keys[:i], k) {
				return config.Edit{}, nil, fmt.Errorf("--%s %q: give each key once, and not empty", s.flag, k)
			}

			addSecret(config.Key{s.table, k}, s.have[k], k, pendingSecret{label: s.kind + " " + k})
		}
	}

	for _, sk := range slices.Concat(keysOf(e.Set), secretKeys) {
		for _, uk := range e.Unset {
			if hasKeyPrefix(sk, uk) || hasKeyPrefix(uk, sk) {
				return config.Edit{}, nil, fmt.Errorf("%s is both set and unset", sk)
			}
		}
	}

	return e, secrets, nil
}

// needsClientID refuses the flag, --client-secret or --signing-key, on a
// table that has no client_id after the edit: a credential, what, belongs
// to a client you registered.
func needsClientID(cmd *cobra.Command, cur config.Connector, f connectorFlags, unset []config.Key, flag, what string) error {
	var id string

	if cur.Auth != nil {
		id = cur.Auth.ClientID
	}

	if slices.ContainsFunc(unset, func(k config.Key) bool { return hasKeyPrefix(config.Key{"auth", "client_id"}, k) }) {
		id = ""
	}

	if cmd.Flags().Changed("client-id") {
		id = f.clientID
	}

	if id == "" {
		return fmt.Errorf("--%s needs a client_id, and %s has none: give --client-id too; %s belongs to a client you registered", flag, cur.Name, what)
	}

	return nil
}

// secretName returns the secret t references when t is that reference
// and nothing else.
func secretName(t config.Template) (string, bool) {
	refs, err := t.Refs()

	if err != nil || len(refs) != 1 || refs[0].Kind != config.RefSecret || string(t) != refs[0].String() {
		return "", false
	}

	return refs[0].Name, true
}

// changedKeys returns the keys of e that change the connector on their
// own, in e's order: sets that give a key a new value, and unsets.
func changedKeys(src []byte, name string, e config.Edit, opts config.Options) []config.Key {
	var keys []config.Key

	for _, s := range e.Set {
		// A set that changes nothing is reported as unchanged before the
		// result is validated, so an error means it changes the key.
		ed, err := config.EditConnector(src, name, config.Edit{Set: []config.Set{s}}, opts)

		if err != nil || ed.Changed {
			keys = append(keys, s.Key)
		}
	}

	return append(keys, e.Unset...)
}

// parseKey reads a dotted key as TOML writes one: bare parts such as
// auth.scopes, or quoted ones such as headers."X.Team", without escapes. A
// key that starts with a key of the auth table, such as
// authorize_params.prompt, is read inside it.
func parseKey(s string) (config.Key, error) {
	bad := fmt.Errorf("--unset %q: want a dotted key such as auth.scopes or headers.X-Team", s)
	var k config.Key

	for rest := s; ; {
		var part string

		if q := rest[:min(1, len(rest))]; q == `"` || q == `'` {
			end := strings.Index(rest[1:], q)

			if end < 0 {
				return nil, bad
			}

			part, rest = rest[1:1+end], rest[2+end:]
		} else {
			i := strings.IndexByte(rest, '.')

			if i < 0 {
				i = len(rest)
			}

			part, rest = rest[:i], rest[i:]

			if part == "" {
				return nil, bad
			}
		}

		k = append(k, part)

		if rest == "" {
			break
		}

		if rest[0] != '.' || len(rest) == 1 {
			return nil, bad
		}

		rest = rest[1:]
	}

	if slices.Contains(authKeys(), k[0]) {
		k = append(config.Key{"auth"}, k...)
	}

	return k, nil
}

// authKeys are the keys of a connector's auth table, as config decodes it.
func authKeys() []string {
	t := reflect.TypeFor[config.Auth]()
	keys := make([]string, 0, t.NumField())

	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("toml"), ","); name != "" && name != "-" {
			keys = append(keys, name)
		}
	}

	return keys
}

func hasKeyPrefix(k, prefix config.Key) bool {
	return len(k) >= len(prefix) && slices.Equal(k[:len(prefix)], prefix)
}

func keysOf(sets []config.Set) []config.Key {
	keys := make([]config.Key, len(sets))

	for i, s := range sets {
		keys[i] = s.Key
	}

	return keys
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}
