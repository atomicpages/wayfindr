//go:build !windows

package shim_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// These tests run the real binary: `wayfindr stdio` must start a detached
// `wayfindr start --foreground`, which only a separate executable can show. Each test
// builds one into its temp dir; the go build cache keeps that quick. They
// find the daemons with ps and stop them with SIGTERM, so they do not build
// on Windows.

// machine is one isolated host for the binary: config, data directory,
// master key, and client token all come from its environment, so the
// binary never touches the user's files or keychain.
type machine struct {
	bin     string
	env     []string
	config  string
	dataDir string
	logs    string
	// project is the directory shims start in, below HOME, where a test
	// may write a project scope.
	project string
}

func newMachine(t *testing.T) machine {
	t.Helper()

	return newMachineAt(t, fakeupstream.V20251125)
}

// newMachineAt is newMachine with the issues upstream at version, with
// extra fake options.
func newMachineAt(t *testing.T, version string, extra ...fakeupstream.Option) machine {
	t.Helper()
	goTool, err := exec.LookPath("go")

	if err != nil {
		t.Skip("go tool not on PATH")
	}

	// The upstream first: its cleanup must run after the daemons are gone,
	// or closing it waits on their open streams.
	issues := fakeupstream.StartStreamable(t, version, append([]fakeupstream.Option{fakeupstream.WithTools(&mcp.Tool{
		Name:        "create_issue",
		Description: "Creates an issue in the tracker.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"title": map[string]any{"type": "string"}},
			"required":   []any{"title"},
		},
	})}, extra...)...)

	dir, err := filepath.EvalSymlinks(t.TempDir())

	if err != nil {
		t.Fatal(err)
	}

	m := machine{
		// Resolved, so it matches the command line ps shows for the daemon.
		bin:     filepath.Join(dir, "bin", "wayfindr"),
		config:  filepath.Join(dir, "config", "wayfindr.toml"),
		dataDir: filepath.Join(dir, "data"),
		logs:    filepath.Join(dir, "logs"),
		project: filepath.Join(dir, "project"),
	}
	build := exec.CommandContext(t.Context(), goTool, "build", "-o", m.bin, "github.com/atomicpages/wayfindr/cmd/wayfindr") //nolint:gosec // G204: the go tool building this module

	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Every daemon the test started is stopped, and none may linger.
	t.Cleanup(func() { stopDaemons(t, m.bin) })

	writeConfig(t, m.config, map[string]any{
		"server":     map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"issues": map[string]any{"url": issues.URL}},
	})

	for _, d := range []string{m.dataDir, m.logs, m.project} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	m.env = []string{
		"HOME=" + dir,
		"PATH=" + os.Getenv("PATH"),
		"WAYFINDR_CONFIG=" + m.config,
		"WAYFINDR_DATA_DIR=" + m.dataDir,
		"WAYFINDR_SECRET_KEY=" + base64.StdEncoding.EncodeToString(randomBytes(t, 32)),
		"WAYFINDR_CLIENT_TOKEN=wf_" + base64.RawURLEncoding.EncodeToString(randomBytes(t, 32)),
	}

	return m
}

// startShim runs `wayfindr stdio` as an MCP client would and returns the
// client's session. Closing the session (at cleanup) ends the shim.
func (m machine) startShim(t *testing.T, name string) *mcp.ClientSession {
	t.Helper()
	cs, err := m.connectShim(t, name)

	if err != nil {
		t.Fatalf("%v\n%s", err, m.logOf(name))
	}

	return cs
}

// connectShim is startShim for use off the test goroutine: it returns its
// error rather than failing the test.
func (m machine) connectShim(t *testing.T, name string) (*mcp.ClientSession, error) {
	t.Helper()

	return m.connectShimWith(t, name, nil, nil)
}

// startShimAt is startShim for a client that asks for protocol version.
func (m machine) startShimAt(t *testing.T, name, version string) *mcp.ClientSession {
	t.Helper()
	cs, err := m.connectShimWith(t, name, &mcp.ClientSessionOptions{ProtocolVersion: version}, nil)

	if err != nil {
		t.Fatalf("%v\n%s", err, m.logOf(name))
	}

	if got := cs.InitializeResult().ProtocolVersion; got != version {
		t.Fatalf("negotiated protocol version %s, want %s", got, version)
	}

	return cs
}

