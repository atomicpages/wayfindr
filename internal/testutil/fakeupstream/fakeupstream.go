// Package fakeupstream provides fake upstream MCP servers for tests. Each fake
// is an SDK server pinned to one protocol version and reachable over one
// transport:
//
//	cmd := fakeupstream.StdioCommand(t, fakeupstream.V20250618)     // stdio
//	srv := fakeupstream.StartStreamable(t, fakeupstream.V20260728)  // Streamable HTTP
//	srv := fakeupstream.StartSSE(t, fakeupstream.V20251125)         // legacy HTTP+SSE
//
// Options switch on behavior: the tool set, list_changed, elicitation,
// sampling, slow responses, tools/list page size and cache hint, a barrier
// for concurrent calls, and 4xx on Streamable.
//
// # Stdio
//
// The stdio fake re-executes the running test binary. A package that uses
// [StdioCommand] must call [ServeStdioIfChild] first thing in its TestMain:
//
//	func TestMain(m *testing.M) {
//		fakeupstream.ServeStdioIfChild()
//		os.Exit(m.Run())
//	}
package fakeupstream

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Protocol versions a fake can be pinned to.
const (
	V20250618 = "2025-06-18"
	V20251125 = "2025-11-25"
	V20260728 = "2026-07-28"
)

// Versions returns every protocol version a fake can be pinned to, oldest
// first.
func Versions() []string { return []string{V20250618, V20251125, V20260728} }

// Names of the tools that switches add.
const (
	// ToolEcho is the default tool. Every fake tool returns its arguments as
	// a single compact JSON text block.
	ToolEcho = "echo"
	// ToolChangeTools is added by [WithListChanged]. Calling it adds a tool
	// named "added_<n>" (n counts from 1), which makes the server send
	// notifications/tools/list_changed.
	ToolChangeTools = "change_tools"
	// ToolElicit is added by [WithElicitation].
	ToolElicit = "elicit"
	// ToolSample is added by [WithSampling].
	ToolSample = "sample"
	// ToolBarrier is added by [WithBarrier].
	ToolBarrier = "barrier"
	// ToolFail is added by [WithFailingTool].
	ToolFail = "fail"
)

// Option configures a fake. Options are plain data so that the stdio fake
// can carry them into its child process.
type Option func(*config)

// config is the full description of a fake. It crosses the process boundary
// as JSON for the stdio fake.
type config struct {
	Version          string        `json:"version"`
	Tools            []*mcp.Tool   `json:"tools,omitempty"`
	ListChanged      bool          `json:"listChanged,omitempty"`
	Elicitation      bool          `json:"elicitation,omitempty"`
	Sampling         bool          `json:"sampling,omitempty"`
	Delay            time.Duration `json:"delay,omitempty"`
	StreamableStatus int           `json:"streamableStatus,omitempty"`
	PageSize         int           `json:"pageSize,omitempty"`
	Barrier          int           `json:"barrier,omitempty"`
	ToolsTTL         time.Duration `json:"toolsTTL,omitempty"`
	HangDelete       bool          `json:"hangDelete,omitempty"`
	FailingTool      bool          `json:"failingTool,omitempty"`
}

// WithTools replaces the default tool set ([ToolEcho]) with tools. A tool
// without an InputSchema gets an empty object schema. Every tool returns its
// arguments as text. Tools added by other switches are kept.
func WithTools(tools ...*mcp.Tool) Option {
	return func(c *config) { c.Tools = append(make([]*mcp.Tool, 0, len(tools)), tools...) }
}

// WithListChanged adds [ToolChangeTools]. Calling it adds a tool, which makes
// the server notify clients with notifications/tools/list_changed (under
// 2026-07-28, on the client's subscriptions/listen stream).
func WithListChanged() Option { return func(c *config) { c.ListChanged = true } }

// WithElicitation adds [ToolElicit], which asks the client for input with a
// form elicitation and returns the client's answer (action and content) as
// JSON text.
//
// How it asks depends on the pinned version. Under 2026-07-28 the call
// returns an input_required result carrying the elicitation, and completes
// when the client retries with the answer. Earlier versions have no
// input_required, so the server sends elicitation/create to the client
// during the call (the SDK's multi round-trip fallback); a client without
// the elicitation capability makes the call fail.
func WithElicitation() Option { return func(c *config) { c.Elicitation = true } }

