package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/daemon"
	"github.com/atomicpages/wayfindr/internal/setup"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/drivers"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// The tests in this file edit the config file of a running daemon and watch
// the result through its admin status and its MCP endpoint (design §7.8
// step 2), as a user editing wayfindr.toml would.

// running is a daemon serving in the background, with clients for its admin
// API and its MCP endpoint. It shuts down when the test ends.
type running struct {
	d      *daemon.Daemon
	addr   string
	client *http.Client
	mcp    *mcp.ClientSession
	// stop shuts the daemon down and waits for Serve to return. It is
	// idempotent; the test's cleanup calls it too.
	stop func()
}

// serve writes cfg as h's config, initializes h, and serves a daemon with
// opts until the test ends.
func serve(t *testing.T, h host, opts daemon.Options, cfg map[string]any) running {
	t.Helper()
	initHost(t, h, cfg)

	return start(t, h, opts)
}

// initHost writes cfg as h's config and runs `wayfindr init` on h.
func initHost(t *testing.T, h host, cfg map[string]any) {
	t.Helper()
	h.writeConfig(t, cfg)
	refuse := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no daemon") }

	if _, err := setup.Init(t.Context(), setup.Options{Env: h.env, Keychain: h.keychain, Dial: refuse, Logger: zerolog.Nop()}); err != nil {
		t.Fatalf("setup.Init: %v", err)
	}
}

// start serves a daemon on the initialized h with opts until the test ends.
func start(t *testing.T, h host, opts daemon.Options) running {
	t.Helper()
	token, err := clienttoken.Read(t.Context(), h.env, h.paths)

	if err != nil {
		t.Fatalf("clienttoken.Read: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d, err := daemon.New(ctx, opts)

	if err != nil {
		cancel()
		t.Fatalf("daemon.New: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()
	transport := &http.Transport{}
	client := &http.Client{Transport: bearer{token: token, next: transport}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   "http://" + d.Addr().String() + "/mcp",
		HTTPClient: client,
		MaxRetries: -1,
	}, nil)

	if err != nil {
		cancel()
		t.Fatalf("connect to the daemon: %v", err)
	}

	stop := sync.OnceFunc(func() {
		_ = cs.Close()
		// Before shutdown, not after: the transport may hold a connection
		// it dialed for a request that another connection served first.
		// The daemon has read no request on it, and http.Server.Shutdown
		// waits up to 5s for such a connection in case one is on its way,
		// which would hold up a test that times the shutdown.
		transport.CloseIdleConnections()
		cancel()

		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("Serve did not return after its context was cancelled")
		}
	})
	t.Cleanup(stop)

	return running{d: d, addr: d.Addr().String(), client: client, mcp: cs, stop: stop}
}

// waitStatus polls /admin/status until ok accepts it, and returns it.
func (r running) waitStatus(t *testing.T, want string, ok func(admin.Status) bool) admin.Status {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)

	for {
		got := getStatus(t, r.client, "http://"+r.addr)

		if ok(got) {
			return got
		}

		if time.Now().After(deadline) {
			t.Fatalf("status never showed %s; last status: %+v", want, got)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// search returns the qualified names search_tools finds for query.
func (r running) search(t *testing.T, query string) []string {
	t.Helper()
	var page struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}

	if err := json.Unmarshal([]byte(call(t, r.mcp, "search_tools", map[string]any{"query": query})), &page); err != nil {
		t.Fatalf("search_tools response: %v", err)
	}

	var names []string

	for _, tool := range page.Tools {
		names = append(names, tool.Name)
	}

	return names
}

// connector returns the named connector's row of s.
func connector(s admin.Status, name string) (admin.Connector, bool) {
	i := slices.IndexFunc(s.Connectors, func(c admin.Connector) bool { return c.Name == name })

	if i < 0 {
		return admin.Connector{}, false
	}

	return s.Connectors[i], true
}

// synced reports whether every named connector is ready with its tools in
// the catalog, and no other connector is configured.
func synced(names ...string) func(admin.Status) bool {
	return func(s admin.Status) bool {
		if len(s.Connectors) != len(names) {
			return false
		}

		for _, name := range names {
			c, ok := connector(s, name)

			if !ok || c.State != upstream.StateReady || c.Tools == 0 || c.LastSync == nil {
				return false
			}
		}

		return true
	}
}

// encodeConfig renders cfg as TOML.
func encodeConfig(t *testing.T, cfg map[string]any) []byte {
	t.Helper()
	var buf bytes.Buffer

	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		t.Fatalf("encode config: %v", err)
	}

	return buf.Bytes()
}

// saveAtomically saves cfg as an editor does: it writes a temp file beside
// the config and renames it over the config, replacing its inode.
func (h host) saveAtomically(t *testing.T, cfg map[string]any) {
	t.Helper()
	saveAtomically(t, h.paths.ConfigFile, cfg)
}

// saveAtomically saves cfg to path as an editor does: it writes a temp file
// beside path and renames it over path, replacing its inode.
func saveAtomically(t *testing.T, path string, cfg map[string]any) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".swp")

	if err := os.WriteFile(tmp, encodeConfig(t, cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestReloadKeepsTheRunningConfigWhenAnEditIsInvalid(t *testing.T) {
	h := newHost(t)
	listen := freeAddr(t)
	files := stdioConnector(t, fakeupstream.V20250618)
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files},
	})
	r.waitStatus(t, "files synced", synced("files"))

	// A half-typed table header.
	if err := os.WriteFile(h.paths.ConfigFile, []byte("[connectors.files\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := r.waitStatus(t, "a config error", func(s admin.Status) bool { return s.ConfigError != "" })

	if !synced("files")(got) {
		t.Errorf("after an invalid edit, connectors = %+v, want files still ready and synced", got.Connectors)
	}

	if names := r.search(t, "echo"); !slices.Contains(names, "files.echo") {
		t.Errorf("after an invalid edit, search_tools echo = %v, want files.echo still found", names)
	}

	// The next good save clears the error and applies.
	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files, "notes": stdioConnector(t, fakeupstream.V20251125)},
	})
	got = r.waitStatus(t, "files and notes synced", synced("files", "notes"))

	if got.ConfigError != "" {
		t.Errorf("after a good save, config error = %q, want none", got.ConfigError)
	}
}

