package daemon

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
)

const (
	// defaultRemoteTestTimeout bounds a whole connection test, both probes
	// together: a console waits on it.
	defaultRemoteTestTimeout = 10 * time.Second
	// maxRemoteTestReply caps the bytes read of any one answer. The test
	// never reads more, whatever a tunnel or a wrong server sends.
	maxRemoteTestReply = 1 << 20
	// probeProtocolVersion is the MCP version the test's initialize offers;
	// the tools/list that follows uses whatever the server answers with.
	probeProtocolVersion = "2025-11-25"
	// wayfindrNotFoundBody is the body of the 404 Wayfindr itself gives
	// (httpd's http.NotFound), which a tunnel provider's 404 never matches.
	wayfindrNotFoundBody = "404 page not found"
	// ngrokErrorHeader marks an answer as ngrok's own, such as its offline
	// page, rather than the daemon's.
	ngrokErrorHeader = "Ngrok-Error-Code"
	// sessionHeader carries an MCP session's id.
	sessionHeader = "Mcp-Session-Id"
	// closeSessionTimeout bounds the courtesy DELETE that ends a session the
	// test opened.
	closeSessionTimeout = 2 * time.Second
)

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + probeProtocolVersion +
	`","capabilities":{},"clientInfo":{"name":"wayfindr-connection-test","version":"v0"}}}`

