package secrets

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/newfile"
	"github.com/atomicpages/wayfindr/internal/paths"
)

// Where the master key is looked up (design §7.1).
const (
	// EnvSecretKey holds the master key as standard base64 of KeySize bytes.
	EnvSecretKey = "WAYFINDR_SECRET_KEY"
	// KeychainService and KeychainAccount name the OS keychain item.
	KeychainService = "wayfindr"
	KeychainAccount = "master-key"
)

// Source names where a master key came from. It is safe to log.
type Source string

// Key sources, in lookup order.
const (
	SourceEnv      Source = "env"
	SourceKeychain Source = "keychain"
	SourceFile     Source = "file"
)

// Errors returned by Load and LoadOrCreate.
var (
	// ErrNoKey means no source holds a master key. The daemon must refuse to
	// start.
	ErrNoKey = errors.New("no master key")
	// ErrSealedDataExists means LoadOrCreate found no master key but the
	// caller reported sealed data in the store. A new key would make that
	// data unreadable, so none is generated.
	ErrSealedDataExists = errors.New("sealed data exists but no master key was found")
	// ErrKeychainNotFound is what a Keychain returns from Get when the item
	// does not exist. Any other Get error means the keychain is unavailable.
	ErrKeychainNotFound = errors.New("keychain item not found")
	// ErrKeyFilePermissions means the key file grants some permission to
	// group or others. Like ssh with a loose private key, Load refuses it
	// rather than use a key other users may have read.
	ErrKeyFilePermissions = errors.New("master key file permissions are too open")
)

// Keychain is the OS credential store. OSKeychain is the real one; tests
// substitute a fake so they never touch the user's keychain.
type Keychain interface {
	// Get returns the item's value, or ErrKeychainNotFound if there is none.
	Get(ctx context.Context, service, account string) (string, error)
	// Set creates or replaces the item.
	Set(ctx context.Context, service, account, secret string) error
}

// KeySources is where Load and LoadOrCreate look for the master key.
type KeySources struct {
	// Getenv reads the process environment. Nil means no variables are set.
	Getenv func(key string) string
	// Keychain is the OS keychain. Nil means no keychain is available.
	Keychain Keychain
	// KeyFile is the key file fallback, normally paths.Paths.KeyFile().
	// Empty disables the file source.
	KeyFile string
	// Logger receives source selection and fallback events. Key material is
	// never logged.
	Logger zerolog.Logger
}

// Load finds the master key and returns a Sealer for it, trying in order
// WAYFINDR_SECRET_KEY, the keychain, then the key file. A source that holds
// a malformed key is an error rather than a reason to try the next one, so
// a typo never silently selects a different key. If no source holds a key
// it returns ErrNoKey; Load never creates one.
func Load(ctx context.Context, src KeySources) (*Sealer, Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	key, source, _, err := find(ctx, src)

	if err != nil {
		return nil, "", err
	}

	if key == nil {
		return nil, "", fmt.Errorf("%w: set %s, store one in the OS keychain (service %q, account %q), or create %s with `wayfindr init`",
			ErrNoKey, EnvSecretKey, KeychainService, KeychainAccount, src.KeyFile)
	}

	return sealerFrom(src, key, source)
}

// LoadOrCreate is Load for `wayfindr init`. When no source holds a key it
// generates one and stores it in the first writable source among the
// keychain and the key file; it never writes WAYFINDR_SECRET_KEY.
//
// The caller must report whether the store already holds sealed data in
// sealedDataExists. If it does and no key is found, LoadOrCreate returns
// ErrSealedDataExists and generates nothing, because a new key would make
// that data unreadable.
//
// The keychain item is global to the user account, so other data
// directories may depend on it. LoadOrCreate writes it only after a lookup
// reported ErrKeychainNotFound. If the lookup failed any other way (access
// denied, locked keyring), the keychain is treated as unavailable for this
// run and the new key goes to the key file.
func LoadOrCreate(ctx context.Context, src KeySources, sealedDataExists bool) (*Sealer, Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	key, source, keychainErr, err := find(ctx, src)

	if err != nil {
		return nil, "", err
	}

	if key == nil {
		if sealedDataExists {
			return nil, "", fmt.Errorf("%w: restore the original key via %s, the OS keychain, or %s",
				ErrSealedDataExists, EnvSecretKey, src.KeyFile)
		}

		if keychainErr != nil {
			if src.KeyFile == "" {
				return nil, "", fmt.Errorf("%w: the OS keychain lookup failed and no key file path is set: %w",
					ErrNoKey, keychainErr)
			}

			src.Logger.Warn().Err(keychainErr).
				Msg("OS keychain lookup failed; not writing the keychain, creating the master key in the key file")
			src.Keychain = nil
		}

		key, source, err = create(ctx, src)

		if err != nil {
			return nil, "", err
		}
	}

	return sealerFrom(src, key, source)
}

