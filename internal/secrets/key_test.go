package secrets_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/testutil/fakekeychain"
)

var errNoKeychain = errors.New("keychain: no secret service on this host")

// fillKey returns a 32-byte key with every byte set to b.
func fillKey(b byte) []byte { return bytes.Repeat([]byte{b}, secrets.KeySize) }

func b64(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

func envWith(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func writeKeyFile(t *testing.T, path string, key []byte) {
	t.Helper()

	if err := os.WriteFile(path, []byte(b64(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertSealerKey checks that s seals under want by opening its output with
// an independent Sealer built from want.
func assertSealerKey(t *testing.T, s *secrets.Sealer, want []byte) {
	t.Helper()
	ref, err := secrets.NewSealer(want)

	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	aad := secrets.AAD("secrets", "user-1", "probe")

	if _, err := ref.Open(s.Seal([]byte("probe"), aad), aad); err != nil {
		t.Errorf("sealer does not use the expected master key: %v", err)
	}
}

// Distinct keys per source, so a test can tell which one was used.
func envKey() []byte      { return fillKey(0xE1) }
func keychainKey() []byte { return fillKey(0xC2) }
func fileKey() []byte     { return fillKey(0xF3) }

// keychainWith returns a fake keychain holding key under wayfindr/master-key.
func keychainWith(key []byte) *fakekeychain.Keychain {
	return &fakekeychain.Keychain{Items: map[string]string{"wayfindr/master-key": b64(key)}}
}

func TestLoadSourceOrder(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		keychain   *fakekeychain.Keychain
		file       []byte // nil: no key file
		wantSource secrets.Source
		wantKey    []byte
	}{
		{
			name:       "env wins over keychain and file",
			env:        map[string]string{"WAYFINDR_SECRET_KEY": b64(envKey())},
			keychain:   keychainWith(keychainKey()),
			file:       fileKey(),
			wantSource: secrets.SourceEnv,
			wantKey:    envKey(),
		},
		{
			name:       "keychain wins over file",
			keychain:   keychainWith(keychainKey()),
			file:       fileKey(),
			wantSource: secrets.SourceKeychain,
			wantKey:    keychainKey(),
		},
		{
			name:       "file when keychain has no item",
			keychain:   &fakekeychain.Keychain{},
			file:       fileKey(),
			wantSource: secrets.SourceFile,
			wantKey:    fileKey(),
		},
		{
			name:       "file when keychain is unavailable",
			keychain:   &fakekeychain.Keychain{GetErr: errNoKeychain},
			file:       fileKey(),
			wantSource: secrets.SourceFile,
			wantKey:    fileKey(),
		},
		{
			name:       "file when no keychain is configured",
			file:       fileKey(),
			wantSource: secrets.SourceFile,
			wantKey:    fileKey(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyFile := filepath.Join(t.TempDir(), "master.key")

			if tt.file != nil {
				writeKeyFile(t, keyFile, tt.file)
			}

			src := secrets.KeySources{
				Getenv:  envWith(tt.env),
				KeyFile: keyFile,
				Logger:  zerolog.Nop(),
			}

			if tt.keychain != nil {
				src.Keychain = tt.keychain
			}

			s, source, err := secrets.Load(context.Background(), src)

			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}

			assertSealerKey(t, s, tt.wantKey)
		})
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		keychain *fakekeychain.Keychain
		file     string // "": no key file
		wantErr  error
	}{
		{name: "no source holds a key", keychain: &fakekeychain.Keychain{}, wantErr: secrets.ErrNoKey},
		{name: "keychain unavailable and no file", keychain: &fakekeychain.Keychain{GetErr: errNoKeychain}, wantErr: secrets.ErrNoKey},
		{
			name:     "env is not base64",
			env:      map[string]string{"WAYFINDR_SECRET_KEY": "not base64!"},
			keychain: keychainWith(keychainKey()),
			wantErr:  secrets.ErrInvalidKey,
		},
		{
			name:     "env is 16 bytes",
			env:      map[string]string{"WAYFINDR_SECRET_KEY": b64(make([]byte, 16))},
			keychain: keychainWith(keychainKey()),
			wantErr:  secrets.ErrInvalidKey,
		},
		{
			name:     "keychain item is malformed",
			keychain: &fakekeychain.Keychain{Items: map[string]string{"wayfindr/master-key": "short"}},
			file:     b64(fileKey()),
			wantErr:  secrets.ErrInvalidKey,
		},
		{name: "key file is malformed", file: "garbage", wantErr: secrets.ErrInvalidKey},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyFile := filepath.Join(t.TempDir(), "master.key")

			if tt.file != "" {
				if err := os.WriteFile(keyFile, []byte(tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			src := secrets.KeySources{Getenv: envWith(tt.env), KeyFile: keyFile, Logger: zerolog.Nop()}

			if tt.keychain != nil {
				src.Keychain = tt.keychain
			}

			s, _, err := secrets.Load(context.Background(), src)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Load() error = %v, want %v", err, tt.wantErr)
			}

			if s != nil {
				t.Error("Load() returned a Sealer on error")
			}
		})
	}
}

func TestLoadNoKeyErrorNamesAllSources(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "master.key")
	_, _, err := secrets.Load(context.Background(), secrets.KeySources{KeyFile: keyFile, Logger: zerolog.Nop()})

	if !errors.Is(err, secrets.ErrNoKey) {
		t.Fatalf("Load() error = %v, want ErrNoKey", err)
	}

	for _, want := range []string{"WAYFINDR_SECRET_KEY", "keychain", keyFile} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// logEntry is the subset of a zerolog JSON line the tests inspect.
type logEntry struct {
	Level  string `json:"level"`
	Source string `json:"source"`
	Path   string `json:"path"`
	Error  string `json:"error"`
}

// warnings parses zerolog JSON output and returns its warn-level entries.
func warnings(t *testing.T, logs string) []logEntry {
	t.Helper()
	var warns []logEntry

	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}

		var e logEntry

		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}

		if e.Level == "warn" {
			warns = append(warns, e)
		}
	}

	return warns
}

func TestLoadFromFileWarnsWithoutKeyMaterial(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "master.key")
	writeKeyFile(t, keyFile, fileKey())
	var logs bytes.Buffer

	_, _, err := secrets.Load(context.Background(), secrets.KeySources{
		Keychain: &fakekeychain.Keychain{GetErr: errNoKeychain},
		KeyFile:  keyFile,
		Logger:   zerolog.New(&logs).Level(zerolog.DebugLevel),
	})

	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []logEntry{{Level: "warn", Source: "file", Path: keyFile}}

	if diff := cmp.Diff(want, warnings(t, logs.String())); diff != "" {
		t.Errorf("warnings mismatch (-want +got):\n%s", diff)
	}

	if strings.Contains(logs.String(), b64(fileKey())) {
		t.Error("logs contain the master key")
	}
}

