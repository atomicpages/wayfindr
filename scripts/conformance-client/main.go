// Command conformance-client is the client the MCP conformance suite runs in
// `make conformance-client` (scripts/conformance-client.sh). It is test
// tooling, never part of the wayfindr binary.
//
// The suite starts one scenario's test server (and, for auth scenarios, its
// authorization server), then runs
//
//	conformance-client <serverUrl>
//
// with MCP_CONFORMANCE_SCENARIO, MCP_CONFORMANCE_PROTOCOL_VERSION, and, for
// some scenarios, MCP_CONFORMANCE_CONTEXT (JSON) in the environment. Wayfindr
// is the client under test: its upstream connection, and for auth scenarios
// its OAuth client. So for one scenario this command:
//
//  1. writes a temp config with one url connector, "conf", at serverUrl.
//     An auth/* scenario gets an [auth] table of type "oauth" made from the
//     context (see configFor): the authorization code flow with the
//     context's client_id and client_secret when it has them, or an
//     unattended grant (client_credentials, jwt_bearer), whose key or
//     workload token reaches the daemon through its environment. A context
//     this version can't express (an IdP token) ends the run with exit
//     status 3;
//  2. starts a throwaway daemon, as scripts/conformance.sh does: the temp
//     config and data dir, a random master key in WAYFINDR_SECRET_KEY and a
//     random client token in WAYFINDR_CLIENT_TOKEN, so nothing touches the
//     real home or keychain;
//  3. while the connector is needs_auth (step-up included), plays the user
//     (an unattended grant never is, so it goes straight to step 4)
//     in the auth flow through the admin API, the way `wayfindr auth` does:
//     StartAuth, a GET of the authorize URL that follows the test
//     authorization server's redirect to the daemon's /oauth/callback, then
//     WaitAuth. At most maxAuthorizations times;
//  4. calls search_tools and call_tool through /mcp, so the upstream sees a
//     real tools/list and tools/call: every tool the connector serves, with
//     arguments made from its input schema, or the calls the context's
//     toolCalls names (see plan). It plays the user when the upstream asks
//     for input: its /mcp session accepts each elicitation with the form's
//     defaults.
//
// The suite scores what the test servers saw, not this command's exit
// status; a non-zero status only marks the run as incomplete in its output.
// Progress and, on failure, the daemon's log go to stderr, which the suite
// saves as stderr.txt next to the scenario's checks.json.
//
// Environment, besides the suite's:
//
//	WAYFINDR_BIN   wayfindr binary (default "wayfindr" on PATH)
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

const (
	// connector is the name of the one connector in the temp config.
	connector = "conf"
	// maxAuthorizations bounds the auth flows one run plays: the first
	// sign-in plus the step-ups after it.
	maxAuthorizations = 3
	// runTimeout bounds the whole run. It is shorter than the suite's
	// per-scenario timeout (scripts/conformance-client.sh), so the daemon is
	// always stopped by this command, not left behind by a killed one.
	runTimeout = 100 * time.Second
	// settleTimeout bounds a wait for the connector to leave starting.
	settleTimeout = 30 * time.Second
	// flowWait is one WaitAuth long-poll.
	flowWait = 20 * time.Second
	// exitInexpressible is the exit status for a context this version of
	// Wayfindr can't express in its config.
	exitInexpressible = 3
)

// errInexpressible marks a scenario context the config can't express.
var errInexpressible = errors.New("context not expressible in the Wayfindr config")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stderr)
	stop()

	if errors.Is(err, errInexpressible) {
		fmt.Fprintf(os.Stderr, "conformance-client: %v\n", err)
		os.Exit(exitInexpressible)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance-client: %v\n", err)
		os.Exit(1)
	}
}

// scenario is what the suite says about the run.
type scenario struct {
	name, protocolVersion, serverURL string
	// context is MCP_CONFORMANCE_CONTEXT, decoded; nil when unset.
	context map[string]any
}

// auth reports whether the scenario tests the OAuth client.
func (s scenario) auth() bool { return strings.HasPrefix(s.name, "auth/") }

