package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
)

const oauthTunnel = "https://wayfindr.example.com"

func TestOAuthDefaults(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"empty file", "", []string{config.DefaultOAuthClient}},
		{"table without clients", "[server.oauth]\nenabled = false\n", []string{config.DefaultOAuthClient}},
		{"explicit empty list stays empty", "[server.oauth]\nclients = []\n", []string{}},
		{"explicit list", "[server.oauth]\nclients = [\"https://a.example.com/c.json\"]\n", []string{"https://a.example.com/c.json"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustParse(t, tt.src).Server.OAuth

			if diff := cmp.Diff(tt.want, got.Clients); diff != "" {
				t.Errorf("clients (-want +got):\n%s", diff)
			}

			if got.Enabled {
				t.Error("enabled defaults to true, want false")
			}
		})
	}
}

func TestServerIssuer(t *testing.T) {
	tests := []struct{ publicURL, want string }{
		{"", ""},
		{"https://wayfindr.example.com", "https://wayfindr.example.com"},
		{"https://wayfindr.example.com/", "https://wayfindr.example.com"},
		{"https://wayfindr.example.com:8443/", "https://wayfindr.example.com:8443"},
		{"https://Wayfindr.Example.COM", "https://wayfindr.example.com"},
		{"HTTPS://WAYFINDR.example.com/", "https://wayfindr.example.com"},
		{"https://wayfindr.example.com:443", "https://wayfindr.example.com"},
		{"https://Wayfindr.example.com:443/", "https://wayfindr.example.com"},
		{"https://[2001:DB8::1]:443/", "https://[2001:db8::1]"},
	}

	for _, tt := range tests {
		if got := (config.Server{PublicURL: tt.publicURL}).Issuer(); got != tt.want {
			t.Errorf("Issuer of %q = %q, want %q", tt.publicURL, got, tt.want)
		}
	}
}

