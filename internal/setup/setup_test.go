package setup_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	_ "modernc.org/sqlite" // database/sql driver "sqlite"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/paths"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/setup"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
	"github.com/atomicpages/wayfindr/internal/testutil/fakekeychain"
)

// fixture is an isolated host: config and data dirs under a temp dir, a
// home directory that must never be needed, and a fake keychain.
type fixture struct {
	vars     map[string]string
	keychain *fakekeychain.Keychain
	paths    paths.Paths
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		vars: map[string]string{
			"WAYFINDR_CONFIG":   filepath.Join(dir, "config", "wayfindr.toml"),
			"WAYFINDR_DATA_DIR": filepath.Join(dir, "data"),
		},
		keychain: &fakekeychain.Keychain{},
	}
	f.paths = paths.Paths{ConfigFile: f.vars["WAYFINDR_CONFIG"], DataDir: f.vars["WAYFINDR_DATA_DIR"]}

	return f
}

func (f *fixture) options() setup.Options {
	return setup.Options{
		Env: paths.Env{
			Getenv:  func(k string) string { return f.vars[k] },
			HomeDir: func() (string, error) { return "", errors.New("tests must not use the home directory") },
		},
		Keychain: f.keychain,
		// No daemon by default, whatever happens to listen on this machine.
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("connection refused")
		},
		Logger: zerolog.Nop(),
	}
}

// writeConfig writes a config file before init runs.
func (f *fixture) writeConfig(t *testing.T, body string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(f.paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(f.paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)

	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	return fi.Mode().Perm()
}

func TestInitOnAFreshHostCreatesEverything(t *testing.T) {
	f := newFixture(t)

	got, err := setup.Init(context.Background(), f.options())

	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if !got.ConfigCreated || !got.TokenCreated {
		t.Errorf("ConfigCreated, TokenCreated = %v, %v, want true, true", got.ConfigCreated, got.TokenCreated)
	}

	if got.Listen != "127.0.0.1:7777" {
		t.Errorf("Listen = %q, want the default 127.0.0.1:7777", got.Listen)
	}

	if got.KeySource != secrets.SourceKeychain || f.keychain.Sets != 1 {
		t.Errorf("KeySource = %q with %d keychain writes, want keychain with 1", got.KeySource, f.keychain.Sets)
	}

	if id, err := uuid.Parse(got.UserID); err != nil || id.Version() != 7 {
		t.Errorf("UserID = %q, want a UUIDv7", got.UserID)
	}

	if _, err := os.Stat(f.paths.DBFile()); err != nil {
		t.Errorf("database not created: %v", err)
	}

	token, err := os.ReadFile(f.paths.ClientTokenFile())

	if err != nil {
		t.Fatalf("read client token: %v", err)
	}

	// wf_ + unpadded base64url of 32 bytes (43 characters).
	if !regexp.MustCompile(`^wf_[A-Za-z0-9_-]{43}\n?$`).Match(token) {
		t.Errorf("client token = %q, want wf_ + base64url of 32 bytes", token)
	}

	// Windows has no Unix mode bits (Go reports 0666 for a writable file).
	if m := mode(t, f.paths.ClientTokenFile()); runtime.GOOS != "windows" && m != 0o600 {
		t.Errorf("client token mode = %v, want 0600", m)
	}
}

// snapshot records every file under dir except the database, which init
// opens (and SQLite may touch) on every run; dbSnapshot covers its content.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), "wayfindr.db") {
			return err
		}

		b, err := os.ReadFile(path) //nolint:gosec // path comes from walking the test's temp dir.

		if err != nil {
			return err
		}

		files[path] = fmt.Sprintf("%v %s", mode(t, path), b)

		return nil
	})

	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}

	return files
}

// dbSnapshot records the database's schema version and the row count of
// every table, which snapshot leaves out. It opens the file read-only, so
// taking it changes nothing.
func dbSnapshot(t *testing.T, path string) map[string]int {
	t.Helper()
	ctx := t.Context()
	// A SQLite URI path uses forward slashes, and a Windows drive letter
	// follows a leading slash (file:///C:/…), as in the store's own DSN;
	// otherwise SQLite reads the drive as the URI's authority.
	p := filepath.ToSlash(path)

	if filepath.VolumeName(path) != "" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}

	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro"}).String())

	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}

	defer func() { _ = db.Close() }()

	got := map[string]int{}
	var version int

	if err := db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}

	got["schema version"] = version

	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'")

	if err != nil {
		t.Fatalf("list tables: %v", err)
	}

	var tables []string

	for rows.Next() {
		var name string

		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}

		tables = append(tables, name)
	}

	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatalf("list tables: %v", err)
	}

	for _, table := range tables {
		var n int

		// table comes from sqlite_schema, not from input.
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+table+`"`).Scan(&n); err != nil { //nolint:gosec // see above
			t.Fatalf("count %s: %v", table, err)
		}

		got["rows in "+table] = n
	}

	return got
}

func TestSecondInitChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keychain *fakekeychain.Keychain
	}{
		{name: "key in keychain", keychain: &fakekeychain.Keychain{}},
		{name: "key in key file", keychain: &fakekeychain.Keychain{SetErr: errors.New("no secret service")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.keychain = tc.keychain
			ctx := context.Background()
			first, err := setup.Init(ctx, f.options())

			if err != nil {
				t.Fatalf("first Init: %v", err)
			}

			root := filepath.Dir(f.paths.DataDir)
			before := snapshot(t, root)
			keychainBefore := maps.Clone(f.keychain.Items)
			dbBefore := dbSnapshot(t, f.paths.DBFile())

			if dbBefore["rows in users"] != 1 {
				t.Fatalf("first run left %d users, want 1", dbBefore["rows in users"])
			}

			second, err := setup.Init(ctx, f.options())

			if err != nil {
				t.Fatalf("second Init: %v", err)
			}

			if diff := cmp.Diff(before, snapshot(t, root)); diff != "" {
				t.Errorf("files changed on the second run (-before +after):\n%s", diff)
			}

			if diff := cmp.Diff(dbBefore, dbSnapshot(t, f.paths.DBFile())); diff != "" {
				t.Errorf("database changed on the second run (-before +after):\n%s", diff)
			}

			if diff := cmp.Diff(keychainBefore, f.keychain.Items); diff != "" {
				t.Errorf("keychain changed on the second run (-before +after):\n%s", diff)
			}

			if second.ConfigCreated || second.TokenCreated {
				t.Errorf("second run ConfigCreated, TokenCreated = %v, %v, want false, false", second.ConfigCreated, second.TokenCreated)
			}

			if second.UserID != first.UserID || second.KeySource != first.KeySource {
				t.Errorf("second run user, key source = %q, %q, want %q, %q",
					second.UserID, second.KeySource, first.UserID, first.KeySource)
			}
		})
	}
}

func TestInitWritesTheKeyFileWith0600WhenTheKeychainIsUnavailable(t *testing.T) {
	f := newFixture(t)
	f.keychain.SetErr = errors.New("no secret service")

	got, err := setup.Init(context.Background(), f.options())

	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if got.KeySource != secrets.SourceFile {
		t.Errorf("KeySource = %q, want file", got.KeySource)
	}

	// Windows has no Unix mode bits (Go reports 0666 for a writable file).
	if m := mode(t, f.paths.KeyFile()); runtime.GOOS != "windows" && m != 0o600 {
		t.Errorf("key file mode = %v, want 0600", m)
	}
}

func TestInitRefusesWhileADaemonIsListening(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })
	f := newFixture(t)
	f.writeConfig(t, fmt.Sprintf("[server]\nlisten = %q\n", ln.Addr().String()))
	opts := f.options()
	opts.Dial = nil // the real TCP probe

	_, err = setup.Init(context.Background(), opts)

	if !errors.Is(err, setup.ErrDaemonRunning) {
		t.Fatalf("Init: err = %v, want ErrDaemonRunning", err)
	}

	if _, err := os.Stat(f.paths.DataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("data dir stat err = %v, want it never created", err)
	}

	if f.keychain.Sets != 0 {
		t.Errorf("keychain writes = %d, want 0", f.keychain.Sets)
	}
}

func TestInitKeepsAnExistingConfigAndUsesItsListenAddress(t *testing.T) {
	f := newFixture(t)
	const custom = "[server]\nlisten = \"127.0.0.1:9999\"\n"
	f.writeConfig(t, custom)

	got, err := setup.Init(context.Background(), f.options())

	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if got.ConfigCreated || got.Listen != "127.0.0.1:9999" {
		t.Errorf("ConfigCreated, Listen = %v, %q, want false, 127.0.0.1:9999", got.ConfigCreated, got.Listen)
	}

	if b, _ := os.ReadFile(f.paths.ConfigFile); string(b) != custom {
		t.Errorf("config = %q, want it unchanged", b)
	}
}

func TestInitAsksAboutTelemetryOnlyWhenItWritesTheConfig(t *testing.T) {
	const custom = "[server]\nlisten = \"127.0.0.1:9999\"\n"

	tests := []struct {
		name      string
		existing  string // config written before init; empty means none
		env       map[string]string
		answer    bool
		wantAsked bool
		wantOn    bool
	}{
		{name: "yes turns it on", answer: true, wantAsked: true, wantOn: true},
		{name: "no leaves it off", answer: false, wantAsked: true},
		{name: "an existing config is not asked about", existing: custom, answer: true},
		{name: "an existing config that turns it on is reported on", existing: "[telemetry]\nenabled = true\n", wantOn: true},
		{name: "DO_NOT_TRACK is not asked about", env: map[string]string{"DO_NOT_TRACK": "1"}, answer: true},
		{name: "DO_NOT_TRACK=0 is asked about", env: map[string]string{"DO_NOT_TRACK": "0"}, answer: true, wantAsked: true, wantOn: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			maps.Copy(f.vars, tt.env)

			if tt.existing != "" {
				f.writeConfig(t, tt.existing)
			}

			asked := false
			opts := f.options()
			opts.AskTelemetry = func() bool { asked = true; return tt.answer }

			got, err := setup.Init(context.Background(), opts)

			if err != nil {
				t.Fatalf("Init: %v", err)
			}

			if asked != tt.wantAsked {
				t.Errorf("asked = %v, want %v", asked, tt.wantAsked)
			}

			if got.TelemetryEnabled != tt.wantOn {
				t.Errorf("TelemetryEnabled = %v, want %v", got.TelemetryEnabled, tt.wantOn)
			}

			data, err := os.ReadFile(f.paths.ConfigFile)

			if err != nil {
				t.Fatal(err)
			}

			if tt.existing != "" {
				if string(data) != tt.existing {
					t.Errorf("config = %q, want it unchanged", data)
				}

				return
			}

			cfg, err := config.Parse(data, config.Options{DataDir: f.paths.DataDir})

			if err != nil {
				t.Fatalf("parse written config: %v", err)
			}

			if cfg.Telemetry.Enabled != tt.wantOn {
				t.Errorf("written config has telemetry.enabled = %v, want %v", cfg.Telemetry.Enabled, tt.wantOn)
			}
		})
	}
}

