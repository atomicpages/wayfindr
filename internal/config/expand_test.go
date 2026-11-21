package config_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
)

// TestParseExpandsLeadingTilde covers the ~ rule of design §4: a leading
// "~" alone or "~/…" becomes the home directory in a sqlite storage.dsn and
// in a stdio connector's command and args; "~user/…", a ~ anywhere but the
// start, a postgres DSN, and ${…} templates are left as written.
func TestParseExpandsLeadingTilde(t *testing.T) {
	dsn := func(c *config.Config) any { return c.Storage.DSN }
	command := func(c *config.Config) any { return c.Connectors["x"].Command }
	args := func(c *config.Config) any { return c.Connectors["x"].Args }
	tls := func(c *config.Config) any { return c.Connectors["x"].TLS }
	assertion := func(c *config.Config) any { return c.Connectors["x"].Auth.Assertion }

	tests := []struct {
		name string
		toml string
		get  func(*config.Config) any
		want any
	}{
		{"sqlite dsn under home", `
[storage]
driver = "sqlite"
dsn = "~/x.db"`, dsn, "/home/tester/x.db"},
		{"dsn with the driver defaulted to sqlite", `
[storage]
dsn = "~/data/x.db"`, dsn, "/home/tester/data/x.db"},
		{"sqlite dsn of ~ alone", `
[storage]
dsn = "~"`, dsn, "/home/tester"},
		{"sqlite dsn naming another user's home stays literal", `
[storage]
dsn = "~bob/x.db"`, dsn, "~bob/x.db"},
		{"postgres dsn is a URL and stays untouched", `
[storage]
driver = "postgres"
dsn = "~/not-a-path"`, dsn, "~/not-a-path"},
		{"command under home", `
[connectors.x]
command = "~/bin/srv"`, command, "/home/tester/bin/srv"},
		{"command of ~ alone", `
[connectors.x]
command = "~"`, command, "/home/tester"},
		{"command naming another user's home stays literal", `
[connectors.x]
command = "~bob/bin/srv"`, command, "~bob/bin/srv"},
		{"each arg expanded on its own", `
[connectors.x]
command = "srv"
args = ["-y", "~/code", "~", "~bob/code", "--dir=~/code", "${env:ROOT}/code", "~/${env:SUB}"]`,
			args, []config.Template{
				"-y", "/home/tester/code", "/home/tester", "~bob/code",
				"--dir=~/code", "${env:ROOT}/code", "/home/tester/${env:SUB}",
			}},
		{"tls files under home; key is a template, not a path", `
[connectors.x]
url = "https://x.example.com/mcp"
tls = { cert_file = "~/corp/c.pem", key = "~/${secret:k}", ca_file = "~bob/ca.pem" }`,
			tls, &config.TLS{CertFile: "/home/tester/corp/c.pem", Key: "~/${secret:k}", CAFile: "~bob/ca.pem"}},
		{"tls key_file under home", `
[connectors.x]
url = "https://x.example.com/mcp"
tls = { cert_file = "/c.pem", key_file = "~/c.key" }`,
			tls, &config.TLS{CertFile: "/c.pem", KeyFile: "/home/tester/c.key"}},
		{"assertion file under home", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", grant = "jwt_bearer", authorization_server = "https://as.example", client_id = "c", assertion = { file = "~/tokens/mcp" } }`,
			assertion, &config.Assertion{File: "/home/tester/tokens/mcp"}},
		{"assertion value is a template, not a path", `
[connectors.x]
url = "https://x.example.com/mcp"
auth = { type = "oauth", grant = "jwt_bearer", authorization_server = "https://as.example", client_id = "c", assertion = { value = "~/${env:T}" } }`,
			assertion, &config.Assertion{Value: "~/${env:T}"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := config.Parse([]byte(tt.toml), options())

			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if diff := cmp.Diff(tt.want, tt.get(c)); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

// TestParseTildeWithoutHomeDir: a value that needs the home directory when
// none can be resolved is a validation issue at that value's key.
func TestParseTildeWithoutHomeDir(t *testing.T) {
	const body = `
[storage]
dsn = "~/x.db"

[connectors.x]
command = "~/bin/srv"
args = ["-y", "~/code"]

[connectors.y]
url = "https://y.example.com/mcp"
tls = { cert_file = "~/c.pem", key_file = "~/c.key", ca_file = "~/ca.pem" }`

	for name, home := range map[string]func() (string, error){
		"lookup fails": func() (string, error) { return "", errors.New("no $HOME") },
		"none given":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.Parse([]byte(body), config.Options{DataDir: dataDir, HomeDir: home})
			var verr *config.ValidationError

			if !errors.As(err, &verr) {
				t.Fatalf("Parse error = %v, want *config.ValidationError", err)
			}

			var got []string

			for _, is := range verr.Issues {
				got = append(got, is.Path)
			}

			want := []string{
				"storage.dsn", "connectors.x.command", "connectors.x.args[1]",
				"connectors.y.tls.cert_file", "connectors.y.tls.key_file", "connectors.y.tls.ca_file",
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("issue paths (-want +got):\n%s\nerror: %v", diff, err)
			}
		})
	}
}

// TestParseWithoutTildeNeedsNoHomeDir: the home directory is looked up only
// when a value needs expanding, so a config without ~ loads without one.
func TestParseWithoutTildeNeedsNoHomeDir(t *testing.T) {
	body := `
[storage]
dsn = "/var/lib/wayfindr.db"

[connectors.x]
command = "srv"
args = ["--dir=~/code", "~bob"]`
	calls := 0
	opts := config.Options{DataDir: dataDir, HomeDir: func() (string, error) {
		calls++

		return "", errors.New("no $HOME")
	}}

	if _, err := config.Parse([]byte(body), opts); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if calls != 0 {
		t.Errorf("HomeDir called %d times, want 0", calls)
	}
}