func run(ctx context.Context, args []string, log io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: conformance-client <serverUrl>")
	}

	s := scenario{
		name:            os.Getenv("MCP_CONFORMANCE_SCENARIO"),
		protocolVersion: os.Getenv("MCP_CONFORMANCE_PROTOCOL_VERSION"),
		serverURL:       args[0],
	}

	if raw := os.Getenv("MCP_CONFORMANCE_CONTEXT"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &s.context); err != nil {
			return fmt.Errorf("decode MCP_CONFORMANCE_CONTEXT: %w", err)
		}
	}

	logf(log, "scenario %s at %s, server %s\n", s.name, s.protocolVersion, s.serverURL)

	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	tmp, err := os.MkdirTemp("", "wayfindr-conformance-client-")

	if err != nil {
		return err
	}

	defer func() { _ = os.RemoveAll(tmp) }()

	d, err := startDaemon(ctx, tmp, s, log)

	if err != nil {
		return err
	}

	err = exercise(ctx, d, s, log)

	if err != nil {
		d.dumpLog(log)
	}

	return errors.Join(err, d.stop())
}

// exercise plays the user and the agent against the running daemon.
func exercise(ctx context.Context, d *daemon, s scenario, log io.Writer) error {
	authorizations := 0
	authorize := func() error {
		if authorizations >= maxAuthorizations {
			return fmt.Errorf("still needs auth after %d authorizations", maxAuthorizations)
		}

		authorizations++

		return d.authorize(ctx, log)
	}

	c, err := d.settle(ctx)

	if err != nil {
		return err
	}

	for c.State == upstream.StateNeedsAuth {
		if err := authorize(); err != nil {
			return err
		}

		if c, err = d.settle(ctx); err != nil {
			return err
		}
	}

	logf(log, "connector is %s\n", c.State)

	if c.State != upstream.StateReady {
		return fmt.Errorf("connector is %s, not ready: %s", c.State, adviceText(c.Advice))
	}

	session, err := d.mcpSession(ctx)

	if err != nil {
		return err
	}

	defer func() { _ = session.Close() }()

	tools, err := d.tools(ctx, session, log)

	if err != nil {
		return err
	}

	calls, err := plan(s, tools)

	if err != nil {
		return err
	}

	var failed []string

	for _, cl := range calls {
		for {
			res, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "call_tool",
				Arguments: map[string]any{"name": cl.name, "arguments": cl.args},
			})

			if err != nil {
				return fmt.Errorf("call_tool %s: %w", cl.name, err)
			}

			if !res.IsError {
				logf(log, "call_tool %s: ok\n", cl.name)

				break
			}

			logf(log, "call_tool %s: error: %s\n", cl.name, resultText(res))

			// A step-up or a lost token leaves the connector in needs_auth:
			// the user authorizes again, and the agent retries the call.
			c, err := d.status(ctx)

			if err != nil {
				return err
			}

			if c.State != upstream.StateNeedsAuth {
				failed = append(failed, cl.name)

				break
			}

			if err := authorize(); err != nil {
				return err
			}

			if _, err := d.settle(ctx); err != nil {
				return err
			}
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("call_tool failed for %s", strings.Join(failed, ", "))
	}

	return nil
}

// daemon is a throwaway wayfindr daemon and the clients that talk to it.
type daemon struct {
	bin, dir, base, token string
	env                   []string
	cmd                   *exec.Cmd
	exited                chan struct{}
	admin                 *admin.Client
	// browser follows redirects, as a browser does, and keeps no cookies
	// across runs.
	browser *http.Client
}