func TestReloadAppliesAnAtomicRenameSave(t *testing.T) {
	h := newHost(t)
	listen := freeAddr(t)
	files := stdioConnector(t, fakeupstream.V20250618)
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files},
	})
	r.waitStatus(t, "files synced", synced("files"))

	h.saveAtomically(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files, "notes": stdioConnector(t, fakeupstream.V20251125)},
	})
	r.waitStatus(t, "files and notes synced", synced("files", "notes"))

	// The watch survives the rename: a second save applies too.
	h.saveAtomically(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"notes": stdioConnector(t, fakeupstream.V20251125)},
	})
	r.waitStatus(t, "only notes", synced("notes"))
}

// TestReloadFollowsASymlinkedConfig: a config file that is a symlink into
// another directory, as stow, chezmoi, and home-manager make, reloads on
// each save of its target there; a repoint of the link reloads; and edits
// of the new target, in a third directory, reload too (wayfindr-a2b0).
func TestReloadFollowsASymlinkedConfig(t *testing.T) {
	h := newHost(t)
	listen := freeAddr(t)
	files := stdioConnector(t, fakeupstream.V20250618)
	notes := stdioConnector(t, fakeupstream.V20251125)
	initHost(t, h, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files},
	})
	target := filepath.Join(t.TempDir(), "wayfindr.toml")

	if err := os.Rename(h.paths.ConfigFile, target); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(target, h.paths.ConfigFile); err != nil {
		t.Fatal(err)
	}

	r := start(t, h, h.options(t))
	r.waitStatus(t, "files synced", synced("files"))

	saveAtomically(t, target, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files, "notes": notes},
	})
	r.waitStatus(t, "files and notes synced after the target was saved", synced("files", "notes"))

	// Each save, not just the first.
	saveAtomically(t, target, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"notes": notes},
	})
	r.waitStatus(t, "only notes after the target was saved again", synced("notes"))

	// Repointed as home-manager does: a new link renamed over the old one,
	// to a target with another name in another directory.
	next := filepath.Join(t.TempDir(), "generation.toml")

	if err := os.WriteFile(next, encodeConfig(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files},
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(filepath.Dir(h.paths.ConfigFile), ".wayfindr.toml.link")

	if err := os.Symlink(next, link); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(link, h.paths.ConfigFile); err != nil {
		t.Fatal(err)
	}

	r.waitStatus(t, "only files after the link was repointed", synced("files"))

	// The new target is polled: an edit in place applies.
	if err := os.WriteFile(next, encodeConfig(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files, "notes": notes},
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	r.waitStatus(t, "files and notes synced after the new target was edited", synced("files", "notes"))
}

func TestReloadRemovedConnectorDropsItsToolsAndKeepsItsCredential(t *testing.T) {
	h := newHost(t)
	listen := freeAddr(t)
	notes := stdioConnector(t, fakeupstream.V20251125)
	initHost(t, h, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": stdioConnector(t, fakeupstream.V20250618), "notes": notes},
	})
	// A credential of files, which the reload below removes.
	st, err := drivers.Open(t.Context(), "sqlite", h.paths.DBFile())

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatal(err)
	}

	if err := st.PutCredential(t.Context(), store.Credential{UserID: user, Connector: "files", Resource: "https://files.example/mcp"}); err != nil {
		t.Fatal(err)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	r := start(t, h, h.options(t))
	r.waitStatus(t, "files and notes synced", synced("files", "notes"))

	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"notes": notes},
	})
	// The tools of files leave search; its credential is kept, listed as
	// orphaned.
	deadline := time.Now().Add(15 * time.Second)

	for got := r.search(t, "echo"); !cmp.Equal([]string{"notes.echo"}, got); got = r.search(t, "echo") {
		if time.Now().After(deadline) {
			t.Fatalf("search_tools echo after removing files = %v, want [notes.echo]", got)
		}

		time.Sleep(50 * time.Millisecond)
	}

	got := r.waitStatus(t, "only notes", synced("notes"))

	if diff := cmp.Diff([]string{"files"}, got.OrphanedCredentials); diff != "" {
		t.Errorf("orphaned credentials after removing files (-want +got):\n%s", diff)
	}
}