// connectShimWith connects a new shim with the client session options opts
// and the client options copts (nil for the SDK's defaults), passing args
// after `wayfindr stdio`.
func (m machine) connectShimWith(t *testing.T, name string, opts *mcp.ClientSessionOptions, copts *mcp.ClientOptions, args ...string) (*mcp.ClientSession, error) {
	t.Helper()
	// Not t.Context(): it ends before cleanup, which would kill the shim
	// instead of letting it exit when the session closes its stdin.
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), m.bin, append([]string{"stdio"}, args...)...) //nolint:gosec // G204: the binary this test built
	cmd.Env = m.env
	cmd.Dir = m.project
	stderr, err := os.Create(filepath.Join(m.logs, name+".stderr")) //nolint:gosec // inside the test's temp dir

	if err != nil {
		return nil, err
	}

	t.Cleanup(func() { _ = stderr.Close() })
	cmd.Stderr = stderr
	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "v0"}, copts)
	cs, err := client.Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, opts)

	if err != nil {
		return nil, fmt.Errorf("connect to the shim %s: %w", name, err)
	}

	t.Cleanup(func() {
		if err := cs.Close(); err != nil {
			t.Errorf("close shim %s: %v", name, err)
		}
	})

	return cs, nil
}

// logOf returns what a shim wrote to stderr and what the daemons logged.
func (m machine) logOf(name string) string {
	shim, _ := os.ReadFile(filepath.Join(m.logs, name+".stderr"))      //nolint:gosec // inside the test's temp dir
	daemon, _ := os.ReadFile(filepath.Join(m.dataDir, "wayfindr.log")) //nolint:gosec // inside the test's temp dir

	return "shim stderr:\n" + string(shim) + "\ndaemon log:\n" + string(daemon)
}

func TestShimStartsTheDaemonAndForwardsSearchAndCall(t *testing.T) {
	m := newMachine(t)

	if pids := daemons(t, m.bin); len(pids) != 0 {
		t.Fatalf("daemons %v running before the shim started", pids)
	}

	cs := m.startShim(t, "shim")

	tools, err := cs.ListTools(t.Context(), nil)

	if err != nil {
		t.Fatalf("tools/list through the shim: %v", err)
	}

	var names []string

	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}

	slices.Sort(names)

	if diff := cmp.Diff([]string{"call_tool", "search_tools"}, names); diff != "" {
		t.Errorf("tools through the shim (-want +got):\n%s", diff)
	}

	waitForIssueTool(t, cs)
	got := callText(t, cs, "call_tool", map[string]any{"name": "issues.create_issue", "arguments": map[string]any{"title": "Broken"}})

	if diff := cmp.Diff(`{"title":"Broken"}`, got); diff != "" {
		t.Errorf("call_tool through the shim (-want +got):\n%s", diff)
	}

	if pids := daemons(t, m.bin); len(pids) != 1 {
		t.Errorf("daemons = %v, want exactly one", pids)
	}
}

// The shim declares only tools, as the gateway does, so every request of the
// prompts, resources, and completions capabilities is "method not found" at
// every client protocol version, the answer /mcp gives (design §7.6, §7.9),
// rather than an empty list.
func TestShimAnswersUndeclaredCapabilitiesWithMethodNotFoundAtEveryVersion(t *testing.T) {
	m := newMachine(t)
	requests := []struct {
		method string
		do     func(context.Context, *mcp.ClientSession) error
	}{
		{"prompts/list", func(ctx context.Context, cs *mcp.ClientSession) error {
			_, err := cs.ListPrompts(ctx, nil)

			return err
		}},
		{"prompts/get", func(ctx context.Context, cs *mcp.ClientSession) error {
			_, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greeting"})

			return err
		}},
		{"resources/list", func(ctx context.Context, cs *mcp.ClientSession) error {
			_, err := cs.ListResources(ctx, nil)

			return err
		}},
		{"resources/templates/list", func(ctx context.Context, cs *mcp.ClientSession) error {
			_, err := cs.ListResourceTemplates(ctx, nil)

			return err
		}},
		{"resources/read", func(ctx context.Context, cs *mcp.ClientSession) error {
			_, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///notes.txt"})

			return err
		}},
		{"completion/complete", func(ctx context.Context, cs *mcp.ClientSession) error {
			_, err := cs.Complete(ctx, &mcp.CompleteParams{
				Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "greeting"},
				Argument: mcp.CompleteParamsArgument{Name: "name"},
			})

			return err
		}},
	}

	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			cs := m.startShimAt(t, "shim-"+version, version)

			for _, r := range requests {
				var werr *jsonrpc.Error

				if err := r.do(t.Context(), cs); !errors.As(err, &werr) || werr.Code != -32601 {
					t.Errorf("%s: err = %v, want JSON-RPC error -32601", r.method, err)
				}
			}

			// The session survives, and the declared capability is served.
			if _, err := cs.ListTools(t.Context(), nil); err != nil {
				t.Errorf("tools/list after them: %v", err)
			}
		})
	}
}

