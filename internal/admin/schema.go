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
	// Type is the property's type: its string, or the first non-null entry
	// of a list of types; "" when the schema names none.
	Type     string
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
			Type        json.RawMessage   `json:"type"`
			Title       string            `json:"title"`
			Description string            `json:"description"`
			Enum        []json.RawMessage `json:"enum"`
			OneOf       []struct {
				Const json.RawMessage `json:"const"`
			} `json:"oneOf"`
		}

		if err := dec.Decode(&p); err != nil {
			return fields, fmt.Errorf("read the schema's property %s: %w", name, err)
		}

		f := SchemaField{Name: name, Title: p.Title, Type: firstType(p.Type), Required: slices.Contains(s.Required, name),
			Enum: p.Enum, Description: p.Description}

		for _, o := range p.OneOf {
			if len(o.Const) > 0 {
				f.Enum = append(f.Enum, o.Const)
			}
		}

		fields = append(fields, f)
	}

	return fields, nil
}

// firstType is a schema's type: the string, or the first non-null entry
// of a list of types.
func firstType(raw json.RawMessage) string {
	var one string

	if json.Unmarshal(raw, &one) == nil {
		return one
	}

	var many []string

	if json.Unmarshal(raw, &many) == nil {
		for _, t := range many {
			if t != "null" {
				return t
			}
		}
	}

	return ""
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