func TestLoadOrCreateStoresNewKeyInKeychain(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "master.key")
	kc := &fakekeychain.Keychain{}
	src := secrets.KeySources{Keychain: kc, KeyFile: keyFile, Logger: zerolog.Nop()}

	created, source, err := secrets.LoadOrCreate(context.Background(), src, false)

	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	if source != secrets.SourceKeychain {
		t.Errorf("source = %q, want %q", source, secrets.SourceKeychain)
	}

	if _, err := os.Stat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("key file exists after keychain write (stat err = %v)", err)
	}

	stored, err := base64.StdEncoding.DecodeString(kc.Items["wayfindr/master-key"])

	if err != nil || len(stored) != secrets.KeySize {
		t.Fatalf("keychain item is not a base64 32-byte key (len %d, err %v)", len(stored), err)
	}

	assertSealerKey(t, created, stored)

	// A later Load finds the same key.
	loaded, _, err := secrets.Load(context.Background(), src)

	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	assertSealerKey(t, loaded, stored)
}

func TestLoadOrCreateFallsBackToKeyFile(t *testing.T) {
	// The data directory does not exist yet; LoadOrCreate creates it.
	keyFile := filepath.Join(t.TempDir(), "data", "master.key")
	var logs bytes.Buffer
	src := secrets.KeySources{
		Keychain: &fakekeychain.Keychain{GetErr: errNoKeychain, SetErr: errNoKeychain},
		KeyFile:  keyFile,
		Logger:   zerolog.New(&logs),
	}

	created, source, err := secrets.LoadOrCreate(context.Background(), src, false)

	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	if source != secrets.SourceFile {
		t.Errorf("source = %q, want %q", source, secrets.SourceFile)
	}

	info, err := os.Stat(keyFile)

	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}

	// Windows has no Unix mode bits (Go reports 0666 for a writable file),
	// so the mode is only checked elsewhere.
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Errorf("key file mode = %#o, want 0600", got)
	}

	raw, err := os.ReadFile(filepath.Clean(keyFile))

	if err != nil {
		t.Fatal(err)
	}

	stored, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))

	if err != nil || len(stored) != secrets.KeySize {
		t.Fatalf("key file is not a base64 32-byte key (len %d, err %v)", len(stored), err)
	}

	assertSealerKey(t, created, stored)

	if len(warnings(t, logs.String())) == 0 {
		t.Error("no warning logged for the key file")
	}

	if strings.Contains(logs.String(), b64(stored)) {
		t.Error("logs contain the master key")
	}
}

