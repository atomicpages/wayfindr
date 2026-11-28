package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
)

// ToolList is the body of GET /admin/tools/{connector}: every tool the
// connector serves now, and whether its tool policy exposes it.
type ToolList struct {
	Connector string `json:"connector"`
	// Tools are sorted by name. Always an array.
	Tools []ToolEntry `json:"tools"`
}

// ToolEntry is one upstream tool.
type ToolEntry struct {
	// Name is the upstream's name, as a tools list matches it.
	Name string `json:"name"`
	// Class is "read", "write", or "destructive" (GLOSSARY: Tool class).
	Class string `json:"class"`
	// Exposed is whether the connector's tools list lets agents see and
	// call it.
	Exposed bool `json:"exposed"`
	// RequiredArgs is how many arguments the definition's inputSchema
	// requires.
	RequiredArgs int `json:"required_args"`
	// Definition is the tool as the upstream serves it, in MCP's wire form
	// (name, description, inputSchema, annotations, ...), re-encoded by the
	// SDK, so its keys may be in another order.
	Definition json.RawMessage `json:"definition,omitempty"`
}

// KindUnknownConnector is the ToolsError Kind for a connector that is not
// configured; it is upstream's Kind, which the daemon copies.
const KindUnknownConnector = "unknown_connector"

// ToolsError is a listing the upstream refused: its Kind and Case and its
// advice, which the daemon copies from upstream. KindUnknownConnector is
// answered 404, anything else (the connector isn't ready) 503, each with
// the advice's summary and action.
type ToolsError struct {
	Kind, Case string
	Advice     Advice
}

// Error is technical text for logs: the Kind and Case. What the error
// means to the user is its Advice.
func (e *ToolsError) Error() string { return "list tools: " + e.Kind + "/" + e.Case }

// tools serves GET /admin/tools/{connector}.
func (a api) tools(w http.ResponseWriter, r *http.Request) {
	if a.Tools == nil {
		http.Error(w, "tool listing is not available", http.StatusNotImplemented)

		return
	}

	list, err := a.Tools(r.Context(), r.PathValue("connector"))

	if te := (*ToolsError)(nil); errors.As(err, &te) {
		code := http.StatusServiceUnavailable

		if te.Kind == KindUnknownConnector {
			code = http.StatusNotFound
		}

		msg := te.Advice.Summary

		if te.Advice.Action != "" {
			msg += " " + te.Advice.Action
		}

		http.Error(w, msg, code)

		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Str("connector", r.PathValue("connector")).Msg("admin tools")
		http.Error(w, "listing tools failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	if list.Tools == nil {
		list.Tools = []ToolEntry{}
	}

	writeJSON(w, list)
}

// Tools calls GET /admin/tools/{connector}: the tools connector serves and
// whether its tools list exposes each.
func (c *Client) Tools(ctx context.Context, connector string) (ToolList, error) {
	var list ToolList
	err := c.do(ctx, http.MethodGet, pathTools+url.PathEscape(connector), nil, &list)

	return list, err
}