// startDaemon writes the config for s under dir, runs `wayfindr init`, and
// starts `wayfindr start --foreground`, returning once /healthz answers.
func startDaemon(ctx context.Context, dir string, s scenario, log io.Writer) (*daemon, error) {
	bin := os.Getenv("WAYFINDR_BIN")

	if bin == "" {
		bin = "wayfindr"
	}

	port, err := freePort(ctx)

	if err != nil {
		return nil, err
	}

	key := make([]byte, 32)
	tok := make([]byte, 16)
	_, _ = rand.Read(key)
	_, _ = rand.Read(tok)

	d := &daemon{
		bin:     bin,
		dir:     dir,
		base:    "http://127.0.0.1:" + strconv.Itoa(port),
		token:   "wf_conformance_" + hex.EncodeToString(tok),
		exited:  make(chan struct{}),
		browser: &http.Client{Timeout: 30 * time.Second},
	}
	cfgPath := filepath.Join(dir, "config", "wayfindr.toml")
	d.env = append(os.Environ(),
		"WAYFINDR_CONFIG="+cfgPath,
		"WAYFINDR_DATA_DIR="+filepath.Join(dir, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "xdg", "config"),
		"XDG_DATA_HOME="+filepath.Join(dir, "xdg", "data"),
		"XDG_STATE_HOME="+filepath.Join(dir, "xdg", "state"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "xdg", "cache"),
		"WAYFINDR_SECRET_KEY="+base64.StdEncoding.EncodeToString(key),
		"WAYFINDR_CLIENT_TOKEN="+d.token,
	)
	d.admin = admin.NewClient(d.base, d.token, nil)

	cfg, env, err := configFor(ctx, s, port)

	if err != nil {
		return nil, err
	}

	d.env = append(d.env, env...)

	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return nil, err
	}

	// init never overwrites an existing config, so write ours first.
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return nil, err
	}

	logf(log, "config:\n%s", cfg)

	initCmd := exec.CommandContext(ctx, bin, "init") //nolint:gosec // G204: the test's own wayfindr binary
	initCmd.Env = d.env

	if out, err := initCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("wayfindr init: %w\n%s", err, out)
	}

	d.cmd = exec.CommandContext(ctx, bin, "start", "--foreground", "--log-level", "debug", //nolint:gosec // G204: as above
		"--log-file", d.logPath())
	d.cmd.Env = d.env
	// A cancelled run (the suite's SIGTERM, or runTimeout) still stops the
	// daemon gracefully, so it removes its own lock and socket.
	d.cmd.Cancel = func() error { return d.cmd.Process.Signal(syscall.SIGTERM) }
	d.cmd.WaitDelay = 10 * time.Second

	if err := d.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the daemon: %w", err)
	}

	go func() {
		_ = d.cmd.Wait()
		close(d.exited)
	}()

	if err := d.waitHealthy(ctx); err != nil {
		d.dumpLog(log)

		return nil, errors.Join(err, d.stop())
	}

	logf(log, "daemon on %s\n", d.base)

	return d, nil
}

// The daemon's environment variables the config's ${env:…} references
// read, so no key or workload token is written to the config file.
const (
	envSigningKey = "WAYFINDR_CONFORMANCE_SIGNING_KEY"
	envAssertion  = "WAYFINDR_CONFORMANCE_ASSERTION"
)

// authorizationServer is the first authorization server the protected
// resource metadata of the MCP server at serverURL lists (RFC 9728 §3: at
// the path-suffixed well-known URL, else the root one). An unattended
// grant's config must name it; a user would know it from the provider.
func authorizationServer(ctx context.Context, serverURL string) (string, error) {
	u, err := url.Parse(serverURL)

	if err != nil {
		return "", fmt.Errorf("server url: %w", err)
	}

	origin := u.Scheme + "://" + u.Host
	candidates := []string{origin + "/.well-known/oauth-protected-resource"}

	if p := strings.TrimSuffix(u.Path, "/"); p != "" {
		candidates = append([]string{origin + "/.well-known/oauth-protected-resource" + p}, candidates...)
	}

	for _, c := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c, nil)

		if err != nil {
			return "", err
		}

		resp, err := http.DefaultClient.Do(req)

		if err != nil {
			return "", fmt.Errorf("protected resource metadata: %w", err)
		}

		var prm struct {
			AuthorizationServers []string `json:"authorization_servers"`
		}

		err = json.NewDecoder(resp.Body).Decode(&prm)
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK && err == nil && len(prm.AuthorizationServers) > 0 {
			return prm.AuthorizationServers[0], nil
		}
	}

	return "", fmt.Errorf("%s lists no authorization server in its protected resource metadata", serverURL)
}