// WithSampling adds [ToolSample], which asks the client to sample a message
// and returns the sampled text. Like [WithElicitation], it returns
// input_required under 2026-07-28 and sends sampling/createMessage under
// earlier versions.
func WithSampling() Option { return func(c *config) { c.Sampling = true } }

// WithFailingTool adds [ToolFail], which answers every call with an isError
// result whose text is its "message" argument, or "tool failed" without one.
func WithFailingTool() Option { return func(c *config) { c.FailingTool = true } }

// WithDelay makes the server wait d before answering each tools/list and
// tools/call request. The wait ends early if the request is cancelled. Keep
// d to milliseconds in tests.
func WithDelay(d time.Duration) Option { return func(c *config) { c.Delay = d } }

// WithStreamableStatus makes a Streamable HTTP fake answer every HTTP request
// with status (for example 404 or 405, to drive SSE fallback, or 401). Other
// transports ignore it.
func WithStreamableStatus(status int) Option { return func(c *config) { c.StreamableStatus = status } }

// WithPageSize makes the server answer tools/list in pages of n tools, so a
// client must follow nextCursor to see every tool.
func WithPageSize(n int) Option { return func(c *config) { c.PageSize = n } }

// WithBarrier adds [ToolBarrier], which answers no call until n calls to it
// have arrived, then answers them all (and every later call) with their
// arguments as text. Calls made one at a time never finish, so a test can use
// it to prove that n calls run concurrently. A call that gives up still
// counts as arrived. Calls still held when the test ends fail.
func WithBarrier(n int) Option { return func(c *config) { c.Barrier = n } }

// WithToolsTTL makes the server put the cache hint ttlMs = d on every
// tools/list result (2026-07-28 cacheable lists; the SDK sends the field at
// every version). Without it the hint is 0, "immediately stale".
func WithToolsTTL(d time.Duration) Option { return func(c *config) { c.ToolsTTL = d } }

// WithHangingDelete makes a Streamable HTTP fake never answer a session
// DELETE: the request is held until the client gives up on it or the fake
// closes. Only stateful versions (before 2026-07-28) have sessions to
// delete; other transports ignore it.
func WithHangingDelete() Option { return func(c *config) { c.HangDelete = true } }

func newConfig(version string, opts []Option) (config, error) {
	if !slices.Contains(Versions(), version) {
		return config{}, fmt.Errorf("unsupported protocol version %q, want one of %v", version, Versions())
	}

	cfg := config{Version: version}

	for _, o := range opts {
		o(&cfg)
	}

	return cfg, nil
}

func defaultTools() []*mcp.Tool {
	return []*mcp.Tool{{Name: ToolEcho, Description: "Echo the arguments back.", InputSchema: textSchema()}}
}

func textSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"text": map[string]any{"type": "string"}},
	}
}

// newServer builds the SDK server a config describes. It is the single place
// all three transports share. ctx is the fake's lifetime: calls the barrier
// still holds when it ends fail, since the SDK waits for in-flight handlers
// before a session closes.
func newServer(ctx context.Context, cfg config) *mcp.Server {
	opts := &mcp.ServerOptions{SupportedProtocolVersions: []string{cfg.Version}, PageSize: cfg.PageSize}

	if cfg.ToolsTTL > 0 {
		ttl := int(cfg.ToolsTTL.Milliseconds())
		opts.SetCacheable = func(_ context.Context, req mcp.Request, c *mcp.Cacheable) {
			if _, ok := req.(*mcp.ListToolsRequest); ok {
				c.TTLMs = ttl
			}
		}
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "fakeupstream", Version: "v0"}, opts)
	tools := cfg.Tools

	if tools == nil {
		tools = defaultTools()
	}

	for _, tool := range tools {
		s.AddTool(withSchema(tool), echo)
	}

	if cfg.ListChanged {
		var added atomic.Int64
		s.AddTool(
			&mcp.Tool{Name: ToolChangeTools, Description: "Add a tool, triggering tools/list_changed.", InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				name := fmt.Sprintf("added_%d", added.Add(1))
				s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, echo)

				return textResult(name), nil
			},
		)
	}

	if cfg.Elicitation {
		s.AddTool(&mcp.Tool{Name: ToolElicit, Description: "Ask the client for input.", InputSchema: map[string]any{"type": "object"}}, elicit)
	}

	if cfg.Sampling {
		s.AddTool(&mcp.Tool{Name: ToolSample, Description: "Ask the client to sample a message.", InputSchema: map[string]any{"type": "object"}}, sample)
	}

	if cfg.FailingTool {
		s.AddTool(&mcp.Tool{Name: ToolFail, Description: "Fail every call.", InputSchema: map[string]any{"type": "object"}},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args struct {
					Message string `json:"message"`
				}

				_ = json.Unmarshal(req.Params.Arguments, &args)
				res := textResult(cmp.Or(args.Message, "tool failed"))
				res.IsError = true

				return res, nil
			})
	}

	if cfg.Barrier > 0 {
		s.AddTool(&mcp.Tool{Name: ToolBarrier, Description: "Answer once enough calls have arrived.", InputSchema: map[string]any{"type": "object"}}, barrier(ctx, cfg.Barrier))
	}

	if cfg.Delay > 0 {
		s.AddReceivingMiddleware(delay(cfg.Delay))
	}

	return s
}

