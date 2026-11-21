// Package shim is `wayfindr stdio`: a stateless forwarder for clients that
// only speak stdio (design §7.9, ADR 0003). It serves MCP on the stdin and
// stdout the CLI gives it and forwards tools/list and tools/call to the
// daemon's /mcp, starting the daemon first when it isn't running.
//
// The shim is an SDK server toward the client and an SDK client toward the
// daemon, so each hop negotiates its own protocol version and the shim never
// parses raw JSON-RPC.
package shim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/daemonctl"
	"github.com/atomicpages/wayfindr/internal/paths"
	"github.com/atomicpages/wayfindr/internal/toolsonly"
)

// Options is what the shim reads from the host.
type Options struct {
	// Env is the process environment and home directory: where the config
	// and data directory are, and the client token variable. A daemon the
	// shim starts inherits the real process environment.
	Env paths.Env
	// Dir is the directory the client started the shim in: its project,
	// where the project scope is looked up (ADR 0010).
	Dir    string
	Logger zerolog.Logger
	// Version is sent to the client and the daemon.
	Version string
	// Profile names the config's profile every request is scoped by (ADR
	// 0011); "" for none. The daemon resolves it against its running
	// config, so the shim checks only that it is a valid name.
	Profile string
}

// Run serves MCP on stdin and stdout until the client closes stdin and
// every request it sent has been answered, or until ctx ends. Before it
// serves, it makes sure the daemon is running (starting it if need be) and
// connects to it, so a missing `wayfindr init` or a daemon that will not
// start fails here, on stderr, rather than on the first call. Run never
// closes stdin or stdout.
func Run(ctx context.Context, stdin io.Reader, stdout io.Writer, opts Options) error {
	// A malformed name would get every request refused; failing here says
	// so once, on stderr.
	if opts.Profile != "" && !config.IsProfileName(opts.Profile) {
		return fmt.Errorf("profile %q: %w", opts.Profile, config.ErrProfileName)
	}

	p, err := paths.Resolve(opts.Env)

	if err != nil {
		return err
	}

	token, err := clienttoken.Read(ctx, opts.Env, p)

	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("client token: %w: run `wayfindr init` first", err)
	}

	if err != nil {
		return fmt.Errorf("client token: %w", err)
	}

	scope, err := projectScope(opts)

	if err != nil {
		return err
	}

	if opts.Profile != "" {
		opts.Logger.Info().Str("profile", opts.Profile).Msg("profile applies")
	}

	daemon, err := daemonctl.NewTarget(p, opts.Logger)

	if err != nil {
		return err
	}

	f := &forwarder{
		daemon:  daemon,
		token:   token,
		scope:   scope,
		profile: opts.Profile,
		version: opts.Version,
	}
	defer f.close()

	if _, err := f.session(ctx); err != nil {
		return err
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "wayfindr", Version: opts.Version}, &mcp.ServerOptions{
		// Tools are forwarded, never registered, so the capability is
		// declared rather than inferred. The daemon sends no list_changed.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	// The shim declares only tools, as the gateway does, so it refuses the
	// other capabilities with the same -32601 /mcp gives (design §7.9).
	server.AddReceivingMiddleware(toolsonly.Middleware, f.intercept)

	if err := server.Run(ctx, newStdio(stdin, stdout).transport()); err != nil && ctx.Err() == nil {
		return fmt.Errorf("serve stdio: %w", err)
	}

	return nil
}

// forwarder holds the one client session to the daemon. A session that
// fails in transport is dropped, and the next request reconnects, starting
// the daemon again if it is gone.
type forwarder struct {
	daemon daemonctl.Target
	token  string
	// scope is the project scope as config.ScopeHeader carries it; "" for
	// none.
	scope string
	// profile names the config's profile, as config.ProfileHeader carries
	// it; "" for none.
	profile string
	version string

	mu sync.Mutex
	cs *mcp.ClientSession
}

// intercept answers tools/list and tools/call from the daemon. Everything
// else (initialize, ping, …) is the shim's own server's to answer.
func (f *forwarder) intercept(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		switch r := req.(type) {
		case *mcp.ListToolsRequest:
			return f.listTools(ctx, r.Params)
		case *mcp.CallToolRequest:
			return f.callTool(ctx, r)
		}

		return next(ctx, method, req)
	}
}

// listTools forwards the listing. tools/list has no isError result, so
// failing to reach the daemon is a JSON-RPC error carrying the same advice
// as callTool's.
func (f *forwarder) listTools(ctx context.Context, p *mcp.ListToolsParams) (mcp.Result, error) {
	cs, err := f.session(ctx)

	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, unreachableAdvice)
	}

	var params mcp.ListToolsParams

	if p != nil {
		params.Cursor = p.Cursor
	}

	res, err := cs.ListTools(ctx, &params)

	if err != nil {
		err = f.failed(ctx, cs, err)

		if errors.Is(err, errUnreachable) {
			return nil, fmt.Errorf("%w\n%s", err, unreachableAdvice)
		}

		return nil, err
	}

	return res, nil
}

