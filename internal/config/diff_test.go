package config_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/atomicpages/wayfindr/internal/config"
)

func mustParse(t *testing.T, s string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(s), options())

	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, s)
	}

	return c
}

const diffBase = `
[embedding]
base_url = "http://localhost:11434/v1"
model = "qwen3-embedding"
dimensions = 1024

[connectors.linear]
url  = "https://mcp.linear.app/mcp"
auth = { type = "oauth" }

[connectors.exa]
url     = "https://mcp.exa.ai/mcp"
headers = { "x-api-key" = "${env:EXA_API_KEY}" }

[connectors.fs]
command = "npx"
args    = ["-y", "server-filesystem"]
`

// TestDiff covers each set and flag design §4 and §7.8 name: Added, Removed,
// URLChanged (ADR 0006: tokens are bound to the old url), Changed, Unchanged,
// EmbeddingChanged, and RestartRequired (server.*, storage.*).
func TestDiff(t *testing.T) {
	tests := []struct {
		name string
		old  string
		new  string
		want config.Changes
	}{
		{
			name: "identical",
			old:  diffBase,
			new:  diffBase,
			want: config.Changes{Unchanged: []string{"exa", "fs", "linear"}},
		},
		{
			name: "connector added",
			old:  diffBase,
			new:  diffBase + "\n[connectors.deepwiki]\nurl = \"https://mcp.deepwiki.com/mcp\"\n",
			want: config.Changes{Added: []string{"deepwiki"}, Unchanged: []string{"exa", "fs", "linear"}},
		},
		{
			name: "connector removed",
			old:  diffBase + "\n[connectors.deepwiki]\nurl = \"https://mcp.deepwiki.com/mcp\"\n",
			new:  diffBase,
			want: config.Changes{Removed: []string{"deepwiki"}, Unchanged: []string{"exa", "fs", "linear"}},
		},
		{
			name: "url changed",
			old:  "[connectors.linear]\nurl = \"https://mcp.linear.app/mcp\"\n",
			new:  "[connectors.linear]\nurl = \"https://mcp.linear.app/v2/mcp\"\n",
			want: config.Changes{URLChanged: []string{"linear"}},
		},
		{
			name: "url and auth changed is URLChanged only",
			old:  "[connectors.linear]\nurl = \"https://mcp.linear.app/mcp\"\n",
			new:  "[connectors.linear]\nurl = \"https://mcp.linear.app/v2/mcp\"\nauth = { type = \"oauth\" }\n",
			want: config.Changes{URLChanged: []string{"linear"}},
		},
		{
			name: "url template text changed",
			old:  "[connectors.x]\nurl = \"https://${env:HOST}/mcp\"\n",
			new:  "[connectors.x]\nurl = \"https://${env:OTHER_HOST}/mcp\"\n",
			want: config.Changes{URLChanged: []string{"x"}},
		},
		{
			name: "header changed",
			old:  "[connectors.exa]\nurl = \"https://mcp.exa.ai/mcp\"\nheaders = { k = \"${env:A}\" }\n",
			new:  "[connectors.exa]\nurl = \"https://mcp.exa.ai/mcp\"\nheaders = { k = \"${secret:a}\" }\n",
			want: config.Changes{Changed: []string{"exa"}},
		},
		{
			name: "tools list changed",
			old:  "[connectors.fs]\ncommand = \"npx\"\ntools = [\"@read\"]\n",
			new:  "[connectors.fs]\ncommand = \"npx\"\ntools = [\"@read\", \"write_file\"]\n",
			want: config.Changes{Changed: []string{"fs"}},
		},
		{
			name: "auth scopes changed",
			old:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\nauth = { type = \"oauth\", scopes = [\"a\"] }\n",
			new:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\nauth = { type = \"oauth\", scopes = [\"a\", \"b\"] }\n",
			want: config.Changes{Changed: []string{"s"}},
		},
		{
			name: "auth added",
			old:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\n",
			new:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\nauth = { type = \"oauth\" }\n",
			want: config.Changes{Changed: []string{"s"}},
		},
		{
			name: "validate_issuer turned off",
			old:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\nauth = { type = \"oauth\" }\n",
			new:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\nauth = { type = \"oauth\", validate_issuer = false }\n",
			want: config.Changes{Changed: []string{"s"}},
		},
		{
			name: "authorize_params changed",
			old:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\nauth = { type = \"oauth\", authorize_params = { access_type = \"online\" } }\n",
			new:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\nauth = { type = \"oauth\", authorize_params = { access_type = \"offline\" } }\n",
			want: config.Changes{Changed: []string{"s"}},
		},
		{
			name: "grant changed",
			old:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\nauth = { type = \"oauth\", client_id = \"c\", client_secret = \"${secret:s}\" }\n",
			new:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\nauth = { type = \"oauth\", client_id = \"c\", client_secret = \"${secret:s}\", grant = \"client_credentials\", authorization_server = \"https://as.example\" }\n",
			want: config.Changes{Changed: []string{"s"}},
		},
		{
			name: "the default grant and dpop written out are unchanged",
			old:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\nauth = { type = \"oauth\" }\n",
			new:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\nauth = { type = \"oauth\", grant = \"authorization_code\", dpop = \"auto\" }\n",
			want: config.Changes{Unchanged: []string{"s"}},
		},
		{
			name: "signing key changed",
			old:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\nauth = { type = \"oauth\", client_id = \"c\", signing_key = { key = \"${secret:a}\" } }\n",
			new:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\nauth = { type = \"oauth\", client_id = \"c\", signing_key = { key = \"${secret:b}\" } }\n",
			want: config.Changes{Changed: []string{"s"}},
		},
		{
			name: "assertion source changed",
			old: "[connectors.s]\nurl = \"https://s.example.com/mcp\"\n" +
				"auth = { type = \"oauth\", grant = \"jwt_bearer\", authorization_server = \"https://as.example\", client_id = \"c\", assertion = { file = \"/t\" } }\n",
			new: "[connectors.s]\nurl = \"https://s.example.com/mcp\"\n" +
				"auth = { type = \"oauth\", grant = \"jwt_bearer\", authorization_server = \"https://as.example\", client_id = \"c\", assertion = { value = \"${env:T}\" } }\n",
			want: config.Changes{Changed: []string{"s"}},
		},
		{
			name: "tls added",
			old:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\n",
			new:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\ntls = { ca_file = \"/ca.pem\" }\n",
			want: config.Changes{Changed: []string{"s"}},
		},
		{
			name: "tls key changed",
			old:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\ntls = { cert_file = \"/c.pem\", key_file = \"/c.key\" }\n",
			new:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\ntls = { cert_file = \"/c.pem\", key = \"${secret:k}\" }\n",
			want: config.Changes{Changed: []string{"s"}},
		},
		{
			name: "tls written as a table or inline is unchanged",
			old:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\ntls = { ca_file = \"/ca.pem\" }\n",
			new:  "[connectors.s]\nurl = \"https://s.example.com/mcp\"\n[connectors.s.tls]\nca_file = \"/ca.pem\"\n",
			want: config.Changes{Unchanged: []string{"s"}},
		},
		{
			name: "timeout changed",
			old:  "[connectors.fs]\ncommand = \"npx\"\n",
			new:  "[connectors.fs]\ncommand = \"npx\"\ntimeout = \"5s\"\n",
			want: config.Changes{Changed: []string{"fs"}},
		},
		{
			name: "args changed",
			old:  "[connectors.fs]\ncommand = \"npx\"\nargs = [\"a\"]\n",
			new:  "[connectors.fs]\ncommand = \"npx\"\nargs = [\"b\"]\n",
			want: config.Changes{Changed: []string{"fs"}},
		},
		{
			name: "explicit default timeout is unchanged",
			old:  "[connectors.fs]\ncommand = \"npx\"\n",
			new:  "[connectors.fs]\ncommand = \"npx\"\ntimeout = \"120s\"\n",
			want: config.Changes{Unchanged: []string{"fs"}},
		},
		{
			name: "empty tables equal absent ones",
			old:  "[connectors.fs]\ncommand = \"npx\"\n",
			new:  "[connectors.fs]\ncommand = \"npx\"\nargs = []\nenv = {}\n",
			want: config.Changes{Unchanged: []string{"fs"}},
		},
		{
			name: "stdio to url is Changed, not URLChanged",
			old:  "[connectors.x]\ncommand = \"npx\"\n",
			new:  "[connectors.x]\nurl = \"https://x.example.com/mcp\"\n",
			want: config.Changes{Changed: []string{"x"}},
		},
		{
			name: "url to stdio is Changed, not URLChanged",
			old:  "[connectors.x]\nurl = \"https://x.example.com/mcp\"\n",
			new:  "[connectors.x]\ncommand = \"npx\"\n",
			want: config.Changes{Changed: []string{"x"}},
		},
		{
			name: "embedding model changed",
			old:  diffBase,
			new:  replace(diffBase, `model = "qwen3-embedding"`, `model = "nomic-embed-text"`),
			want: config.Changes{EmbeddingChanged: true, Unchanged: []string{"exa", "fs", "linear"}},
		},
		{
			name: "embedding timeout changed",
			old:  diffBase,
			new:  replace(diffBase, "dimensions = 1024", "dimensions = 1024\ntimeout = \"5s\""),
			want: config.Changes{EmbeddingChanged: true, Unchanged: []string{"exa", "fs", "linear"}},
		},
		{
			name: "embedding removed",
			old:  "[embedding]\nbase_url = \"http://localhost:11434/v1\"\nmodel = \"m\"\ndimensions = 1\n",
			new:  "",
			want: config.Changes{EmbeddingChanged: true},
		},
		{
			name: "embedding added",
			old:  "",
			new:  "[embedding]\nbase_url = \"http://localhost:11434/v1\"\nmodel = \"m\"\ndimensions = 1\n",
			want: config.Changes{EmbeddingChanged: true},
		},
		{
			name: "listen changed requires restart",
			old:  "",
			new:  "[server]\nlisten = \"127.0.0.1:7000\"\n",
			want: config.Changes{RestartRequired: []string{"server.listen"}},
		},
		{
			name: "storage changed requires restart",
			old:  "",
			new:  "[storage]\ndriver = \"postgres\"\ndsn = \"postgres://localhost/wayfindr\"\n",
			want: config.Changes{RestartRequired: []string{"storage.driver", "storage.dsn"}},
		},
		{
			name: "refresh_interval alone changes nothing listed",
			old:  "",
			new:  "[index]\nrefresh_interval = \"5m\"\n",
			want: config.Changes{},
		},
		{
			name: "several at once, names sorted",
			old: "[connectors.b]\ncommand = \"x\"\n[connectors.a]\ncommand = \"x\"\n" +
				"[connectors.d]\ncommand = \"x\"\n[connectors.c]\ncommand = \"x\"\n",
			new: "[connectors.z]\ncommand = \"x\"\n[connectors.y]\ncommand = \"x\"\n" +
				"[connectors.d]\ncommand = \"y\"\n[connectors.c]\ncommand = \"y\"\n",
			want: config.Changes{
				Added:   []string{"y", "z"},
				Removed: []string{"a", "b"},
				Changed: []string{"c", "d"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := config.Diff(mustParse(t, tt.old), mustParse(t, tt.new))

			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Diff (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDiffFromNil is the startup case: with no previous config every
// connector is Added and nothing needs a restart.
func TestDiffFromNil(t *testing.T) {
	got := config.Diff(nil, mustParse(t, diffBase))
	want := config.Changes{Added: []string{"exa", "fs", "linear"}, EmbeddingChanged: true}

	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Diff (-want +got):\n%s", diff)
	}
}

// replace swaps the one occurrence of from in s, so a typo in a case can't
// silently leave the config unchanged.
func replace(s, from, to string) string {
	if strings.Count(s, from) != 1 {
		panic("replace: want exactly one " + from)
	}

	return strings.Replace(s, from, to, 1)
}
