package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// SchemaField is one property of a JSON Schema object, as `wayfindr call`
// and the console's tool form read it: a tool's inputSchema, or a form
// elicitation's requestedSchema.
type SchemaField struct {
	Name  string
	Title string
	// Type is the property's one non-null type: from type (a string, or a
	// list with one non-null entry), or the one distinct non-null type of
	// its anyOf or oneOf members; "" when it has none or several.
	Type string
	// Types are all the property's types in schema order, null included:
	// type's, else those of its anyOf or oneOf members. Empty when the
	// schema names none.
	Types []string
	// Items is an array's item type, by Type's rule; "" when unknown.
	Items    string
	Required bool
	// Enum are the allowed values as JSON, from enum or from oneOf's consts.
	Enum        []json.RawMessage
	Description string
}

// SchemaFields reads an object schema's properties in the order the schema
// lists them, which a map would lose. A schema with no properties has no
// fields. On an error it returns the fields read before it.
func SchemaFields(schema json.RawMessage) ([]SchemaField, error) {
	if len(bytes.TrimSpace(schema)) == 0 {
		return nil, nil
	}

	var s struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}

	if err := json.Unmarshal(schema, &s); err != nil {
		return nil, fmt.Errorf("read the schema: %w", err)
	}

	if len(s.Properties) == 0 || string(s.Properties) == "null" {
		return nil, nil
	}

	dec := json.NewDecoder(bytes.NewReader(s.Properties))

	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("read the schema: properties is not an object")
	}

	var fields []SchemaField

	for dec.More() {
		t, err := dec.Token()

		if err != nil {
			return fields, fmt.Errorf("read the schema: %w", err)
		}

		name, _ := t.(string) // an object's keys are strings

		var p struct {
			typed
			Title       string            `json:"title"`
			Description string            `json:"description"`
			Enum        []json.RawMessage `json:"enum"`
			Items       json.RawMessage   `json:"items"`
		}

		if err := dec.Decode(&p); err != nil {
			return fields, fmt.Errorf("read the schema's property %s: %w", name, err)
		}

		f := SchemaField{Name: name, Title: p.Title, Types: p.types(), Type: p.one(), Required: slices.Contains(s.Required, name),
			Enum: slices.Concat(p.Enum, p.closed()), Description: p.Description}

		// items may be a schema, a list of them (a tuple), or a boolean;
		// only a schema names one item type.
		var items typed

		if json.Unmarshal(p.Items, &items) == nil {
			f.Items = items.one()
		}

		fields = append(fields, f)
	}

	return fields, nil
}

// typed is the part of a schema that says its type: type itself, and the
// members of anyOf and oneOf, whose consts and enums are allowed values.
type typed struct {
	Type  json.RawMessage `json:"type"`
	AnyOf []member        `json:"anyOf"`
	OneOf []member        `json:"oneOf"`
}

// member is one entry of anyOf or oneOf.
type member struct {
	Type  json.RawMessage   `json:"type"`
	Const json.RawMessage   `json:"const"`
	Enum  []json.RawMessage `json:"enum"`
}

// types are t's types in schema order, null included: type's, else the
// distinct types of its anyOf and oneOf members. A member that names no
// type and no value, such as one with an anyOf of its own, adds "any",
// so the list never claims to be complete when it isn't.
func (t typed) types() []string {
	if ts := typeList(t.Type); len(ts) > 0 {
		return ts
	}

	var ts []string

	add := func(x string) {
		if !slices.Contains(ts, x) {
			ts = append(ts, x)
		}
	}

	for _, m := range slices.Concat(t.AnyOf, t.OneOf) {
		mts := typeList(m.Type)

		for _, x := range mts {
			add(x)
		}

		if len(mts) == 0 && len(m.Const) == 0 && len(m.Enum) == 0 {
			add("any")
		}
	}

	return ts
}

// one is t's one type, for a form field or a scaffold value: type's first
// non-null entry, as a list of types has always meant here; else the one
// distinct non-null type of its anyOf and oneOf members; "" for none or
// several.
func (t typed) one() string {
	if ts := typeList(t.Type); len(ts) > 0 {
		for _, x := range ts {
			if x != "null" {
				return x
			}
		}

		return ""
	}

	return single(t.types())
}

// closed is the allowed values of t's anyOf and oneOf members, from their
// consts and enums, but only when every member that isn't null names its
// values: one open member, such as {"type": "string"}, allows any value.
func (t typed) closed() []json.RawMessage {
	var values []json.RawMessage

	for _, m := range slices.Concat(t.AnyOf, t.OneOf) {
		switch {
		case len(m.Const) > 0:
			values = append(values, m.Const)
		case len(m.Enum) > 0:
			values = append(values, m.Enum...)
		case slices.Equal(typeList(m.Type), []string{"null"}):
		default:
			return nil
		}
	}

	return values
}

// typeList is a type keyword's value: a string, or a list of strings.
func typeList(raw json.RawMessage) []string {
	var one string

	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}

	var many []string

	if json.Unmarshal(raw, &many) == nil {
		return many
	}

	return nil
}

// single is the one distinct non-null type in ts; "" for none, several,
// or an unknown one.
func single(ts []string) string {
	one := ""

	for _, t := range ts {
		switch {
		case t == "any":
			return ""
		case t == "null", t == one:
		case one == "":
			one = t
		default:
			return ""
		}
	}

	return one
}

// Summary is how a content item reads in one line of a result: a text's
// text, "image, 48 KiB", "link: <uri>", an embedded resource's text or its
// URI and size, sizes worded by FormatBytes.
func (c Content) Summary() string {
	switch c.Type {
	case "text":
		return c.Text
	case "image", "audio":
		return c.Type + ", " + FormatBytes(int64(len(c.Data)))
	case "resource_link":
		return "link: " + c.URI
	case "resource":
		if c.Resource == nil {
			return "resource"
		}

		if c.Resource.Text != "" {
			return c.Resource.Text
		}

		return "resource " + c.Resource.URI + ", " + FormatBytes(int64(len(c.Resource.Blob)))
	}

	return c.Type + " content"
}