// A keychain that fails a lookup for any reason other than "not found" may
// still hold a key, and the item is global to the user account, so another
// data directory's sealed data may depend on it. LoadOrCreate must not write
// the keychain then; it falls back to the key file.
func TestLoadOrCreateSkipsKeychainWhenLookupFails(t *testing.T) {
	errDenied := errors.New("keychain: user denied access")
	keyFile := filepath.Join(t.TempDir(), "master.key")
	var logs bytes.Buffer
	// Set would succeed, so only the failed Get stops a replacement.
	kc := &fakekeychain.Keychain{GetErr: errDenied}
	src := secrets.KeySources{Keychain: kc, KeyFile: keyFile, Logger: zerolog.New(&logs)}

	created, source, err := secrets.LoadOrCreate(context.Background(), src, false)

	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	if kc.Sets != 0 {
		t.Errorf("Keychain.Set called %d times after a failed lookup, want 0", kc.Sets)
	}

	if source != secrets.SourceFile {
		t.Errorf("source = %q, want %q", source, secrets.SourceFile)
	}

	raw, err := os.ReadFile(filepath.Clean(keyFile))

	if err != nil {
		t.Fatalf("read key file: %v", err)
	}

	stored, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))

	if err != nil || len(stored) != secrets.KeySize {
		t.Fatalf("key file is not a base64 32-byte key (len %d, err %v)", len(stored), err)
	}

	assertSealerKey(t, created, stored)

	named := false

	for _, w := range warnings(t, logs.String()) {
		if strings.Contains(w.Error, errDenied.Error()) {
			named = true
		}
	}

	if !named {
		t.Errorf("no warning names the keychain error; logs:\n%s", logs.String())
	}

	if strings.Contains(logs.String(), b64(stored)) {
		t.Error("logs contain the master key")
	}
}

func TestLoadOrCreateFailsWhenLookupFailsAndNoKeyFile(t *testing.T) {
	errLocked := errors.New("keychain: keyring is locked")
	kc := &fakekeychain.Keychain{GetErr: errLocked}
	src := secrets.KeySources{Keychain: kc, Logger: zerolog.Nop()}

	s, _, err := secrets.LoadOrCreate(context.Background(), src, false)

	if !errors.Is(err, secrets.ErrNoKey) {
		t.Fatalf("LoadOrCreate() error = %v, want ErrNoKey", err)
	}

	if !errors.Is(err, errLocked) {
		t.Errorf("LoadOrCreate() error = %v, want it to wrap the keychain error", err)
	}

	if s != nil {
		t.Error("LoadOrCreate() returned a Sealer on error")
	}

	if kc.Sets != 0 {
		t.Errorf("Keychain.Set called %d times after a failed lookup, want 0", kc.Sets)
	}
}

// Another process (say, a concurrent `wayfindr init` for a different data
// directory) creates the keychain item after LoadOrCreate's lookup found
// none. LoadOrCreate must not replace it.
func TestLoadOrCreateNeverReplacesKeychainItem(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "master.key")
	other := b64(fillKey(0x0A))
	kc := &fakekeychain.Keychain{OnGet: func(f *fakekeychain.Keychain, call int) {
		if call == 2 {
			f.Items = map[string]string{"wayfindr/master-key": other}
		}
	}}
	src := secrets.KeySources{Keychain: kc, KeyFile: keyFile, Logger: zerolog.Nop()}

	s, _, err := secrets.LoadOrCreate(context.Background(), src, false)

	if err == nil {
		t.Fatal("LoadOrCreate() succeeded, want an error for a keychain item that appeared during creation")
	}

	if s != nil {
		t.Error("LoadOrCreate() returned a Sealer on error")
	}

	if kc.Sets != 0 {
		t.Errorf("Keychain.Set called %d times over an existing item, want 0", kc.Sets)
	}

	if got := kc.Items["wayfindr/master-key"]; got != other {
		t.Error("the existing keychain item was replaced")
	}

	if _, err := os.Stat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a key file was written (stat err = %v)", err)
	}
}

func TestLoadOrCreateUsesExistingKey(t *testing.T) {
	tests := []struct {
		name             string
		sealedDataExists bool
	}{
		{"no sealed data", false},
		{"sealed data exists", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyFile := filepath.Join(t.TempDir(), "master.key")
			writeKeyFile(t, keyFile, fileKey())
			kc := &fakekeychain.Keychain{}
			src := secrets.KeySources{Keychain: kc, KeyFile: keyFile, Logger: zerolog.Nop()}

			s, source, err := secrets.LoadOrCreate(context.Background(), src, tt.sealedDataExists)

			if err != nil {
				t.Fatalf("LoadOrCreate: %v", err)
			}

			if source != secrets.SourceFile {
				t.Errorf("source = %q, want %q", source, secrets.SourceFile)
			}

			assertSealerKey(t, s, fileKey())

			if len(kc.Items) != 0 {
				t.Error("LoadOrCreate wrote a new key to the keychain")
			}
		})
	}
}