// configFor is the temp config for s: one url connector at s's server, with
// OAuth for an auth scenario, and the variables to add to the daemon's
// environment for it. The context maps to the [auth] table:
//
//   - a client-credentials scenario (auth/client-credentials-*) gets grant
//     "client_credentials", with the context's client_secret, or its
//     private_key_pem as the signing key (private_key_jwt);
//   - a context with valid_jwt, a workload token, gets grant "jwt_bearer"
//     with the token as [auth.assertion] value;
//   - both unattended grants get authorization_server, which config
//     requires: the one the scenario server's protected resource metadata
//     lists, as the user would know it (see authorizationServer);
//   - any other gets the authorization code flow, with the context's
//     client_id and client_secret when it has them.
//
// An IdP token (Enterprise-Managed Authorization) has no config yet.
func configFor(ctx context.Context, s scenario, port int) (string, []string, error) {
	var b strings.Builder
	var env []string

	fmt.Fprintf(&b, "[server]\nlisten = %q\n\n[storage]\ndriver = \"sqlite\"\n\n", "127.0.0.1:"+strconv.Itoa(port))
	fmt.Fprintf(&b, "[connectors.%s]\nurl = %q\n", connector, s.serverURL)

	if !s.auth() {
		return b.String(), nil, nil
	}

	// Keys this version has no config for.
	for _, k := range []string{"idp_id_token", "idp_issuer"} {
		if _, ok := s.context[k]; ok {
			return "", nil, fmt.Errorf("%w: the context has %s", errInexpressible, k)
		}
	}

	str := func(k string) string {
		v, _ := s.context[k].(string)

		return v
	}

	fmt.Fprintf(&b, "\n[connectors.%s.auth]\ntype = \"oauth\"\n", connector)

	unattended := true

	switch {
	case strings.HasPrefix(s.name, "auth/client-credentials"):
		b.WriteString("grant = \"client_credentials\"\n")
	case str("valid_jwt") != "":
		b.WriteString("grant = \"jwt_bearer\"\n")
	default:
		unattended = false
	}

	if unattended {
		as, err := authorizationServer(ctx, s.serverURL)

		if err != nil {
			return "", nil, err
		}

		fmt.Fprintf(&b, "authorization_server = %q\n", as)
	}

	for _, k := range []string{"client_id", "client_secret"} {
		if v := str(k); v != "" {
			fmt.Fprintf(&b, "%s = %q\n", k, v)
		}
	}

	if pemKey := str("private_key_pem"); pemKey != "" {
		fmt.Fprintf(&b, "\n[connectors.%s.auth.signing_key]\nkey = \"${env:%s}\"\n", connector, envSigningKey)

		if alg := str("signing_algorithm"); alg != "" {
			fmt.Fprintf(&b, "alg = %q\n", alg)
		}

		env = append(env, envSigningKey+"="+pemKey)
	}

	if jwt := str("valid_jwt"); jwt != "" {
		fmt.Fprintf(&b, "\n[connectors.%s.auth.assertion]\nvalue = \"${env:%s}\"\n", connector, envAssertion)
		env = append(env, envAssertion+"="+jwt)
	}

	return b.String(), env, nil
}

func (d *daemon) logPath() string { return filepath.Join(d.dir, "daemon.log") }

func (d *daemon) waitHealthy(ctx context.Context) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(30 * time.Second)

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/healthz", nil)

		if err != nil {
			return err
		}

		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.exited:
			return errors.New("the daemon exited before it became healthy")
		case <-deadline:
			return errors.New("the daemon did not become healthy in 30s")
		case <-tick.C:
		}
	}
}

// stop ends the daemon with SIGTERM, then SIGKILL if it lingers.
func (d *daemon) stop() error {
	if d.cmd == nil || d.cmd.Process == nil {
		return nil
	}

	select {
	case <-d.exited:
		return nil
	default:
	}

	_ = d.cmd.Process.Signal(syscall.SIGTERM)

	select {
	case <-d.exited:
		return nil
	case <-time.After(10 * time.Second):
		_ = d.cmd.Process.Kill()
		<-d.exited

		return errors.New("the daemon did not stop on SIGTERM; killed it")
	}
}

// dumpLog copies the end of the daemon's log to w.
func (d *daemon) dumpLog(w io.Writer) {
	b, err := os.ReadFile(d.logPath())

	if err != nil {
		return
	}

	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	logf(w, "--- daemon log (last 60 lines)\n")

	for _, l := range lines[max(0, len(lines)-60):] {
		logf(w, "%s\n", l)
	}
}

// status returns the connector's row.
func (d *daemon) status(ctx context.Context) (admin.Connector, error) {
	s, err := d.admin.Status(ctx)

	if err != nil {
		return admin.Connector{}, err
	}

	for _, c := range s.Connectors {
		if c.Name == connector {
			return c, nil
		}
	}

	if s.ConfigError != "" {
		return admin.Connector{}, fmt.Errorf("the daemon refused the config: %s", s.ConfigError)
	}

	return admin.Connector{}, fmt.Errorf("the daemon lists no connector %q", connector)
}

