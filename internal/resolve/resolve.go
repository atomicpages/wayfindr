// Package resolve implements config.Resolver over the process environment and
// the store's sealed secrets (design §4, §7.1). It sits above config, store,
// and secrets, which are siblings and must not import each other; the daemon
// builds one Resolver per user and hands it to upstream.
//
// A Resolver owns its user's secrets at rest: PutSecret seals and stores
// one (the admin API's secret set, through a function the daemon passes
// in) and Resolve opens it, with one row-bound AAD between them.
//
// Resolve reports why a reference won't resolve through config's sentinels,
// so its callers need not know secrets' errors. The upstream Manager
// receives them inside a *config.ResolveError and maps them:
//
//   - config.ErrNotFound (unset env var, missing secret) → auth_error,
//     missing_value, naming the reference (design §4).
//   - config.ErrUnreadable (secret exists but cannot be opened; the
//     secrets.ErrOpen or secrets.ErrUnknownVersion stays in the chain) →
//     auth_error as well (auth_config, case secret_unreadable): retrying
//     cannot help; the user must re-run `wayfindr secret set` or restore the
//     master key.
//   - anything else (store I/O, context cancellation) → transient; treat like
//     unreachable and retry with backoff.
package resolve

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/store"
)

// SecretStore is the part of store.Store a Resolver reads and writes.
type SecretStore interface {
	// GetSecret returns a sealed secret value, or an error wrapping
	// store.ErrNotFound.
	GetSecret(ctx context.Context, userID, name string) ([]byte, error)
	// PutSecret stores a sealed secret value, replacing any it had.
	PutSecret(ctx context.Context, userID, name string, sealed []byte) error
}

// Resolver resolves ${env:NAME} and ${secret:name} references for one user.
// It never logs a resolved value. It is safe for concurrent use.
type Resolver struct {
	userID    string
	lookupEnv func(string) (string, bool)
	store     SecretStore
	sealer    *secrets.Sealer
	log       zerolog.Logger
}

var _ config.Resolver = (*Resolver)(nil)

// New returns a Resolver for userID. lookupEnv has os.LookupEnv semantics: a
// variable that is set to "" is a value, and only an unset one is not found.
func New(userID string, lookupEnv func(string) (string, bool), st SecretStore, sealer *secrets.Sealer, log zerolog.Logger) *Resolver {
	return &Resolver{userID: userID, lookupEnv: lookupEnv, store: st, sealer: sealer, log: log}
}

// secretAAD is the AAD that binds a user secret to its row in the secrets
// table. PutSecret seals with it and Resolve opens with it, so the two
// cannot drift.
func secretAAD(userID, name string) []byte {
	return secrets.AAD("secrets", userID, name)
}

// PutSecret seals value for the row of the user's secret name and stores
// it, replacing any value it had (`wayfindr secret set`). It never logs
// the value.
func (r *Resolver) PutSecret(ctx context.Context, name, value string) error {
	sealed := r.sealer.Seal([]byte(value), secretAAD(r.userID, name))

	if err := r.store.PutSecret(ctx, r.userID, name, sealed); err != nil {
		return fmt.Errorf("store secret %s: %w", name, err)
	}

	return nil
}

// Resolve implements config.Resolver. A missing variable or secret returns an
// error wrapping config.ErrNotFound. A secret that exists but fails to open
// (wrong master key, tampered row, or sealed for another user or name)
// returns an error wrapping config.ErrUnreadable (and secrets.ErrOpen or
// secrets.ErrUnknownVersion) and never config.ErrNotFound; other store
// failures are returned wrapped as is.
// Errors name the reference but never contain a value.
func (r *Resolver) Resolve(ctx context.Context, ref config.Ref) (string, error) {
	v, err := r.resolve(ctx, ref)

	if err != nil {
		return "", err
	}

	// Only the reference is logged; the value never is (design §9).
	r.log.Debug().Str("user_id", r.userID).Str("ref", ref.String()).Msg("reference resolved")

	return v, nil
}

func (r *Resolver) resolve(ctx context.Context, ref config.Ref) (string, error) {
	switch ref.Kind {
	case config.RefEnv:
		v, ok := r.lookupEnv(ref.Name)

		if !ok {
			return "", fmt.Errorf("env %s: %w", ref.Name, config.ErrNotFound)
		}

		return v, nil
	case config.RefSecret:
		return r.secret(ctx, ref.Name)
	default:
		return "", fmt.Errorf("unknown reference kind %q", ref.Kind)
	}
}

func (r *Resolver) secret(ctx context.Context, name string) (string, error) {
	sealed, err := r.store.GetSecret(ctx, r.userID, name)

	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("secret %s: %w: %w", name, config.ErrNotFound, err)
	}

	if err != nil {
		return "", fmt.Errorf("secret %s: %w", name, err)
	}

	plaintext, err := r.sealer.Open(sealed, secretAAD(r.userID, name))

	if errors.Is(err, secrets.ErrOpen) || errors.Is(err, secrets.ErrUnknownVersion) {
		return "", fmt.Errorf("secret %s: %w: %w", name, config.ErrUnreadable, err)
	}

	if err != nil {
		return "", fmt.Errorf("secret %s: %w", name, err)
	}

	return string(plaintext), nil
}