func TestOAuthValidation(t *testing.T) {
	const on = "[server]\npublic_url = \"" + oauthTunnel + "\"\n"

	tests := []struct {
		name     string
		src      string
		wantPath string
		wantMsg  string
	}{
		{"enabled without a public URL", "[server.oauth]\nenabled = true\n", "server.oauth.enabled", "needs server.public_url"},
		{
			"enabled with no clients",
			on + "[server.oauth]\nenabled = true\nclients = []\n",
			"server.oauth.clients",
			"must list at least one client when server.oauth.enabled is true",
		},
		{"http", "[server.oauth]\nclients = [\"http://a.example.com/c.json\"]\n", "server.oauth.clients", "entry 1 must be an absolute https URL"},
		{"no host", "[server.oauth]\nclients = [\"https:///c.json\"]\n", "server.oauth.clients", "entry 1 must be an absolute https URL"},
		{"no scheme", "[server.oauth]\nclients = [\"a.example.com/c.json\"]\n", "server.oauth.clients", "entry 1 must be an absolute https URL"},
		{"userinfo", "[server.oauth]\nclients = [\"https://u:hunter2@a.example.com/c.json\"]\n", "server.oauth.clients", "entry 1 must not contain a username"},
		{"query", "[server.oauth]\nclients = [\"https://a.example.com/c.json?x=hunter2\"]\n", "server.oauth.clients", "entry 1 must not have a query"},
		{"empty query", "[server.oauth]\nclients = [\"https://a.example.com/c.json?\"]\n", "server.oauth.clients", "entry 1 must not have a query"},
		{"fragment", "[server.oauth]\nclients = [\"https://a.example.com/c.json#hunter2\"]\n", "server.oauth.clients", "entry 1 must not have a fragment"},
		{"empty fragment", "[server.oauth]\nclients = [\"https://a.example.com/c.json#\"]\n", "server.oauth.clients", "entry 1 must not have a fragment"},
		{"no path", "[server.oauth]\nclients = [\"https://a.example.com\"]\n", "server.oauth.clients", "entry 1 must have a path"},
		{"root path", "[server.oauth]\nclients = [\"https://a.example.com/\"]\n", "server.oauth.clients", "entry 1 must have a path"},
		{"uppercase scheme", "[server.oauth]\nclients = [\"HTTPS://a.example.com/c.json\"]\n", "server.oauth.clients", "entry 1 is not in canonical form"},
		{"uppercase host", "[server.oauth]\nclients = [\"https://A.Example.com/c.json\"]\n", "server.oauth.clients", "entry 1 is not in canonical form"},
		{"dot segment", "[server.oauth]\nclients = [\"https://a.example.com/x/./c.json\"]\n", "server.oauth.clients", "entry 1 is not in canonical form"},
		{"dot-dot segment", "[server.oauth]\nclients = [\"https://a.example.com/x/../c.json\"]\n", "server.oauth.clients", "entry 1 is not in canonical form"},
		{"trailing dot-dot segment", "[server.oauth]\nclients = [\"https://a.example.com/x/..\"]\n", "server.oauth.clients", "entry 1 is not in canonical form"},
		{"not a URL", "[server.oauth]\nclients = [\"https://a b.example.com/c.json\"]\n", "server.oauth.clients", "entry 1 is not a URL"},
		{
			"position is 1-based",
			"[server.oauth]\nclients = [\"https://a.example.com/c.json\", \"https://b.example.com/\"]\n",
			"server.oauth.clients",
			"entry 2 must have a path",
		},
		{
			"duplicate",
			"[server.oauth]\nclients = [\"https://a.example.com/c.json\", \"https://b.example.com/c.json\", \"https://a.example.com/c.json\"]\n",
			"server.oauth.clients",
			"entry 3 repeats an earlier entry",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tt.src), options())

			var verr *config.ValidationError

			if !errors.As(err, &verr) {
				t.Fatalf("Parse error = %v, want *config.ValidationError", err)
			}

			if len(verr.Issues) != 1 || verr.Issues[0].Path != tt.wantPath || !strings.Contains(verr.Issues[0].Message, tt.wantMsg) {
				t.Errorf("issues = %+v, want one at %s containing %q", verr.Issues, tt.wantPath, tt.wantMsg)
			}

			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "example.com") {
				t.Errorf("error %q quotes an entry", err)
			}
		})
	}

	t.Run("entries are checked while disabled", func(t *testing.T) {
		if _, err := config.Parse([]byte("[server.oauth]\nenabled = false\nclients = [\"http://x/\"]\n"), options()); err == nil {
			t.Error("Parse accepted a bad client while OAuth is off")
		}
	})

	t.Run("valid", func(t *testing.T) {
		src := on + "[server.oauth]\nenabled = true\nclients = [\"https://a.example.com/c.json\", \"https://b.example.com/x/y\"]\n"
		got := mustParse(t, src).Server.OAuth

		want := config.OAuth{Enabled: true, Clients: []string{"https://a.example.com/c.json", "https://b.example.com/x/y"}}

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("oauth (-want +got):\n%s", diff)
		}
	})
}

func TestDiffIgnoresOAuth(t *testing.T) {
	before := mustParse(t, "[server]\npublic_url = \""+oauthTunnel+"\"\n")
	after := mustParse(t, "[server]\npublic_url = \""+oauthTunnel+"\"\n[server.oauth]\nenabled = true\nclients = [\"https://a.example.com/c.json\"]\n")

	if diff := cmp.Diff(config.Changes{}, config.Diff(before, after)); diff != "" {
		t.Errorf("Diff (-want +got):\n%s", diff)
	}
}

