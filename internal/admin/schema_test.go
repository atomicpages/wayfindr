package admin_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// TestSchemaFields: properties in the schema's order, required marked,
// enum and oneOf consts as the allowed values, and a list of types read as
// its first non-null one.
func TestSchemaFields(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{` +
		`"title":{"type":"string","title":"Title","description":"The title."},` +
		`"priority":{"enum":[1,2]},` +
		`"mode":{"oneOf":[{"const":"a","title":"A"},{"const":"b"}]},` +
		`"due":{"type":["null","string"]},` +
		`"meta":{}},"required":["mode","title"]}`)
	got, err := admin.SchemaFields(schema)

	if err != nil {
		t.Fatalf("SchemaFields: %v", err)
	}

	want := []admin.SchemaField{
		{Name: "title", Title: "Title", Type: "string", Required: true, Description: "The title."},
		{Name: "priority", Enum: []json.RawMessage{json.RawMessage("1"), json.RawMessage("2")}},
		{Name: "mode", Required: true, Enum: []json.RawMessage{json.RawMessage(`"a"`), json.RawMessage(`"b"`)}},
		{Name: "due", Type: "string"},
		{Name: "meta"},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("fields (-want +got):\n%s", diff)
	}

	for _, empty := range []string{``, `{}`, `{"properties":null}`} {
		if got, err := admin.SchemaFields(json.RawMessage(empty)); err != nil || len(got) != 0 {
			t.Errorf("SchemaFields(%q) = %v, %v; want no fields", empty, got, err)
		}
	}

	if _, err := admin.SchemaFields(json.RawMessage(`{"properties":[1]}`)); err == nil {
		t.Error("properties that aren't an object: no error")
	}
}

// TestContentSummary: each content item reads as one line, an embedded
// resource without its resource included.
func TestContentSummary(t *testing.T) {
	tests := []struct {
		c    admin.Content
		want string
	}{
		{admin.Content{Type: "text", Text: "hi"}, "hi"},
		{admin.Content{Type: "image", Data: []byte{1, 2}}, "image, 2 B"},
		{admin.Content{Type: "audio", Data: []byte{1}}, "audio, 1 B"},
		{admin.Content{Type: "resource_link", URI: "file:///a"}, "link: file:///a"},
		{admin.Content{Type: "resource", Resource: &admin.EmbeddedResource{URI: "file:///a", Text: "body"}}, "body"},
		{admin.Content{Type: "resource", Resource: &admin.EmbeddedResource{URI: "file:///a", Blob: []byte{1, 2, 3}}}, "resource file:///a, 3 B"},
		{admin.Content{Type: "resource"}, "resource"},
		{admin.Content{Type: "video"}, "video content"},
	}

	for _, tt := range tests {
		if got := tt.c.Summary(); got != tt.want {
			t.Errorf("Summary(%+v) = %q, want %q", tt.c, got, tt.want)
		}
	}
}
