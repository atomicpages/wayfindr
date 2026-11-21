// Package redact keeps secrets that ride inside URLs out of errors, logs, and
// stored text (design §9: URL query strings are never logged), and, with
// Printable, keeps terminal control sequences out of text printed to a
// terminal. It is a leaf: it imports only the standard library.
package redact

import (
	"net/url"
	"strings"
)

// Mask replaces each redacted part.
const Mask = "REDACTED"

// URL returns raw, a single URL, with its userinfo, query and fragment each
// replaced by Mask, so "https://u:p@h/p?key=v#f" becomes
// "https://REDACTED@h/p?REDACTED#REDACTED". Use it on a value known to be a
// URL, such as a configured endpoint; use URLQueries on free text. A value
// that does not parse is replaced by Mask whole, since where its secrets
// sit is unknown.
func URL(raw string) string {
	if _, err := url.Parse(raw); err != nil {
		return Mask
	}

	return maskURL(raw)
}

// URLQueries returns s with the userinfo, query and fragment of every URL in
// it replaced by Mask, so "https://u:p@h/p?key=v#f" becomes
// "https://REDACTED@h/p?REDACTED#REDACTED", as URL does. Use it on free text, such as a wrapped
// error, that may quote a URL; the rest of the text is unchanged.
//
// A URL is a scheme followed by "://", and runs until whitespace, a quote,
// an angle bracket, or a backtick. A backslash escape (as in a %q-quoted URL)
// stays inside it, so an escaped quote in a query does not end the
// redaction early. A URL without a scheme is not recognized.
func URLQueries(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}

	var b strings.Builder
	rest := s

	for {
		sep := strings.Index(rest, "://")

		if sep < 0 {
			break
		}

		start := schemeStart(rest, sep)
		end := urlEnd(rest, sep+len("://"))

		if start == sep {
			// No scheme: not a URL. Copy past the separator and look again.
			b.WriteString(rest[:end])
			rest = rest[end:]
			continue
		}

		b.WriteString(rest[:start])
		b.WriteString(maskURL(rest[start:end]))
		rest = rest[end:]
	}

	b.WriteString(rest)

	return b.String()
}

// schemeStart returns where the scheme ending at s[sep] begins, or sep if
// there is none. A scheme is a letter followed by letters, digits, '+', '-',
// or '.' (RFC 3986 §3.1).
func schemeStart(s string, sep int) int {
	i := sep

	for i > 0 && isSchemeChar(s[i-1]) {
		i--
	}

	for i < sep && !isLetter(s[i]) {
		i++
	}

	return i
}

// urlEnd returns the index just past the URL whose authority starts at
// s[from].
func urlEnd(s string, from int) int {
	i := from

	for i < len(s) {
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s):
			i += 2
			continue
		case c <= ' ', c == '"', c == '\'', c == '<', c == '>', c == '`':
			return i
		}

		i++
	}

	return len(s)
}

// maskURL replaces the userinfo, query and fragment of the single URL u with
// Mask. It works on the text rather than a parsed URL, so it also handles a
// URL cut out of free text that url.Parse would reject. Both URL and
// URLQueries use it, so the two mask the same parts.
func maskURL(u string) string {
	head, tail := u, ""

	if cut := strings.IndexAny(u, "?#"); cut >= 0 {
		head, tail = u[:cut], u[cut:]
	}

	return maskUserinfo(head) + maskTail(tail)
}

// maskUserinfo replaces the userinfo in head, a URL with its query and
// fragment removed. The authority follows "://" (or a leading "//") and runs
// to the first '/'; like url.Parse, the userinfo ends at its last '@'.
func maskUserinfo(head string) string {
	var from int

	switch i := strings.Index(head, "://"); {
	case i >= 0:
		from = i + len("://")
	case strings.HasPrefix(head, "//"):
		from = len("//")
	default:
		return head
	}

	authority := head[from:]

	if slash := strings.IndexByte(authority, '/'); slash >= 0 {
		authority = authority[:slash]
	}

	at := strings.LastIndexByte(authority, '@')

	if at < 0 {
		return head
	}

	return head[:from] + Mask + head[from+at:]
}

// maskTail masks tail, the query and fragment of a URL starting at its first
// '?' or '#'. An empty tail stays empty.
func maskTail(tail string) string {
	switch {
	case tail == "":
		return ""
	case tail[0] == '#':
		return "#" + Mask
	case strings.Contains(tail, "#"):
		return "?" + Mask + "#" + Mask
	default:
		return "?" + Mask
	}
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isSchemeChar(c byte) bool {
	return isLetter(c) || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'
}
