package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// maxSchemas bounds the schema cache. Definitions change rarely, so the
// cache is simply emptied when it fills, rather than tracking recency.
const maxSchemas = 1024

// maxProblems is how many schema failures an invalid_arguments result
// lists.
const maxProblems = 10

// schemaURL is the URL each input schema is compiled under. It is not
// loadable: a relative $ref resolves against it and fails to load, like any
// other remote reference.
const schemaURL = "https://wayfindr.invalid/input-schema.json"

// schemaCache holds compiled input schemas keyed by the SHA-256 of the tool's
// full definition. The catalog's content hash covers only the embedded text
// (names and descriptions), so a schema-only change would keep it; the
// definition bytes change whenever the schema does.
type schemaCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]schemaEntry
}

// schemaEntry is one definition's compiled schema, or why it did not
// compile. Both are nil when the definition has no inputSchema.
type schemaEntry struct {
	schema *inputSchema
	err    error
}

func newSchemaCache() *schemaCache {
	return &schemaCache{entries: map[[sha256.Size]byte]schemaEntry{}}
}

// get returns the compiled inputSchema of definition, compiling it on first
// use. fresh is true when this call compiled it, so a compile failure is
// logged once per definition rather than on every call.
func (c *schemaCache) get(definition json.RawMessage) (e schemaEntry, fresh bool) {
	key := sha256.Sum256(definition)
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()

	if ok {
		return e, false
	}

	// Compiled outside the lock: two calls racing on a new definition both
	// compile it, which is harmless.
	e = compileDefinition(definition)
	c.mu.Lock()

	if len(c.entries) >= maxSchemas {
		clear(c.entries)
	}

	c.entries[key] = e
	c.mu.Unlock()

	return e, true
}

// compileDefinition compiles a tool definition's inputSchema with remote
// $ref loading off (design §9) and format as an annotation only.
func compileDefinition(definition json.RawMessage) schemaEntry {
	var def struct {
		InputSchema json.RawMessage `json:"inputSchema"`
	}

	if err := json.Unmarshal(definition, &def); err != nil {
		return schemaEntry{err: fmt.Errorf("decode definition: %w", err)}
	}

	if isNull(def.InputSchema) {
		return schemaEntry{}
	}

	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(def.InputSchema))

	if err != nil {
		return schemaEntry{err: fmt.Errorf("decode inputSchema: %w", err)}
	}

	c := jsonschema.NewCompiler()
	// MCP schemas without $schema are JSON Schema 2020-12.
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(noLoader{})

	// NewCompiler leaves format and content assertions off.
	if err := c.AddResource(schemaURL, doc); err != nil {
		return schemaEntry{err: fmt.Errorf("add inputSchema: %w", err)}
	}

	s, err := c.Compile(schemaURL)

	if err != nil {
		return schemaEntry{err: fmt.Errorf("compile inputSchema: %w", err)}
	}

	return schemaEntry{schema: &inputSchema{s: s}}
}

// noLoader refuses every resource the compiler was not given, so an
// upstream schema can make Wayfindr fetch nothing, remote or local.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("loading %s: remote $ref is disabled", url)
}

// inputSchema is a compiled inputSchema.
type inputSchema struct{ s *jsonschema.Schema }

// check validates args, a JSON object, and returns the failures one per
// line ("at /title: got number, want string"), or "" if args are valid.
func (s *inputSchema) check(args json.RawMessage) (string, error) {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))

	if err != nil {
		return "", fmt.Errorf("decode arguments: %w", err)
	}

	err = s.s.Validate(inst)

	if err == nil {
		return "", nil
	}

	var ve *jsonschema.ValidationError

	if !errors.As(err, &ve) {
		return "", fmt.Errorf("validate arguments: %w", err)
	}

	var lines []string
	collect(ve, &lines)

	if len(lines) > maxProblems {
		lines = append(lines[:maxProblems], fmt.Sprintf("… and %d more", len(lines)-maxProblems))
	}

	return strings.Join(lines, "\n"), nil
}

// collect appends one line per leaf failure under ve: the instance location
// as a JSON pointer, and what is wrong there.
func collect(ve *jsonschema.ValidationError, lines *[]string) {
	if len(ve.Causes) == 0 {
		// A leaf's basic output is the leaf itself, with its message.
		out := ve.BasicOutput()
		at := out.InstanceLocation

		if at == "" {
			at = "/"
		}

		msg := ""

		if out.Error != nil {
			msg = out.Error.String()
		}

		*lines = append(*lines, "- at "+at+": "+msg)

		return
	}

	for _, c := range ve.Causes {
		collect(c, lines)
	}
}
