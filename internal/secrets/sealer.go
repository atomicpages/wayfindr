// Package secrets sources the master key and seals values for the store with
// AES-256-GCM (design §7.1).
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the length of the master key in bytes (AES-256).
const KeySize = 32

// formatV1 is the version byte that prefixes every sealed value.
const formatV1 byte = 0x01

// aadPrefix versions the AAD layout separately from the sealed format.
const aadPrefix = "wayfindr/v1"

// Errors returned by NewSealer and Sealer.Open.
var (
	// ErrInvalidKey means a master key is not exactly KeySize bytes.
	ErrInvalidKey = errors.New("invalid master key")
	// ErrUnknownVersion means a sealed value's version byte is not one this
	// build can open.
	ErrUnknownVersion = errors.New("unknown sealed format version")
	// ErrOpen means a sealed value failed authentication: it was truncated or
	// tampered with, sealed under another key, or bound to a different AAD.
	ErrOpen = errors.New("open sealed value")
)

// Sealer encrypts and authenticates values with the master key. A sealed
// value is 0x01 ‖ nonce(12) ‖ ciphertext ‖ tag(16). It is safe for
// concurrent use.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer returns a Sealer for a KeySize-byte master key. The cipher keeps
// its own copy of the key schedule, so the caller may zero key afterwards.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: want %d bytes, got %d", ErrInvalidKey, KeySize, len(key))
	}

	block, err := aes.NewCipher(key)

	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidKey, err)
	}

	// NewGCMWithRandomNonce draws a fresh 12-byte nonce from crypto/rand on
	// every Seal and prepends it to the ciphertext, which is exactly the
	// nonce ‖ ciphertext ‖ tag layout we store.
	aead, err := cipher.NewGCMWithRandomNonce(block)

	if err != nil {
		return nil, fmt.Errorf("create aes-gcm: %w", err)
	}

	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext bound to aad. Build aad with AAD so that a value
// copied into another row fails to open.
func (s *Sealer) Seal(plaintext, aad []byte) []byte {
	out := make([]byte, 1, 1+s.aead.Overhead()+len(plaintext))
	out[0] = formatV1

	return s.aead.Seal(out, nil, plaintext, aad) //nolint:gosec // G407 false positive: the AEAD draws its own random nonce; nil is required.
}

// Open authenticates and decrypts a value produced by Seal with the same
// aad. It returns ErrUnknownVersion for any version byte other than 0x01,
// and ErrOpen for anything that fails authentication.
func (s *Sealer) Open(sealed, aad []byte) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, fmt.Errorf("%w: empty value", ErrOpen)
	}

	if sealed[0] != formatV1 {
		return nil, fmt.Errorf("%w: 0x%02x", ErrUnknownVersion, sealed[0])
	}

	body := sealed[1:]

	if len(body) < s.aead.Overhead() {
		return nil, fmt.Errorf("%w: truncated value", ErrOpen)
	}

	plaintext, err := s.aead.Open(nil, nil, body, aad)

	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOpen, err)
	}

	return plaintext, nil
}

// AAD builds the additional authenticated data that binds a sealed value to
// its row: "wayfindr/v1|<table>|<user_id>|<key…>". The key parts identify
// the row within the table (for example a connector name, or a secret name).
func AAD(table, userID string, key ...string) []byte {
	parts := append([]string{aadPrefix, table, userID}, key...)

	return []byte(strings.Join(parts, "|"))
}
