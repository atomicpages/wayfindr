package webui

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestBuildFields is the form builder's decision table (design §7.10), a
// pure function of the schema: which input each property gets, in the
// schema's order. The pages that use it are covered in call_test.go.
func TestBuildFields(t *testing.T) {
	opt := func(v, l string) formOption { return formOption{Value: v, Label: l} }
	none := formOption{Value: "", Label: "—"}

	tests := []struct {
		name, schema string
		want         []formField
	}{
		{"no schema", ``, []formField{}},
		{"no properties", `{"type":"object"}`, []formField{}},
		{"flat types in order", `{"properties":{"title":{"type":"string","title":"Title","description":"The issue's title."},` +
			`"count":{"type":"integer"},"ratio":{"type":"number"}},"required":["title"]}`,
			[]formField{
				{ID: "a-0", Name: "arg.title", Label: "Title", Kind: "text", Type: "string", Required: true, Description: "The issue's title."},
				{ID: "a-1", Name: "arg.count", Label: "count", Kind: "number", Step: "1", Type: "integer"},
				{ID: "a-2", Name: "arg.ratio", Label: "ratio", Kind: "number", Step: "any", Type: "number"},
			}},
		{"enum and boolean are selects", `{"properties":{"p":{"type":"string","enum":["low","high"]},"b":{"type":"boolean"},` +
			`"o":{"oneOf":[{"const":1,"title":"one"}]}},"required":["p"]}`,
			[]formField{
				{ID: "a-0", Name: "arg.p", Label: "p", Kind: "select", Type: "string", Required: true,
					Options: []formOption{none, opt(`"low"`, "low"), opt(`"high"`, "high")}},
				{ID: "a-1", Name: "arg.b", Label: "b", Kind: "select", Type: "boolean",
					Options: []formOption{none, opt("true", "true"), opt("false", "false")}},
				{ID: "a-2", Name: "arg.o", Label: "o", Kind: "select", Type: "choice", Options: []formOption{none, opt("1", "1")}},
			}},
		// A required select still starts blank, so the browser preselects
		// nothing and a required flag is never sent as true untouched.
		{"required boolean starts blank", `{"properties":{"force":{"type":"boolean"}},"required":["force"]}`,
			[]formField{
				{ID: "a-0", Name: "arg.force", Label: "force", Kind: "select", Type: "boolean", Required: true,
					Options: []formOption{none, opt("true", "true"), opt("false", "false")}},
			}},
		{"nested and untyped take JSON", `{"properties":{"labels":{"type":"array"},"meta":{"type":"object"},"any":{},` +
			`"maybe":{"type":["null","string"]}}}`,
			[]formField{
				{ID: "a-0", Name: "arg.labels", Label: "labels", Kind: "json", Type: "array"},
				{ID: "a-1", Name: "arg.meta", Label: "meta", Kind: "json", Type: "object"},
				{ID: "a-2", Name: "arg.any", Label: "any", Kind: "json", Type: "JSON"},
				{ID: "a-3", Name: "arg.maybe", Label: "maybe", Kind: "text", Type: "string"},
			}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildFields(json.RawMessage(tt.schema), argPrefix, "a-")

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("fields (-want +got):\n%s", diff)
			}
		})
	}
}

// TestReadArguments is the other half of the table: each kind's posted
// value as JSON, empty optional fields left out, and a bad value refused.
func TestReadArguments(t *testing.T) {
	fields := buildFields(json.RawMessage(`{"properties":{"title":{"type":"string"},"count":{"type":"integer"},`+
		`"ratio":{"type":"number"},"p":{"enum":["low","high"]},"b":{"type":"boolean"},"labels":{"type":"array"},`+
		`"note":{"type":"string"}},"required":["title"]}`), argPrefix, "a-")

	tests := []struct {
		name    string
		form    url.Values
		want    string
		wantErr bool
	}{
		{"every kind", url.Values{"arg.title": {"x"}, "arg.count": {"3"}, "arg.ratio": {"0.5"}, "arg.p": {`"high"`},
			"arg.b": {"false"}, "arg.labels": {`["a"]`}},
			`{"b":false,"count":3,"labels":["a"],"p":"high","ratio":0.5,"title":"x"}`, false},
		{"empty required text is sent, empty optional left out", url.Values{}, `{"title":""}`, false},
		// The parsed number is sent, so what was typed needn't be JSON.
		{"leading zeros", url.Values{"arg.count": {"007"}, "arg.ratio": {"1e2"}}, `{"count":7,"ratio":100,"title":""}`, false},
		{"not a whole number", url.Values{"arg.count": {"3.5"}}, "", true},
		{"NaN", url.Values{"arg.ratio": {"NaN"}}, "", true},
		{"infinity", url.Values{"arg.ratio": {"+Inf"}}, "", true},
		{"not a choice", url.Values{"arg.p": {`"mid"`}}, "", true},
		{"not JSON", url.Values{"arg.labels": {"[a"}}, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readArguments(fields, tt.form)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want an error %v", err, tt.wantErr)
			}

			if !tt.wantErr && string(got) != tt.want {
				t.Errorf("arguments = %s, want %s", got, tt.want)
			}
		})
	}
}
