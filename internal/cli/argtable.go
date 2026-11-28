package cli

import (
	"encoding/json"
	"strings"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// minDescriptionWidth is the narrowest description column argTable wraps
// to; a terminal too narrow for it gets one line per argument instead.
const minDescriptionWidth = 30

// argTable is a tool's arguments as `connector tools <connector> <tool>`
// prints them: a header, then one row per argument with its type, whether
// it is required (a column only when one is), and its description. Upstream
// text is made printable, with its whitespace runs, newlines included,
// made one space. With width > 0, descriptions wrap to it under their
// column; with 0 each argument is one line, for grep.
func argTable(fields []admin.SchemaField, width int) string {
	required := false

	for _, f := range fields {
		required = required || f.Required
	}

	headers := []string{"NAME", "TYPE"}

	if required {
		headers = append(headers, "REQUIRED")
	}

	rows := make([][]string, 0, len(fields))

	for _, f := range fields {
		row := []string{redact.Printable(f.Name), typeText(f)}

		if required {
			req := ""

			if f.Required {
				req = "required"
			}

			row = append(row, req)
		}

		rows = append(rows, append(row, description(f)))
	}

	widths := make([]int, len(headers))

	for i, h := range headers {
		widths[i] = len(h)

		for _, r := range rows {
			widths[i] = max(widths[i], len([]rune(r[i])))
		}
	}

	indent := 2

	for _, w := range widths {
		indent += w + 2
	}

	wrapAt := width - indent

	if width <= 0 || wrapAt < minDescriptionWidth {
		wrapAt = 0
	}

	var b strings.Builder

	line := func(cells []string, desc string) {
		var l strings.Builder

		l.WriteString("  ")

		for i, c := range cells {
			l.WriteString(c + strings.Repeat(" ", widths[i]-len([]rune(c))+2))
		}

		lines := wrap(desc, wrapAt)
		l.WriteString(lines[0])

		// A row with no description would end in the padding before it.
		b.WriteString(strings.TrimRight(l.String(), " ") + "\n")

		for _, cont := range lines[1:] {
			b.WriteString(strings.Repeat(" ", indent) + cont + "\n")
		}
	}

	line(headers, "DESCRIPTION")

	for _, r := range rows {
		line(r[:len(r)-1], r[len(r)-1])
	}

	return b.String()
}

// typeText is an argument's types as the table shows them: joined by |,
// an array as its item type and [] when known, and any when the schema
// names no type.
func typeText(f admin.SchemaField) string {
	if len(f.Types) == 0 {
		return "any"
	}

	parts := make([]string, len(f.Types))

	for i, t := range f.Types {
		parts[i] = t

		if t == "array" && f.Items != "" {
			parts[i] = f.Items + "[]"
		}
	}

	return redact.Printable(strings.Join(parts, "|"))
}

// description is an argument's description on one line, printable, with
// its allowed values when it has some.
func description(f admin.SchemaField) string {
	d := strings.Join(strings.Fields(f.Description), " ")

	if len(f.Enum) > 0 {
		if d != "" && !strings.ContainsAny(d[len(d)-1:], ".!?") {
			d += "."
		}

		d = strings.TrimSpace(d + " One of " + enumList(f.Enum) + ".")
	}

	return redact.Printable(d)
}

// enumList words enum values as they are written: "a", "b", 3.
func enumList(enum []json.RawMessage) string {
	parts := make([]string, len(enum))

	for i, e := range enum {
		parts[i] = string(e)
	}

	return strings.Join(parts, ", ")
}

// wrap breaks s into lines of at most width runes at spaces; a word longer
// than width gets a line of its own. With width 0, s is one line.
func wrap(s string, width int) []string {
	words := strings.Fields(s)

	if width <= 0 || len(words) == 0 {
		return []string{s}
	}

	var lines []string

	cur := words[0]

	for _, w := range words[1:] {
		if len([]rune(cur))+1+len([]rune(w)) > width {
			lines = append(lines, cur)
			cur = w

			continue
		}

		cur += " " + w
	}

	return append(lines, cur)
}
