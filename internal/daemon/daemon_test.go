package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
	"go.uber.org/goleak"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/daemon"
	"github.com/atomicpages/wayfindr/internal/paths"
	"github.com/atomicpages/wayfindr/internal/setup"
	"github.com/atomicpages/wayfindr/internal/testutil/fakekeychain"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

func TestMain(m *testing.M) {
	fakeupstream.ServeStdioIfChild()
	os.Exit(m.Run())
}

// host is a temp machine for one daemon: its environment points the config
// and data directory into a temp dir, and its keychain is a fake.
type host struct {
	env      paths.Env
	paths    paths.Paths
	keychain *fakekeychain.Keychain
}

func newHost(t *testing.T) host {
	t.Helper()
	dir := t.TempDir()
	vars := map[string]string{
		"WAYFINDR_CONFIG":   filepath.Join(dir, "config", "wayfindr.toml"),
		"WAYFINDR_DATA_DIR": filepath.Join(dir, "data"),
	}
	env := paths.Env{
		Getenv:  func(k string) string { return vars[k] },
		HomeDir: func() (string, error) { return dir, nil },
	}
	p, err := paths.Resolve(env)

	if err != nil {
		t.Fatalf("paths.Resolve: %v", err)
	}

	return host{env: env, paths: p, keychain: &fakekeychain.Keychain{}}
}

// writeConfig writes cfg as the host's wayfindr.toml.
func (h host) writeConfig(t *testing.T, cfg map[string]any) {
	t.Helper()
	var buf bytes.Buffer

	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		t.Fatalf("encode config: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(h.paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(h.paths.ConfigFile, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// options are the daemon options for this host. The resolver sees no
// environment variables.
func (h host) options(t *testing.T) daemon.Options {
	return daemon.Options{
		Env:       h.env,
		LookupEnv: func(string) (string, bool) { return "", false },
		Keychain:  h.keychain,
		Logger:    zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.InfoLevel),
		Version:   "test",
	}
}

// freeAddr returns a loopback address with a port nothing listens on.
// Config validation rejects port 0, so the test picks a free port and
// releases it for the daemon.
func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatal(err)
	}

	addr := ln.Addr().String()

	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	return addr
}

// stdioConnector is the config table for a stdio fake.
func stdioConnector(t *testing.T, version string, opts ...fakeupstream.Option) map[string]any {
	t.Helper()
	cmd := fakeupstream.StdioCommand(t, version, opts...)
	env := map[string]string{}

	for _, kv := range cmd.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "WAYFINDR_FAKEUPSTREAM_CONFIG" {
			env[k] = v
		}
	}

	return map[string]any{"command": cmd.Path, "args": cmd.Args[1:], "env": env}
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

// call calls a meta-tool and returns its single text block, failing the
// test on a protocol error or an isError result.
func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})

	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}

	if len(res.Content) != 1 {
		t.Fatalf("%s returned %d content blocks, want 1", tool, len(res.Content))
	}

	tc, ok := res.Content[0].(*mcp.TextContent)

	if !ok {
		t.Fatalf("%s content is %T, want text", tool, res.Content[0])
	}

	if res.IsError {
		t.Fatalf("%s failed: %s", tool, tc.Text)
	}

	return tc.Text
}

// searchUntilFound searches for query until the qualified tool name is
// among the results; the catalog fills in the background after startup.
func searchUntilFound(t *testing.T, cs *mcp.ClientSession, query, name string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)

	for {
		var page struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}

		if err := json.Unmarshal([]byte(call(t, cs, "search_tools", map[string]any{"query": query})), &page); err != nil {
			t.Fatalf("search_tools response: %v", err)
		}

		for _, tool := range page.Tools {
			if tool.Name == name {
				return
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("search_tools %q never found %s; last page: %+v", query, name, page.Tools)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// children returns the pids of this process's children running the
// executable at path: the stdio fakes the daemon spawned.
func children(t *testing.T, path string) []int {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ps", "-A", "-o", "pid=,ppid=,command=").Output()

	if err != nil {
		t.Fatalf("ps: %v", err)
	}

	var pids []int

	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)

		if len(f) < 3 || f[1] != strconv.Itoa(os.Getpid()) || f[2] != path {
			continue
		}

		pid, err := strconv.Atoi(f[0])

		if err != nil {
			t.Fatalf("ps pid %q: %v", f[0], err)
		}

		pids = append(pids, pid)
	}

	return pids
}