// settle waits for the connector to leave starting.
func (d *daemon) settle(ctx context.Context) (admin.Connector, error) {
	st, err := d.admin.WaitConnector(ctx, connector, "", 100*time.Millisecond, settleTimeout)

	switch {
	case err != nil:
		return admin.Connector{}, err
	case st.ConfigError != "":
		return admin.Connector{}, fmt.Errorf("the daemon refused the config: %s", st.ConfigError)
	case st.TimedOut:
		return admin.Connector{}, fmt.Errorf("the connector was still starting after %s", settleTimeout)
	}

	return st.Connector, nil
}

// authorize plays the user once, as `wayfindr auth` and a browser do.
func (d *daemon) authorize(ctx context.Context, log io.Writer) error {
	f, err := d.admin.StartAuth(ctx, connector, admin.AuthStart{})

	if err != nil {
		return fmt.Errorf("start auth: %w", err)
	}

	logf(log, "auth flow %s: GET %s\n", f.FlowID, withoutQuery(f.AuthorizeURL))

	// The browser leg: the test authorization server approves at once and
	// redirects to the daemon's /oauth/callback, which the client follows.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.AuthorizeURL, nil)

	if err != nil {
		return err
	}

	resp, err := d.browser.Do(req)

	if err != nil {
		// The flow reports what went wrong, if it got that far.
		logf(log, "browser: %v\n", err)
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		logf(log, "browser: ended at %s with %d\n", withoutQuery(resp.Request.URL.String()), resp.StatusCode)
	}

	for {
		s, err := d.admin.WaitAuth(ctx, f.FlowID, flowWait)

		if err != nil {
			return fmt.Errorf("wait for auth: %w", err)
		}

		switch s.State {
		case admin.FlowPending:
			continue
		case admin.FlowDone:
			logf(log, "auth flow done\n")

			return nil
		case admin.FlowFailed, admin.FlowExpired:
			return fmt.Errorf("auth flow %s: %s", s.State, adviceText(s.Advice))
		}

		return fmt.Errorf("auth flow in unknown state %q", s.State)
	}
}

// mcpSession connects to the daemon's /mcp as an agent would.
func (d *daemon) mcpSession(ctx context.Context) (*mcp.ClientSession, error) {
	hc := &http.Client{Transport: bearer{token: d.token, next: http.DefaultTransport}}
	client := mcp.NewClient(&mcp.Implementation{Name: "wayfindr-conformance-client", Version: "0"},
		&mcp.ClientOptions{ElicitationHandler: elicit})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: d.base + "/mcp", HTTPClient: hc}, nil)

	if err != nil {
		return nil, fmt.Errorf("connect to /mcp: %w", err)
	}

	return session, nil
}

// bearer adds the client token to every request.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)

	return b.next.RoundTrip(r)
}