func TestStartupDropsTheCatalogOfAConnectorRemovedWhileStopped(t *testing.T) {
	h := newHost(t)
	listen := freeAddr(t)
	files := stdioConnector(t, fakeupstream.V20250618)
	initHost(t, h, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files, "notes": stdioConnector(t, fakeupstream.V20251125)},
	})
	// A credential of notes, which is removed while the daemon is stopped.
	putCredential(t, h, "notes")

	r := start(t, h, h.options(t))
	r.waitStatus(t, "files and notes synced", synced("files", "notes"))
	r.stop()

	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files},
	})
	r = start(t, h, h.options(t))

	// From the first request on, not only once files has connected.
	if names := r.search(t, "echo"); slices.Contains(names, "notes.echo") {
		t.Errorf("search_tools echo right after startup = %v, want notes.echo gone", names)
	}

	// The credential is kept, and listed as orphaned.
	r.waitStatus(t, "only files, with the credential of notes orphaned", func(s admin.Status) bool {
		return synced("files")(s) && cmp.Equal([]string{"notes"}, s.OrphanedCredentials)
	})

	if diff := cmp.Diff([]string{"files.echo"}, r.search(t, "echo")); diff != "" {
		t.Errorf("search_tools echo after restarting without notes (-want +got):\n%s", diff)
	}

	r.stop()

	st, err := drivers.Open(t.Context(), "sqlite", h.paths.DBFile())

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })
	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatal(err)
	}

	counts, err := st.ToolCounts(t.Context(), user)

	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(map[string]int{"files": 1}, counts); diff != "" {
		t.Errorf("tool counts after restarting without notes (-want +got):\n%s", diff)
	}
}

// putCredential gives connector a credential in h's store, as a finished
// OAuth flow would.
func putCredential(t *testing.T, h host, connector string) {
	t.Helper()
	st, err := drivers.Open(t.Context(), "sqlite", h.paths.DBFile())

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatal(err)
	}

	if err := st.PutCredential(t.Context(), store.Credential{UserID: user, Connector: connector, Resource: "https://" + connector + ".example/mcp"}); err != nil {
		t.Fatal(err)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReloadURLChangedReconnectsAndSyncs(t *testing.T) {
	before := fakeupstream.StartStreamable(t, fakeupstream.V20251125, fakeupstream.WithTools(&mcp.Tool{Name: "create_issue", Description: "Creates an issue."}))
	after := fakeupstream.StartStreamable(t, fakeupstream.V20251125, fakeupstream.WithTools(&mcp.Tool{Name: "close_issue", Description: "Closes an issue."}))
	h := newHost(t)
	listen := freeAddr(t)
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"issues": map[string]any{"url": before.URL}},
	})
	r.waitStatus(t, "issues synced", synced("issues"))
	searchUntilFound(t, r.mcp, "create issue", "issues.create_issue")

	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"issues": map[string]any{"url": after.URL}},
	})
	searchUntilFound(t, r.mcp, "close issue", "issues.close_issue")

	if names := r.search(t, "create issue"); slices.Contains(names, "issues.create_issue") {
		t.Errorf("search_tools create issue = %v after the url changed, want issues.create_issue gone", names)
	}
}

