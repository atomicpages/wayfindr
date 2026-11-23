package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
)

func TestPublicURLDefaultsEmpty(t *testing.T) {
	for _, src := range []string{"", "[server]\nlisten = \"127.0.0.1:7777\"\n"} {
		if got := mustParse(t, src).Server.PublicURL; got != "" {
			t.Errorf("public_url of %q = %q, want empty", src, got)
		}
	}
}

func TestPublicURLValidation(t *testing.T) {
	valid := []string{
		"https://wayfindr.example.com",
		"https://wayfindr.example.com/",
		"https://abc-123.ngrok.app",
		"https://wayfindr.example.com:8443",
		"https://wayfindr.tail1234.ts.net/",
		"HTTPS://Wayfindr.Example.com",
		"https://[2001:db8::1]:8443",
	}

	for _, u := range valid {
		t.Run("valid "+u, func(t *testing.T) {
			got := mustParse(t, "[server]\npublic_url = \""+u+"\"\n").Server.PublicURL

			if got != u {
				t.Errorf("public_url = %q, want it as written, %q", got, u)
			}
		})
	}

	invalid := []struct {
		name, url, want string
	}{
		{"http", "http://wayfindr.example.com", "absolute https URL"},
		{"no scheme", "wayfindr.example.com", "absolute https URL"},
		{"scheme only", "https://", "absolute https URL"},
		{"opaque", "https:wayfindr.example.com", "absolute https URL"},
		{"relative path", "/wayfindr", "absolute https URL"},
		{"other scheme", "ftp://wayfindr.example.com", "absolute https URL"},
		{"no host with a port", "https://:8443", "absolute https URL"},
		{"userinfo", "https://user:hunter2@wayfindr.example.com", "username or password"},
		{"username only", "https://user@wayfindr.example.com", "username or password"},
		{"path", "https://wayfindr.example.com/mcp", "must not have a path"},
		{"deep path", "https://wayfindr.example.com/a/b/", "must not have a path"},
		{"double slash", "https://wayfindr.example.com//", "must not have a path"},
		{"query", "https://wayfindr.example.com/?key=hunter2", "query string"},
		{"query without path", "https://wayfindr.example.com?key=hunter2", "query string"},
		{"empty query", "https://wayfindr.example.com/?", "query string"},
		{"fragment", "https://wayfindr.example.com/#x", "fragment"},
		{"empty fragment", "https://wayfindr.example.com#", "fragment"},
		{"space in host", "https://wayfindr example.com", "not a URL"},
		{"bad port", "https://wayfindr.example.com:http", "not a URL"},
		{"padded", " https://wayfindr.example.com", "not a URL"},
	}

	for _, tt := range invalid {
		t.Run("invalid "+tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte("[server]\npublic_url = \""+tt.url+"\"\n"), options())

			var verr *config.ValidationError

			if !errors.As(err, &verr) {
				t.Fatalf("Parse error = %v, want *config.ValidationError", err)
			}

			if len(verr.Issues) != 1 || verr.Issues[0].Path != "server.public_url" || !strings.Contains(verr.Issues[0].Message, tt.want) {
				t.Errorf("issues = %+v, want one at server.public_url containing %q", verr.Issues, tt.want)
			}

			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q quotes the credential in the URL", err)
			}
		})
	}
}

func TestPublicURLWrongTypeIsATomlError(t *testing.T) {
	_, err := config.Parse([]byte("[server]\npublic_url = 7\n"), options())

	if err == nil {
		t.Fatal("Parse accepted a number for public_url")
	}
}

