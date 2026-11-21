package gateway_test

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// schemaTool is linear.create_issue with the given inputSchema.
func schemaTool(schema map[string]any) *mcp.Tool {
	tool := createIssue()
	tool.InputSchema = schema

	return tool
}

func TestSchemaChangeWithTheSameContentHashIsValidatedAgainstTheNewSchema(t *testing.T) {
	up := &stubUpstreams{res: &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}}
	cat := &stubCatalog{tool: linearTool(t, createIssue())}
	cs := newStubbedCatalog(t, up, cat)
	args := map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "x"}}

	if res := call(t, cs, "call_tool", args); res.IsError {
		t.Fatalf("call under the first schema failed: %s", text(t, res))
	}

	// Only the schema changes: title is now an integer. The content hash,
	// which covers names and descriptions, stays the same.
	cat.set(linearTool(t, schemaTool(map[string]any{
		"type":       "object",
		"properties": map[string]any{"title": map[string]any{"type": "integer", "description": "Issue title"}},
		"required":   []any{"title"},
	})))
	res := call(t, cs, "call_tool", args)

	if got := text(t, res); !res.IsError || !strings.Contains(got, "- at /title: got string, want integer") {
		t.Errorf("result (isError %v) %q, want the new schema's failure", res.IsError, got)
	}

	if n := up.callCount(); n != 1 {
		t.Errorf("upstream was called %d times, want 1", n)
	}
}

func TestSchemaThatDoesNotCompileIsForwardedWithoutValidation(t *testing.T) {
	tests := []struct {
		name   string
		schema map[string]any
	}{
		// Remote $ref loading is off, so the schema cannot compile.
		{"remote ref", map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"$ref": "https://example.com/title.json"}}}},
		{"relative ref", map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"$ref": "title.json"}}}},
		{"local file ref", map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"$ref": "file:///etc/passwd"}}}},
		{"invalid keyword value", map[string]any{"type": "object", "minProperties": "two"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := &stubUpstreams{res: &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}}
			cs := newStubbedCatalog(t, up, &stubCatalog{tool: linearTool(t, schemaTool(tt.schema))})

			for range 2 { // the second call hits the cached failure
				res := call(t, cs, "call_tool", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": 1}})

				if got := text(t, res); res.IsError || got != "ok" {
					t.Errorf("result (isError %v) %q, want the upstream's result", res.IsError, got)
				}
			}

			if n := up.callCount(); n != 2 {
				t.Errorf("upstream was called %d times, want 2", n)
			}
		})
	}
}

func TestFormatIsAnAnnotationOnly(t *testing.T) {
	up := &stubUpstreams{res: &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}}
	cs := newStubbedCatalog(t, up, &stubCatalog{tool: linearTool(t, schemaTool(map[string]any{
		"type":       "object",
		"properties": map[string]any{"title": map[string]any{"type": "string", "format": "email"}},
	}))})
	res := call(t, cs, "call_tool", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "not an email"}})

	if got := text(t, res); res.IsError || got != "ok" {
		t.Errorf("result (isError %v) %q, want the upstream's result", res.IsError, got)
	}
}