func TestLoadOrCreateRefusesOverSealedData(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "master.key")
	kc := &fakekeychain.Keychain{}
	src := secrets.KeySources{Keychain: kc, KeyFile: keyFile, Logger: zerolog.Nop()}

	s, _, err := secrets.LoadOrCreate(context.Background(), src, true)

	if !errors.Is(err, secrets.ErrSealedDataExists) {
		t.Fatalf("LoadOrCreate() error = %v, want ErrSealedDataExists", err)
	}

	if s != nil {
		t.Error("LoadOrCreate() returned a Sealer on error")
	}

	if len(kc.Items) != 0 {
		t.Error("a key was written to the keychain")
	}

	if _, err := os.Stat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a key file was written (stat err = %v)", err)
	}
}

func TestLoadOrCreateFailsWhenNoSourceIsWritable(t *testing.T) {
	src := secrets.KeySources{
		Keychain: &fakekeychain.Keychain{GetErr: errNoKeychain, SetErr: errNoKeychain},
		Logger:   zerolog.Nop(),
	}
	_, _, err := secrets.LoadOrCreate(context.Background(), src, false)

	if !errors.Is(err, secrets.ErrNoKey) {
		t.Fatalf("LoadOrCreate() error = %v, want ErrNoKey", err)
	}
}

// writeKeyFileMode writes a valid key file and pins its mode, bypassing the
// umask.
func writeKeyFileMode(t *testing.T, path string, key []byte, mode os.FileMode) {
	t.Helper()
	writeKeyFile(t, path, key)

	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestKeyFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix mode bits")
	}

	loaders := []struct {
		name string
		load func(context.Context, secrets.KeySources) (*secrets.Sealer, secrets.Source, error)
	}{
		{name: "Load", load: secrets.Load},
		{name: "LoadOrCreate", load: func(ctx context.Context, src secrets.KeySources) (*secrets.Sealer, secrets.Source, error) {
			return secrets.LoadOrCreate(ctx, src, false)
		}},
	}
	tests := []struct {
		mode    os.FileMode
		modeStr string
		refused bool
	}{
		{mode: 0o644, modeStr: "0644", refused: true},
		{mode: 0o640, modeStr: "0640", refused: true},
		{mode: 0o604, modeStr: "0604", refused: true},
		{mode: 0o600, modeStr: "0600"},
		{mode: 0o400, modeStr: "0400"},
	}

	for _, l := range loaders {
		for _, tt := range tests {
			t.Run(l.name+"/"+tt.modeStr, func(t *testing.T) {
				keyFile := filepath.Join(t.TempDir(), "master.key")
				writeKeyFileMode(t, keyFile, fileKey(), tt.mode)
				var logs bytes.Buffer

				s, source, err := l.load(context.Background(), secrets.KeySources{
					Keychain: &fakekeychain.Keychain{GetErr: errNoKeychain},
					KeyFile:  keyFile,
					Logger:   zerolog.New(&logs).Level(zerolog.DebugLevel),
				})

				if strings.Contains(logs.String(), b64(fileKey())) {
					t.Error("logs contain the master key")
				}

				if !tt.refused {
					if err != nil {
						t.Fatalf("%s() error = %v, want nil", l.name, err)
					}

					if source != secrets.SourceFile {
						t.Errorf("source = %q, want %q", source, secrets.SourceFile)
					}

					assertSealerKey(t, s, fileKey())

					return
				}

				if !errors.Is(err, secrets.ErrKeyFilePermissions) {
					t.Fatalf("%s() error = %v, want ErrKeyFilePermissions", l.name, err)
				}

				if s != nil {
					t.Errorf("%s() returned a Sealer on error", l.name)
				}

				msg := err.Error()

				for _, want := range []string{keyFile, "mode " + tt.modeStr, "chmod 600 " + keyFile} {
					if !strings.Contains(msg, want) {
						t.Errorf("error %q does not mention %q", msg, want)
					}
				}

				if strings.Contains(msg, b64(fileKey())) {
					t.Error("error contains the master key")
				}

				if got, err := os.ReadFile(filepath.Clean(keyFile)); err != nil || string(got) != b64(fileKey())+"\n" {
					t.Errorf("key file was modified: %q, %v", got, err)
				}
			})
		}
	}
}