func TestWriteReportSaysWhetherTelemetryIsOn(t *testing.T) {
	for _, on := range []bool{true, false} {
		var b strings.Builder

		if err := setup.WriteReport(&b, setup.Result{TelemetryEnabled: on}); err != nil {
			t.Fatalf("WriteReport: %v", err)
		}

		if got := strings.Contains(b.String(), "Usage telemetry is on"); got != on {
			t.Errorf("TelemetryEnabled = %v: report says it is on = %v", on, got)
		}
	}
}

func TestInitWithoutAnAskerLeavesTelemetryOff(t *testing.T) {
	f := newFixture(t)

	got, err := setup.Init(context.Background(), f.options())

	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if got.TelemetryEnabled {
		t.Error("TelemetryEnabled = true with no asker, want false")
	}
}

// TestInitCreatesATildeDSNUnderTheHomeDirectory: a sqlite dsn of "~/…" is
// expanded at load (design §4), so the database lands under the injected
// home directory and no literal ./~ directory appears.
func TestInitCreatesATildeDSNUnderTheHomeDirectory(t *testing.T) {
	f := newFixture(t)
	f.writeConfig(t, "[storage]\ndsn = \"~/store/wayfindr.db\"\n")
	home := t.TempDir()
	opts := f.options()
	opts.Env.HomeDir = func() (string, error) { return home, nil }

	if _, err := setup.Init(context.Background(), opts); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if _, err := os.Stat(filepath.Join(home, "store", "wayfindr.db")); err != nil {
		t.Errorf("database not created under the home directory: %v", err)
	}

	if _, err := os.Stat("~"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat ./~ = %v, want it not to exist", err)
	}
}

func TestInitKeepsAnExistingClientTokenFile(t *testing.T) {
	f := newFixture(t)

	if err := os.MkdirAll(f.paths.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	const existing = "wf_existing"

	if err := os.WriteFile(f.paths.ClientTokenFile(), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := setup.Init(context.Background(), f.options())

	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if got.TokenCreated {
		t.Error("TokenCreated = true, want false")
	}

	if b, _ := os.ReadFile(f.paths.ClientTokenFile()); string(b) != existing {
		t.Errorf("client token = %q, want it unchanged", b)
	}
}

func TestInitWritesTheTokenFileEvenWhenTheEnvironmentHoldsAToken(t *testing.T) {
	f := newFixture(t)
	f.vars["WAYFINDR_CLIENT_TOKEN"] = "wf_from_env"

	got, err := setup.Init(context.Background(), f.options())

	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if !got.TokenFromEnv || !got.TokenCreated {
		t.Errorf("TokenFromEnv, TokenCreated = %v, %v, want true, true", got.TokenFromEnv, got.TokenCreated)
	}

	if b, _ := os.ReadFile(f.paths.ClientTokenFile()); string(b) == "wf_from_env" {
		t.Error("token file holds the environment's token, want a newly generated one")
	}
}

func TestInitRefusesToCreateAKeyOverSealedData(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := setup.Init(ctx, f.options()); err != nil {
		t.Fatalf("first Init: %v", err)
	}

	// Arrange: sealed data exists, then the key is lost.
	s, err := sqlite.Open(ctx, f.paths.DBFile())

	if err != nil {
		t.Fatal(err)
	}

	user, err := s.EnsureUser(ctx)

	if err == nil {
		err = s.PutSecret(ctx, user, "linear_key", []byte("sealed"))
	}

	if cerr := s.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		t.Fatal(err)
	}

	f.keychain = &fakekeychain.Keychain{}

	_, err = setup.Init(ctx, f.options())

	if !errors.Is(err, secrets.ErrSealedDataExists) {
		t.Fatalf("Init: err = %v, want ErrSealedDataExists", err)
	}

	if f.keychain.Sets != 0 {
		t.Errorf("keychain writes = %d, want 0", f.keychain.Sets)
	}
}
