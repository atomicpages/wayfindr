package config_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
)

const (
	dataDir = "/data/wayfindr"
	homeDir = "/home/tester"
)

// options is what most tests load with: a fixed data and home directory.
func options() config.Options {
	return config.Options{
		DataDir: dataDir,
		HomeDir: func() (string, error) { return homeDir, nil },
	}
}

func dur(d time.Duration) config.Duration { return config.Duration{Duration: d} }

// TestLoadPRDExample loads the configuration example from prd.md
// ("Configuration (ADR 0006)") and checks every value plus the defaults that
// design §4 and prd.md fill in for keys the example leaves out. Its ~ paths
// come back expanded against the home directory (design §4).
func TestLoadPRDExample(t *testing.T) {
	got, err := config.Load(context.Background(), filepath.Join("testdata", "prd_configuration.toml"), options())

	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := &config.Config{
		Server:  config.Server{Listen: "127.0.0.1:7777", OAuth: config.OAuth{Clients: []string{config.DefaultOAuthClient}}},
		Storage: config.Storage{Driver: "sqlite", DSN: "/home/tester/.local/share/wayfindr/wayfindr.db"},
		Embedding: &config.Embedding{
			BaseURL:    "http://localhost:11434/v1",
			Model:      "qwen3-embedding",
			Dimensions: 1024,
			Timeout:    dur(2 * time.Second),
		},
		Index: config.Index{RefreshInterval: dur(time.Hour)},
		Calls: config.Calls{Retention: dur(720 * time.Hour)},
		Connectors: config.Connectors{
			"linear": {
				Name:    "linear",
				URL:     "https://mcp.linear.app/mcp",
				Timeout: dur(120 * time.Second),
				Auth: &config.Auth{
					Type: "oauth", PKCE: true, ValidateIssuer: true, ResourceIndicator: true,
					Grant: config.GrantAuthorizationCode, DPoP: config.DPoPAuto,
				},
			},
			"slack": {
				Name:    "slack",
				URL:     "https://mcp.slack.com/mcp",
				Timeout: dur(120 * time.Second),
				Auth: &config.Auth{
					Type:              "oauth",
					ClientID:          "1234.5678",
					ClientSecret:      "${secret:slack_client_secret}",
					Scopes:            []string{"channels:read", "chat:write"},
					PKCE:              true,
					ValidateIssuer:    true,
					ResourceIndicator: true,
					Grant:             config.GrantAuthorizationCode,
					DPoP:              config.DPoPAuto,
				},
			},
			"exa": {
				Name:    "exa",
				URL:     "https://mcp.exa.ai/mcp",
				Headers: map[string]config.Template{"x-api-key": "${env:EXA_API_KEY}"},
				Timeout: dur(120 * time.Second),
			},
			"deepwiki": {
				Name:    "deepwiki",
				URL:     "https://mcp.deepwiki.com/mcp",
				Timeout: dur(120 * time.Second),
			},
			"fs": {
				Name:    "fs",
				Command: "npx",
				Args:    []config.Template{"-y", "@modelcontextprotocol/server-filesystem", "/home/tester/code"},
				Env:     map[string]config.Template{"LOG_LEVEL": "warn"},
				Timeout: dur(120 * time.Second),
			},
		},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Load mismatch (-want +got):\n%s", diff)
	}
}

// TestParseDefaults checks the defaults for an empty file: design §3/§4
// (listen, sqlite at <data>/wayfindr.db, no embedding) and prd.md
// (refresh_interval 1h), and the call log's 30-day retention.
func TestParseDefaults(t *testing.T) {
	got, err := config.Parse(nil, options())

	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := &config.Config{
		Server:     config.Server{Listen: "127.0.0.1:7777", OAuth: config.OAuth{Clients: []string{config.DefaultOAuthClient}}},
		Storage:    config.Storage{Driver: "sqlite", DSN: filepath.Join(dataDir, "wayfindr.db")},
		Index:      config.Index{RefreshInterval: dur(time.Hour)},
		Calls:      config.Calls{Retention: dur(720 * time.Hour)},
		Connectors: config.Connectors{},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Parse mismatch (-want +got):\n%s", diff)
	}
}

// TestParseZeroRetentionTurnsTheCallLogOff checks that "0s" is kept, not
// replaced by the default.
func TestParseZeroRetentionTurnsTheCallLogOff(t *testing.T) {
	got, err := config.Parse([]byte("[calls]\nretention = \"0s\"\n"), options())

	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got.Calls.Retention.Duration != 0 {
		t.Errorf("calls.retention = %v, want 0", got.Calls.Retention.Duration)
	}
}

