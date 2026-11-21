package catalog_test

import (
	"encoding/json"
	"testing"

	"github.com/atomicpages/wayfindr/internal/catalog"
	"github.com/atomicpages/wayfindr/internal/store"
)

func TestEmbedTextFormat(t *testing.T) {
	tests := []struct {
		name string
		def  string
		want string
	}{
		{
			name: "params sorted by name, nested properties ignored",
			def: `{"name":"create_issue","title":"Create issue","description":"Creates an issue.",
				"inputSchema":{"type":"object","properties":{
					"title":{"type":"string","description":"Issue title"},
					"labels":{"type":"object","description":"Labels to add","properties":{"zeta":{"description":"nested"}}},
					"assignee":{"type":"string"}}}}`,
			want: "linear.create_issue\nCreate issue\nCreates an issue.\n" +
				"assignee: \n" +
				"labels: Labels to add\n" +
				"title: Issue title\n",
		},
		{
			name: "no title and no params",
			def:  `{"name":"ping","description":"Checks the connection.","inputSchema":{"type":"object"}}`,
			want: "linear.ping\n\nChecks the connection.\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, err := store.NewTool("linear", json.RawMessage(tt.def), nil)

			if err != nil {
				t.Fatalf("NewTool: %v", err)
			}

			if got := catalog.EmbedText(tool); got != tt.want {
				t.Errorf("EmbedText =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestEmbedTextIsStableAcrossPropertyOrder(t *testing.T) {
	defs := []string{
		`{"name":"search","inputSchema":{"type":"object","properties":{"b":{"description":"B"},"a":{"description":"A"},"c":{}}}}`,
		`{"inputSchema":{"properties":{"c":{},"a":{"description":"A"},"b":{"description":"B"}},"type":"object"},"name":"search"}`,
	}
	want := "gh.search\n\n\na: A\nb: B\nc: \n"

	for _, def := range defs {
		tool, err := store.NewTool("gh", json.RawMessage(def), nil)

		if err != nil {
			t.Fatalf("NewTool: %v", err)
		}

		if got := catalog.EmbedText(tool); got != want {
			t.Errorf("EmbedText(%s) = %q, want %q", def, got, want)
		}
	}
}
