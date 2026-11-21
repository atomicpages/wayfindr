package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
	"go.uber.org/goleak"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/daemon"
	"github.com/atomicpages/wayfindr/internal/setup"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

func TestSecretSetRecoversAConnectorWaitingOnItAndItsCallsSendTheValue(t *testing.T) {
	const value = "tok-3f9a7c1e-never-logged"
	fake := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	seen := &headerLog{}
	front := recordingProxy(t, fake.URL, seen)

	h := newHost(t)
	h.writeConfig(t, map[string]any{
		"server": map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{
			"linear": map[string]any{
				"url":     front.URL,
				"headers": map[string]string{"Authorization": "Bearer ${secret:tok}"},
			},
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

	logs := &syncBuffer{}
	opts := h.options(t)
	// Debug, so the resolver's per-reference lines are captured too.
	opts.Logger = zerolog.New(zerolog.MultiLevelWriter(logs, zerolog.NewTestWriter(t))).Level(zerolog.DebugLevel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := daemon.New(ctx, opts)

	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: bearer{token: token, next: transport}}
	base := "http://" + d.Addr().String()

	// Without the secret, linear waits on it.
	deadline := time.Now().Add(15 * time.Second)

	for {
		s := getStatus(t, client, base)

		if len(s.Connectors) == 1 && s.Connectors[0].State == upstream.StateAuthError {
			if a := s.Connectors[0].Advice; a == nil || !strings.Contains(a.Summary, "secret:tok") {
				t.Fatalf("linear = %+v, want auth_error waiting on secret:tok", s.Connectors[0])
			}

			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("linear never reported the missing secret; last status: %+v", s)
		}

		time.Sleep(50 * time.Millisecond)
	}

	if n := seen.len(); n != 0 {
		t.Errorf("upstream saw %d requests before the secret was set, want none", n)
	}

	got := putSecret(t, client, base, "tok", value)

	if got.Name != "tok" || len(got.Retried) != 1 || got.Retried[0].Name != "linear" ||
		got.Retried[0].State != upstream.StateReady || got.Retried[0].Advice != nil {
		t.Errorf("PUT /admin/secrets/tok = %+v, want linear retried and ready", got)
	}

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   base + "/mcp",
		HTTPClient: client,
		MaxRetries: -1,
	}, nil)

	if err != nil {
		t.Fatalf("connect to the daemon: %v", err)
	}

	searchUntilFound(t, cs, "echo", "linear.echo")
	before := seen.len()
	call(t, cs, "call_tool", map[string]any{"name": "linear.echo", "arguments": map[string]any{"text": "hi"}})
	calls := seen.since(before)

	if len(calls) == 0 {
		t.Fatal("upstream saw no request for the call")
	}

	for _, auth := range calls {
		if auth != "Bearer "+value {
			t.Errorf("upstream saw Authorization %q, want the secret's value", auth)
		}
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

	log := logs.String()

	// The capture works: it holds the set and the reference's resolution.
	for _, want := range []string{`"message":"secret set"`, `"ref":"${secret:tok}"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the daemon log lacks %s; is the capture wired?", want)
		}
	}

	if strings.Contains(log, value) {
		t.Errorf("the daemon log contains the secret's value")
	}
}

func TestShutdownCancelsASecretSetWhoseRetryHangs(t *testing.T) {
	// The drain timeout and the grace after it (design §7.8), plus a margin.
	const budget = 10*time.Second + time.Second + 3*time.Second
	// Registered first, so it runs last: after the upstream is released and
	// closed.
	t.Cleanup(func() { goleak.VerifyNone(t) })

	// An upstream that accepts the connect and never answers it.
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}

		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(hang.Close)
	t.Cleanup(func() { close(release) })

	h := newHost(t)
	h.writeConfig(t, map[string]any{
		"server": map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{
			"slow": map[string]any{
				"url":     hang.URL,
				"headers": map[string]string{"Authorization": "Bearer ${secret:tok}"},
			},
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
	client := &http.Client{Transport: bearer{token: token, next: transport}}
	base := "http://" + d.Addr().String()

	deadline := time.Now().Add(15 * time.Second)

	for {
		s := getStatus(t, client, base)

		if len(s.Connectors) == 1 && s.Connectors[0].State == upstream.StateAuthError {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("slow never reported the missing secret; last status: %+v", s)
		}

		time.Sleep(50 * time.Millisecond)
	}

	// The set's retry connects to the upstream, which never answers.
	put := make(chan struct{})
	go func() {
		defer close(put)
		// Its error is expected: shutdown cancels the request.
		_, _ = admin.NewClient(base, "", client).PutSecret(context.Background(), "tok", "v")
	}()

	select {
	case <-arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("the secret set never connected to the upstream")
	}

	start := time.Now()
	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}

		t.Logf("Serve returned %v after shutdown began", time.Since(start).Round(time.Millisecond))
	case <-time.After(budget):
		t.Fatalf("Serve did not return within %v of shutdown while a secret set was retrying", budget)
	}

	select {
	case <-put:
	case <-time.After(5 * time.Second):
		t.Error("the secret set's request never returned")
	}
}

// A secret set's retried rows show the connector's tools: the set waits
// for the sync its retry's new session requested, here slowed down at the
// upstream, rather than read the catalog before it is filled
// (wayfindr-t08.9).
func TestSecretSetReportsTheRetriedConnectorsTools(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	proxy := reverseProxy(t, fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			body, err := io.ReadAll(r.Body)

			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)

				return
			}

			if bytes.Contains(body, []byte(`"method":"tools/list"`)) {
				select {
				case <-time.After(300 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}

			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		slow.CloseClientConnections()
		slow.Close()
	})
	h := newHost(t)
	r := serve(t, h, h.options(t), map[string]any{
		"server": map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"linear": map[string]any{
			"url":     slow.URL,
			"headers": map[string]string{"Authorization": "Bearer ${secret:tok}"},
		}},
	})
	r.waitStatus(t, "linear waiting on secret:tok", stateIs("linear", upstream.StateAuthError))

	got := putSecret(t, r.client, "http://"+r.addr, "tok", "tok-value")

	if len(got.Retried) != 1 || got.Retried[0].State != upstream.StateReady ||
		got.Retried[0].Tools != 1 || got.Retried[0].LastSync == nil {
		t.Errorf("PUT /admin/secrets/tok retried = %+v, want linear ready with its 1 tool synced", got.Retried)
	}

	r.stop()
}

// TestSecretSetRebuildsTheEmbedderItsAPIKeyRefersTo: [embedding]'s
// api_key is resolved again when the secret it refers to is set, with no
// reload or restart (design §4, wayfindr-9ydc). The catalog synced while
// the key was missing is then embedded with it, and setting the secret
// again replaces the key for the next query's embedding.
func TestSecretSetRebuildsTheEmbedderItsAPIKeyRefersTo(t *testing.T) {
	fake := &embeddings{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	h := newHost(t)
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"files": stdioConnector(t, fakeupstream.V20250618)},
		"embedding": map[string]any{
			"base_url": srv.URL + "/v1", "model": "test-embed", "dimensions": 4,
			"api_key": "${secret:embed_key}",
		},
	})
	r.waitStatus(t, "files synced", synced("files"))
	base := "http://" + r.addr

	// Without the key there is no embedder: search is lexical only.
	if got := r.search(t, "echo"); !slices.Contains(got, "files.echo") {
		t.Fatalf("search_tools echo = %v, want files.echo found lexically", got)
	}

	if got := fake.authorizations(0); len(got) != 0 {
		t.Fatalf("provider saw %d requests before the key was set, want none", len(got))
	}

	if got := putSecret(t, r.client, base, "embed_key", "key-1"); len(got.Retried) != 0 {
		t.Errorf("PUT /admin/secrets/embed_key retried %+v, want no connector", got.Retried)
	}

	deadline := time.Now().Add(15 * time.Second)

	for !fake.embedded("Echo the arguments back.") {
		if time.Now().After(deadline) {
			t.Fatal("the embedding pass never embedded files.echo after the api_key secret was set")
		}

		time.Sleep(50 * time.Millisecond)
	}

	for _, got := range fake.authorizations(0) {
		if got != "Bearer key-1" {
			t.Errorf("provider saw Authorization %q, want the secret's value", got)
		}
	}

	putSecret(t, r.client, base, "embed_key", "key-2")
	before := len(fake.authorizations(0))
	r.search(t, "echo")

	if diff := cmp.Diff([]string{"Bearer key-2"}, fake.authorizations(before)); diff != "" {
		t.Errorf("query embedding Authorization after the secret was set again (-want +got):\n%s", diff)
	}
}

// putSecret sets secret name to value through the admin client of the
// daemon at base and returns the response.
func putSecret(t *testing.T, client *http.Client, base, name, value string) admin.SecretSet {
	t.Helper()
	s, err := admin.NewClient(base, "", client).PutSecret(t.Context(), name, value)

	if err != nil {
		t.Fatalf("PutSecret: %v", err)
	}

	return s
}

// recordingProxy forwards to target and records each request's
// Authorization header in seen.
func recordingProxy(t *testing.T, target string, seen *headerLog) *httptest.Server {
	t.Helper()
	u, err := url.Parse(target)

	if err != nil {
		t.Fatal(err)
	}

	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.FlushInterval = -1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.add(r.Header.Get("Authorization"))
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// headerLog is the Authorization headers an upstream saw, in order, and
// those of its DELETEs (a session's close) apart.
type headerLog struct {
	mu      sync.Mutex
	seen    []string
	deletes []string
}

func (l *headerLog) add(v string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, v)
}

// record adds r's Authorization header, to the DELETEs too if it is one.
func (l *headerLog) record(r *http.Request) {
	v := r.Header.Get("Authorization")
	l.add(v)

	if r.Method == http.MethodDelete {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.deletes = append(l.deletes, v)
	}
}

// deleted is the Authorization headers of the DELETEs, in order.
func (l *headerLog) deleted() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.deletes...)
}

func (l *headerLog) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.seen)
}

func (l *headerLog) since(i int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.seen[i:]...)
}

// syncBuffer is a bytes.Buffer safe for the daemon's concurrent log writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// TestSecretListNamesWhatTheRunningConfigRefersToEachSecretFrom: the list
// follows the running config, so a reload that drops a connector drops it
// from its secrets' users.
func TestSecretListNamesWhatTheRunningConfigRefersToEachSecretFrom(t *testing.T) {
	h := newHost(t)
	listen := freeAddr(t)
	// Nothing listens on port 1, so neither connector ever connects; only
	// their references matter here.
	linear := map[string]any{"url": "http://127.0.0.1:1/mcp", "headers": map[string]string{"Authorization": "Bearer ${secret:tok}"}}
	github := map[string]any{"url": "http://127.0.0.1:1/mcp", "headers": map[string]string{"Authorization": "Bearer ${secret:tok}"}}
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"linear": linear, "github": github},
	})
	c := admin.NewClient("http://"+r.addr, "", r.client)

	for _, name := range []string{"tok", "spare"} {
		if _, err := c.PutSecret(t.Context(), name, "v-"+name); err != nil {
			t.Fatalf("PutSecret(%s): %v", name, err)
		}
	}

	got, err := c.ListSecrets(t.Context())

	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}

	want := admin.SecretList{Secrets: []admin.Secret{
		{Name: "spare", UsedBy: []string{}},
		{Name: "tok", UsedBy: []string{"connectors.github", "connectors.linear"}},
	}}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ListSecrets (-want +got):\n%s", diff)
	}

	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"connectors": map[string]any{"linear": linear},
	})
	r.waitStatus(t, "github removed", func(s admin.Status) bool { return len(s.Connectors) == 1 })
	want.Secrets[1].UsedBy = []string{"connectors.linear"}

	if got, err = c.ListSecrets(t.Context()); err != nil {
		t.Fatalf("ListSecrets after reload: %v", err)
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ListSecrets after reload (-want +got):\n%s", diff)
	}
}