// TestParseTelemetry: the usage report is off unless the file turns it on
// (ADR 0020), and a misspelt key under [telemetry] is an error, not a
// silent off.
func TestParseTelemetry(t *testing.T) {
	for _, tt := range []struct {
		name string
		toml string
		want bool
	}{
		{"absent", "", false},
		{"on", "[telemetry]\nenabled = true\n", true},
		{"off", "[telemetry]\nenabled = false\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.Parse([]byte(tt.toml), options())

			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if got.Telemetry.Enabled != tt.want {
				t.Errorf("telemetry.enabled = %t, want %t", got.Telemetry.Enabled, tt.want)
			}
		})
	}

	_, err := config.Parse([]byte("[telemetry]\nenable = true\n"), options())
	var verr *config.ValidationError

	if !errors.As(err, &verr) {
		t.Fatalf("Parse error = %v, want *config.ValidationError", err)
	}

	want := []config.Issue{{Path: "telemetry.enable", Message: "unknown key"}}

	if diff := cmp.Diff(want, verr.Issues); diff != "" {
		t.Errorf("issues (-want +got):\n%s", diff)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := config.Load(context.Background(), filepath.Join(t.TempDir(), "nope.toml"), options())

	if err == nil {
		t.Fatal("Load of a missing file succeeded")
	}

	var verr *config.ValidationError

	if errors.As(err, &verr) {
		t.Errorf("missing file reported as a validation error: %v", err)
	}
}

func TestConnectorUsesOAuth(t *testing.T) {
	tests := []struct {
		name string
		conn config.Connector
		want bool
	}{
		{"oauth auth table", config.Connector{URL: "https://mcp.example.com", Auth: &config.Auth{Type: "oauth"}}, true},
		{"no auth table", config.Connector{URL: "https://mcp.example.com"}, false},
		{"api key in headers", config.Connector{
			URL:     "https://mcp.example.com",
			Headers: map[string]config.Template{"Authorization": "Bearer ${secret:k}"},
		}, false},
		// Validation rejects any other type; a connector that skipped it is
		// still not an OAuth connector.
		{"auth table of another type", config.Connector{URL: "https://mcp.example.com", Auth: &config.Auth{Type: "basic"}}, false},
		{"stdio", config.Connector{Command: "npx"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.conn.UsesOAuth(); got != tt.want {
				t.Errorf("UsesOAuth() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestConnectorSecrets: a connector's secrets are the ${secret:…}
// references in any of its templates, sorted, each once.
func TestConnectorSecrets(t *testing.T) {
	c := config.Connector{
		URL:     "https://x.test/${secret:x_path}/mcp",
		Headers: map[string]config.Template{"Authorization": "Bearer ${secret:x_token}", "X-Env": "${env:HOME}"},
		Args:    []config.Template{"--key=${secret:x_token}"},
		Env:     map[string]config.Template{"A": "${secret:x_a}"},
		Auth: &config.Auth{ //nolint:gosec // G101: references, not credentials
			Type: config.AuthTypeOAuth, ClientSecret: "${secret:x_client}",
			SigningKey: &config.SigningKey{Key: "${secret:x_signing_key}"},
			Assertion:  &config.Assertion{Value: "${secret:x_assertion}${env:T}"},
		},
		TLS: &config.TLS{CertFile: "/c.pem", Key: "${secret:x_tls_key}"},
	}
	want := []string{"x_a", "x_assertion", "x_client", "x_path", "x_signing_key", "x_tls_key", "x_token"}

	if diff := cmp.Diff(want, c.Secrets()); diff != "" {
		t.Errorf("Secrets (-want +got):\n%s", diff)
	}

	if got := (config.Connector{Command: "ls"}).Secrets(); len(got) != 0 {
		t.Errorf("Secrets of a connector without references = %v, want none", got)
	}
}

// TestEmbeddingSecrets: [embedding]'s secrets are the ${secret:…}
// references in its api_key, sorted, each once.
func TestEmbeddingSecrets(t *testing.T) {
	e := config.Embedding{APIKey: "${secret:k_b}${env:HOME}${secret:k_a}${secret:k_b}"} //nolint:gosec // G101: references, not a credential
	want := []string{"k_a", "k_b"}

	if diff := cmp.Diff(want, e.Secrets()); diff != "" {
		t.Errorf("Secrets (-want +got):\n%s", diff)
	}

	if got := (config.Embedding{APIKey: "sk-literal"}).Secrets(); len(got) != 0 {
		t.Errorf("Secrets of a literal api_key = %v, want none", got)
	}
}
