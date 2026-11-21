package gateway

import (
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Names of the meta-tools (GLOSSARY: Meta-tools).
const (
	toolSearch = "search_tools"
	toolCall   = "call_tool"
)

// search_tools limits (prd.md, "Client-facing tools"; ADR 0001's
// amendment). Brief entries are small, so a brief page may be larger. The
// schema and the advice are built from these.
const (
	defaultSize      = 5
	maxSize          = 10
	defaultBriefSize = 15
	maxBriefSize     = 25
	maxNames         = 10
)

// search_tools' detail values.
const (
	detailFull  = "full"
	detailBrief = "brief"
)

func searchToolsTool() *mcp.Tool {
	return &mcp.Tool{
		Name:  toolSearch,
		Title: "Search tools",
		Description: "Find tools across every connected MCP server by natural language or keywords. " +
			"Returns full tool definitions (qualified name, description, inputSchema); call one with call_tool. " +
			"Use detail=brief to survey many tools cheaply, then names=[…] to get the full definitions of the ones you will call. " +
			"Connectors that cannot be used right now are listed under unavailable, with how to fix them; " +
			"connectors about to stop working are listed under warnings. Tell the user about either.",
		InputSchema: json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "What you want to do, in natural language or keywords. Required unless names is given."},
    "page": {"type": "integer", "minimum": 1, "default": 1, "description": "1-based page of results."},
    "size": {"type": "integer", "minimum": 1, "maximum": %[1]d, "description": "Results per page: %[2]s."},
    "connectors": {"type": "array", "items": {"type": "string"}, "description": "Only search these connectors. Omit to search all."},
    "detail": {"type": "string", "enum": ["full", "brief"], "default": "full", "description": "full returns whole definitions; brief returns name, title, summary, required arguments, and annotations."},
    "names": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": %[3]d, "description": "Qualified names (<connector>.<tool>) to return the full definitions of, in order. Use alone, without query."}
  }
}`, maxBriefSize, sizeLimits(), maxNames)),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(bool)},
	}
}

// sizeLimits describes size's range and defaults in both modes.
func sizeLimits() string {
	return fmt.Sprintf("1 to %d (default %d), or up to %d (default %d) with detail brief",
		maxSize, defaultSize, maxBriefSize, defaultBriefSize)
}

func callToolTool() *mcp.Tool {
	return &mcp.Tool{
		Name:  toolCall,
		Title: "Call tool",
		Description: "Call a tool found with search_tools. name is its qualified name (<connector>.<tool>); " +
			"arguments must match the tool's inputSchema. Returns the tool's result unchanged.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Qualified tool name from search_tools, e.g. linear.create_issue."},
    "arguments": {"type": "object", "description": "The tool's arguments, matching its inputSchema."}
  },
  "required": ["name"]
}`),
	}
}