func TestShimForwardsIsErrorResultsUnchanged(t *testing.T) {
	m := newMachine(t)
	cs := m.startShim(t, "shim")

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "call_tool", Arguments: map[string]any{"name": "nosuch.tool"}})

	if err != nil {
		t.Fatalf("call_tool through the shim: %v", err)
	}

	if !res.IsError || !strings.Contains(text(t, res), "search_tools") {
		t.Errorf("result = %+v, want the gateway's isError result suggesting search_tools", res)
	}
}

// The daemon's call log names the client behind the shim, at every
// version: the shim forwards its clientInfo, which the daemon's stateless
// endpoint would otherwise see as the shim's own, wayfindr-stdio.
func TestShimCallsAreLoggedUnderTheStdioClientsName(t *testing.T) {
	m := newMachine(t)

	for _, version := range fakeupstream.Versions() {
		name := "agent-" + version
		cs := m.startShimAt(t, name, version)
		waitForIssueTool(t, cs)
		callText(t, cs, "call_tool", map[string]any{"name": "issues.create_issue", "arguments": map[string]any{"title": "x"}})
	}

	var want []string

	for _, version := range slices.Backward(fakeupstream.Versions()) {
		want = append(want, "agent-"+version)
	}

	deadline := time.Now().Add(15 * time.Second)

	for {
		cmd := exec.CommandContext(t.Context(), m.bin, "calls", "--json") //nolint:gosec // G204: the binary this test built
		cmd.Env = m.env
		out, err := cmd.Output()

		if err != nil {
			t.Fatalf("wayfindr calls --json: %v\n%s", err, out)
		}

		var res struct {
			Calls []struct {
				Client string `json:"client"`
			} `json:"calls"`
		}

		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("wayfindr calls --json: %v\n%s", err, out)
		}

		var got []string

		for _, c := range res.Calls {
			got = append(got, c.Client)
		}

		if cmp.Equal(want, got) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("clients in the call log, newest first (-want +got):\n%s", cmp.Diff(want, got))
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func TestShimReportsALostDaemonAndRestartsItOnTheNextCall(t *testing.T) {
	m := newMachine(t)
	cs := m.startShim(t, "shim")
	waitForIssueTool(t, cs)

	stopDaemons(t, m.bin)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "search_tools", Arguments: map[string]any{"query": "issue"}})

	if err != nil {
		t.Fatalf("search_tools with the daemon gone: %v, want an isError result", err)
	}

	if !res.IsError || !strings.Contains(text(t, res), "wayfindr status") {
		t.Errorf("result = %+v, want an isError result suggesting `wayfindr status`", res)
	}

	waitForIssueTool(t, cs)

	if pids := daemons(t, m.bin); len(pids) != 1 {
		t.Errorf("daemons after the restart = %v, want exactly one", pids)
	}
}

// A config the daemon can't load leaves it running its last good config, so
// a shim must still reach it: it reads only the listen address.
func TestShimConnectsToARunningDaemonDespiteAnInvalidConfig(t *testing.T) {
	m := newMachine(t)
	waitForIssueTool(t, m.startShim(t, "first"))

	f, err := os.OpenFile(m.config, os.O_APPEND|os.O_WRONLY, 0)

	if err != nil {
		t.Fatal(err)
	}

	// An unknown key and a url that fails validation.
	if _, err := f.WriteString("\n[connectors.broken]\nurl = \"not a url\"\nbogus = 1\n"); err != nil {
		t.Fatal(err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	cs := m.startShim(t, "second")
	waitForIssueTool(t, cs)

	if pids := daemons(t, m.bin); len(pids) != 1 {
		t.Errorf("daemons = %v, want exactly one", pids)
	}
}

// After server.listen is edited, a shim finds no daemon at the new address.
// The daemon lock says one runs on the data directory, so the shim reports
// it, with the address it listens on, instead of starting a second daemon
// on the same store.
func TestShimReportsTheRunningDaemonWhenServerListenChanged(t *testing.T) {
	m := newMachine(t)
	waitForIssueTool(t, m.startShim(t, "first"))

	data, err := os.ReadFile(m.config)

	if err != nil {
		t.Fatal(err)
	}

	var cfg struct {
		Server struct {
			Listen string `toml:"listen"`
		} `toml:"server"`
	}

	if err := toml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}

	running := cfg.Server.Listen
	edited := strings.Replace(string(data), running, freeAddr(t), 1)

	if err := os.WriteFile(m.config, []byte(edited), 0o600); err != nil { //nolint:gosec // inside the test's temp dir
		t.Fatal(err)
	}

	if _, err := m.connectShim(t, "second"); err == nil {
		t.Fatal("the shim connected with server.listen changed under a running daemon, want it to fail")
	}

	// The shim's own stderr: a daemon it started would log the lock's
	// refusal too, into the daemon log.
	stderr, err := os.ReadFile(filepath.Join(m.logs, "second.stderr")) //nolint:gosec // inside the test's temp dir

	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(stderr), "daemon already running at "+running) || !strings.Contains(string(stderr), "restart") {
		t.Errorf("the shim's stderr does not name the daemon at %s and suggest a restart:\n%s", running, m.logOf("second"))
	}

	if strings.Contains(string(stderr), "starting it") {
		t.Errorf("the shim started a daemon:\n%s", stderr)
	}

	if pids := daemons(t, m.bin); len(pids) != 1 {
		t.Errorf("daemons = %v, want exactly one", pids)
	}
}

