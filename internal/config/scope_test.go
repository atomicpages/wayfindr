package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
)

func TestParseScope(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want *config.Scope
		// issues are the ValidationError paths; nil means valid.
		issues []string
		// decodeErr means the TOML doesn't decode into a scope at all.
		decodeErr bool
	}{
		{name: "empty file is no narrowing", toml: "", want: &config.Scope{}},
		{
			name: "allowlist and tools",
			toml: "connectors = [\"github\", \"linear\"]\n[tools]\ngithub = [\"@read\", \"create_pull_request\"]\n",
			want: &config.Scope{
				Connectors: []string{"github", "linear"},
				Tools:      map[string]config.ToolPolicy{"github": {"@read", "create_pull_request"}},
			},
		},
		{name: "empty allowlist allows none", toml: "connectors = []\n", want: &config.Scope{Connectors: []string{}}},
		{name: "tools without allowlist", toml: "[tools]\ngmail = [\"!send_*\"]\n", want: &config.Scope{Tools: map[string]config.ToolPolicy{"gmail": {"!send_*"}}}},
		{name: "connector settings are refused", toml: "[connectors.evil]\ncommand = \"sh\"\n", decodeErr: true},
		{name: "unknown top-level key", toml: "url = \"http://x\"\n", issues: []string{"url"}},
		{name: "unknown table", toml: "[auth]\ntype = \"oauth\"\n", issues: []string{"auth", "auth.type"}},
		{name: "bad connector name", toml: "connectors = [\"Git Hub\"]\n", issues: []string{"connectors[0]"}},
		{name: "tools for a connector left out", toml: "connectors = [\"linear\"]\n[tools]\ngithub = [\"@read\"]\n", issues: []string{"tools.github"}},
		{name: "bad tools entry", toml: "[tools]\ngithub = [\"@admin\", \"[\"]\n", issues: []string{"tools.github[0]", "tools.github[1]"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.ParseScope([]byte(tt.toml))

			if tt.decodeErr {
				if err == nil {
					t.Fatalf("ParseScope = %+v, want an error", got)
				}

				return
			}

			if tt.issues != nil {
				var ve *config.ValidationError

				if !errors.As(err, &ve) {
					t.Fatalf("err = %v, want *ValidationError", err)
				}

				var paths []string

				for _, is := range ve.Issues {
					paths = append(paths, is.Path)
				}

				if diff := cmp.Diff(tt.issues, paths); diff != "" {
					t.Errorf("issue paths (-want +got):\n%s", diff)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseScope: %v", err)
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("scope (-want +got):\n%s", diff)
			}
		})
	}
}

// TestScopeNarrows checks the scope's own answer; the gateway ANDs it with
// the config's policy.
func TestScopeNarrows(t *testing.T) {
	s := &config.Scope{
		Connectors: []string{"github", "linear"},
		Tools:      map[string]config.ToolPolicy{"github": {"@read", "create_pull_request"}},
	}
	tests := []struct {
		name            string
		scope           *config.Scope
		connector, tool string
		class           config.ToolClass
		allows, exposes bool
		hidesName       bool
	}{
		{"nil scope exposes all", nil, "slack", "post", config.ClassDestructive, true, true, false},
		{"connector left out", s, "slack", "search", config.ClassRead, false, false, true},
		{"allowed connector without tools list", s, "linear", "delete_issue", config.ClassDestructive, true, true, false},
		{"read tool under @read", s, "github", "get_issue", config.ClassRead, true, true, false},
		{"named write", s, "github", "create_pull_request", config.ClassDestructive, true, true, false},
		{"unnamed write", s, "github", "delete_repository", config.ClassDestructive, true, false, false},
		{"empty allowlist", &config.Scope{Connectors: []string{}}, "github", "get_issue", config.ClassRead, false, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.scope.Allows(tt.connector); got != tt.allows {
				t.Errorf("Allows = %v, want %v", got, tt.allows)
			}

			if got := tt.scope.Exposes(tt.connector, tt.tool, tt.class); got != tt.exposes {
				t.Errorf("Exposes = %v, want %v", got, tt.exposes)
			}

			if got := tt.scope.HidesName(tt.connector, tt.tool); got != tt.hidesName {
				t.Errorf("HidesName = %v, want %v", got, tt.hidesName)
			}
		})
	}
}

func TestLoadScope(t *testing.T) {
	write := func(t *testing.T, dir, body string) {
		t.Helper()

		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dir, config.ScopeFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("nearest wins", func(t *testing.T) {
		home := t.TempDir()
		repo := filepath.Join(home, "src", "repo")
		sub := filepath.Join(repo, "pkg", "a")
		write(t, repo, "connectors = [\"github\"]\n")
		write(t, filepath.Join(repo, "pkg"), "connectors = [\"linear\"]\n")

		if err := os.MkdirAll(sub, 0o750); err != nil {
			t.Fatal(err)
		}

		s, file, err := config.LoadScope(sub, home)

		if err != nil {
			t.Fatal(err)
		}

		if want := filepath.Join(repo, "pkg", config.ScopeFile); file != want {
			t.Errorf("file = %q, want %q", file, want)
		}

		if diff := cmp.Diff([]string{"linear"}, s.Connectors); diff != "" {
			t.Errorf("connectors (-want +got):\n%s", diff)
		}
	})

	t.Run("home file never applies", func(t *testing.T) {
		home := t.TempDir()
		write(t, home, "connectors = []\n")
		dir := filepath.Join(home, "src")

		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}

		for _, d := range []string{dir, home} {
			s, file, err := config.LoadScope(d, home)

			if err != nil || s != nil || file != "" {
				t.Errorf("LoadScope(%s) = %v, %q, %v; want no scope", d, s, file, err)
			}
		}
	})

	t.Run("invalid file is an error", func(t *testing.T) {
		home := t.TempDir()
		repo := filepath.Join(home, "repo")
		write(t, repo, "[connectors.evil]\ncommand = \"sh\"\n")

		if _, _, err := config.LoadScope(repo, home); err == nil {
			t.Error("LoadScope succeeded, want an error")
		}
	})
}

