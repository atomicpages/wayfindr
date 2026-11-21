package store_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/store"
)

func TestNewToolParsesDefinition(t *testing.T) {
	hash := []byte{0xab, 0xcd}
	tests := []struct {
		name string
		def  string
		want store.Tool
	}{
		{
			name: "title, description, params sorted by name, required as listed",
			def: `{"name":"create_issue","title":"Create issue","description":"Creates an issue.",
				"inputSchema":{"type":"object","properties":{
					"title":{"type":"string","description":"Issue title"},
					"team":{"type":"string"},
					"assignee":{"type":"string"}},
					"required":["title","team"]}}`,
			want: store.Tool{
				Name: "create_issue", Title: "Create issue", Description: "Creates an issue.",
				Params:   []store.Param{{Name: "assignee"}, {Name: "team"}, {Name: "title", Description: "Issue title"}},
				Required: []string{"title", "team"},
			},
		},
		{
			name: "title falls back to annotations.title",
			def:  `{"name":"list","annotations":{"title":"List things"},"inputSchema":{"type":"object"}}`,
			want: store.Tool{Name: "list", Title: "List things"},
		},
		{
			name: "top-level title wins over annotations.title",
			def:  `{"name":"list","title":"Top","annotations":{"title":"Annotated"},"inputSchema":{"type":"object"}}`,
			want: store.Tool{Name: "list", Title: "Top"},
		},
		{
			// Draft-3 style: a boolean, not a list. The tool still syncs.
			name: "required that is not an array is ignored",
			def:  `{"name":"list","inputSchema":{"type":"object","required":true}}`,
			want: store.Tool{Name: "list"},
		},
		{
			name: "required entries that are not strings are skipped",
			def:  `{"name":"list","inputSchema":{"type":"object","required":["a",1,null,"b"]}}`,
			want: store.Tool{Name: "list", Required: []string{"a", "b"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.NewTool("linear", json.RawMessage(tt.def), hash)

			if err != nil {
				t.Fatalf("NewTool: %v", err)
			}

			tt.want.Connector = "linear"
			tt.want.Definition = json.RawMessage(tt.def)
			tt.want.ContentHash = hash

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("NewTool mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNewToolRejectsInvalidDefinitions(t *testing.T) {
	tests := []struct {
		name string
		def  string
	}{
		{"not JSON", `{"name":`},
		{"missing name", `{"description":"x"}`},
		{"not an object", `["create_issue"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := store.NewTool("linear", json.RawMessage(tt.def), nil); err == nil {
				t.Fatal("NewTool: err = nil, want error")
			}
		})
	}
}
