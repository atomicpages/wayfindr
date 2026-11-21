package clienttoken_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/paths"
)

func env(vars map[string]string) paths.Env {
	return paths.Env{
		Getenv:  func(k string) string { return vars[k] },
		HomeDir: func() (string, error) { return "", errors.New("no home in tests") },
	}
}

func TestRead(t *testing.T) {
	tests := []struct {
		name    string
		vars    map[string]string
		file    *string // nil means the token file is absent
		want    string
		wantErr error
	}{
		{name: "file only", file: ptr("wf_fromfile"), want: "wf_fromfile"},
		{name: "file whitespace trimmed", file: ptr("  wf_fromfile\n"), want: "wf_fromfile"},
		{
			name: "env beats file",
			vars: map[string]string{clienttoken.EnvVar: "wf_fromenv"},
			file: ptr("wf_fromfile"),
			want: "wf_fromenv",
		},
		{
			name: "env without file",
			vars: map[string]string{clienttoken.EnvVar: "wf_fromenv"},
			want: "wf_fromenv",
		},
		{name: "no env and no file", wantErr: fs.ErrNotExist},
		{name: "empty file", file: ptr(" \n"), wantErr: clienttoken.ErrEmpty},
		{
			name:    "whitespace-only env",
			vars:    map[string]string{clienttoken.EnvVar: "  "},
			file:    ptr("wf_fromfile"),
			wantErr: clienttoken.ErrEmpty,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := paths.Paths{DataDir: t.TempDir()}

			if tt.file != nil {
				if err := os.WriteFile(p.ClientTokenFile(), []byte(*tt.file), paths.SecretFileMode); err != nil {
					t.Fatal(err)
				}
			}

			got, err := clienttoken.Read(context.Background(), env(tt.vars), p)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Read error = %v, want %v", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("Read: %v", err)
			}

			if got != tt.want {
				t.Errorf("Read = %q, want %q", got, tt.want)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// A client running `wayfindr stdio` with its scoped token may start the
// daemon; the daemon must not inherit that token as its default one.
func TestWithoutScopedDropsOnlyAScopedClientToken(t *testing.T) {
	scoped := clienttoken.EnvVar + "=" + clienttoken.ScopedPrefix + "abc"
	def := clienttoken.EnvVar + "=wf_default"
	other := "WAYFINDR_DATA_DIR=/data"

	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{"scoped dropped", []string{other, scoped}, []string{other}},
		{"scoped with whitespace dropped", []string{scoped + "\n", other}, []string{other}},
		{"default kept", []string{other, def}, []string{other, def}},
		{"none", []string{other}, []string{other}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := append([]string(nil), tt.env...)

			if diff := cmp.Diff(tt.want, clienttoken.WithoutScoped(in)); diff != "" {
				t.Errorf("WithoutScoped (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff(tt.env, in); diff != "" {
				t.Errorf("WithoutScoped changed its input (-want +got):\n%s", diff)
			}
		})
	}
}
