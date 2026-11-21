package redact

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Printable returns s with every rune a terminal would obey rather than
// show replaced by U+FFFD: invalid UTF-8 (each run of bad bytes becomes one
// U+FFFD), control characters (C0, DEL, and C1, which include ESC and so
// every escape sequence), and bidirectional controls such as U+202E, which
// reorder the text around them. Use it on upstream or agent text, such as a
// tool name or summary, before printing it in a table; JSON output escapes
// such text itself and needs no Printable.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return utf8.RuneError
		}

		return r
	}, strings.ToValidUTF8(s, string(utf8.RuneError)))
}
