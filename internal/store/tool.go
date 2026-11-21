package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// toolDefinition is the subset of mcp.Tool that the catalog indexes.
type toolDefinition struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Annotations struct {
		Title string `json:"title"`
	} `json:"annotations"`
	InputSchema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
		// Required is decoded leniently by requiredNames: an upstream's odd
		// required (draft-3 true, mixed types) must not fail a sync.
		Required json.RawMessage `json:"required"`
	} `json:"inputSchema"`
}

// NewTool builds a catalog entry from an upstream tool definition. The name,
// title, description, parameters, and required parameter names are parsed
// from definition; the title falls back to annotations.title, as older
// protocol versions put it there. Stores call NewTool when reading rows
// back, so the parsed fields always agree with the stored definition.
func NewTool(connector string, definition json.RawMessage, contentHash []byte) (Tool, error) {
	var d toolDefinition

	if err := json.Unmarshal(definition, &d); err != nil {
		return Tool{}, fmt.Errorf("parse tool definition: %w", err)
	}

	if d.Name == "" {
		return Tool{}, errors.New("parse tool definition: missing name")
	}

	title := d.Title

	if title == "" {
		title = d.Annotations.Title
	}

	var params []Param

	for name, p := range d.InputSchema.Properties {
		params = append(params, Param{Name: name, Description: p.Description})
	}

	sort.Slice(params, func(i, j int) bool { return params[i].Name < params[j].Name })

	return Tool{
		Connector:   connector,
		Name:        d.Name,
		Definition:  definition,
		ContentHash: contentHash,
		Title:       title,
		Description: d.Description,
		Params:      params,
		Required:    requiredNames(d.InputSchema.Required),
	}, nil
}

// requiredNames returns the string entries of an inputSchema's required,
// in order. Anything that is not an array yields none, and entries that are
// not strings are skipped.
func requiredNames(raw json.RawMessage) []string {
	var entries []json.RawMessage

	if json.Unmarshal(raw, &entries) != nil {
		return nil
	}

	var names []string

	for _, e := range entries {
		var s string

		// A null would decode as "", so only a JSON string counts.
		if len(e) > 0 && e[0] == '"' && json.Unmarshal(e, &s) == nil {
			names = append(names, s)
		}
	}

	return names
}

// EncodeHidden renders hidden tools for a sync record's hidden_tools column.
// None renders as an empty array, so a successful sync with nothing hidden
// is told apart from no successful sync (NULL).
func EncodeHidden(hidden []HiddenTool) (string, error) {
	if hidden == nil {
		hidden = []HiddenTool{}
	}

	b, err := json.Marshal(hidden)

	if err != nil {
		return "", fmt.Errorf("encode hidden tools: %w", err)
	}

	return string(b), nil
}

// DecodeHidden parses a hidden_tools column. Empty (NULL) and an empty
// array both yield nil.
func DecodeHidden(s string) ([]HiddenTool, error) {
	if s == "" {
		return nil, nil
	}

	var hidden []HiddenTool

	if err := json.Unmarshal([]byte(s), &hidden); err != nil {
		return nil, fmt.Errorf("decode hidden tools: %w", err)
	}

	if len(hidden) == 0 {
		return nil, nil
	}

	return hidden, nil
}
