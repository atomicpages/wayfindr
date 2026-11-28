package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// MaxCallBody bounds the body of POST /admin/tools/call: the arguments of
// one call, as /mcp bounds a request (the SDK's
// DefaultMaxRequestBodyBytes).
const MaxCallBody = 4 << 20

// The callers POST /admin/tools/call accepts, by the client name the call
// log records for them. Admin is the owner's alone (httpd's adminOnly
// guard), so the owner names their own tool; any other name is refused.
const (
	CallClientCLI     = "wayfindr-cli"
	CallClientConsole = "console"
)

// CallRequest is the body of POST /admin/tools/call: one call_tool call,
// run by hand, through the gateway's call path.
type CallRequest struct {
	// Name is the qualified name, <connector>.<tool>.
	Name string `json:"name"`
	// Arguments are the tool's arguments, a JSON object; absent means {}.
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// RequestState and InputResponses answer an earlier input_required
	// result: its sealed requestState, and an answer per request id.
	RequestState   string                   `json:"request_state,omitempty"`
	InputResponses map[string]InputResponse `json:"input_responses,omitempty"`
	// Client is CallClientCLI or CallClientConsole.
	Client string `json:"client"`
	// Token, when set, is a client token's value: the call runs as that
	// token, narrowed to its connectors, and the call log names it. Without
	// it the call runs as the owner (the default token). Like
	// RemoteTestRequest.Token, it is held for this one request and never
	// stored or logged.
	Token string `json:"token,omitempty"`
}

// InputResponse is the answer to one elicitation of an input_required
// result.
type InputResponse struct {
	// Action is "accept", "decline", or "cancel".
	Action string `json:"action"`
	// Content is the form's answers; accept only.
	Content map[string]any `json:"content,omitempty"`
}

// CallOutcome is what Options.Call returns: the result and the tool's
// class.
type CallOutcome struct {
	// Result is the CallToolResult in MCP's wire form.
	Result json.RawMessage
	// Class is the tool's class as the catalog has it; empty when the
	// catalog has no such tool.
	Class string
	// Approval is the id of the pending approval the call was refused for
	// (ADR 0024); empty for any other outcome. Go-side only: the gateway
	// knows it, and the MCP result carries it only in its text.
	Approval string
}

// CallResponse is the answer to POST /admin/tools/call, whatever the call's
// outcome: a refused or failed call is an isError result in Result, as
// call_tool answers it.
type CallResponse struct {
	// Result is the CallToolResult in MCP's wire form (camelCase, as an
	// MCP client receives it).
	Result json.RawMessage `json:"result"`
	// Class is "read", "write", or "destructive"; absent when the catalog
	// has no such tool.
	Class string `json:"class,omitempty"`
	// Approval is the id of the pending approval the call was refused for
	// (ADR 0024, Case pending), for `wayfindr approvals approve <id>`;
	// absent for any other outcome, a denied or expired approval included.
	Approval string `json:"approval,omitempty"`
	// ElapsedMS is how long the call took, the gateway's checks included.
	ElapsedMS int64 `json:"elapsed_ms"`
}

// CallCase is why a call request was refused before it reached the
// gateway (GLOSSARY: Case). The set is closed.
type CallCase string

// CallUnknownToken means the request's token is no client token the
// daemon accepts: not the default one, never created, revoked, or
// expired.
const CallUnknownToken CallCase = "unknown_token"

// CallError is a call request the daemon refused before the call: its
// Case, which admin words (Advice), answered 400 with the advice. The daemon
// sets only the Case. It never holds the token.
type CallError struct{ Case CallCase }

// Error is technical text for logs: the Case. What the error means to the
// user is its Advice.
func (e *CallError) Error() string { return "call: " + string(e.Case) }

// Advice words e for the CLI and the console. The switch is exhaustive over
// CallCase, so a new Case can't fall back on another's words.
func (e *CallError) Advice() Advice {
	switch e.Case { //nolint:gocritic // singleCaseSwitch: exhaustive over CallCase, so each new Case gets its own words
	case CallUnknownToken:
		return Advice{
			Summary: "The daemon accepts no client token with that value: it may be mistyped, revoked, or expired.",
			Action:  "List the tokens with `wayfindr token list`.",
		}
	}

	return Advice{Summary: "The call was refused: " + string(e.Case) + "."}
}

// The actions of an elicitation answer (InputResponse.Action).
const (
	ActionAccept  = "accept"
	ActionDecline = "decline"
	ActionCancel  = "cancel"
)

// The modes of an elicitation (ElicitRequest.Mode). An empty mode is a
// form.
const (
	ElicitForm = "form"
	ElicitURL  = "url"
)