// callTool forwards the call and returns the daemon's result unchanged,
// isError results included. A JSON-RPC error from the daemon is passed on
// as one; failing to reach the daemon becomes an isError result, as every
// gateway-side failure is (design §7.6).
//
// The calling client's clientInfo goes along in _meta, so the daemon's call
// log names it rather than the shim's own client, wayfindr-stdio.
//
// When the daemon asks the user something (input_required), a client at
// 2026-07-28 gets the question as is and answers with another call, whose
// inputResponses and requestState go along. An older client can't take
// input_required, but over stdio the shim can ask it directly with
// elicitation/create: the shim does, and calls the daemon again with the
// answers until the daemon stops asking. That is why the daemon session
// runs with the SDK's multi round-trip handling off.
func (f *forwarder) callTool(ctx context.Context, req *mcp.CallToolRequest) (mcp.Result, error) {
	cs, err := f.session(ctx)

	if err != nil {
		return errorResult(err), nil
	}

	p := req.Params
	params := &mcp.CallToolParams{Name: p.Name, RequestState: p.RequestState, InputResponses: p.InputResponses}

	if info := req.ClientInfo(); info != nil {
		params.Meta = mcp.Meta{mcp.MetaKeyClientInfo: info}
	}

	if len(p.Arguments) > 0 {
		params.Arguments = p.Arguments
	}

	for {
		res, err := cs.CallTool(ctx, params)

		if err != nil {
			err = f.failed(ctx, cs, err)

			if errors.Is(err, errUnreachable) {
				return errorResult(err), nil
			}

			return nil, err
		}

		if !res.NeedsInput() {
			return res, nil
		}

		var (
			answers mcp.InputResponseMap
			askErr  error
		)

		if req.ProtocolVersion() >= protocolInputRequired {
			if askErr = canAnswer(req.ClientCapabilities(), res.InputRequests); askErr == nil {
				return res, nil
			}

			answers = cancelAll(res.InputRequests)
		} else {
			answers, askErr = askDirectly(ctx, req.Session, res.InputRequests)
		}

		params.RequestState, params.InputResponses = res.RequestState, answers

		if askErr != nil {
			// Tell the daemon no one will answer, so the upstream isn't
			// left waiting; what it then returns is not what the user
			// asked for.
			_, _ = cs.CallTool(ctx, params)

			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "This tool needs your input, but your client could not be asked (" +
					askErr.Error() + ").\nTo fix: " + cannotAskAdvice}},
			}, nil
		}
	}
}

// cannotAskAdvice follows the error a client gets when it can't be asked
// a tool's question.
const cannotAskAdvice = "Use a client that supports elicitation."

// protocolInputRequired is the first protocol version whose clients take
// input_required results.
const protocolInputRequired = "2026-07-28"

// askDirectly puts each of reqs to the stdio client with elicitation/create
// and returns its answers. On a failure it returns cancel for every request
// with the error: a request that is not an elicitation (the daemon passes
// on no other), or a client that can't elicit.
func askDirectly(ctx context.Context, ss *mcp.ServerSession, reqs mcp.InputRequestMap) (mcp.InputResponseMap, error) {
	answers := make(mcp.InputResponseMap, len(reqs))
	var askErr error

	for id, r := range reqs {
		p, ok := r.(*mcp.ElicitParams)

		if !ok {
			askErr = fmt.Errorf("unsupported input request %T", r)

			break
		}

		answer, err := ss.Elicit(ctx, p)

		if err != nil {
			askErr = err

			break
		}

		answers[id] = answer
	}

	if askErr != nil {
		return cancelAll(reqs), askErr
	}

	return answers, nil
}

// cancelAll answers every one of reqs with cancel: no one will answer them.
func cancelAll(reqs mcp.InputRequestMap) mcp.InputResponseMap {
	answers := make(mcp.InputResponseMap, len(reqs))

	for id := range reqs {
		answers[id] = &mcp.ElicitResult{Action: "cancel"}
	}

	return answers
}

// canAnswer reports, as an error, why a client with caps can't answer
// every one of reqs, which a client at 2026-07-28 gets as input_required.
// As in the spec, a client that declares elicitation with neither mode
// supports form.
func canAnswer(caps *mcp.ClientCapabilities, reqs mcp.InputRequestMap) error {
	if caps == nil || caps.Elicitation == nil {
		return errors.New("client does not support elicitation")
	}

	e := caps.Elicitation

	for _, r := range reqs {
		p, ok := r.(*mcp.ElicitParams)

		switch {
		case !ok:
			return fmt.Errorf("unsupported input request %T", r)
		case p.Mode == "url" && e.URL == nil:
			return errors.New(`client does not support "url" elicitation`)
		case p.Mode != "url" && e.Form == nil && e.URL != nil:
			return errors.New(`client does not support "form" elicitation`)
		}
	}

	return nil
}