// tool is one tool definition search_tools returned.
type tool struct {
	Name        string          `json:"name"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// tools lists the connector's tools: their names from the admin API (a live
// tools/list upstream), then their definitions from search_tools.
func (d *daemon) tools(ctx context.Context, session *mcp.ClientSession, log io.Writer) ([]tool, error) {
	list, err := d.admin.Tools(ctx, connector)

	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}

	var names []string

	for _, t := range list.Tools {
		if t.Exposed {
			names = append(names, connector+"."+t.Name)
		}
	}

	logf(log, "tools: %v\n", names)

	if len(names) == 0 {
		return nil, nil
	}

	var tools []tool

	// search_tools looks up at most 10 names at a time.
	for chunk := range slices.Chunk(names, 10) {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "search_tools",
			Arguments: map[string]any{"names": chunk},
		})

		if err != nil {
			return nil, fmt.Errorf("search_tools: %w", err)
		}

		if res.IsError {
			return nil, fmt.Errorf("search_tools: %s", resultText(res))
		}

		var found struct {
			Tools []tool `json:"tools"`
		}

		if err := json.Unmarshal([]byte(resultText(res)), &found); err != nil {
			return nil, fmt.Errorf("search_tools: decode: %w", err)
		}

		tools = append(tools, found.Tools...)
	}

	return tools, nil
}

// call is one call_tool call the agent makes.
type call struct {
	// name is the qualified tool name.
	name string
	args map[string]any
}

const (
	// schemaEchoTool and schemaTool are json-schema-2020-12-preservation's
	// tools: the first takes the second's inputSchema, as the client saw
	// it, and the suite checks what survived.
	schemaEchoTool = "json_schema_echo"
	schemaTool     = "json_schema_2020_12_tool"
)

// plan is the calls for s: the context's toolCalls when the scenario names
// them (http-custom-headers), else every tool once, with arguments made
// from its schema. json_schema_echo gets json_schema_2020_12_tool's
// inputSchema as search_tools returned it, so the suite sees what
// Wayfindr's catalog kept of it.
func plan(s scenario, tools []tool) ([]call, error) {
	if raw, ok := s.context["toolCalls"]; ok {
		b, err := json.Marshal(raw)

		if err != nil {
			return nil, err
		}

		var named []struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}

		if err := json.Unmarshal(b, &named); err != nil {
			return nil, fmt.Errorf("decode the context's toolCalls: %w", err)
		}

		calls := make([]call, 0, len(named))

		for _, n := range named {
			calls = append(calls, call{name: connector + "." + n.Name, args: n.Arguments})
		}

		return calls, nil
	}

	calls := make([]call, 0, len(tools))

	for _, t := range tools {
		args := valuesFor(t.InputSchema, false)

		if t.Name == connector+"."+schemaEchoTool {
			for _, u := range tools {
				if u.Name == connector+"."+schemaTool {
					var schema map[string]any

					if err := json.Unmarshal(u.InputSchema, &schema); err != nil {
						return nil, fmt.Errorf("decode %s's inputSchema: %w", schemaTool, err)
					}

					args = map[string]any{"schema": schema}
				}
			}
		}

		calls = append(calls, call{name: t.Name, args: args})
	}

	return calls, nil
}

// valuesFor makes a value for each required property of an object schema,
// enough for a test server to accept it: the property's default, else its
// first enum value, else a sample of its type. With all, it also gives
// every optional property that has a default its default, as a user who
// accepts a prefilled form does (SEP-1034).
func valuesFor(schema json.RawMessage, all bool) map[string]any {
	var s struct {
		Properties map[string]struct {
			Type    any   `json:"type"`
			Enum    []any `json:"enum"`
			Default any   `json:"default"`
		} `json:"properties"`
		Required []string `json:"required"`
	}

	values := map[string]any{}

	if json.Unmarshal(schema, &s) != nil {
		return values
	}

	for name, p := range s.Properties {
		required := slices.Contains(s.Required, name)

		switch {
		case p.Default != nil && (required || all):
			values[name] = p.Default
		case !required:
		case len(p.Enum) > 0:
			values[name] = p.Enum[0]
		default:
			values[name] = valueOf(p.Type)
		}
	}

	return values
}

// elicit plays the user who is asked for input: they accept, with the
// form's defaults and a sample value for each other required field.
func elicit(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	schema, err := json.Marshal(req.Params.RequestedSchema)

	if err != nil {
		return nil, err
	}

	return &mcp.ElicitResult{Action: "accept", Content: valuesFor(schema, true)}, nil
}

// valueOf is a sample value of a JSON Schema type ("string", or a list of
// types, whose first is used).
func valueOf(typ any) any {
	if list, ok := typ.([]any); ok && len(list) > 0 {
		typ = list[0]
	}

	switch typ {
	case "number", "integer":
		return 1
	case "boolean":
		return true
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	case "null":
		return nil
	default:
		return "test"
	}
}

// resultText joins a tool result's text content.
func resultText(res *mcp.CallToolResult) string {
	var b bytes.Buffer

	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}

	return b.String()
}

// logf writes one progress line; a lost line is no reason to stop.
func logf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

// withoutQuery drops a URL's query, which carries the state and the
// authorization code (design §9: never log them).
func withoutQuery(u string) string {
	before, _, _ := strings.Cut(u, "?")

	return before
}

func adviceText(a *admin.Advice) string {
	if a == nil {
		return "no advice"
	}

	return strings.TrimSpace(a.Summary + " " + a.Action)
}

// freePort returns a loopback TCP port that was free a moment ago.
func freePort(ctx context.Context) (int, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")

	if err != nil {
		return 0, err
	}

	port := l.Addr().(*net.TCPAddr).Port

	return port, l.Close()
}