// find returns the first key found, or a nil key if no source holds one.
// keychainErr is the keychain lookup error when it was anything other than
// ErrKeychainNotFound: the keychain was unavailable, so it may hold a key
// that find could not see.
func find(ctx context.Context, src KeySources) (key []byte, source Source, keychainErr, err error) {
	if src.Getenv != nil {
		if v := src.Getenv(EnvSecretKey); v != "" {
			key, err := decodeKey(v)

			if err != nil {
				return nil, "", nil, fmt.Errorf("read %s: %w", EnvSecretKey, err)
			}

			return key, SourceEnv, nil, nil
		}
	}

	if src.Keychain != nil {
		v, err := src.Keychain.Get(ctx, KeychainService, KeychainAccount)

		switch {
		case err == nil:
			key, err := decodeKey(v)

			if err != nil {
				return nil, "", nil, fmt.Errorf("read keychain item %s/%s: %w", KeychainService, KeychainAccount, err)
			}

			return key, SourceKeychain, nil, nil
		case errors.Is(err, ErrKeychainNotFound):
		default:
			src.Logger.Debug().Err(err).Msg("keychain unavailable")
			keychainErr = err
		}
	}

	if src.KeyFile != "" {
		b, err := readKeyFile(src.KeyFile)

		switch {
		case err == nil:
			key, err := decodeKey(b)

			if err != nil {
				return nil, "", nil, fmt.Errorf("read key file %s: %w", src.KeyFile, err)
			}

			return key, SourceFile, nil, nil
		case errors.Is(err, fs.ErrNotExist):
		case errors.Is(err, ErrKeyFilePermissions):
			return nil, "", nil, err
		default:
			return nil, "", nil, fmt.Errorf("read key file %s: %w", src.KeyFile, err)
		}
	}

	return nil, "", keychainErr, nil
}

// readKeyFile returns the key file's contents. It refuses a file that grants
// any permission to group or others, checking the mode of the open file so
// the check and the read see the same file.
func readKeyFile(path string) (s string, err error) {
	f, err := os.Open(filepath.Clean(path))

	if err != nil {
		return "", err
	}

	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()
	info, err := f.Stat()

	if err != nil {
		return "", err
	}

	// Windows has no Unix mode bits: Go synthesizes the mode from the
	// read-only attribute, so a check there would refuse every key file.
	// Windows ACLs are left to the user.
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm&0o077 != 0 {
		return "", fmt.Errorf("%w: master key file %s is accessible by group or others (mode %04o); run: chmod 600 %s",
			ErrKeyFilePermissions, path, perm, path)
	}

	b, err := io.ReadAll(f)

	if err != nil {
		return "", err
	}

	return string(b), nil
}

func sealerFrom(src KeySources, key []byte, source Source) (*Sealer, Source, error) {
	s, err := NewSealer(key)

	if err != nil {
		return nil, "", err
	}

	if source == SourceFile {
		src.Logger.Warn().Str("source", string(source)).Str("path", src.KeyFile).
			Msg("master key read from key file; prefer the OS keychain or WAYFINDR_SECRET_KEY")
	} else {
		src.Logger.Debug().Str("source", string(source)).Msg("master key loaded")
	}

	return s, source, nil
}

func decodeKey(s string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))

	if err != nil {
		// The decoder's error quotes an offset, not the input, so it is safe
		// to wrap.
		return nil, fmt.Errorf("%w: not standard base64: %w", ErrInvalidKey, err)
	}

	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: want %d bytes, got %d", ErrInvalidKey, KeySize, len(key))
	}

	return key, nil
}

// create generates a key and stores it in the keychain or, when the keychain
// refuses it, in a new key file. It never overwrites an existing key file.
//
// The caller must pass a nil Keychain unless its lookup just returned
// ErrKeychainNotFound. Keychain.Set replaces an existing item, so create
// looks the item up again immediately before Set and refuses if one has
// appeared. A process that creates the item between that lookup and Set can
// still be overwritten; the keychain API offers no create-only write.
func create(ctx context.Context, src KeySources) ([]byte, Source, error) {
	key := make([]byte, KeySize)

	if _, err := rand.Read(key); err != nil {
		return nil, "", fmt.Errorf("generate master key: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(key)

	if src.Keychain != nil {
		_, err := src.Keychain.Get(ctx, KeychainService, KeychainAccount)

		switch {
		case err == nil:
			return nil, "", fmt.Errorf("keychain item %s/%s appeared while creating the master key; not replacing it, run the command again to use it",
				KeychainService, KeychainAccount)
		case errors.Is(err, ErrKeychainNotFound):
			err = src.Keychain.Set(ctx, KeychainService, KeychainAccount, encoded)

			if err == nil {
				src.Logger.Info().Str("source", string(SourceKeychain)).Msg("master key created")

				return key, SourceKeychain, nil
			}

			src.Logger.Debug().Err(err).Msg("keychain unavailable")
		default:
			src.Logger.Warn().Err(err).
				Msg("OS keychain lookup failed; not writing the keychain, creating the master key in the key file")
		}
	}

	if src.KeyFile == "" {
		return nil, "", fmt.Errorf("%w: the OS keychain is unavailable and no key file path is set", ErrNoKey)
	}

	if err := writeKeyFile(src.KeyFile, encoded); err != nil {
		return nil, "", err
	}

	src.Logger.Info().Str("source", string(SourceFile)).Str("path", src.KeyFile).Msg("master key created")

	return key, SourceFile, nil
}

// writeKeyFile creates path with paths.SecretFileMode, failing if it exists.
func writeKeyFile(path, encoded string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create key file directory: %w", err)
	}

	return newfile.Create(path, []byte(encoded+"\n"), paths.SecretFileMode, "key file "+path)
}
