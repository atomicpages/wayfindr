package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// dialed is an SDK transport for one connector, built from its config with
// every template resolved.
type dialed struct {
	transport mcp.Transport
	// fallback is the HTTP+SSE transport to retry with when transport is
	// Streamable HTTP and the upstream turns it away (see fallsBack). Nil
	// for stdio and for a connector already on SSE.
	fallback mcp.Transport
	// probe watches the Streamable transport's first POST. Nil when
	// fallback is.
	probe *probeTransport
	// halt ends every request of transport and fallback, in flight or yet
	// to come (see haltTransport); the session's close calls it. A no-op
	// for stdio. Idempotent.
	halt func()
	// mask replaces each value the connector's references resolved to (its
	// url, headers, args, and env) with the reference, in error and log
	// text: a value can sit where URLQueries doesn't look, such as a url's
	// path (wayfindr-wt2v).
	mask redact.Values
}

// fallsBack reports whether a failed Streamable connect should be retried
// over HTTP+SSE: its initialize POST got a 4xx other than 401 or 403, which
// is how a legacy SSE server answers (design §7.2). 401 and 403 are auth
// failures, which SSE would meet too.
func (d dialed) fallsBack() bool {
	if d.fallback == nil {
		return false
	}

	st := d.probe.initializeStatus()

	return st >= 400 && st < 500 && st != http.StatusUnauthorized && st != http.StatusForbidden
}

// dial builds the transport for a connector, resolving its templates through
// the manager's Resolver. It runs on every connect, so a changed environment
// variable or secret takes effect without a reload. A resolve failure is
// returned as the *config.ResolveError naming the reference. sse says a
// url connector has fallen back to HTTP+SSE.
func (m *Manager) dial(ctx context.Context, name string, c config.Connector, sse bool) (dialed, error) {
	if c.IsStdio() {
		return m.dialStdio(ctx, name, c)
	}

	return m.dialHTTP(ctx, c, sse)
}

func (m *Manager) dialStdio(ctx context.Context, name string, c config.Connector) (dialed, error) {
	var d dialed
	var used []config.Resolved
	args := make([]string, 0, len(c.Args))

	for i, a := range c.Args {
		v, refs, err := a.ResolveRefs(ctx, m.resolver)

		if err != nil {
			return d, fmt.Errorf("args[%d]: %w", i, err)
		}

		args = append(args, v)
		used = append(used, refs...)
	}

	env := inheritedEnv(os.Environ())

	for _, k := range slices.Sorted(maps.Keys(c.Env)) {
		v, refs, err := c.Env[k].ResolveRefs(ctx, m.resolver)

		if err != nil {
			return d, fmt.Errorf("env %s: %w", k, err)
		}

		env = append(env, k+"="+v)
		used = append(used, refs...)
	}

	d.mask = config.Mask(used)

	// The child must outlive the connect that started it, so its context is
	// never cancelled; closing the session stops it.
	//nolint:gosec // G204: running the configured command is the point.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), c.Command, args...)
	cmd.Env = env
	log := m.log.With().Str("connector", name).Logger()
	cmd.Stderr = stderrLog{log: log, mask: d.mask}
	// Wait (and so closing the session) waits for stderr to be drained. A
	// grandchild that keeps the pipe open must not hang Close forever.
	cmd.WaitDelay = stderrWaitDelay
	d.transport = &stdioTransport{cmd: cmd, log: log}
	d.halt = func() {}

	return d, nil
}

// stderrWaitDelay bounds how long closing a stdio upstream waits for its
// stderr after the child has exited.
const stderrWaitDelay = 2 * time.Second

// wayfindrEnvPrefix starts the names of Wayfindr's own environment
// variables, such as WAYFINDR_SECRET_KEY and WAYFINDR_CLIENT_TOKEN.
const wayfindrEnvPrefix = "WAYFINDR_"

// inheritedEnv is the part of the daemon's environment a stdio child gets:
// all of it (the child usually needs PATH, HOME, and the like) except
// Wayfindr's own variables, which hold the daemon's secrets (design §9).
// The connector's configured env goes on top, and may set any name.
func inheritedEnv(environ []string) []string {
	return slices.DeleteFunc(environ, func(kv string) bool {
		return len(kv) >= len(wayfindrEnvPrefix) && strings.EqualFold(kv[:len(wayfindrEnvPrefix)], wayfindrEnvPrefix)
	})
}

// stderrLog is a stdio child's stderr. It logs each line at debug level,
// tagged with the connector. exec copies the child's stderr to it from one
// goroutine, so it needs no lock.
type stderrLog struct {
	log  zerolog.Logger
	mask redact.Values
}