func TestSetOAuthEnabled(t *testing.T) {
	const pub = "[server]\nlisten = \"127.0.0.1:7777\"\npublic_url = \"" + oauthTunnel + "\"\n"

	tests := []struct {
		name    string
		src     string
		enabled bool
		want    string
		changed bool
	}{
		{
			name:    "no oauth table gets one at the end",
			src:     pub + "\n[index]\nrefresh_interval = \"2h\"\n",
			enabled: true,
			want:    pub + "\n[index]\nrefresh_interval = \"2h\"\n\n[server.oauth]\nenabled = true\n",
			changed: true,
		},
		{
			name:    "a file without a final newline is finished first",
			src:     strings.TrimSuffix(pub, "\n"),
			enabled: true,
			want:    pub + "\n[server.oauth]\nenabled = true\n",
			changed: true,
		},
		{
			name:    "missing key goes after the table's last key",
			src:     pub + "\n[server.oauth]\nclients = [\"https://a.example.com/c.json\"]\n\n[index]\nrefresh_interval = \"2h\"\n",
			enabled: true,
			want:    pub + "\n[server.oauth]\nclients = [\"https://a.example.com/c.json\"]\nenabled = true\n\n[index]\nrefresh_interval = \"2h\"\n",
			changed: true,
		},
		{
			name:    "empty table gets the key after its header",
			src:     pub + "\n# agents\n[server.oauth]\n\n[index]\nrefresh_interval = \"2h\"\n",
			enabled: true,
			want:    pub + "\n# agents\n[server.oauth]\nenabled = true\n\n[index]\nrefresh_interval = \"2h\"\n",
			changed: true,
		},
		{
			name:    "existing key is rewritten, comment kept",
			src:     pub + "\n[server.oauth]\nenabled = false   # off for now\nclients = [\"https://a.example.com/c.json\"]\n",
			enabled: true,
			want:    pub + "\n[server.oauth]\nenabled = true   # off for now\nclients = [\"https://a.example.com/c.json\"]\n",
			changed: true,
		},
		{
			name:    "false writes the key and keeps the table and its clients",
			src:     pub + "\n[server.oauth]\nenabled = true\nclients = [\"https://a.example.com/c.json\"]\n",
			enabled: false,
			want:    pub + "\n[server.oauth]\nenabled = false\nclients = [\"https://a.example.com/c.json\"]\n",
			changed: true,
		},
		{
			name:    "false where the key is absent changes nothing",
			src:     pub + "\n[server.oauth]\nclients = [\"https://a.example.com/c.json\"]\n",
			enabled: false,
			want:    pub + "\n[server.oauth]\nclients = [\"https://a.example.com/c.json\"]\n",
		},
		{
			name:    "false with no oauth table changes nothing",
			src:     pub,
			enabled: false,
			want:    pub,
		},
		{
			name:    "same value leaves the file alone",
			src:     pub + "\n[server.oauth]\nenabled = true # on\n",
			enabled: true,
			want:    pub + "\n[server.oauth]\nenabled = true # on\n",
		},
		{
			name:    "oauth after a bare [server] header with no [server.oauth] keeps one server table",
			src:     "[server]\npublic_url = \"" + oauthTunnel + "\"\n",
			enabled: true,
			want:    "[server]\npublic_url = \"" + oauthTunnel + "\"\n\n[server.oauth]\nenabled = true\n",
			changed: true,
		},
		{
			name:    "CRLF file keeps its line breaks",
			src:     "[server]\r\npublic_url = \"" + oauthTunnel + "\"\r\n",
			enabled: true,
			want:    "[server]\r\npublic_url = \"" + oauthTunnel + "\"\r\n\r\n[server.oauth]\r\nenabled = true\r\n",
			changed: true,
		},
		{
			name:    "BOM is kept",
			src:     "\uFEFF" + pub,
			enabled: true,
			want:    "\uFEFF" + pub + "\n[server.oauth]\nenabled = true\n",
			changed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.SetOAuthEnabled([]byte(tt.src), tt.enabled, options())

			if err != nil {
				t.Fatalf("SetOAuthEnabled: %v", err)
			}

			if diff := cmp.Diff(tt.want, string(got.File)); diff != "" {
				t.Errorf("file (-want +got):\n%s", diff)
			}

			if got.Changed != tt.changed {
				t.Errorf("Changed = %v, want %v", got.Changed, tt.changed)
			}

			if got.After.Server.OAuth.Enabled != tt.enabled {
				t.Errorf("After enabled = %v, want %v", got.After.Server.OAuth.Enabled, tt.enabled)
			}

			// Only that setting differs from what the file said before.
			want := mustParse(t, tt.src)
			want.Server.OAuth.Enabled = tt.enabled

			if diff := cmp.Diff(want, mustParse(t, string(got.File))); diff != "" {
				t.Errorf("config after the edit (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSetOAuthEnabledRoundTrip(t *testing.T) {
	src := []byte("[server]\npublic_url = \"" + oauthTunnel + "\"\n")

	for _, on := range []bool{true, false, true} {
		got, err := config.SetOAuthEnabled(src, on, options())

		if err != nil {
			t.Fatalf("SetOAuthEnabled(%v): %v", on, err)
		}

		if p := mustParse(t, string(got.File)).Server.OAuth.Enabled; p != on {
			t.Fatalf("enabled after setting %v = %v", on, p)
		}

		src = got.File
	}
}

func TestSetOAuthEnabledRefusals(t *testing.T) {
	const pub = "[server]\npublic_url = \"" + oauthTunnel + "\"\n"

	validation := []struct {
		name string
		src  string
		path string
	}{
		{"no public URL", "[index]\nrefresh_interval = \"2h\"\n", "server.oauth.enabled"},
		{"no clients", pub + "[server.oauth]\nclients = []\n", "server.oauth.clients"},
	}

	for _, tt := range validation {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.SetOAuthEnabled([]byte(tt.src), true, options())

			var verr *config.ValidationError

			if !errors.As(err, &verr) || len(verr.Issues) != 1 || verr.Issues[0].Path != tt.path {
				t.Fatalf("error = %v, want a *ValidationError at %s", err, tt.path)
			}

			if got.File != nil {
				t.Error("returned a file")
			}
		})
	}

	unsafe := []struct {
		name    string
		src     string
		want    config.Case
		wantKey string
	}{
		{"a file that doesn't parse", "[server\n", config.CaseUnparsable, ""},
		{"an invalid config", "[index]\nrefresh_interval = \"-1s\"\n", config.CaseUnparsable, ""},
		{"oauth as a dotted key in [server]", pub + "oauth.clients = [\"https://a.example.com/c.json\"]\n", config.CaseOAuthNotTable, "server.oauth"},
		{"oauth as an inline table in [server]", pub + "oauth = { clients = [\"https://a.example.com/c.json\"] }\n", config.CaseOAuthNotTable, "server.oauth"},
		{"server.oauth as a dotted key at the top", "server.oauth.clients = [\"https://a.example.com/c.json\"]\nserver.public_url = \"" + oauthTunnel + "\"\n", config.CaseOAuthNotTable, "server"},
		{"server as an inline table", "server = { public_url = \"" + oauthTunnel + "\" }\n", config.CaseOAuthNotTable, "server"},
		{"server as a dotted key", "server.public_url = \"" + oauthTunnel + "\"\n", config.CaseOAuthNotTable, "server"},
		{"a value that is not a boolean", pub + "[server.oauth]\nenabled = \"\"\"\nfalse\"\"\"\n", config.CaseUnparsable, ""},
	}

	for _, tt := range unsafe {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.SetOAuthEnabled([]byte(tt.src), true, options())
			ce, ok := config.AsError(err)

			if !ok || ce.Kind != config.KindUnsafe || ce.Case != tt.want || ce.Key != tt.wantKey {
				t.Fatalf("error = %v, want KindUnsafe %s at key %q", err, tt.want, tt.wantKey)
			}

			// The summary names the table at fault.
			if tt.want == config.CaseOAuthNotTable {
				if reason := config.Reason(ce); !strings.HasPrefix(reason, tt.wantKey+" is not written as a [") {
					t.Errorf("reason = %q, want it to name %s", reason, tt.wantKey)
				}
			}

			if a := config.Advise(ce); a.Summary == "" || a.Action == "" || strings.Contains(a.Summary, "connector") {
				t.Errorf("advice = %+v, want a server-worded summary and an action", a)
			}
		})
	}
}

func TestSetPublicURLRefusesToClearWhileOAuthIsOn(t *testing.T) {
	src := "[server]\npublic_url = \"" + oauthTunnel + "\"\n[server.oauth]\nenabled = true\n"

	got, err := config.SetPublicURL([]byte(src), "", options())

	var verr *config.ValidationError

	if !errors.As(err, &verr) || len(verr.Issues) != 1 || verr.Issues[0].Path != "server.public_url" ||
		!strings.Contains(verr.Issues[0].Message, "turn it off first") {
		t.Fatalf("error = %v, want a *ValidationError at server.public_url", err)
	}

	if got.File != nil {
		t.Error("returned a file")
	}

	// Changing it is fine; only clearing is refused.
	if _, err := config.SetPublicURL([]byte(src), "https://other.example.com", options()); err != nil {
		t.Errorf("changing public_url while OAuth is on: %v", err)
	}
}
