package clientconfig

// stripJSONC returns src with // and /* */ comments and trailing commas
// removed, so encoding/json can decode the JSON-with-comments that VS Code,
// Cursor and Claude Code accept. Text inside strings is left alone. An
// unterminated comment or string is left for the decoder to report.
func stripJSONC(src []byte) []byte {
	out := make([]byte, 0, len(src))

	// pendingComma is the index in out of a comma that is dropped if the
	// next token closes an object or array.
	pendingComma := -1

	for i := 0; i < len(src); i++ {
		c := src[i]

		switch {
		case c == '"':
			pendingComma = -1
			end := stringEnd(src, i)
			out = append(out, src[i:end]...)
			i = end - 1
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}

			if i < len(src) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := i + 2

			for end+1 < len(src) && (src[end] != '*' || src[end+1] != '/') {
				end++
			}

			if end+1 >= len(src) {
				// Unterminated: keep it, so the decoder fails on it.
				out = append(out, src[i:]...)

				return out
			}

			out = append(out, ' ')
			i = end + 1
		case c == ',':
			pendingComma = len(out)
			out = append(out, c)
		case c == '}' || c == ']':
			if pendingComma >= 0 {
				out = append(out[:pendingComma], out[pendingComma+1:]...)
				pendingComma = -1
			}

			out = append(out, c)
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			out = append(out, c)
		default:
			pendingComma = -1
			out = append(out, c)
		}
	}

	return out
}

// stringEnd returns the index just past the JSON string that starts at
// src[start], or len(src) if it is unterminated.
func stringEnd(src []byte, start int) int {
	for i := start + 1; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}

	return len(src)
}
