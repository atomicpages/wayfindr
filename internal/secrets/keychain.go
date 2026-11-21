package secrets

import (
	"context"
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// OSKeychain is the Keychain backed by the host's credential store (macOS
// Keychain, Secret Service on Linux, Windows Credential Manager) through
// go-keyring, which needs no cgo. The calls do not honor ctx cancellation.
type OSKeychain struct{}

var _ Keychain = OSKeychain{}

// Get returns the item, or ErrKeychainNotFound when there is none.
func (OSKeychain) Get(_ context.Context, service, account string) (string, error) {
	v, err := keyring.Get(service, account)

	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrKeychainNotFound
	}

	if err != nil {
		return "", fmt.Errorf("keychain get %s/%s: %w", service, account, err)
	}

	return v, nil
}

// Set creates or replaces the item.
func (OSKeychain) Set(_ context.Context, service, account, secret string) error {
	if err := keyring.Set(service, account, secret); err != nil {
		return fmt.Errorf("keychain set %s/%s: %w", service, account, err)
	}

	return nil
}
