// Package fakekeychain is an in-memory secrets.Keychain for tests, so they
// never touch the host's real keychain.
package fakekeychain

import (
	"context"

	"github.com/atomicpages/wayfindr/internal/secrets"
)

// Keychain stands in for the OS keychain. The zero value is an empty,
// working keychain. Items are keyed by "service/account". GetErr and SetErr
// simulate an unavailable keychain. Sets counts Set calls, including failed
// ones; Gets counts Get calls. OnGet, when set, runs before each Get with the
// 1-based call number, so a test can change the keychain between lookups.
type Keychain struct {
	Items  map[string]string
	GetErr error
	SetErr error
	Sets   int
	Gets   int
	OnGet  func(k *Keychain, call int)
}

var _ secrets.Keychain = (*Keychain)(nil)

// Get returns the item, GetErr, or secrets.ErrKeychainNotFound.
func (k *Keychain) Get(_ context.Context, service, account string) (string, error) {
	k.Gets++

	if k.OnGet != nil {
		k.OnGet(k, k.Gets)
	}

	if k.GetErr != nil {
		return "", k.GetErr
	}

	v, ok := k.Items[service+"/"+account]

	if !ok {
		return "", secrets.ErrKeychainNotFound
	}

	return v, nil
}

// Set stores the item, or returns SetErr.
func (k *Keychain) Set(_ context.Context, service, account, secret string) error {
	k.Sets++

	if k.SetErr != nil {
		return k.SetErr
	}

	if k.Items == nil {
		k.Items = map[string]string{}
	}

	k.Items[service+"/"+account] = secret

	return nil
}