// alive returns those of pids that still exist, as any process's child.
func alive(t *testing.T, pids []int) []int {
	t.Helper()
	var live []int

	for _, pid := range pids {
		// ps -p exits non-zero when no such process exists.
		if err := exec.CommandContext(t.Context(), "ps", "-p", strconv.Itoa(pid)).Run(); err == nil { //nolint:gosec // G204: ps with a numeric pid
			live = append(live, pid)
		}
	}

	return live
}

func TestDaemonServesSearchAndCallThenShutsDownCleanly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("child-process check uses ps")
	}

	// Registered first, so it runs last: after the fakes' cleanups close
	// their servers and the idle client connections to them.
	t.Cleanup(func() { goleak.VerifyNone(t) })

	issues := fakeupstream.StartStreamable(t, fakeupstream.V20251125, fakeupstream.WithTools(&mcp.Tool{
		Name:        "create_issue",
		Description: "Creates an issue in the tracker.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"title": map[string]any{"type": "string"}},
			"required":   []any{"title"},
		},
	}))
	files := stdioConnector(t, fakeupstream.V20250618)

	h := newHost(t)
	h.writeConfig(t, map[string]any{
		"server": map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{
			"issues": map[string]any{"url": issues.URL},
			"files":  files,
		},
	})
	refuse := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no daemon") }

	if _, err := setup.Init(t.Context(), setup.Options{Env: h.env, Keychain: h.keychain, Dial: refuse, Logger: zerolog.Nop()}); err != nil {
		t.Fatalf("setup.Init: %v", err)
	}

	token, err := clienttoken.Read(t.Context(), h.env, h.paths)

	if err != nil {
		t.Fatalf("clienttoken.Read: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := daemon.New(ctx, h.options(t))

	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()

	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   "http://" + d.Addr().String() + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{token: token, next: transport}},
		MaxRetries: -1,
	}, nil)

	if err != nil {
		t.Fatalf("connect to the daemon: %v", err)
	}

	searchUntilFound(t, cs, "create issue", "issues.create_issue")
	searchUntilFound(t, cs, "echo", "files.echo")

	got := call(t, cs, "call_tool", map[string]any{"name": "issues.create_issue", "arguments": map[string]any{"title": "Broken"}})

	if diff := cmp.Diff(`{"title":"Broken"}`, got); diff != "" {
		t.Errorf("call_tool issues.create_issue (-want +got):\n%s", diff)
	}

	got = call(t, cs, "call_tool", map[string]any{"name": "files.echo", "arguments": map[string]any{"text": "hi"}})

	if diff := cmp.Diff(`{"text":"hi"}`, got); diff != "" {
		t.Errorf("call_tool files.echo (-want +got):\n%s", diff)
	}

	spawned := children(t, files["command"].(string))

	if len(spawned) == 0 {
		t.Fatal("found no stdio child of the daemon while it runs")
	}

	if err := cs.Close(); err != nil {
		t.Errorf("close client session: %v", err)
	}

	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}

	if live := alive(t, spawned); len(live) != 0 {
		t.Errorf("stdio children %v outlived the daemon", live)
	}
}

func TestDaemonRefusesATakenPort(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = ln.Close() }()

	h := newHost(t)
	h.writeConfig(t, map[string]any{"server": map[string]any{"listen": ln.Addr().String()}})
	_, err = daemon.New(t.Context(), h.options(t))

	if err == nil || !strings.Contains(err.Error(), "daemon already running?") {
		t.Fatalf("daemon.New on a taken port: %v, want an error asking whether the daemon is already running", err)
	}
}