// Write logs p line by line. A line split across two writes is logged as
// two entries, which keeps stderrLog stateless.
func (w stderrLog) Write(p []byte) (int, error) {
	for line := range strings.Lines(string(p)) {
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			w.log.Debug().Str("stderr", w.mask.Apply(redact.URLQueries(line))).Msg("stdio stderr")
		}
	}

	return len(p), nil
}

// dialHTTP builds a url connector's transport. Its http.Client wraps the
// base transport with, in order, the header injector and the auth hook
// (design §7.2).
//
// sse says to use HTTP+SSE: the config forces it, or an earlier connect fell
// back to it. Otherwise the transport is Streamable HTTP, with an SSE
// fallback for connect to try if the Streamable connect fails after the
// first POST got a 4xx other than 401 or 403.
func (m *Manager) dialHTTP(ctx context.Context, c config.Connector, sse bool) (dialed, error) {
	var d dialed
	endpoint, used, err := c.URL.ResolveRefs(ctx, m.resolver)

	if err != nil {
		return d, fmt.Errorf("url: %w", err)
	}

	headers := make(http.Header, len(c.Headers))

	for _, k := range slices.Sorted(maps.Keys(c.Headers)) {
		v, refs, err := c.Headers[k].ResolveRefs(ctx, m.resolver)

		if err != nil {
			return d, fmt.Errorf("headers %s: %w", k, err)
		}

		headers.Set(k, v)
		used = append(used, refs...)
	}

	d.mask = config.Mask(used)

	base, err := m.baseTransport(ctx, c)

	if err != nil {
		return d, err
	}

	u, err := url.Parse(endpoint)

	if err != nil {
		return d, fmt.Errorf("url: %w", err)
	}

	// Innermost, under the layers that add credentials, so a redirect or
	// an SSE endpoint event naming another origin never reaches it with
	// them, nor with a client certificate.
	var rt http.RoundTripper = &originTransport{base: base, origin: originOf(u)}
	rt = &headerTransport{base: rt, headers: headers}

	if m.authTransport != nil {
		rt = m.authTransport(c, rt)
	}

	// Outermost, so a halt also ends the auth hook's own requests, such as
	// a token refresh.
	halted := make(chan struct{})
	d.halt = sync.OnceFunc(func() { close(halted) })
	rt = &haltTransport{base: rt, session: halted, manager: m.halted}

	if sse || c.Transport == config.TransportSSE {
		d.transport = &mcp.SSEClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: rt}}

		return d, nil
	}

	d.probe = &probeTransport{base: rt}
	d.transport = &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: d.probe}}
	d.fallback = &mcp.SSEClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: rt}}

	return d, nil
}

// errCrossOrigin is what originTransport fails a request with.
var errCrossOrigin = errors.New("refusing a request to another origin than the connector's")

// originTransport sends only requests to the connector's own origin: the
// scheme, host, and port of its url. Every request of a session goes
// through it, so a redirect elsewhere (an https one to http included) and
// an HTTP+SSE endpoint event naming another host fail before anything is
// dialed, and the headers, OAuth token, and client certificate the layers
// around it add never leave the connector's origin.
type originTransport struct {
	base   http.RoundTripper
	origin string
}

func (t *originTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if o := originOf(req.URL); o != t.origin {
		if req.Body != nil {
			_ = req.Body.Close()
		}

		// Only the refused origin: the connector's path may hold a secret.
		return nil, fmt.Errorf("%w: %s", errCrossOrigin, o)
	}

	return t.base.RoundTrip(req)
}

// originOf is u's scheme://host:port, lowercased, with the scheme's default
// port made explicit, so https://Example.com and https://example.com:443
// are one origin.
func originOf(u *url.URL) string {
	scheme, port := strings.ToLower(u.Scheme), u.Port()

	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}

	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// errTLSSetup marks a url connector whose HTTP client could not be built
// from its [tls] table for a reason other than an unresolvable key: a
// ca_file or client certificate that can't be used. judge reads it as
// auth_config; a key's *config.ResolveError reads as any other reference
// does.
var errTLSSetup = errors.New("tls")

// baseTransport is the transport under connector c's header injector and
// auth hook: its HTTP client's (Options.Clients), else
// http.DefaultTransport. A TLS handshake that fails later, such as on a
// certificate the MCP server doesn't trust, is a failure to reach it, as
// before.
func (m *Manager) baseTransport(ctx context.Context, c config.Connector) (http.RoundTripper, error) {
	if m.clients == nil {
		return http.DefaultTransport, nil
	}

	hc, err := m.clients.For(ctx, c)

	if err != nil {
		var rerr *config.ResolveError

		if errors.As(err, &rerr) {
			return nil, fmt.Errorf("tls key: %w", err)
		}

		return nil, fmt.Errorf("%w: %w", errTLSSetup, err)
	}

	return hc.Transport, nil
}