func delay(d time.Duration) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" || method == "tools/call" {
				timer := time.NewTimer(d)
				defer timer.Stop()

				select {
				case <-timer.C:
				case <-ctx.Done():
					return nil, fmt.Errorf("%s: %w", method, ctx.Err())
				}
			}

			return next(ctx, method, req)
		}
	}
}

// barrier returns a tool handler that holds every call until n calls have
// arrived, the call is cancelled, or life ends.
func barrier(life context.Context, n int) mcp.ToolHandler {
	var (
		mu      sync.Mutex
		arrived int
		all     = make(chan struct{})
	)

	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mu.Lock()
		arrived++

		if arrived == n {
			close(all)
		}

		mu.Unlock()

		select {
		case <-all:
			return echo(ctx, req)
		case <-ctx.Done():
			return nil, fmt.Errorf("barrier: %w", ctx.Err())
		case <-life.Done():
			return nil, fmt.Errorf("barrier: fake stopped: %w", life.Err())
		}
	}
}

// inputKey is the ID of the single input request the elicit and sample
// tools make.
const inputKey = "input"

// elicit and sample use the SDK's multi round-trip API (SEP-2322): the first
// round returns InputRequests, and the retry carries InputResponses. The SDK
// puts this on the wire as input_required under 2026-07-28, and as a direct
// server-to-client request under earlier versions.
func elicit(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, ok := req.Params.InputResponses[inputKey]

	if !ok {
		return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{inputKey: &mcp.ElicitParams{
			Message: "Answer the question.",
			RequestedSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"answer": map[string]any{"type": "string"}},
			},
		}}}, nil
	}

	res, ok := resp.(*mcp.ElicitResult)

	if !ok {
		return nil, fmt.Errorf("elicit: input response is %T, want *mcp.ElicitResult", resp)
	}

	data, err := json.Marshal(struct {
		Action  string         `json:"action"`
		Content map[string]any `json:"content,omitempty"`
	}{res.Action, res.Content})

	if err != nil {
		return nil, fmt.Errorf("elicit: encode answer: %w", err)
	}

	return textResult(string(data)), nil
}

// sample uses SDK sampling types that are deprecated as of 2026-07-28
// (SEP-2577). Sampling stays in the fake because upstreams still use it.
//
//nolint:staticcheck // see above.
func sample(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, ok := req.Params.InputResponses[inputKey]

	if !ok {
		return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{inputKey: &mcp.CreateMessageParams{
			MaxTokens: 16,
			Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "Say something."}}},
		}}}, nil
	}

	var content []mcp.Content

	switch r := resp.(type) {
	case *mcp.CreateMessageWithToolsResult:
		content = r.Content
	case *mcp.CreateMessageResult:
		content = []mcp.Content{r.Content}
	default:
		return nil, fmt.Errorf("sample: input response is %T, want a sampling result", resp)
	}

	if len(content) != 1 {
		return nil, fmt.Errorf("sample: got %d content blocks, want 1", len(content))
	}

	text, ok := content[0].(*mcp.TextContent)

	if !ok {
		return nil, fmt.Errorf("sample: content is %T, want text", content[0])
	}

	return textResult(text.Text), nil
}

// withSchema returns tool with a default input schema if it has none, since
// the SDK requires one.
func withSchema(tool *mcp.Tool) *mcp.Tool {
	if tool.InputSchema != nil {
		return tool
	}

	cp := *tool
	cp.InputSchema = map[string]any{"type": "object"}

	return &cp
}

func echo(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.Params.Arguments

	if len(args) == 0 {
		args = json.RawMessage("{}")
	}

	return textResult(string(args)), nil
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}