// A client's scoped token can leak into the daemon's environment as
// WAYFINDR_CLIENT_TOKEN, or be written to <data>/client-token. Taken as the
// default token it would open every connector and the admin API to that
// token's holder, so the daemon refuses to start, naming where the value
// came from but never the value.
func TestDaemonRefusesAScopedTokenAsTheDefault(t *testing.T) {
	tests := []struct {
		name string
		// put makes the scoped value the default token, and returns the
		// source the refusal must name.
		put func(t *testing.T, h *host, scoped string) string
	}{
		{"from the environment", func(_ *testing.T, h *host, scoped string) string {
			getenv := h.env.Getenv
			h.env.Getenv = func(k string) string {
				if k == clienttoken.EnvVar {
					return scoped
				}

				return getenv(k)
			}

			return clienttoken.EnvVar + " holds a scoped client token"
		}},
		{"from the file", func(t *testing.T, h *host, scoped string) string {
			t.Helper()

			if err := os.WriteFile(h.paths.ClientTokenFile(), []byte(scoped+"\n"), paths.SecretFileMode); err != nil {
				t.Fatal(err)
			}

			return h.paths.ClientTokenFile() + " holds a scoped client token"
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHost(t)
			h.writeConfig(t, map[string]any{"server": map[string]any{"listen": freeAddr(t)}})
			refuse := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no daemon") }

			if _, err := setup.Init(t.Context(), setup.Options{Env: h.env, Keychain: h.keychain, Dial: refuse, Logger: zerolog.Nop()}); err != nil {
				t.Fatalf("setup.Init: %v", err)
			}

			scoped, err := clienttoken.Generate()

			if err != nil {
				t.Fatal(err)
			}

			want := tt.put(t, &h, scoped)
			_, err = daemon.New(t.Context(), h.options(t))

			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("daemon.New with a scoped default token: %v, want a refusal containing %q", err, want)
			}

			if strings.Contains(err.Error(), scoped) {
				t.Error("the error quotes the token value")
			}
		})
	}
}

// Two daemons on one data directory would share its store and break the
// single-daemon assumption of the credentials lock and single-flight
// refresh (ADR 0003, 0004). A second daemon whose server.listen differs
// gets past the bind check, so the daemon lock refuses it, naming the
// running one; once that one has stopped, a daemon starts again.
func TestASecondDaemonOnOneDataDirRefusesToStart(t *testing.T) {
	h := newHost(t)
	first := freeAddr(t)
	h.writeConfig(t, map[string]any{"server": map[string]any{"listen": first}})
	refuse := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no daemon") }

	if _, err := setup.Init(t.Context(), setup.Options{Env: h.env, Keychain: h.keychain, Dial: refuse, Logger: zerolog.Nop()}); err != nil {
		t.Fatalf("setup.Init: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := daemon.New(ctx, h.options(t))

	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()

	// server.listen is edited while the first daemon runs.
	second := freeAddr(t)
	h.writeConfig(t, map[string]any{"server": map[string]any{"listen": second}})
	_, err = daemon.New(t.Context(), h.options(t))

	if err == nil || !strings.Contains(err.Error(), "daemon already running at "+first) {
		t.Fatalf("a second daemon.New on the data directory: %v, want an error naming the daemon running at %s", err, first)
	}

	// The refused daemon holds nothing, its port included.
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", second)

	if err != nil {
		t.Errorf("listen on the refused daemon's %s: %v, want it free", second, err)
	} else {
		_ = ln.Close()
	}

	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}

	// The first daemon has stopped, so the next one starts.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	d, err = daemon.New(ctx, h.options(t))

	if err != nil {
		t.Fatalf("daemon.New once the first daemon stopped: %v", err)
	}

	served = make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()
	cancel()

	if err := <-served; err != nil {
		t.Errorf("Serve: %v", err)
	}
}

// statusOf is the HTTP status of an *admin.APIError in err's chain, or 0.
func statusOf(err error) int {
	if ae := (*admin.APIError)(nil); errors.As(err, &ae) {
		return ae.Status
	}

	return 0
}
