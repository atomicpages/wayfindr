package config_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
)

// mapResolver resolves from a fixed map keyed by the reference as written,
// and counts lookups.
type mapResolver struct {
	vals  map[string]string
	calls int
}

func (m *mapResolver) Resolve(_ context.Context, ref config.Ref) (string, error) {
	m.calls++
	v, ok := m.vals[ref.String()]

	if !ok {
		return "", fmt.Errorf("lookup %s: %w", ref.Name, config.ErrNotFound)
	}

	return v, nil
}

func TestTemplateRefs(t *testing.T) {
	tests := []struct {
		in   config.Template
		want []config.Ref
	}{
		{"plain text", nil},
		{"", nil},
		{"costs $5", nil},
		{"${env:EXA_API_KEY}", []config.Ref{{Kind: config.RefEnv, Name: "EXA_API_KEY"}}},
		{"Bearer ${secret:slack_client_secret}", []config.Ref{{Kind: config.RefSecret, Name: "slack_client_secret"}}},
		// A reference may still use the older, wider secret-name rule.
		{"${secret:GitHub.Token}", []config.Ref{{Kind: config.RefSecret, Name: "GitHub.Token"}}},
		{"https://${env:HOST}:${env:PORT}/mcp", []config.Ref{
			{Kind: config.RefEnv, Name: "HOST"},
			{Kind: config.RefEnv, Name: "PORT"},
		}},
	}

	for _, tt := range tests {
		t.Run(string(tt.in), func(t *testing.T) {
			got, err := tt.in.Refs()

			if err != nil {
				t.Fatalf("Refs: %v", err)
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Refs (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTemplateRefsMalformed(t *testing.T) {
	for _, in := range []config.Template{
		"${env:HOME",
		"${HOME}",
		"${env:}",
		"${vault:x}",
		"${env:1X}",
		"${secret:-x}",
		"ok ${env:A} then ${",
	} {
		t.Run(string(in), func(t *testing.T) {
			_, err := in.Refs()
			var terr *config.TemplateError

			if !errors.As(err, &terr) {
				t.Fatalf("Refs error = %v, want *config.TemplateError", err)
			}
		})
	}
}

func TestTemplateResolve(t *testing.T) {
	r := &mapResolver{vals: map[string]string{
		"${env:HOST}":         "mcp.example.com",
		"${secret:token}":     "s3cr3t",
		"${env:LOOKS_NESTED}": "${env:HOST}",
	}}
	tests := []struct {
		in, want string
	}{
		{"no refs, $ kept", "no refs, $ kept"},
		{"https://${env:HOST}/mcp", "https://mcp.example.com/mcp"},
		{"Bearer ${secret:token}", "Bearer s3cr3t"},
		// Values are inserted verbatim, never expanded again.
		{"${env:LOOKS_NESTED}", "${env:HOST}"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := config.Template(tt.in).Resolve(context.Background(), r)

			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			if got != tt.want {
				t.Errorf("Resolve = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestTemplateResolveMissing checks that a missing reference is named, so
// the connector's auth_error reason can show it (design §4).
func TestTemplateResolveMissing(t *testing.T) {
	r := &mapResolver{}
	_, err := config.Template("x-${secret:slack_client_secret}").Resolve(context.Background(), r)
	var rerr *config.ResolveError

	if !errors.As(err, &rerr) {
		t.Fatalf("Resolve error = %v, want *config.ResolveError", err)
	}

	if want := (config.Ref{Kind: config.RefSecret, Name: "slack_client_secret"}); rerr.Ref != want {
		t.Errorf("ResolveError.Ref = %v, want %v", rerr.Ref, want)
	}

	if !errors.Is(err, config.ErrNotFound) {
		t.Errorf("Resolve error %v does not wrap ErrNotFound", err)
	}
}

func TestTemplateResolveMalformedSkipsResolver(t *testing.T) {
	r := &mapResolver{vals: map[string]string{"${env:A}": "a"}}
	_, err := config.Template("${env:A}${bad}").Resolve(context.Background(), r)
	var terr *config.TemplateError

	if !errors.As(err, &terr) {
		t.Fatalf("Resolve error = %v, want *config.TemplateError", err)
	}

	if r.calls != 0 {
		t.Errorf("resolver called %d times for a malformed template, want 0", r.calls)
	}
}

// TestTemplatesRoundTripUnresolved loads the prd.md example, encodes it back
// to TOML, and parses it again: the ${…} references must survive unresolved
// and the config must be unchanged.
func TestTemplatesRoundTripUnresolved(t *testing.T) {
	first, err := config.Load(context.Background(), filepath.Join("testdata", "prd_configuration.toml"), options())

	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var buf bytes.Buffer

	if err := toml.NewEncoder(&buf).Encode(first); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	for _, ref := range []string{"${secret:slack_client_secret}", "${env:EXA_API_KEY}"} {
		if !bytes.Contains(buf.Bytes(), []byte(ref)) {
			t.Errorf("encoded config lacks %s:\n%s", ref, buf.String())
		}
	}

	second, err := config.Parse(buf.Bytes(), options())

	if err != nil {
		t.Fatalf("Parse re-encoded config: %v\n%s", err, buf.String())
	}

	if diff := cmp.Diff(first, second); diff != "" {
		t.Errorf("round trip changed the config (-first +second):\n%s", diff)
	}
}

// TestSecretNameFor: the secret `connector add` stores a key's value under
// is <connector>_<key>, lowercased, with runes a secret name can't hold
// replaced by _, cut to 63 bytes, and is always a valid new secret name.
func TestSecretNameFor(t *testing.T) {
	long := strings.Repeat("a", 60)

	for _, tt := range []struct{ connector, key, want string }{
		{"gh", "Authorization", "gh_authorization"},
		{"gh", "X-API-Key", "gh_x-api-key"},
		{"gh", "API KEY!", "gh_api_key_"},
		{"my.server", "TOKEN", "my_server_token"},
		{long, "Authorization", long + "_au"},
		{strings.Repeat("b", 63), "TOKEN", strings.Repeat("b", 63)},
	} {
		got := config.SecretNameFor(tt.connector, tt.key)

		if got != tt.want {
			t.Errorf("SecretNameFor(%q, %q) = %q, want %q", tt.connector, tt.key, got, tt.want)
		}

		if !config.IsSecretName(got) {
			t.Errorf("SecretNameFor(%q, %q) = %q, not a valid secret name", tt.connector, tt.key, got)
		}
	}
}

// TestNumberedSecretNameFor: the name taken when SecretNameFor's is in use
// ends in _<n>, and the part before it is cut so the whole stays a valid
// secret name.
func TestNumberedSecretNameFor(t *testing.T) {
	long := strings.Repeat("a", 60)

	for _, tt := range []struct {
		connector, key string
		n              int
		want           string
	}{
		{"gh", "Authorization", 2, "gh_authorization_2"},
		{long, "Authorization", 2, long + "__2"},
		{strings.Repeat("b", 63), "TOKEN", 10, strings.Repeat("b", 60) + "_10"},
	} {
		got := config.NumberedSecretNameFor(tt.connector, tt.key, tt.n)

		if got != tt.want {
			t.Errorf("NumberedSecretNameFor(%q, %q, %d) = %q, want %q", tt.connector, tt.key, tt.n, got, tt.want)
		}

		if !config.IsSecretName(got) {
			t.Errorf("NumberedSecretNameFor(%q, %q, %d) = %q, not a valid secret name", tt.connector, tt.key, tt.n, got)
		}
	}
}

// TestCheckSecretName: a new secret follows the connector-name rule; a
// known one may keep a name of the older rule; no secret has a name
// outside both.
func TestCheckSecretName(t *testing.T) {
	const rule = "[a-z0-9][a-z0-9_-]{0,62}"

	for _, tt := range []struct {
		name    string
		known   bool
		wantErr string
	}{
		{"github_token", false, ""},
		{"github_token", true, ""},
		{strings.Repeat("a", 63), false, ""},
		{"GitHub.Token", true, ""},
		{"GitHub.Token", false, `secret name "GitHub.Token" must match ` + rule},
		{"Token", false, `secret name "Token" must match ` + rule},
		{"a.b", false, `secret name "a.b" must match ` + rule},
		{strings.Repeat("a", 64), false, `secret name "` + strings.Repeat("a", 64) + `" must match ` + rule},
		{"-tok", true, `secret name "-tok" must match ` + rule},
		{"a/b", true, `secret name "a/b" must match ` + rule},
		{"", true, `secret name "" must match ` + rule},
	} {
		t.Run(fmt.Sprintf("%s known=%t", tt.name, tt.known), func(t *testing.T) {
			err := config.CheckSecretName(tt.name, tt.known)
			got := ""

			if err != nil {
				got = err.Error()
			}

			if got != tt.wantErr {
				t.Errorf("CheckSecretName = %q, want %q", got, tt.wantErr)
			}
		})
	}
}

// A config written when secret names followed the wider rule still loads.
func TestConfigWithAnOlderRuleSecretReferenceLoads(t *testing.T) {
	const src = `[connectors.github]
url = "https://github.test/mcp"
headers = { Authorization = "Bearer ${secret:GitHub.Token}" }
`
	c, err := config.Parse([]byte(src), options())

	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if diff := cmp.Diff([]string{"GitHub.Token"}, c.SecretRefs()); diff != "" {
		t.Errorf("SecretRefs (-want +got):\n%s", diff)
	}
}