// ResultInputRequired is the resultType of a result that asks for input.
const ResultInputRequired = "input_required"

// validAction reports whether a is an elicitation answer's action.
func validAction(a string) bool {
	return a == ActionAccept || a == ActionDecline || a == ActionCancel
}

// callTool serves POST /admin/tools/call. Nothing in the body is logged:
// the token, the arguments, and the answers may all be secret.
func (a api) callTool(w http.ResponseWriter, r *http.Request) {
	if a.Call == nil {
		http.Error(w, "calling a tool is not available", http.StatusNotImplemented)

		return
	}

	var req CallRequest

	if err := readJSON(w, r, MaxCallBody, &req); err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			http.Error(w, "the call is too large: its body may be at most 4 MiB", http.StatusRequestEntityTooLarge)

			return
		}

		http.Error(w, `body must be {"name": "<connector>.<tool>", "client": "wayfindr-cli" or "console"} with optional `+
			`arguments, request_state, input_responses, and token`, http.StatusBadRequest)

		return
	}

	if req.Client != CallClientCLI && req.Client != CallClientConsole {
		http.Error(w, `client must be "wayfindr-cli" or "console"`, http.StatusBadRequest)

		return
	}

	for _, resp := range req.InputResponses {
		if !validAction(resp.Action) {
			http.Error(w, `each input response's action must be "accept", "decline", or "cancel"`, http.StatusBadRequest)

			return
		}
	}

	started := time.Now()
	out, err := a.Call(r.Context(), req)

	if ce := (*CallError)(nil); errors.As(err, &ce) {
		http.Error(w, ce.Advice().text(), http.StatusBadRequest)

		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Msg("admin call")
		http.Error(w, "the call failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	writeJSON(w, CallResponse{Result: out.Result, Class: out.Class, Approval: out.Approval, ElapsedMS: time.Since(started).Milliseconds()})
}

// Call calls POST /admin/tools/call: it runs one tool call through the
// gateway's call path and returns its result, whatever the call's outcome.
// An error is the request itself failing (a bad client or token, 501, the
// daemon down).
func (c *Client) Call(ctx context.Context, req CallRequest) (CallResponse, error) {
	var res CallResponse
	err := c.do(ctx, http.MethodPost, pathCall, req, &res)

	return res, err
}

// ToolResult is a CallToolResult's wire form as the CLI and the console
// read it, so neither imports the MCP SDK. Fields they don't show are
// dropped.
type ToolResult struct {
	Content           []Content               `json:"content"`
	StructuredContent json.RawMessage         `json:"structuredContent,omitempty"`
	IsError           bool                    `json:"isError,omitempty"`
	ResultType        string                  `json:"resultType,omitempty"`
	InputRequests     map[string]InputRequest `json:"inputRequests,omitempty"`
	RequestState      string                  `json:"requestState,omitempty"`
}

// InputRequired reports whether the result asks the caller for input, to
// be answered with RequestState and an InputResponse per request.
func (r ToolResult) InputRequired() bool {
	return r.ResultType == ResultInputRequired || len(r.InputRequests) > 0
}

// Content is one content item of a result. Type is "text", "image",
// "audio", "resource_link", or "resource".
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	// Data is an image's or audio's bytes (base64 on the wire).
	Data []byte `json:"data,omitempty"`
	// URI and Name are a resource link's.
	URI      string            `json:"uri,omitempty"`
	Name     string            `json:"name,omitempty"`
	Resource *EmbeddedResource `json:"resource,omitempty"`
}

// EmbeddedResource is a resource content item's resource.
type EmbeddedResource struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     []byte `json:"blob,omitempty"`
}

// InputRequest is one request of an input_required result. The gateway
// puts only elicitations to its callers (design §7.6), so Method is
// "elicitation/create".
type InputRequest struct {
	Method string        `json:"method"`
	Params ElicitRequest `json:"params"`
}

// ElicitRequest is an elicitation: a form (Mode "form", the default) or a
// URL to open (Mode "url").
type ElicitRequest struct {
	Mode    string `json:"mode,omitempty"`
	Message string `json:"message"`
	// RequestedSchema is a form's flat JSON Schema object.
	RequestedSchema json.RawMessage `json:"requestedSchema,omitempty"`
	URL             string          `json:"url,omitempty"`
	ElicitationID   string          `json:"elicitationId,omitempty"`
}

// Decode reads the response's result.
func (r CallResponse) Decode() (ToolResult, error) {
	var res ToolResult

	if err := json.Unmarshal(r.Result, &res); err != nil {
		return ToolResult{}, err
	}

	return res, nil
}