// A daemon killed outright releases nothing itself; the OS drops its
// daemon lock, so the next shim starts a daemon again.
func TestShimStartsADaemonAfterTheLastOneWasKilled(t *testing.T) {
	m := newMachine(t)
	waitForIssueTool(t, m.startShim(t, "first"))

	for _, pid := range daemons(t, m.bin) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatalf("SIGKILL daemon %d: %v", pid, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)

	for len(daemons(t, m.bin)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the killed daemon is still running")
		}

		time.Sleep(50 * time.Millisecond)
	}

	waitForIssueTool(t, m.startShim(t, "second"))

	if pids := daemons(t, m.bin); len(pids) != 1 {
		t.Errorf("daemons = %v, want exactly one", pids)
	}
}

func TestTwoShimsStartingAtOnceShareOneDaemon(t *testing.T) {
	m := newMachine(t)

	sessions := make([]*mcp.ClientSession, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup

	for i := range sessions {
		wg.Go(func() { sessions[i], errs[i] = m.connectShim(t, "shim"+strconv.Itoa(i)) })
	}

	wg.Wait()

	for i, cs := range sessions {
		if errs[i] != nil {
			t.Fatalf("%v\n%s", errs[i], m.logOf("shim"+strconv.Itoa(i)))
		}

		waitForIssueTool(t, cs)
	}

	// A daemon that lost the race for the port exits on its own; it may
	// still be on its way out when both shims are already served.
	deadline := time.Now().Add(5 * time.Second)

	for {
		pids := daemons(t, m.bin)

		if len(pids) == 1 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("daemons = %v, want exactly one\n%s", pids, m.logOf("shim0"))
		}

		time.Sleep(50 * time.Millisecond)
	}

	// Whether both shims started a daemon depends on timing; say which ran.
	if strings.Contains(m.logOf("shim0"), "daemon already running?") {
		t.Log("both shims started a daemon; the second lost the port")
	}
}

// waitForIssueTool searches for the fake's create_issue through the shim
// until it is among the results; the catalog fills in the background
// after the daemon starts.
func waitForIssueTool(t *testing.T, cs *mcp.ClientSession) {
	t.Helper()
	const query, name = "create issue", "issues.create_issue"
	deadline := time.Now().Add(15 * time.Second)

	for {
		var page struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}

		if err := json.Unmarshal([]byte(callText(t, cs, "search_tools", map[string]any{"query": query})), &page); err != nil {
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

// callText calls a meta-tool and returns its single text block, failing
// the test on a protocol error or an isError result.
func callText(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})

	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}

	if res.IsError {
		t.Fatalf("%s failed: %s", tool, text(t, res))
	}

	return text(t, res)
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()

	if len(res.Content) != 1 {
		t.Fatalf("result has %d content blocks, want 1", len(res.Content))
	}

	tc, ok := res.Content[0].(*mcp.TextContent)

	if !ok {
		t.Fatalf("content is %T, want text", res.Content[0])
	}

	return tc.Text
}

// daemons returns the pids of the live `<bin> start` processes.
func daemons(t *testing.T, bin string) []int {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=,stat=,command=").Output() //nolint:noctx // also runs at cleanup, after the test's context ends

	if err != nil {
		t.Fatalf("ps: %v", err)
	}

	var pids []int

	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)

		// A zombie (stat Z) has exited; only its exit status is left.
		if len(f) < 4 || strings.HasPrefix(f[1], "Z") || f[2] != bin || f[3] != "start" {
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

// stopDaemons sends SIGTERM to every daemon of bin and waits for them to
// exit. One that ignores it is killed and fails the test.
func stopDaemons(t *testing.T, bin string) {
	t.Helper()
	pids := daemons(t, bin)

	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("SIGTERM daemon %d: %v", pid, err)
		}
	}

	deadline := time.Now().Add(15 * time.Second)

	for len(daemons(t, bin)) != 0 {
		if time.Now().After(deadline) {
			for _, pid := range daemons(t, bin) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}

			t.Errorf("daemons %v ignored SIGTERM and were killed", pids)

			return
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func writeConfig(t *testing.T, path string, cfg map[string]any) {
	t.Helper()
	var buf bytes.Buffer

	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		t.Fatalf("encode config: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
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

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)

	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}

	return b
}
