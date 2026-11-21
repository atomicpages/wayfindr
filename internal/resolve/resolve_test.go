package resolve_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/resolve"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
)

// fixture is a resolver for one user over a real temp-dir store and a real
// Sealer with a fixed test key.
type fixture struct {
	store  *sqlite.Store
	sealer *secrets.Sealer
	userID string
	env    map[string]string
	logs   *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })
	userID, err := st.EnsureUser(ctx)

	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	sealer, err := secrets.NewSealer(bytes.Repeat([]byte{0x42}, secrets.KeySize))

	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	return &fixture{store: st, sealer: sealer, userID: userID, env: map[string]string{}, logs: &bytes.Buffer{}}
}

func (f *fixture) resolver(userID string) *resolve.Resolver {
	lookup := func(k string) (string, bool) { v, ok := f.env[k]; return v, ok }
	log := zerolog.New(f.logs).Level(zerolog.TraceLevel)

	return resolve.New(userID, lookup, f.store, f.sealer, log)
}

// putSecret stores value the way `wayfindr secret set` does.
func (f *fixture) putSecret(t *testing.T, userID, name, value string) {
	t.Helper()

	if err := f.resolver(userID).PutSecret(context.Background(), name, value); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
}

func TestResolveEnvReference(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr error
	}{
		{name: "set variable resolves to its value", env: map[string]string{"LINEAR_TOKEN": "lin_abc"}, want: "lin_abc"},
		{name: "empty but set variable is a value", env: map[string]string{"LINEAR_TOKEN": ""}, want: ""},
		{name: "unset variable is not found", env: map[string]string{"OTHER": "x"}, wantErr: config.ErrNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.env = tt.env
			got, err := f.resolver(f.userID).Resolve(context.Background(),
				config.Ref{Kind: config.RefEnv, Name: "LINEAR_TOKEN"})

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}

			if got != tt.want {
				t.Errorf("value = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveSecretReference(t *testing.T) {
	f := newFixture(t)
	f.putSecret(t, f.userID, "slack_client_secret", "s3cr3t-value")
	r := f.resolver(f.userID)
	ctx := context.Background()

	got, err := r.Resolve(ctx, config.Ref{Kind: config.RefSecret, Name: "slack_client_secret"})

	if err != nil {
		t.Fatalf("Resolve stored secret: %v", err)
	}

	if got != "s3cr3t-value" {
		t.Errorf("value = %q, want %q", got, "s3cr3t-value")
	}

	if _, err := r.Resolve(ctx, config.Ref{Kind: config.RefSecret, Name: "missing"}); !errors.Is(err, config.ErrNotFound) {
		t.Errorf("Resolve missing secret: err = %v, want ErrNotFound", err)
	}
}

func TestResolveSecretSealedForAnotherRowFailsToOpen(t *testing.T) {
	tests := []struct {
		name       string
		sealedUser string // "" means the fixture's user
		sealedName string
	}{
		{name: "sealed for another user", sealedUser: "01900000-0000-7000-8000-000000000000", sealedName: "linear_key"},
		{name: "sealed for another secret name", sealedName: "github_key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			sealedUser := tt.sealedUser

			if sealedUser == "" {
				sealedUser = f.userID
			}

			sealed := f.sealer.Seal([]byte("lin_abc"), secrets.AAD("secrets", sealedUser, tt.sealedName))

			if err := f.store.PutSecret(ctx, f.userID, "linear_key", sealed); err != nil {
				t.Fatalf("PutSecret: %v", err)
			}

			got, err := f.resolver(f.userID).Resolve(ctx, config.Ref{Kind: config.RefSecret, Name: "linear_key"})

			if !errors.Is(err, secrets.ErrOpen) {
				t.Fatalf("err = %v, want secrets.ErrOpen", err)
			}

			if errors.Is(err, config.ErrNotFound) {
				t.Errorf("err = %v; an unopenable secret must not read as not found", err)
			}

			if got != "" {
				t.Errorf("value = %q, want empty on failure", got)
			}
		})
	}
}

// A stored secret that can't be opened (sealed under another master key,
// or in a sealed format this build doesn't know) reads as
// config.ErrUnreadable, never config.ErrNotFound; the secrets sentinel
// stays in the chain.
func TestResolveUnopenableSecretIsUnreadable(t *testing.T) {
	otherKey, err := secrets.NewSealer(bytes.Repeat([]byte{0x24}, secrets.KeySize))

	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	tests := []struct {
		name   string
		sealed func(f *fixture) []byte
		cause  error
	}{
		{"sealed under another master key", func(f *fixture) []byte {
			return otherKey.Seal([]byte("lin_abc"), secrets.AAD("secrets", f.userID, "linear_key"))
		}, secrets.ErrOpen},
		{"sealed in an unknown format", func(*fixture) []byte {
			return []byte{0x7f, 1, 2, 3}
		}, secrets.ErrUnknownVersion},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()

			if err := f.store.PutSecret(ctx, f.userID, "linear_key", tt.sealed(f)); err != nil {
				t.Fatalf("PutSecret: %v", err)
			}

			_, err := f.resolver(f.userID).Resolve(ctx, config.Ref{Kind: config.RefSecret, Name: "linear_key"})

			if !errors.Is(err, config.ErrUnreadable) || !errors.Is(err, tt.cause) {
				t.Errorf("err = %v, want config.ErrUnreadable wrapping %v", err, tt.cause)
			}

			if errors.Is(err, config.ErrNotFound) {
				t.Errorf("err = %v; an unopenable secret must not read as not found", err)
			}
		})
	}
}

