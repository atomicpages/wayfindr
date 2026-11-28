package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// zeroValue is the JSON a scaffold gives a field: its first enum value, or
// its type's zero value.
func zeroValue(f admin.SchemaField) string {
	if len(f.Enum) > 0 {
		return string(f.Enum[0])
	}

	switch f.Type {
	case "string":
		return `""`
	case "number", "integer":
		return "0"
	case "boolean":
		return "false"
	case "array":
		return "[]"
	case "object":
		return "{}"
	}

	return "null"
}

// scaffold is a JSON object of the required fields with zeroValue each,
// in the schema's order: {"team": "", "title": ""}.
func scaffold(fields []admin.SchemaField) string {
	var parts []string

	for _, f := range fields {
		if f.Required {
			key, _ := json.Marshal(f.Name) // a string always marshals
			parts = append(parts, string(key)+": "+zeroValue(f))
		}
	}

	return "{" + strings.Join(parts, ", ") + "}"
}

// writeScaffold prints the scaffold of a tool definition's inputSchema to
// out, and nothing else, so it can be saved and fed back with --args-file.
// `wayfindr connector tools <connector> <tool>` describes the arguments.
func writeScaffold(out io.Writer, definition json.RawMessage) error {
	fields, err := inputFields(definition)

	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(out, scaffold(fields))

	return err
}

// inputFields are the fields of a tool definition's inputSchema.
func inputFields(definition json.RawMessage) ([]admin.SchemaField, error) {
	var d struct {
		InputSchema json.RawMessage `json:"inputSchema"`
	}

	if err := json.Unmarshal(definition, &d); err != nil {
		return nil, fmt.Errorf("read the tool's definition: %w", err)
	}

	return admin.SchemaFields(d.InputSchema)
}

// errNoAnswer means stdin ended before a question was answered.
var errNoAnswer = errors.New("stdin ended before every question was answered: run on a terminal, or use --json and answer yourself")

// answerAll asks each of an input_required result's requests on in and
// out, in id order, and returns the answers by id.
func answerAll(in *bufio.Reader, out io.Writer, reqs map[string]admin.InputRequest) (map[string]admin.InputResponse, error) {
	answers := make(map[string]admin.InputResponse, len(reqs))

	for _, id := range slices.Sorted(maps.Keys(reqs)) {
		p := reqs[id].Params
		var (
			a   admin.InputResponse
			err error
		)

		if p.Mode == admin.ElicitURL {
			a, err = answerURL(in, out, p)
		} else {
			a, err = answerForm(in, out, p)
		}

		if err != nil {
			return nil, err
		}

		answers[id] = a
	}

	return answers, nil
}

// answerURL prints a URL request and waits for Enter: the user finishes
// there, and the answer is accept.
func answerURL(in *bufio.Reader, out io.Writer, p admin.ElicitRequest) (admin.InputResponse, error) {
	if _, err := fmt.Fprintf(out, "%s\nOpen %s\nPress Enter once you have finished there. ",
		printableText(p.Message), redact.Printable(p.URL)); err != nil {
		return admin.InputResponse{}, err
	}

	if _, err := readLine(in); err != nil {
		return admin.InputResponse{}, err
	}

	return admin.InputResponse{Action: admin.ActionAccept}, nil
}

// answerForm asks a form request's fields, one line each. "n" to the
// first question declines.
func answerForm(in *bufio.Reader, out io.Writer, p admin.ElicitRequest) (admin.InputResponse, error) {
	fields, err := admin.SchemaFields(p.RequestedSchema)

	if err != nil {
		return admin.InputResponse{}, err
	}

	if _, err := fmt.Fprintf(out, "%s\nAnswer? [Y/n] ", printableText(p.Message)); err != nil {
		return admin.InputResponse{}, err
	}

	line, err := readLine(in)

	if err != nil {
		return admin.InputResponse{}, err
	}

	if l := strings.ToLower(line); l == "n" || l == "no" {
		return admin.InputResponse{Action: admin.ActionDecline}, nil
	}

	content := map[string]any{}

	for _, f := range fields {
		v, ok, err := askField(in, out, f)

		if err != nil {
			return admin.InputResponse{}, err
		}

		if ok {
			content[f.Name] = v
		}
	}

	return admin.InputResponse{Action: admin.ActionAccept, Content: content}, nil
}

// askField asks for one field until the answer parses. An empty answer
// skips an optional field (false) and asks a required one again.
func askField(in *bufio.Reader, out io.Writer, f admin.SchemaField) (any, bool, error) {
	for {
		if _, err := fmt.Fprint(out, fieldPrompt(f)); err != nil {
			return nil, false, err
		}

		line, err := readLine(in)

		if err != nil {
			return nil, false, err
		}

		if line == "" && !f.Required {
			return nil, false, nil
		}

		v, err := parseAnswer(f, line)

		if err == nil {
			return v, true, nil
		}

		if _, err := fmt.Fprintln(out, "  "+err.Error()); err != nil {
			return nil, false, err
		}
	}
}

// fieldPrompt is the question for f: "  title (string, required) The
// team's key. Choose from "a", "b": ".
func fieldPrompt(f admin.SchemaField) string {
	label := f.Title

	if label == "" {
		label = f.Name
	}

	typ := f.Type

	if f.Required {
		typ += ", required"
	}

	s := "  " + redact.Printable(label) + " (" + strings.TrimPrefix(typ, ", ") + ")"

	if f.Description != "" {
		s += " " + redact.Printable(f.Description)
	}

	if len(f.Enum) > 0 {
		s += " One of " + redact.Printable(enumList(f.Enum)) + "."
	}

	return s + ": "
}

// parseAnswer reads line as f's value: its type, or one of its enum values
// (written with or without quotes).
func parseAnswer(f admin.SchemaField, line string) (any, error) {
	if len(f.Enum) > 0 {
		for _, e := range f.Enum {
			var v any

			if json.Unmarshal(e, &v) == nil && (string(e) == line || fmt.Sprint(v) == line) {
				return v, nil
			}
		}

		return nil, fmt.Errorf("want one of %s", redact.Printable(enumList(f.Enum)))
	}

	switch f.Type {
	case "integer":
		n, err := strconv.ParseInt(line, 10, 64)

		if err != nil {
			return nil, errors.New("want a whole number")
		}

		return n, nil
	case "number":
		n, err := strconv.ParseFloat(line, 64)

		// JSON has no NaN or infinity.
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, errors.New("want a number")
		}

		return n, nil
	case "boolean":
		switch strings.ToLower(line) {
		case "y", "yes", "true":
			return true, nil
		case "n", "no", "false":
			return false, nil
		}

		return nil, errors.New("want yes or no")
	case "array":
		var items []string

		for _, s := range strings.Split(line, ",") {
			if s = strings.TrimSpace(s); s != "" {
				items = append(items, s)
			}
		}

		return items, nil
	}

	return line, nil
}

// readLine reads one line, without its line ending: errNoAnswer when stdin
// ended first, and the read's own error, wrapped, when it failed.
func readLine(in *bufio.Reader) (string, error) {
	line, err := in.ReadString('\n')

	switch {
	case err == nil, errors.Is(err, io.EOF) && line != "":
		return strings.TrimSpace(line), nil
	case errors.Is(err, io.EOF):
		return "", errNoAnswer
	}

	return "", fmt.Errorf("read an answer from stdin: %w", err)
}