func TestReloadURLChangedOAuthConnectorNeedsAuth(t *testing.T) {
	before := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	after := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	h := newHost(t)
	listen := freeAddr(t)
	oauth := map[string]any{"type": "oauth"}
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"tracker": map[string]any{"url": before.URL, "auth": oauth}},
	})
	r.waitStatus(t, "tracker settled", func(s admin.Status) bool {
		c, ok := connector(s, "tracker")

		return ok && c.State != upstream.StateStarting
	})

	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"tracker": map[string]any{"url": after.URL, "auth": oauth}},
	})
	r.waitStatus(t, "tracker needs_auth because its url changed", func(s admin.Status) bool {
		c, ok := connector(s, "tracker")

		// The link is the daemon's auth page for the connector.
		return ok && c.State == upstream.StateNeedsAuth && c.Advice != nil &&
			strings.Contains(c.Advice.Summary, "URL changed") && c.Advice.Link == "http://"+listen+"/auth/tracker"
	})
}

func TestReloadChangedConnectorReconnectsAndSyncs(t *testing.T) {
	h := newHost(t)
	listen := freeAddr(t)
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": stdioConnector(t, fakeupstream.V20250618)},
	})
	r.waitStatus(t, "files synced", synced("files"))
	searchUntilFound(t, r.mcp, "echo", "files.echo")

	// Different tools mean a different child environment: Changed.
	h.writeConfig(t, map[string]any{
		"server": map[string]any{"listen": listen},
		"connectors": map[string]any{"files": stdioConnector(t, fakeupstream.V20250618,
			fakeupstream.WithTools(&mcp.Tool{Name: "grep", Description: "Searches file contents."}))},
	})
	searchUntilFound(t, r.mcp, "searches file contents", "files.grep")

	if names := r.search(t, "echo"); slices.Contains(names, "files.echo") {
		t.Errorf("search_tools echo = %v after files changed, want files.echo gone", names)
	}
}

// embeddings is a fake OpenAI-compatible embeddings endpoint that records
// every input it embeds and each request's Authorization header.
type embeddings struct {
	mu     sync.Mutex
	inputs []string
	auths  []string
}

func (e *embeddings) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input      []string `json:"input"`
		Dimensions int      `json:"dimensions"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	e.mu.Lock()
	e.inputs = append(e.inputs, req.Input...)
	e.auths = append(e.auths, r.Header.Get("Authorization"))
	e.mu.Unlock()
	type datum struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	resp := struct {
		Data []datum `json:"data"`
	}{}

	for i := range req.Input {
		v := make([]float32, req.Dimensions)
		v[0] = 1
		resp.Data = append(resp.Data, datum{Index: i, Embedding: v})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (e *embeddings) embedded(text string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return slices.ContainsFunc(e.inputs, func(in string) bool { return strings.Contains(in, text) })
}

// authorizations returns the Authorization header of each request from
// the i-th on, in order.
func (e *embeddings) authorizations(i int) []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return slices.Clone(e.auths[min(i, len(e.auths)):])
}

func TestReloadEmbeddingChangedEmbedsTheCatalog(t *testing.T) {
	fake := &embeddings{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	h := newHost(t)
	listen := freeAddr(t)
	files := stdioConnector(t, fakeupstream.V20250618)
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files},
	})
	r.waitStatus(t, "files synced", synced("files"))

	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files},
		"embedding":  map[string]any{"base_url": srv.URL + "/v1", "model": "test-embed", "dimensions": 4},
	})
	deadline := time.Now().Add(15 * time.Second)

	for !fake.embedded("Echo the arguments back.") {
		if time.Now().After(deadline) {
			t.Fatal("the embedding pass never embedded files.echo after [embedding] was added")
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func TestReloadRestartRequiredLogsAWarningAndKeepsListening(t *testing.T) {
	h := newHost(t)
	listen := freeAddr(t)
	logs := &syncBuffer{}
	opts := h.options(t)
	opts.Logger = zerolog.New(io.MultiWriter(logs, zerolog.NewTestWriter(t))).Level(zerolog.InfoLevel)
	files := stdioConnector(t, fakeupstream.V20250618)
	r := serve(t, h, opts, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"files": files},
	})
	r.waitStatus(t, "files synced", synced("files"))

	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"files": files},
	})
	deadline := time.Now().Add(15 * time.Second)

	for !restartWarned(logs.String(), "server.listen") {
		if time.Now().After(deadline) {
			t.Fatalf("no restart warning for server.listen; log:\n%s", logs.String())
		}

		time.Sleep(50 * time.Millisecond)
	}

	// Still served on the old address.
	r.waitStatus(t, "files synced", synced("files"))
}

// restartWarned reports whether log has a warning that key takes effect on
// restart.
func restartWarned(log, key string) bool {
	for line := range strings.Lines(log) {
		var e struct {
			Level string `json:"level"`
			Key   string `json:"key"`
		}

		if json.Unmarshal([]byte(line), &e) == nil && e.Level == "warn" && e.Key == key {
			return true
		}
	}

	return false
}
