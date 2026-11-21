package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/store"
)

// SeedToken stores an OAuth token for connector as the authorization code
// flow would, bound to resource and obtained at tokenEndpoint, so tests can
// start from an authorized connector without the flow (wayfindr-421). Like
// the flow, it stores through storeGrant, so it fires OnCredentialChanged.
func SeedToken(ctx context.Context, b *Broker, connector, resource, tokenEndpoint, access, refresh string, expiry time.Time, granted string) error {
	return SeedGrant(ctx, b, connector, resource, tokenEndpoint, "", access, refresh, expiry, granted)
}

// SeedGrant is [SeedToken] for a token obtained with clientID, as the flow
// records it.
func SeedGrant(ctx context.Context, b *Broker, connector, resource, tokenEndpoint, clientID, access, refresh string, expiry time.Time, granted string) error {
	tok := token{Access: access, Refresh: refresh, Type: "Bearer", Expiry: expiry, TokenEndpoint: tokenEndpoint}

	return b.storeGrant(ctx, connector, tok, resource, clientID, granted, nil)
}

// ResourceKey is what a credential row stores for resource (see
// resourceKey).
func ResourceKey(resource string) string { return resourceKey(resource) }

// SetPollSleep replaces how a device code flow's poller waits between
// polls, so tests step the poller by hand. Set it before a flow starts.
func SetPollSleep(b *Broker, sleep func(ctx context.Context, d time.Duration) error) {
	b.sleep = sleep
}

// NewRegistrar returns a Registrar of userID's credentials in st, sealed by
// sealer, for tests of the client choice an auth flow makes, without a
// Broker. It has its own credentials module, and so its own row lock.
func NewRegistrar(st store.Store, sealer *secrets.Sealer, userID string, resolver config.Resolver, client *http.Client, log zerolog.Logger) *Registrar {
	return newRegistrar(&credentials{store: st, sealer: sealer, userID: userID}, resolver, client, log)
}

// Register is [Registrar.obtain]: the client an auth flow authorizes as.
func (r *Registrar) Register(ctx context.Context, req RegisterRequest) (Registration, error) {
	return r.obtain(ctx, req)
}