// errUnreachable marks a failure to reach the daemon, as opposed to an
// error the daemon answered with.
var errUnreachable = errors.New("wayfindr daemon unreachable")

// unreachableAdvice follows errUnreachable to the client.
const unreachableAdvice = "To fix: run `wayfindr status` to check the daemon. The next call starts it again if it is not running."

// codeRejected is the code of the SDK's jsonrpc2.ErrRejected, which is
// internal to it: the Streamable client transport wraps a request that
// never reached the server (connection refused, say) in it.
//
// The SDK does not export this code, so nothing breaks if it changes: on
// every go-sdk upgrade, re-check that ErrRejected (internal/jsonrpc2/wire.go
// in v1.8.0) is still -32005 and that the client transport still wraps
// unsent requests in it, or a daemon that is down would be passed on as its
// own JSON-RPC error and the session would never be dropped.
const codeRejected = -32005

// failed sorts an error from the daemon session. A JSON-RPC error the
// daemon answered with is returned as is. Anything else means the daemon
// could not be reached: the session is dropped, so the next request
// reconnects and, if the daemon is gone, starts it again. The error is the
// caller's to hand to the client; failed does not log it.
func (f *forwarder) failed(ctx context.Context, cs *mcp.ClientSession, err error) error {
	if ctx.Err() != nil {
		// The client cancelled the request; the session is fine.
		return err
	}

	// The daemon's own error comes first in the chain even when the
	// transport also marks the request rejected (a non-2xx response).
	if wire, ok := errors.AsType[*jsonrpc.Error](err); ok && wire.Code != codeRejected {
		return wire
	}

	f.mu.Lock()

	if f.cs == cs {
		f.cs = nil
	}

	f.mu.Unlock()
	_ = cs.Close()

	return fmt.Errorf("%w at %s: %w", errUnreachable, f.daemon.URL(), err)
}

func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error() + "\n" + unreachableAdvice}},
	}
}

// session returns the session to the daemon, connecting first (and starting
// the daemon if it is not running) when there is none.
func (f *forwarder) session(ctx context.Context) (*mcp.ClientSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.cs != nil {
		return f.cs, nil
	}

	if _, err := daemonctl.Ensure(ctx, f.daemon, daemonctl.StartOptions{}); err != nil {
		return nil, fmt.Errorf("%w: %w", errUnreachable, err)
	}

	// The client declares elicitation so the daemon puts questions to the
	// shim, and handles none itself: callTool passes each on to the stdio
	// client (see callTool).
	client := mcp.NewClient(&mcp.Implementation{Name: "wayfindr-stdio", Version: f.version}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{
			Form: &mcp.FormElicitationCapabilities{},
			URL:  &mcp.URLElicitationCapabilities{},
		}},
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	})
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: f.daemon.URL() + "/mcp",
		// Every connection is checked, not just this session's first: after
		// the daemon restarts, the client dials again (wayfindr-zvjw).
		HTTPClient: &http.Client{Transport: headers{token: f.token, scope: f.scope, profile: f.profile, next: f.daemon.Transport()}},
	}, nil)

	if err != nil {
		return nil, fmt.Errorf("%w: connect to %s/mcp: %w", errUnreachable, f.daemon.URL(), err)
	}

	f.cs = cs

	return cs, nil
}

func (f *forwarder) close() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.cs != nil {
		_ = f.cs.Close()
		f.cs = nil
	}
}

// headers presents the client token, and the project scope and profile if
// any, on every request to the daemon. They go on each request because the
// gateway is stateless.
type headers struct {
	token, scope, profile string
	next                  http.RoundTripper
}

func (h headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.token)

	if h.scope != "" {
		r.Header.Set(config.ScopeHeader, h.scope)
	}

	if h.profile != "" {
		r.Header.Set(config.ProfileHeader, h.profile)
	}

	return h.next.RoundTrip(r)
}

// projectScope is the project scope for opts.Dir, encoded for
// config.ScopeHeader, or "" when no scope file applies. A scope file that
// doesn't validate stops the shim: serving without it would give the
// project more than it asked for.
func projectScope(opts Options) (string, error) {
	if opts.Dir == "" {
		return "", nil
	}

	// Without a home directory the walk goes to the root.
	var home string

	if opts.Env.HomeDir != nil {
		home, _ = opts.Env.HomeDir()
	}

	scope, file, err := config.LoadScope(opts.Dir, home)

	if err != nil {
		return "", err
	}

	if scope == nil {
		return "", nil
	}

	opts.Logger.Info().Str("file", file).Msg("project scope applies")

	return scope.Header()
}
