package paths_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/paths"
)

// fakeEnv builds an Env from a fixed variable map and home directory, so
// tests never touch the process environment.
func fakeEnv(vars map[string]string, home string) paths.Env {
	return paths.Env{
		Getenv:  func(k string) string { return vars[k] },
		HomeDir: func() (string, error) { return home, nil },
	}
}

func TestResolveDefaults(t *testing.T) {
	// Resolve has no GOOS branch: ~/.config and ~/.local/share apply on every
	// OS. Each case uses that OS's typical home directory.
	tests := []struct {
		name string
		home string
		want paths.Paths
	}{
		{
			name: "linux",
			home: "/home/ada",
			want: paths.Paths{
				ConfigFile: filepath.FromSlash("/home/ada/.config/wayfindr/wayfindr.toml"),
				DataDir:    filepath.FromSlash("/home/ada/.local/share/wayfindr"),
			},
		},
		{
			name: "darwin",
			home: "/Users/ada",
			want: paths.Paths{
				ConfigFile: filepath.FromSlash("/Users/ada/.config/wayfindr/wayfindr.toml"),
				DataDir:    filepath.FromSlash("/Users/ada/.local/share/wayfindr"),
			},
		},
		{
			name: "windows",
			home: filepath.FromSlash("C:/Users/ada"),
			want: paths.Paths{
				ConfigFile: filepath.FromSlash("C:/Users/ada/.config/wayfindr/wayfindr.toml"),
				DataDir:    filepath.FromSlash("C:/Users/ada/.local/share/wayfindr"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := paths.Resolve(fakeEnv(nil, tt.home))

			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Resolve mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResolveWayfindrOverrides(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	xdg := map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(root, "xdg-config"),
		"XDG_DATA_HOME":   filepath.Join(root, "xdg-data"),
	}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}

		for k, v := range xdg {
			m[k] = v
		}

		for k, v := range extra {
			m[k] = v
		}

		return m
	}

	tests := []struct {
		name string
		vars map[string]string
		want paths.Paths
	}{
		{
			name: "config override beats XDG",
			vars: with(map[string]string{"WAYFINDR_CONFIG": filepath.Join(root, "etc", "wf.toml")}),
			want: paths.Paths{
				ConfigFile: filepath.Join(root, "etc", "wf.toml"),
				DataDir:    filepath.Join(root, "xdg-data", "wayfindr"),
			},
		},
		{
			name: "data override beats XDG",
			vars: with(map[string]string{"WAYFINDR_DATA_DIR": filepath.Join(root, "var", "wf")}),
			want: paths.Paths{
				ConfigFile: filepath.Join(root, "xdg-config", "wayfindr", "wayfindr.toml"),
				DataDir:    filepath.Join(root, "var", "wf"),
			},
		},
		{
			// Overrides are user-supplied and used as given, unlike XDG values.
			name: "relative overrides kept",
			vars: map[string]string{"WAYFINDR_CONFIG": "wf.toml", "WAYFINDR_DATA_DIR": "data"},
			want: paths.Paths{ConfigFile: "wf.toml", DataDir: "data"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := paths.Resolve(fakeEnv(tt.vars, home))

			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Resolve mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResolveHomeDirError(t *testing.T) {
	errNoHome := errors.New("no home")
	noHome := func(vars map[string]string) paths.Env {
		return paths.Env{
			Getenv:  func(k string) string { return vars[k] },
			HomeDir: func() (string, error) { return "", errNoHome },
		}
	}

	t.Run("needed and missing", func(t *testing.T) {
		_, err := paths.Resolve(noHome(map[string]string{"WAYFINDR_CONFIG": "wf.toml"}))

		if !errors.Is(err, errNoHome) {
			t.Fatalf("Resolve error = %v, want wrapping %v", err, errNoHome)
		}
	})

	t.Run("not needed when both overridden", func(t *testing.T) {
		got, err := paths.Resolve(noHome(map[string]string{
			"WAYFINDR_CONFIG":   "wf.toml",
			"WAYFINDR_DATA_DIR": "data",
		}))

		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}

		want := paths.Paths{ConfigFile: "wf.toml", DataDir: "data"}

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Resolve mismatch (-want +got):\n%s", diff)
		}
	})
}

func TestDataFiles(t *testing.T) {
	p := paths.Paths{DataDir: filepath.FromSlash("/home/ada/.local/share/wayfindr")}
	got := map[string]string{
		"db":           p.DBFile(),
		"client token": p.ClientTokenFile(),
		"key":          p.KeyFile(),
	}
	want := map[string]string{
		"db":           filepath.FromSlash("/home/ada/.local/share/wayfindr/wayfindr.db"),
		"client token": filepath.FromSlash("/home/ada/.local/share/wayfindr/client-token"),
		"key":          filepath.FromSlash("/home/ada/.local/share/wayfindr/master.key"),
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("data files mismatch (-want +got):\n%s", diff)
	}
}

func TestOSReadsProcessEnv(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WAYFINDR_CONFIG", filepath.Join(root, "wf.toml"))
	t.Setenv("WAYFINDR_DATA_DIR", filepath.Join(root, "data"))

	got, err := paths.Resolve(paths.OS())

	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := paths.Paths{
		ConfigFile: filepath.Join(root, "wf.toml"),
		DataDir:    filepath.Join(root, "data"),
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Resolve mismatch (-want +got):\n%s", diff)
	}
}

func TestResolveXDG(t *testing.T) {
	// t.TempDir gives a path that is absolute on every OS.
	root := t.TempDir()
	home := filepath.Join(root, "home")
	defaultConfig := filepath.Join(root, "home", ".config", "wayfindr", "wayfindr.toml")
	defaultData := filepath.Join(root, "home", ".local", "share", "wayfindr")

	tests := []struct {
		name string
		vars map[string]string
		want paths.Paths
	}{
		{
			name: "both set",
			vars: map[string]string{
				"XDG_CONFIG_HOME": filepath.Join(root, "xdg-config"),
				"XDG_DATA_HOME":   filepath.Join(root, "xdg-data"),
			},
			want: paths.Paths{
				ConfigFile: filepath.Join(root, "xdg-config", "wayfindr", "wayfindr.toml"),
				DataDir:    filepath.Join(root, "xdg-data", "wayfindr"),
			},
		},
		{
			name: "config only",
			vars: map[string]string{"XDG_CONFIG_HOME": filepath.Join(root, "xdg-config")},
			want: paths.Paths{
				ConfigFile: filepath.Join(root, "xdg-config", "wayfindr", "wayfindr.toml"),
				DataDir:    defaultData,
			},
		},
		{
			name: "data only",
			vars: map[string]string{"XDG_DATA_HOME": filepath.Join(root, "xdg-data")},
			want: paths.Paths{
				ConfigFile: defaultConfig,
				DataDir:    filepath.Join(root, "xdg-data", "wayfindr"),
			},
		},
		{
			// The XDG spec says relative values are invalid and must be ignored.
			name: "relative values ignored",
			vars: map[string]string{"XDG_CONFIG_HOME": "rel/config", "XDG_DATA_HOME": "rel/data"},
			want: paths.Paths{ConfigFile: defaultConfig, DataDir: defaultData},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := paths.Resolve(fakeEnv(tt.vars, home))

			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Resolve mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
