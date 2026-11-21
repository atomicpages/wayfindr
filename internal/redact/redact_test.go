package redact_test

import (
	"testing"

	"github.com/atomicpages/wayfindr/internal/redact"
)

func TestURLQueries(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"plain text is untouched", "connection refused: is it running?", "connection refused: is it running?"},
		{"question mark outside a url is untouched", "what? no://", "what? no://"},
		{"url without a query is untouched", "dial https://x.example.com/mcp failed", "dial https://x.example.com/mcp failed"},
		{"query is replaced", "https://x.example.com/mcp?key=hunter2", "https://x.example.com/mcp?REDACTED"},
		{"query ends at whitespace", "GET https://x.example.com/?key=hunter2 timed out", "GET https://x.example.com/?REDACTED timed out"},
		{"several urls", "a http://a.example/?t=one b https://b.example/p?t=two c https://c.example/",
			"a http://a.example/?REDACTED b https://b.example/p?REDACTED c https://c.example/"},
		{"url.Error text", `Post "http://127.0.0.1:9/mcp?key=hunter2": dial tcp 127.0.0.1:9: connect: connection refused`,
			`Post "http://127.0.0.1:9/mcp?REDACTED": dial tcp 127.0.0.1:9: connect: connection refused`},
		{"escaped quote inside a quoted url", `parse "https://x.example/?k=\"hunter2\"": bad`,
			`parse "https://x.example/?REDACTED": bad`},
		{"query and fragment", "https://x.example/cb?code=abc#state=xyz", "https://x.example/cb?REDACTED#REDACTED"},
		{"fragment alone", "https://x.example/cb#access_token=abc", "https://x.example/cb#REDACTED"},
		{"uppercase scheme", "HTTPS://X.EXAMPLE/?key=hunter2", "HTTPS://X.EXAMPLE/?REDACTED"},
		{"other scheme", "postgres://db.example/app?password=hunter2", "postgres://db.example/app?REDACTED"},
		{"angle brackets end the url", "<https://x.example/?k=v> next", "<https://x.example/?REDACTED> next"},
		{"user and password", "dial https://alice:hunter2@x.example/mcp failed", "dial https://REDACTED@x.example/mcp failed"},
		{"user only", "dial https://hunter2@x.example/mcp failed", "dial https://REDACTED@x.example/mcp failed"},
		{"userinfo without a path", "dial https://alice:hunter2@x.example: refused", "dial https://REDACTED@x.example: refused"},
		{"userinfo, query and fragment", "https://alice:hunter2@x.example/cb?code=abc#state=xyz",
			"https://REDACTED@x.example/cb?REDACTED#REDACTED"},
		{"userinfo inside a quoted url", `parse "https://alice:hunter2@x.example/?k=v": bad`,
			`parse "https://REDACTED@x.example/?REDACTED": bad`},
		{"userinfo in url.Error text", `Post "http://alice:hunter2@127.0.0.1:9/mcp": dial tcp 127.0.0.1:9: connect: connection refused`,
			`Post "http://REDACTED@127.0.0.1:9/mcp": dial tcp 127.0.0.1:9: connect: connection refused`},
		{"at sign in the path is not userinfo", "https://x.example/users/@bob", "https://x.example/users/@bob"},
		{"at sign in the query is not userinfo", "https://x.example/?to=a@b.example", "https://x.example/?REDACTED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redact.URLQueries(tt.in); got != tt.want {
				t.Errorf("URLQueries(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestURL(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"no query or fragment is untouched", "https://as.example.com/.well-known/oauth-authorization-server",
			"https://as.example.com/.well-known/oauth-authorization-server"},
		{"query", "https://as.example.com/token?key=hunter2", "https://as.example.com/token?REDACTED"},
		{"fragment", "https://x.example/cb#access_token=abc", "https://x.example/cb#REDACTED"},
		{"query and fragment", "https://x.example/cb?code=abc#state=xyz", "https://x.example/cb?REDACTED#REDACTED"},
		{"userinfo", "https://alice:hunter2@as.example.com/", "https://REDACTED@as.example.com/"},
		{"relative reference keeps its path", "/mcp?key=hunter2", "/mcp?REDACTED"},
		{"unparseable is masked whole", "https://as.example.com/%zz?key=hunter2", "REDACTED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redact.URL(tt.in); got != tt.want {
				t.Errorf("URL(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestPrintable spells bidi controls as escapes, since a literal one is
// invisible in the source.
func TestPrintable(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"plain text is untouched", "github.search_issues", "github.search_issues"},
		{"non-ascii text is untouched", "Café 日本語 é", "Café 日本語 é"},
		{"replacement character is kept", "a�b", "a�b"},
		{"nul", "a\x00b", "a�b"},
		{"escape sequence", "\x1b[31mred\x1b[0m", "�[31mred�[0m"},
		{"tab and newlines", "a\tb\nc\rd", "a�b�c�d"},
		{"delete", "a\x7fb", "a�b"},
		{"c1 controls", "a\u009bb\u0085c", "a�b�c"},
		{"right-to-left override", "read\u202eetirw", "read�etirw"},
		{"bidi isolates and marks", "a\u2066b\u2069c\u200fd\u061ce", "a�b�c�d�e"},
		{"invalid utf-8 byte", "a\xffb", "a�b"},
		{"run of invalid bytes is one replacement", "a\xff\xfe\xfdb", "a�b"},
		{"truncated rune", "a\xe6\x97", "a�"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redact.Printable(tt.in); got != tt.want {
				t.Errorf("Printable(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}