const toolsListBody = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`

// remoteTester serves admin.Options.TestRemote (ADR 0021): it tests a cloud
// agent's path to the daemon the way the agent takes it, over the public
// URL, with a remote client token.
//
// Probe A is an initialize and a tools/list with the token as the bearer. It
// proves the token gets through the tunnel to the gateway. Probe B is the
// same initialize with no Authorization header. A request that really came
// from the internet gets the 404 every tokenless remote request gets; one the
// daemon takes for local gets 401 with Wayfindr's own challenge, exactly
// `Bearer` (a proxy's own bearer auth names a realm, and is not that). That
// 401 means the tunnel or proxy adds no forwarding header and keeps a
// loopback Host (wayfindr-gz1y), so the default client token would work from
// the internet: the test fails with a warning even when probe A passed.
//
// A 404 counts as Wayfindr's only when it is Wayfindr's own. A tunnel
// provider's 404 (ngrok's offline answer, for one) is "unreachable" for both
// probes, so a tunnel that is down is never read as a bad token, nor as a
// request the daemon saw as remote.
//
// It never logs, stores, or echoes the token, a URL's query, or an answer's
// body. It follows no redirect.
type remoteTester struct {
	client   *http.Client
	timeout  time.Duration
	registry *clienttoken.Registry
	log      zerolog.Logger
}

// newRemoteTester returns a tester that sends through a copy of base, which
// shares its transport but follows no redirect. A zero timeout means the
// default.
func newRemoteTester(base *http.Client, timeout time.Duration, registry *clienttoken.Registry, log zerolog.Logger) *remoteTester {
	c := *base
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	if timeout <= 0 {
		timeout = defaultRemoteTestTimeout
	}

	return &remoteTester{client: &c, timeout: timeout, registry: registry, log: log}
}

// test runs both probes against publicURL's /mcp.
func (t *remoteTester) test(ctx context.Context, publicURL, token string) admin.RemoteTestResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, t.timeout)

	defer cancel()

	res := t.run(ctx, publicURL, token)
	t.log.Info().Str("outcome", string(res.Outcome)).Int("status", res.Status).
		Int64("duration_ms", time.Since(start).Milliseconds()).Msg("remote connection tested")

	return res
}

// probe is the address under test.
type probe struct {
	endpoint string
	host     string
}

// reply is one answer: its status, headers, and at most maxRemoteTestReply
// bytes of its body.
type reply struct {
	status int
	header http.Header
	body   []byte
}

func (t *remoteTester) run(ctx context.Context, publicURL, token string) admin.RemoteTestResult {
	u, err := url.Parse(publicURL)

	// The config validates the URL, so this cannot happen; the guard is here
	// anyway, once, because this is what sends a secret to whatever it names.
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return invalidPublicURL()
	}

	p := probe{endpoint: u.Scheme + "://" + u.Host + "/mcp", host: u.Host}

	// Probe A: the token's path.
	res, answered := t.probeA(ctx, p, token)

	if !answered {
		return res
	}

	// Probe B runs whenever the far side answered probe A, whatever it
	// said, so a proxy that makes the internet look local is never hidden
	// behind a refused token or a 404: the warning outranks them all.
	bare, fail := t.send(ctx, p, "", "", "", initializeBody)

	if fail != nil {
		if res.Outcome == admin.RemoteOK {
			return *fail
		}

		return res
	}

	switch {
	case bare.status == http.StatusUnauthorized && isWayfindrChallenge(bare.header):
		return p.result(admin.RemoteProxyTreatsRemoteAsLocal, bare.status, admin.RemoteDetailNone)
	case res.Outcome != admin.RemoteOK, bare.status == http.StatusNotFound:
		return res
	default:
		return p.result(admin.RemoteUnexpected, bare.status, admin.RemoteDetailBareStatus)
	}
}

// isWayfindrChallenge reports whether h carries the challenge Wayfindr's own
// guard sends to a request it refuses (httpd: `WWW-Authenticate: Bearer`,
// with no parameters). A proxy with its own bearer auth sends a challenge
// with a realm or an error, which is not proof that the daemon saw the
// request.
func isWayfindrChallenge(h http.Header) bool {
	for _, v := range h.Values("WWW-Authenticate") {
		if strings.EqualFold(strings.TrimSpace(v), "Bearer") {
			return true
		}
	}

	return false
}

// invalidPublicURL is the result for an address that cannot be tested.
func invalidPublicURL() admin.RemoteTestResult {
	return admin.DescribeRemote(admin.RemoteNotConfigured, admin.RemoteFacts{Detail: admin.RemoteDetailInvalidURL})
}

// result words outcome for p's address; the words are admin's.
func (p probe) result(o admin.RemoteOutcome, status int, detail admin.RemoteDetail) admin.RemoteTestResult {
	return admin.DescribeRemote(o, admin.RemoteFacts{Host: p.host, URL: p.endpoint, Status: status, Detail: detail})
}

// probeA sends the token's requests: an initialize and a tools/list. It
// returns the outcome of the token's path alone, and whether the far side
// answered with an HTTP response that is neither a redirect nor a web page,
// which is when probe B is worth sending. A transport failure, a timeout, a
// redirect, and a web page are answers probe B could not improve on.
func (t *remoteTester) probeA(ctx context.Context, p probe, token string) (res admin.RemoteTestResult, answered bool) {
	initReply, fail := t.send(ctx, p, token, "", "", initializeBody)

	if fail != nil {
		return *fail, false
	}

	// Probe A opens an MCP session when the server keeps one; it is closed
	// when the probe ends, whatever the outcome, so the test leaves none
	// behind.
	if session := initReply.header.Get(sessionHeader); session != "" {
		defer t.closeSession(ctx, p, token, session)
	}

	if fail := p.tokenStatus(initReply); fail != nil {
		return *fail, true
	}

	var initResult struct {
		ProtocolVersion string `json:"protocolVersion"`
	}

	if !p.rpcResult(initReply, &initResult) {
		return p.result(admin.RemoteUnexpected, initReply.status, admin.RemoteDetailNotMCP), true
	}

	version := initResult.ProtocolVersion

	if version == "" {
		version = probeProtocolVersion
	}

	listReply, fail := t.send(ctx, p, token, version, initReply.header.Get(sessionHeader), toolsListBody)

	if fail != nil {
		return *fail, false
	}

	if fail := p.tokenStatus(listReply); fail != nil {
		return *fail, true
	}

	var list struct {
		Tools []json.RawMessage `json:"tools"`
	}

	if !p.rpcResult(listReply, &list) {
		return p.result(admin.RemoteUnexpected, listReply.status, admin.RemoteDetailNotMCP), true
	}

	facts := admin.RemoteFacts{URL: p.endpoint, Tools: len(list.Tools)}

	if c, ok := t.registry.Lookup(clienttoken.Digest(token)); ok {
		facts.Connectors = len(c.Connectors)
	}

	return admin.DescribeRemote(admin.RemoteOK, facts), true
}

// closeSession ends the MCP session the test's initialize opened, with a
// DELETE as the spec has a client do. It is a courtesy: the answer, and a
// failure to send, are ignored, since the test's result is already decided
// and a server that refuses a DELETE (Wayfindr's /mcp does) keeps no session.
func (t *remoteTester) closeSession(ctx context.Context, p probe, token, session string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeSessionTimeout)

	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, p.endpoint, http.NoBody)

	if err != nil {
		return
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "wayfindr-connection-test")
	req.Header.Set(sessionHeader, session)

	resp, err := t.client.Do(req)

	if err != nil {
		return
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRemoteTestReply))
	_ = resp.Body.Close()
}

// send POSTs body to the endpoint, with token as the bearer when it is not
// empty. A non-nil result means the probe cannot go on: the request failed,
// or the answer was a redirect or a web page, which no probe follows.
func (t *remoteTester) send(ctx context.Context, p probe, token, version, session, body string) (reply, *admin.RemoteTestResult) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, strings.NewReader(body))

	// A request that cannot even be built means the address is unusable, not
	// that something answered badly.
	if err != nil {
		res := invalidPublicURL()

		return reply{}, &res
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "wayfindr-connection-test")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}

	if session != "" {
		req.Header.Set(sessionHeader, session)
	}

	resp, err := t.client.Do(req)

	if err != nil {
		res := p.networkFailure(err)

		return reply{}, &res
	}

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteTestReply))

	if err != nil {
		res := p.networkFailure(err)

		return reply{}, &res
	}

	r := reply{status: resp.StatusCode, header: resp.Header, body: data}

	switch {
	case r.status >= 300 && r.status < 400:
		res := p.redirected(resp)

		return r, &res
	case isHTML(r.header):
		res := p.result(admin.RemoteInterstitial, r.status, admin.RemoteDetailNone)

		return r, &res
	case r.status == http.StatusNotFound && !isWayfindrNotFound(r):
		// A 404 that is not Wayfindr's own is the tunnel provider's, such as
		// ngrok's "endpoint is offline" (ERR_NGROK_3200). Taking it for
		// Wayfindr's 404 would blame the token for a tunnel that is down, and
		// would pass probe B for a path that never reached the daemon.
		res := p.result(admin.RemoteUnreachable, r.status, admin.RemoteDetailTunnelOffline)

		return r, &res
	}

	return r, nil
}

// isWayfindrNotFound reports whether r is the 404 Wayfindr itself gives
// (httpd: http.NotFound, a text/plain body of wayfindrNotFoundBody) and not
// a tunnel provider's: ngrok marks its own answers with the ngrokErrorHeader
// header, and any other body is not the daemon's.
func isWayfindrNotFound(r reply) bool {
	if r.header.Get(ngrokErrorHeader) != "" {
		return false
	}

	mt, _, err := mime.ParseMediaType(r.header.Get("Content-Type"))

	return err == nil && mt == "text/plain" && strings.TrimSpace(string(r.body)) == wayfindrNotFoundBody
}

// isHTML reports whether the answer is a web page: what a tunnel's login,
// warning, or offline page is, and never what an MCP endpoint sends.
func isHTML(h http.Header) bool {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))

	return err == nil && (mt == "text/html" || mt == "application/xhtml+xml")
}

// rpcResult decodes the JSON-RPC result in r into out, from a JSON body or
// the first data line of an event stream, and reports whether there was one.
func (p probe) rpcResult(r reply, out any) bool {
	body := r.body

	if mt, _, err := mime.ParseMediaType(r.header.Get("Content-Type")); err == nil && mt == "text/event-stream" {
		body = nil

		for line := range bytes.SplitSeq(r.body, []byte("\n")) {
			if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
				body = bytes.TrimSpace(data)

				break
			}
		}
	}

	var env struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}

	if json.Unmarshal(body, &env) != nil || len(env.Error) > 0 || len(env.Result) == 0 {
		return false
	}

	return json.Unmarshal(env.Result, out) == nil
}

// tokenStatus maps an answer to the token's request that is not a success.
// It is nil for 200.
func (p probe) tokenStatus(r reply) *admin.RemoteTestResult {
	var outcome admin.RemoteOutcome

	switch r.status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		outcome = admin.RemoteNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		outcome = admin.RemoteTokenRefused
	default:
		outcome = admin.RemoteUnexpected
	}

	res := p.result(outcome, r.status, admin.RemoteDetailNone)

	return &res
}

func (p probe) redirected(resp *http.Response) admin.RemoteTestResult {
	facts := admin.RemoteFacts{Host: p.host, URL: p.endpoint, Status: resp.StatusCode}

	// The host only: a Location may carry a query with a credential in it.
	if loc, err := resp.Location(); err == nil {
		facts.Redirect = loc.Host
	}

	return admin.DescribeRemote(admin.RemoteRedirect, facts)
}

// networkFailure maps a failed request to its outcome.
func (p probe) networkFailure(err error) admin.RemoteTestResult {
	ne := net.Error(nil)

	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return p.result(admin.RemoteTimeout, 0, admin.RemoteDetailNone)
	case isTLSError(err):
		return p.result(admin.RemoteTLSError, 0, admin.RemoteDetailNone)
	default:
		return p.result(admin.RemoteUnreachable, 0, admin.RemoteDetailNone)
	}
}

// isTLSError reports whether err is a failed TLS handshake: a certificate
// that does not verify, or a server that does not speak TLS.
func isTLSError(err error) bool {
	var (
		verify  *tls.CertificateVerificationError
		header  tls.RecordHeaderError
		unknown x509.UnknownAuthorityError
		host    x509.HostnameError
		invalid x509.CertificateInvalidError
		op      *net.OpError
	)

	// net/http turns the record header error of a plain-HTTP server into its
	// own unexported error, so its text is all there is to match.
	if strings.Contains(err.Error(), "server gave HTTP response to HTTPS client") {
		return true
	}

	switch {
	case errors.As(err, &verify), errors.As(err, &header), errors.As(err, &unknown), errors.As(err, &host), errors.As(err, &invalid):
		return true
	case errors.As(err, &op):
		// A TLS alert from the server arrives as "remote error: tls: …".
		return op.Op == "remote error"
	}

	return false
}