// haltTransport ends each request of one session once session or manager
// is closed: a request in flight is cancelled, its response body included,
// and one sent after fails at once. The session's close closes session
// when its ctx ends, and the manager's Close closes manager when its own
// does, so no request to an upstream, a session DELETE included, holds
// either longer.
type haltTransport struct {
	base             http.RoundTripper
	session, manager <-chan struct{}
}

func (t *haltTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	// The watch ends with the request: when the body is closed, or at once
	// if there is no response.
	go func() {
		select {
		case <-t.session:
			cancel()
		case <-t.manager:
			cancel()
		case <-ctx.Done():
		}
	}()
	resp, err := t.base.RoundTrip(req.WithContext(ctx))

	if err != nil {
		cancel()

		return nil, err
	}

	resp.Body = &haltBody{ReadCloser: resp.Body, cancel: cancel}

	return resp, nil
}

// haltBody is a response body whose Close ends its request's halt watch.
type haltBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *haltBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()

	return err
}

// headerTransport adds a connector's resolved headers to every request.
type headerTransport struct {
	base    http.RoundTripper
	headers http.Header
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.headers) == 0 {
		return t.base.RoundTrip(req)
	}

	req = req.Clone(req.Context())

	for k, v := range t.headers {
		req.Header[k] = v
	}

	return t.base.RoundTrip(req)
}

// probeTransport records how the upstream answered the initialize POST.
// Its status is how a legacy HTTP+SSE server shows it doesn't speak
// Streamable HTTP. A client at 2026-07-28 or later POSTs server/discover
// first, and every server older than that answers it with a 4xx, so only
// initialize counts. A failed initialize's body is kept too: it is the
// upstream's own account of the failure, which the SDK drops unless it is
// a JSON-RPC error.
type probeTransport struct {
	base http.RoundTripper

	mu     sync.Mutex
	probed bool
	status int    // 0 if initialize got no response
	body   string // a prefix of the body of a non-2xx initialize answer
}

func (t *probeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || t.done() || rpcMethod(req) != "initialize" {
		return t.base.RoundTrip(req)
	}

	resp, err := t.base.RoundTrip(req)
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.probed {
		return resp, err
	}

	t.probed = true

	if err != nil {
		return resp, err
	}

	t.status = resp.StatusCode

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		t.body, resp.Body = peekBody(resp.Body)
	}

	return resp, err
}

func (t *probeTransport) done() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.probed
}

// initializeStatus is the status of the initialize POST, or 0 if there was
// none or it got no response.
func (t *probeTransport) initializeStatus() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.status
}

// annotate adds the body of a failed initialize answer to err, the error
// of the connect it failed. err is returned unchanged when there is no
// body.
func (t *probeTransport) annotate(err error) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.body == "" {
		return err
	}

	return fmt.Errorf("%w (upstream answered %d: %q)", err, t.status, t.body)
}

// rpcMethod is the JSON-RPC method of a POST: its Mcp-Method header, which
// a client at 2026-07-28 or later sends, or else the "method" of its body.
// It is "" when neither names one, such as for a response or a batch. The
// body is read through GetBody, which the SDK's requests have, so req is
// left as it was.
func rpcMethod(req *http.Request) string {
	if m := req.Header.Get("Mcp-Method"); m != "" {
		return m
	}

	if req.GetBody == nil {
		return ""
	}

	body, err := req.GetBody()

	if err != nil {
		return ""
	}

	defer func() { _ = body.Close() }()

	var msg struct {
		Method string `json:"method"`
	}

	if json.NewDecoder(io.LimitReader(body, maxProbedRequest)).Decode(&msg) != nil {
		return ""
	}

	return msg.Method
}

// maxProbedRequest bounds how much of a POST rpcMethod reads. An
// initialize request is far smaller.
const maxProbedRequest = 1 << 20

// maxPeekedBody bounds the prefix of a failed answer's body that is kept
// for the connect error.
const maxPeekedBody = 512

// peekBody returns up to maxPeekedBody bytes of body as one line of text,
// and a body that still reads all of the original and closes it.
func peekBody(body io.ReadCloser) (string, io.ReadCloser) {
	buf := make([]byte, maxPeekedBody)
	n, _ := io.ReadFull(body, buf)
	buf = buf[:n]
	text := strings.Join(strings.Fields(string(buf)), " ")

	if n == maxPeekedBody {
		text += "…"
	}

	return text, struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), body), body}
}
