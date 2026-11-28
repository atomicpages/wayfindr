package redact_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/atomicpages/wayfindr/internal/redact"
)

// TestVisible spells the invisible characters as escapes in the inputs,
// since a literal one can't be seen in the source either.
func TestVisible(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"plain text is untouched", "github.search_issues", "github.search_issues"},
		{"non-ascii text is untouched", "Café 日本語 é 🙂", "Café 日本語 é 🙂"},
		{"replacement character is kept", "a\uFFFDb", "a\uFFFDb"},
		{"a backslash is kept", `a\u200Bb`, `a\u200Bb`},
		{"nul", "a\x00b", `a\u0000b`},
		{"escape sequence and bell", "\x1b[31mred\x07", `\u001B[31mred\u0007`},
		{"tab and newlines", "a\tb\nc\rd", `a\u0009b\u000Ac\u000Dd`},
		{"delete and c1", "a\x7fb\u009bc", `a\u007Fb\u009Bc`},
		{"right-to-left override", "gpj.\u202Eexe", `gpj.\u202Eexe`},
		{"bidi isolates and marks", "a\u2066b\u2069c\u200Fd\u061Ce", `a\u2066b\u2069c\u200Fd\u061Ce`},
		{"zero-width space, non-joiner, joiner", "a\u200Bb\u200Cc\u200Dd", `a\u200Bb\u200Cc\u200Dd`},
		{"word joiner", "a\u2060b", `a\u2060b`},
		{"byte order mark", "\uFEFFkey", `\uFEFFkey`},
		{"soft hyphen", "de\u00ADlete", `de\u00ADlete`},
		{"line and paragraph separators", "a\u2028b\u2029c", `a\u2028b\u2029c`},
		// "rm" in tag characters, which most renderers draw as nothing.
		{"tag characters", "ok\U000E0072\U000E006D\U000E007F", `ok\U000E0072\U000E006D\U000E007F`},
		{"variation selectors", "a\uFE00b\uFE0Fc", `a\uFE00b\uFE0Fc`},
		{"variation selectors supplement", "a\U000E0100b\U000E01EFc", `a\U000E0100b\U000E01EFc`},
		{"hangul fillers", "a\u115Fb\u1160c\u3164d\uFFA0e", `a\u115Fb\u1160c\u3164d\uFFA0e`},
		// Other_Default_Ignorable_Code_Point.
		{"combining grapheme joiner", "to\u034F", `to\u034F`},
		{"khmer inherent vowels", "a\u17B4b\u17B5c", `a\u17B4b\u17B5c`},
		{"unassigned ignorables", "a\u2065b\uFFF0c\uFFF8d", `a\u2065b\uFFF0c\uFFF8d`},
		{"the rest of the tag area", "a\U000E0002b\U000E0080c\U000E00FFd\U000E01F0e\U000E0FFFf", `a\U000E0002b\U000E0080c\U000E00FFd\U000E01F0e\U000E0FFFf`},
		{"braille pattern blank", "a\u2800b", `a\u2800b`},
		{"spaces other than U+0020", "a\u00A0b\u1680c\u2000d\u2003e\u200Af\u202Fg\u205Fh\u3000i", `a\u00A0b\u1680c\u2000d\u2003e\u200Af\u202Fg\u205Fh\u3000i`},
		{"one and two spaces are kept", "a b  c", "a b  c"},
		{"spaces after the second in a run", "a   b     c", `a  \u0020b  \u0020\u0020\u0020c`},
		{"runs at either end", "   a   ", `  \u0020a  \u0020`},
		{"an escape ends a run", "a  \u00A0  b", `a  \u00A0  b`},
		{"one and two combining marks are kept", "e\u0301 e\u0301\u0302", "e\u0301 e\u0301\u0302"},
		{"combining marks after the second in a row", "e\u0301\u0302\u0303\u0304x", "e\u0301\u0302" + `\u0303\u0304` + "x"},
		{"enclosing marks count", "a\u20DD\u0301\u20DD", "a\u20DD\u0301" + `\u20DD`},
		{"a base character ends a stack", "a\u0301\u0302b\u0301\u0302", "a\u0301\u0302b\u0301\u0302"},
		{"neighbours of the ranges are kept", "\uFDFF\uFE10\U000E1000\u115E\u1161\u3163\u3165\uFF9F\uFFA1\u034E\u17B3\u27FF\u2801\uFFEF",
			"\uFDFF\uFE10\U000E1000\u115E\u1161\u3163\u3165\uFF9F\uFFA1\u034E\u17B3\u27FF\u2801\uFFEF"},
		{"invalid utf-8 bytes, each shown", "a\xff\xfeb", `a\xFF\xFEb`},
		{"truncated rune", "a\xe6\x97", `a\xE6\x97`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redact.Visible(tt.in); got != tt.want {
				t.Errorf("Visible(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestVisibleArguments(t *testing.T) {
	long := strings.Repeat("0123456789", 1000)
	tests := []struct {
		name, in, line, indented string
	}{
		{"none", "", "{}", "{}"},
		{"null", "null", "{}", "{}"},
		{"null with white space", " null\n", "{}", "{}"},
		{"empty object", "{}", "{}", "{}"},
		{"laid out", `{ "a": 1,  "b": [true, "x"] }`, `{"a":1,"b":[true,"x"]}`,
			"{\n  \"a\": 1,\n  \"b\": [\n    true,\n    \"x\"\n  ]\n}"},
		{"trailing white space is dropped", "{\"a\":1}\n\t", `{"a":1}`, "{\n  \"a\": 1\n}"},
		{"never cut", `{"t":"` + long + `"}`, `{"t":"` + long + `"}`, "{\n  \"t\": \"" + long + "\"\n}"},
		{"escapes of invisible characters read the same", `{"t":"a\u200Bb\n"}`, `{"t":"a\u200Bb\n"}`, "{\n  \"t\": \"a\\u200Bb\\n\"\n}"},
		{"raw invisible characters in a string", "{\"t\":\"a\u200Db\uFEFF\u00AD\u2028\u202E\U000E0041\"}",
			`{"t":"a\u200Db\uFEFF\u00AD\u2028\u202E\U000E0041"}`,
			"{\n  \"t\": \"a\\u200Db\\uFEFF\\u00AD\\u2028\\u202E\\U000E0041\"\n}"},
		{"raw invisible characters in a key", "{\"to\u200B\":1}", `{"to\u200B":1}`, "{\n  \"to\\u200B\": 1\n}"},
		// Two keys that read alike, one with a combining grapheme joiner:
		// which one the upstream takes must not be a guess. Keys are shown
		// sorted.
		{"look-alike keys", "{\"to\u034F\":\"owner@me\",\"to\":\"attacker@x\"}", `{"to":"attacker@x","to\u034F":"owner@me"}`,
			"{\n  \"to\": \"attacker@x\",\n  \"to\\u034F\": \"owner@me\"\n}"},
		// The arguments as they decode, which the approval binds: escapes
		// of visible characters read as those characters, escapes of
		// invisible ones as Visible's escapes, and HTML characters as they
		// are.
		{"escaped visible characters", `{"cmd":"\u0072\u006d -rf /","html":"\u003cb\u003e \u0026"}`, `{"cmd":"rm -rf /","html":"<b> &"}`,
			"{\n  \"cmd\": \"rm -rf /\",\n  \"html\": \"<b> &\"\n}"},
		{"escaped invisible characters", `{"t":"a\u200bb\u202ec\ud83d\ude00"}`, "{\"t\":\"a\\u200Bb\\u202Ec\U0001F600\"}",
			"{\n  \"t\": \"a\\u200Bb\\u202Ec\U0001F600\"\n}"},
		{"keys in sorted order", `{"b":1,"a":{"d":2,"c":3}}`, `{"a":{"c":3,"d":2},"b":1}`,
			"{\n  \"a\": {\n    \"c\": 3,\n    \"d\": 2\n  },\n  \"b\": 1\n}"},
		{"numbers exactly as written", `{"big":12345678901234567890123,"f":1.50,"e":1E+3,"neg":-0}`,
			`{"big":12345678901234567890123,"e":1E+3,"f":1.50,"neg":-0}`,
			"{\n  \"big\": 12345678901234567890123,\n  \"e\": 1E+3,\n  \"f\": 1.50,\n  \"neg\": -0\n}"},
		{"a value that isn't an object", `[1,"\u0061"]`, `[1,"a"]`, "[\n  1,\n  \"a\"\n]"},
		// Two values are not JSON: shown as they are.
		{"a second value", `{"a":1} {"b":2}`, `{"a":1} {"b":2}`, `{"a":1} {"b":2}`},
		// The layout's indentation stays; a run of spaces in a string, which
		// could push text out of view, doesn't.
		{"a run of spaces in a string", `{"a":{"t":"x     y"}}`, `{"a":{"t":"x  \u0020\u0020\u0020y"}}`,
			"{\n  \"a\": {\n    \"t\": \"x  \\u0020\\u0020\\u0020y\"\n  }\n}"},
		// Not JSON (a raw newline and escape sequence in a string): shown as
		// it is, on one line, in both forms.
		{"not json", "{\"t\":\"\x1b[2J\nx\xff\"", `{"t":"\u001B[2J\u000Ax\xFF"`, `{"t":"\u001B[2J\u000Ax\xFF"`},
		{"not json, with a run of spaces", "{\"t\":    1", `{"t":  \u0020\u00201`, `{"t":  \u0020\u00201`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redact.VisibleArguments(json.RawMessage(tt.in), ""); got != tt.line {
				t.Errorf("VisibleArguments(%q, \"\")\n got %q\nwant %q", tt.in, got, tt.line)
			}

			if got := redact.VisibleArguments(json.RawMessage(tt.in), "  "); got != tt.indented {
				t.Errorf("VisibleArguments(%q, \"  \")\n got %q\nwant %q", tt.in, got, tt.indented)
			}
		})
	}
}

// nested is n objects, each the value of the one around it, with 1 inside.
func nested(n int) string {
	return strings.Repeat(`{"a":`, n) + "1" + strings.Repeat("}", n)
}

// TestVisibleArgumentsLayoutIsBounded: indenting deeply nested arguments
// would multiply their size by their depth (about a thousand levels fit in
// a 4 MiB request), so arguments nested deeper than 32 levels, or whose
// layout would be more than 4 times larger plus 4 KiB, are shown on one
// line, in full.
func TestVisibleArgumentsLayoutIsBounded(t *testing.T) {
	t.Run("32 levels are laid out", func(t *testing.T) {
		got := redact.VisibleArguments(json.RawMessage(nested(32)), "  ")

		if lines := strings.Count(got, "\n"); lines != 64 {
			t.Errorf("%d line breaks, want 64: one after each opening and before each closing brace", lines)
		}
	})

	t.Run("33 levels are on one line", func(t *testing.T) {
		if got, want := redact.VisibleArguments(json.RawMessage(nested(33)), "  "), nested(33); got != want {
			t.Errorf("got %q, want the arguments on one line: %q", got, want)
		}
	})

	t.Run("an indent bomb", func(t *testing.T) {
		in := nested(995)
		got := redact.VisibleArguments(json.RawMessage(in), "  ")

		if got != in {
			t.Errorf("got %d bytes, want the %d-byte arguments on one line", len(got), len(in))
		}
	})

	t.Run("an indent bomb laid out with spaces", func(t *testing.T) {
		in := strings.ReplaceAll(nested(995), ":", ": ")
		got := redact.VisibleArguments(json.RawMessage(in), "  ")

		if got != nested(995) {
			t.Errorf("got %d bytes, want the arguments compacted onto one line", len(got))
		}
	})

	// Within the depth, but a long array of short values three levels
	// down: each value would get a line of its own, 8 bytes of
	// indentation, and a newline, for 2 bytes of its own.
	t.Run("a layout too large for its size", func(t *testing.T) {
		in := `{"a":{"b":{"c":[` + strings.Repeat("1,", 9999) + `1]}}}`
		got := redact.VisibleArguments(json.RawMessage(in), "  ")

		if got != in {
			t.Errorf("got %d bytes, want the %d-byte arguments on one line", len(got), len(in))
		}
	})

	t.Run("a small layout is never refused", func(t *testing.T) {
		in := `{"a":[` + strings.Repeat("1,", 99) + `1]}`
		got := redact.VisibleArguments(json.RawMessage(in), "  ")

		if lines := strings.Count(got, "\n"); lines != 103 {
			t.Errorf("%d line breaks, want 103: the arguments laid out", lines)
		}
	})
}