func TestTemplateResolvesThroughResolver(t *testing.T) {
	f := newFixture(t)
	f.env["LINEAR_HOST"] = "mcp.linear.app"
	f.putSecret(t, f.userID, "linear_key", "lin_api_9f8e7d")
	r := f.resolver(f.userID)
	ctx := context.Background()

	got, err := config.Template("https://${env:LINEAR_HOST}/sse?k=${secret:linear_key}").Resolve(ctx, r)

	if err != nil {
		t.Fatalf("Template.Resolve: %v", err)
	}

	if want := "https://mcp.linear.app/sse?k=lin_api_9f8e7d"; got != want {
		t.Errorf("resolved = %q, want %q", got, want)
	}

	_, err = config.Template("Bearer ${secret:absent}").Resolve(ctx, r)
	var re *config.ResolveError

	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want *config.ResolveError", err)
	}

	if want := (config.Ref{Kind: config.RefSecret, Name: "absent"}); re.Ref != want {
		t.Errorf("ResolveError.Ref = %v, want %v", re.Ref, want)
	}

	if !errors.Is(err, config.ErrNotFound) {
		t.Errorf("err = %v, want it to wrap ErrNotFound", err)
	}
}

func TestResolveNeverLogsResolvedValues(t *testing.T) {
	f := newFixture(t)
	f.env["LINEAR_TOKEN"] = "env-value-7c1a"
	f.putSecret(t, f.userID, "linear_key", "secret-value-3b9d")
	r := f.resolver(f.userID)
	ctx := context.Background()

	if _, err := config.Template("${env:LINEAR_TOKEN} ${secret:linear_key}").Resolve(ctx, r); err != nil {
		t.Fatalf("Template.Resolve: %v", err)
	}

	logs := f.logs.String()

	if logs == "" {
		t.Fatal("no log output at trace level; the assertion below would be vacuous")
	}

	for _, v := range []string{"env-value-7c1a", "secret-value-3b9d"} {
		if strings.Contains(logs, v) {
			t.Errorf("logs contain resolved value %q:\n%s", v, logs)
		}
	}
}

// PutSecret seals the value for its row in the secrets table, so Resolve
// opens it and a ciphertext copied into another row does not.
func TestPutSecretSealsForItsRowAndResolvesBack(t *testing.T) {
	f := newFixture(t)
	r := f.resolver(f.userID)
	ctx := context.Background()

	if err := r.PutSecret(ctx, "linear_key", "lin_api_9f8e7d"); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}

	got, err := r.Resolve(ctx, config.Ref{Kind: config.RefSecret, Name: "linear_key"})

	if err != nil || got != "lin_api_9f8e7d" {
		t.Errorf("Resolve = %q, %v; want %q", got, err, "lin_api_9f8e7d")
	}

	sealed, err := f.store.GetSecret(ctx, f.userID, "linear_key")

	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}

	plain, err := f.sealer.Open(sealed, secrets.AAD("secrets", f.userID, "linear_key"))

	if err != nil || string(plain) != "lin_api_9f8e7d" {
		t.Errorf("open the row with its AAD = %q, %v; want %q", plain, err, "lin_api_9f8e7d")
	}

	if _, err := f.sealer.Open(sealed, secrets.AAD("secrets", f.userID, "github_key")); !errors.Is(err, secrets.ErrOpen) {
		t.Errorf("open the row with another name's AAD: err = %v, want secrets.ErrOpen", err)
	}
}
