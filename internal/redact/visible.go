package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Visible returns s with every character a reader could not see, or could
// not count, written as an escape they can. These become \uXXXX, or
// \U000XXXXX above U+FFFF, in upper-case hex:
//
//   - a rune in Cc (controls, which include ESC and so every terminal
//     escape sequence), Cf (format characters: the bidirectional controls
//     such as U+202E, the zero-width U+200B to U+200D, U+2060, the byte
//     order mark U+FEFF, the soft hyphen U+00AD, and the tag characters
//     U+E0000 to U+E007F), Zl (U+2028), or Zp (U+2029);
//   - a variation selector (U+FE00 to U+FE0F, U+E0100 to U+E01EF);
//   - an Other_Default_Ignorable_Code_Point, which renderers draw as
//     nothing: the combining grapheme joiner U+034F, the Hangul fillers
//     (U+115F, U+1160, U+3164, U+FFA0), U+17B4 and U+17B5, U+2065,
//     U+FFF0 to U+FFF8, and the unassigned rest of the tag area;
//   - a space other than U+0020 (Zs, such as U+00A0, U+2003, or U+3000)
//     and U+2800 BRAILLE PATTERN BLANK, which look like a space or nothing;
//   - each space after the second in a run of U+0020, so a run can't push
//     text out of view or onto a line of its own;
//   - each combining mark (Mn, Me) after the second in a row, so a stack of
//     them can't draw over the text around it.
//
// Each byte of invalid UTF-8 becomes \xXX. Everything else is kept.
//
// Use it where the owner decides on what they read, such as an approval's
// arguments, client, and tool (ADR 0024): Printable turns these characters
// into U+FFFD, or keeps them, and either way the owner can't tell what was
// there; Visible shows each one. Inside a JSON string a \uXXXX escape reads
// as the same value; \U000XXXXX and \xXX are not JSON escapes, and only
// show what was there. A backslash is kept as it is, so a literal "\u200B"
// and an escaped U+200B look alike; both are visible.
func Visible(s string) string {
	var b strings.Builder

	// spaces and marks count the U+0020 and the combining marks just
	// before the rune at hand.
	spaces, marks := 0, 0

	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)

		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\x%02X`, s[0])
		case hidden(r), r == ' ' && spaces >= 2, mark(r) && marks >= 2:
			if r > 0xFFFF {
				fmt.Fprintf(&b, `\U%08X`, r)
			} else {
				fmt.Fprintf(&b, `\u%04X`, r)
			}
		default:
			b.WriteString(s[:size])
		}

		spaces = next(spaces, r == ' ')
		marks = next(marks, mark(r))
		s = s[size:]
	}

	return b.String()
}

// next is a run's count after a rune: one more when the rune continues it,
// else zero.
func next(n int, in bool) int {
	if in {
		return n + 1
	}

	return 0
}

// mark reports whether r is a combining mark that draws on the rune
// before it.
func mark(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me)
}

// hidden reports whether r is a character Visible always escapes. Every
// Bidi_Control is in Cf; the check names it so that stays true.
func hidden(r rune) bool {
	if r == 0x2800 || r != ' ' && unicode.Is(unicode.Zs, r) {
		return true
	}

	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Bidi_Control,
		unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point)
}

// The bounds of VisibleArguments' layout. Indenting repeats the indent once
// per level on every line, so deeply nested arguments grow by a factor of
// their depth: about a thousand levels fit in a 4 MiB request, which would
// lay out to gigabytes. Arguments past either bound are shown on one line
// instead, still in full.
const (
	// maxLayoutDepth is the deepest nesting laid out.
	maxLayoutDepth = 32
	// layoutGrowth and layoutSlack bound the laid-out size: at most
	// layoutGrowth times the one-line size, plus layoutSlack, so small
	// arguments are always laid out.
	layoutGrowth = 4
	layoutSlack  = 4 << 10
)

// VisibleArguments is a tool call's arguments for the owner to read, in
// full and never cut, through Visible: on one line when indent is "", for
// a terminal, or else laid out with indent per level, for the console.
// Arguments that nest deeper than 32 levels, or that the layout would make
// more than 4 times larger (plus 4 KiB), are shown on one line either way,
// so no layout can grow without bound. No arguments and null both mean
// none, and read "{}". A value that is not JSON is shown as it is, through
// Visible, on one line.
//
// JSON is shown as it decodes, not as the agent wrote it (see canonical):
// an escape such as \u0072 shows as the r it stands for, so the owner reads
// the text the upstream will, and an invisible character, written raw or
// escaped, is shown by Visible's escape. Object keys are in sorted order.
//
// An indented value keeps the newlines and the leading indentation the
// layout adds: valid JSON has no raw newline inside a string, so every
// other line break, U+2028 and U+2029 among them, is the agent's and is
// escaped, as is a run of spaces in a string.
func VisibleArguments(raw json.RawMessage, indent string) string {
	// Indent keeps white space after the value, which Visible would show.
	raw = bytes.TrimSpace(raw)

	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}

	line, ok := canonical(raw)

	if !ok {
		return Visible(string(raw))
	}

	if indent == "" || !layoutFits(line, len(indent)) {
		return Visible(string(line))
	}

	var out bytes.Buffer

	if json.Indent(&out, line, "", indent) != nil {
		return Visible(string(line)) // valid JSON, so never
	}

	lines := strings.Split(out.String(), "\n")

	// The indentation is the layout's, not the agent's: Visible would show
	// a run of it as escapes.
	for i, l := range lines {
		content := strings.TrimLeft(l, indent)
		lines[i] = l[:len(l)-len(content)] + Visible(content)
	}

	return strings.Join(lines, "\n")
}

// canonical is raw, one JSON value, decoded and encoded again: on one line,
// with each string's escapes resolved (only what JSON requires is escaped
// again: quotes, backslashes, control characters, U+2028, and U+2029; HTML
// characters are not), numbers exactly as written, and object keys in
// sorted order, as Go encodes a map. That is the reading the approval's
// digest binds, which is independent of key order, so the sorted keys show
// the same call. False when raw is not one JSON value.
func canonical(raw []byte) ([]byte, bool) {
	var v any

	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()

	if d.Decode(&v) != nil {
		return nil, false
	}

	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, false // something after the value
	}

	var out bytes.Buffer

	e := json.NewEncoder(&out)
	e.SetEscapeHTML(false)

	if e.Encode(v) != nil {
		return nil, false // values json decoded always encode
	}

	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), true
}

// layoutFits reports whether json.Indent may lay out compact, JSON with no
// white space outside its strings, with an indent of width bytes per
// level: it nests at most maxLayoutDepth levels, and the layout's size is
// within layoutGrowth times compact's, plus layoutSlack. The size is
// reckoned before anything is laid out, from json.Indent's rules: a
// newline and the indentation of its level after each opening bracket and
// comma and before each closing bracket (empty brackets get none, which
// the bound ignores), and a space after each colon.
func layoutFits(compact []byte, width int) bool {
	depth, size, limit := 0, len(compact), layoutGrowth*len(compact)+layoutSlack
	in := false

	for i := 0; i < len(compact); i++ {
		c := compact[i]

		switch {
		case in && c == '\\':
			i++ // the escaped character, which may be a quote
		case in:
			in = c != '"'
		case c == '"':
			in = true
		case c == '{' || c == '[':
			depth++
			size += 1 + depth*width
		case c == '}' || c == ']':
			depth--
			size += 1 + depth*width
		case c == ',':
			size += 1 + depth*width
		case c == ':':
			size++
		}

		if depth > maxLayoutDepth || size > limit {
			return false
		}
	}

	return true
}