// ListenAddress reads [server].listen alone so the CLI can find a daemon
// whose file is otherwise invalid; a bad public_url must not stop it.
func TestListenAddressIgnoresPublicURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wayfindr.toml")

	if err := os.WriteFile(path, []byte("[server]\nlisten = \"127.0.0.1:9000\"\npublic_url = \"http://nope\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := config.ListenAddress(path)

	if err != nil || got != "127.0.0.1:9000" {
		t.Errorf("ListenAddress = %q, %v; want 127.0.0.1:9000", got, err)
	}
}

func TestSetPublicURL(t *testing.T) {
	const url = "https://wayfindr.example.com"

	tests := []struct {
		name    string
		src     string
		url     string
		want    string
		changed bool
	}{
		{
			name:    "empty file gets a server table",
			src:     "",
			url:     url,
			want:    "[server]\npublic_url = \"" + url + "\"\n",
			changed: true,
		},
		{
			name:    "no server table, after other tables and comments",
			src:     "# my config\n\n[connectors.fs]\ncommand = \"npx\" # run it\n",
			url:     url,
			want:    "# my config\n\n[connectors.fs]\ncommand = \"npx\" # run it\n\n[server]\npublic_url = \"" + url + "\"\n",
			changed: true,
		},
		{
			name:    "a file without a final newline is finished first",
			src:     "[index]\nrefresh_interval = \"2h\"",
			url:     url,
			want:    "[index]\nrefresh_interval = \"2h\"\n\n[server]\npublic_url = \"" + url + "\"\n",
			changed: true,
		},
		{
			name: "existing server table gets the key after its last one",
			src:  editBase,
			url:  url,
			want: strings.Replace(editBase, "listen = \"127.0.0.1:7777\"   # loopback only\n",
				"listen = \"127.0.0.1:7777\"   # loopback only\npublic_url = \""+url+"\"\n", 1),
			changed: true,
		},
		{
			name:    "empty server table gets the key after its header",
			src:     "# tunnel\n[server]\n\n[index]\nrefresh_interval = \"2h\"\n",
			url:     url,
			want:    "# tunnel\n[server]\npublic_url = \"" + url + "\"\n\n[index]\nrefresh_interval = \"2h\"\n",
			changed: true,
		},
		{
			name:    "indentation of the table's keys is kept",
			src:     "[server]\n  listen = \"127.0.0.1:7777\"\n",
			url:     url,
			want:    "[server]\n  listen = \"127.0.0.1:7777\"\n  public_url = \"" + url + "\"\n",
			changed: true,
		},
		{
			name:    "existing key is replaced on its line, comment kept",
			src:     "[server]\npublic_url = \"https://old.example.com\"   # ngrok\nlisten = \"127.0.0.1:7777\"\n",
			url:     url,
			want:    "[server]\npublic_url = \"" + url + "\"   # ngrok\nlisten = \"127.0.0.1:7777\"\n",
			changed: true,
		},
		{
			name:    "existing literal string is replaced",
			src:     "[server]\npublic_url = 'https://old.example.com'\n",
			url:     url,
			want:    "[server]\npublic_url = \"" + url + "\"\n",
			changed: true,
		},
		{
			name: "same value leaves the file alone, spelling included",
			src:  "[server]\npublic_url = 'https://wayfindr.example.com'  # same\n",
			url:  url,
			want: "[server]\npublic_url = 'https://wayfindr.example.com'  # same\n",
		},
		{
			name:    "clear removes the line and nothing else",
			src:     "# top\n[server]\nlisten = \"127.0.0.1:7777\"\npublic_url = \"" + url + "\" # tunnel\n\n[index]\nrefresh_interval = \"2h\"\n",
			url:     "",
			want:    "# top\n[server]\nlisten = \"127.0.0.1:7777\"\n\n[index]\nrefresh_interval = \"2h\"\n",
			changed: true,
		},
		{
			name:    "clear in a file of only the key leaves an empty table",
			src:     "[server]\npublic_url = \"" + url + "\"\n",
			url:     "",
			want:    "[server]\n",
			changed: true,
		},
		{
			name: "clear when the key is absent changes nothing",
			src:  editBase,
			url:  "",
			want: editBase,
		},
		{
			name: "clear with no server table changes nothing",
			src:  "[index]\nrefresh_interval = \"2h\"\n",
			url:  "",
			want: "[index]\nrefresh_interval = \"2h\"\n",
		},
		{
			name:    "CRLF file keeps its line breaks",
			src:     "[server]\r\nlisten = \"127.0.0.1:7777\"\r\n",
			url:     url,
			want:    "[server]\r\nlisten = \"127.0.0.1:7777\"\r\npublic_url = \"" + url + "\"\r\n",
			changed: true,
		},
		{
			name:    "CRLF file gets a table with its line breaks",
			src:     "[index]\r\nrefresh_interval = \"2h\"\r\n",
			url:     url + "/",
			want:    "[index]\r\nrefresh_interval = \"2h\"\r\n\r\n[server]\r\npublic_url = \"" + url + "/\"\r\n",
			changed: true,
		},
		{
			// A byte order mark in front of the [server] header must not hide
			// it: the key goes into that table, not a second one.
			name:    "BOM before the server header keeps one server table",
			src:     "\uFEFF[server]\nlisten = \"127.0.0.1:7777\"\n",
			url:     url,
			want:    "\uFEFF[server]\nlisten = \"127.0.0.1:7777\"\npublic_url = \"" + url + "\"\n",
			changed: true,
		},
		{
			name:    "BOM before another table still gets a server table",
			src:     "\uFEFF[connectors.fs]\ncommand = \"npx\"\n",
			url:     url,
			want:    "\uFEFF[connectors.fs]\ncommand = \"npx\"\n\n[server]\npublic_url = \"" + url + "\"\n",
			changed: true,
		},
		{
			name:    "BOM file already holding the value is unchanged",
			src:     "\uFEFF[server]\npublic_url = \"" + url + "\"\n",
			url:     url,
			want:    "\uFEFF[server]\npublic_url = \"" + url + "\"\n",
			changed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.SetPublicURL([]byte(tt.src), tt.url, options())

			if err != nil {
				t.Fatalf("SetPublicURL: %v", err)
			}

			if diff := cmp.Diff(tt.want, string(got.File)); diff != "" {
				t.Errorf("file (-want +got):\n%s", diff)
			}

			if got.Changed != tt.changed {
				t.Errorf("Changed = %v, want %v", got.Changed, tt.changed)
			}

			if got.After.Server.PublicURL != tt.url {
				t.Errorf("After public_url = %q, want %q", got.After.Server.PublicURL, tt.url)
			}

			// The result reads back as the one edit: the file's other
			// settings are the ones it had.
			want := mustParse(t, tt.src)
			want.Server.PublicURL = tt.url

			if diff := cmp.Diff(want, mustParse(t, string(got.File))); diff != "" {
				t.Errorf("config after the edit (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSetPublicURLRoundTrip(t *testing.T) {
	const a, b = "https://a.example.com", "https://b.example.com/"

	src := []byte(editBase)

	for _, u := range []string{a, b, "", a, ""} {
		got, err := config.SetPublicURL(src, u, options())

		if err != nil {
			t.Fatalf("SetPublicURL(%q): %v", u, err)
		}

		if p := mustParse(t, string(got.File)).Server.PublicURL; p != u {
			t.Fatalf("public_url after setting %q = %q", u, p)
		}

		src = got.File
	}

	// Set then clear leaves the file as it began.
	if diff := cmp.Diff(editBase, string(src)); diff != "" {
		t.Errorf("file after set and clear (-want +got):\n%s", diff)
	}
}

func TestSetPublicURLRefusals(t *testing.T) {
	const url = "https://wayfindr.example.com"

	t.Run("an invalid URL is the validation error, and nothing is written", func(t *testing.T) {
		for _, bad := range []string{"http://x.example.com", "https://x.example.com/mcp", "https://u:hunter2@x.example.com"} {
			got, err := config.SetPublicURL([]byte(editBase), bad, options())

			var verr *config.ValidationError

			if !errors.As(err, &verr) || len(verr.Issues) != 1 || verr.Issues[0].Path != "server.public_url" {
				t.Errorf("SetPublicURL(%q) error = %v, want a *ValidationError at server.public_url", bad, err)
			}

			if got.File != nil {
				t.Errorf("SetPublicURL(%q) returned a file", bad)
			}

			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q quotes the credential", err)
			}
		}
	})

	unsafe := []struct {
		name string
		src  string
		want config.Case
	}{
		{"a file that doesn't parse", "[server\n", config.CaseUnparsable},
		{"an invalid config", "[index]\nrefresh_interval = \"-1s\"\n", config.CaseUnparsable},
		{"server as an inline table", "server = { listen = \"127.0.0.1:7777\" }\n", config.CaseServerNotTable},
		{"server as a dotted key", "server.listen = \"127.0.0.1:7777\"\n", config.CaseServerNotTable},
		{"a multi-line value", "[server]\npublic_url = \"\"\"\nhttps://old.example.com\"\"\"\n", config.CaseMultiline},
	}

	for _, tt := range unsafe {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.SetPublicURL([]byte(tt.src), url, options())
			ce, ok := config.AsError(err)

			if !ok || ce.Kind != config.KindUnsafe || ce.Case != tt.want {
				t.Fatalf("SetPublicURL error = %v, want KindUnsafe %s", err, tt.want)
			}

			if ce.Connector != "" {
				t.Errorf("Connector = %q, want none", ce.Connector)
			}

			if a := config.Advise(ce); a.Summary == "" || a.Action == "" || strings.Contains(a.Summary, "connector") {
				t.Errorf("advice = %+v, want a server-worded summary and an action", a)
			}
		})
	}
}