func TestScopeHeader(t *testing.T) {
	tests := []struct {
		name  string
		scope *config.Scope
	}{
		{"allowlist and tools", &config.Scope{
			Connectors: []string{"github"},
			Tools:      map[string]config.ToolPolicy{"github": {"@read", "créer_*", "😀"}},
		}},
		{"empty allowlist survives", &config.Scope{Connectors: []string{}}},
		{"no allowlist", &config.Scope{Tools: map[string]config.ToolPolicy{"gmail": {"!send_*"}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := tt.scope.Header()

			if err != nil {
				t.Fatal(err)
			}

			for i := 0; i < len(h); i++ {
				if h[i] < 0x20 || h[i] >= 0x7f {
					t.Fatalf("header %q has byte %#x at %d", h, h[i], i)
				}
			}

			got, err := config.ParseScopeHeader(h)

			if err != nil {
				t.Fatalf("ParseScopeHeader(%q): %v", h, err)
			}

			if diff := cmp.Diff(tt.scope, got); diff != "" {
				t.Errorf("round trip (-want +got):\n%s", diff)
			}
		})
	}

	if s, err := config.ParseScopeHeader(""); s != nil || err != nil {
		t.Errorf("ParseScopeHeader(\"\") = %v, %v; want nil, nil", s, err)
	}

	for _, bad := range []string{
		"{",
		`{"connectors":["github"],"extra":1}`,
		`{"connectors":["Git Hub"]}`,
		`{"connectors":[]} {}`,
	} {
		if _, err := config.ParseScopeHeader(bad); err == nil {
			t.Errorf("ParseScopeHeader(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseProfiles(t *testing.T) {
	const connectors = "[connectors.github]\nurl = \"https://example.com/mcp\"\n[connectors.linear]\nurl = \"https://example.org/mcp\"\n"

	tests := []struct {
		name string
		toml string
		want map[string]*config.Scope
		// issues are the ValidationError paths; nil means valid.
		issues []string
	}{
		{name: "no profiles", toml: "", want: nil},
		{
			name: "allowlist and tools",
			toml: "[profiles.acme]\nconnectors = [\"github\", \"linear\"]\n[profiles.acme.tools]\ngithub = [\"@read\"]\n",
			want: map[string]*config.Scope{"acme": {
				Connectors: []string{"github", "linear"},
				Tools:      map[string]config.ToolPolicy{"github": {"@read"}},
			}},
		},
		{name: "empty profile narrows nothing", toml: "[profiles.all]\n", want: map[string]*config.Scope{"all": {}}},
		{name: "empty allowlist allows none", toml: "[profiles.none]\nconnectors = []\n", want: map[string]*config.Scope{"none": {Connectors: []string{}}}},
		{name: "unknown key", toml: "[profiles.acme]\nurl = \"http://x\"\n", issues: []string{"profiles.acme.url"}},
		{name: "bad profile name", toml: "[profiles.\"Acme Co\"]\n", issues: []string{`profiles."Acme Co"`}},
		{name: "unconfigured connector", toml: "[profiles.acme]\nconnectors = [\"gmail\"]\n", issues: []string{"profiles.acme"}},
		{name: "tools for an unconfigured connector", toml: "[profiles.acme.tools]\ngmail = [\"@read\"]\n", issues: []string{"profiles.acme"}},
		{name: "tools for a connector left out", toml: "[profiles.acme]\nconnectors = [\"linear\"]\n[profiles.acme.tools]\ngithub = [\"@read\"]\n", issues: []string{"profiles.acme.tools.github"}},
		{name: "bad entries", toml: "[profiles.acme]\nconnectors = [\"github\", \"Git Hub\"]\n[profiles.acme.tools]\ngithub = [\"@admin\"]\n", issues: []string{"profiles.acme.connectors[1]", "profiles.acme.tools.github[0]"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.Parse([]byte(connectors+tt.toml), options())

			if tt.issues != nil {
				var ve *config.ValidationError

				if !errors.As(err, &ve) {
					t.Fatalf("err = %v, want *ValidationError", err)
				}

				paths := make([]string, len(ve.Issues))

				for i, is := range ve.Issues {
					paths[i] = is.Path
				}

				if diff := cmp.Diff(tt.issues, paths); diff != "" {
					t.Errorf("issue paths (-want +got):\n%s", diff)
				}

				return
			}

			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if diff := cmp.Diff(tt.want, got.Profiles); diff != "" {
				t.Errorf("Profiles (-want +got):\n%s", diff)
			}
		})
	}
}
