package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/store"
)

const (
	// touchEvery is how often the daemon records when each scoped client
	// token was last used. Lookups only mark the registry; this bounds how
	// late a token's last_used_at is, and how often the store is written.
	touchEvery = time.Minute
	// touchTimeout bounds one flush of the touches, at shutdown included.
	touchTimeout = 5 * time.Second
)

// clientTokens are the scoped client tokens (docs/internal/plans/
// scoped-client-tokens.md): the store keeps them, and the registry, which
// httpd resolves bearers with, holds them in memory. Every change goes to the
// store first, so a failed write leaves the registry as it was.
type clientTokens struct {
	store    store.Store
	userID   string
	registry *clienttoken.Registry
	clock    Clock
	log      zerolog.Logger

	// mu serializes Create and Revoke, each held across its store write and
	// its registry update. Without it a revoke racing a create of the same
	// name could delete the new row and find nothing in the registry yet,
	// and the create's Add would then leave a live token with no row.
	mu sync.Mutex
}

var _ admin.ClientTokens = (*clientTokens)(nil)

// loadClientTokens reads every scoped client token of userID into a new
// registry, so httpd accepts them from its first request. now, the daemon
// clock's, stamps the registry's touches.
func loadClientTokens(ctx context.Context, st store.Store, userID string, now func() time.Time) (*clienttoken.Registry, error) {
	rows, err := st.ListClientTokens(ctx, userID)

	if err != nil {
		return nil, fmt.Errorf("load client tokens: %w", err)
	}

	clients := make(map[[sha256.Size]byte]clienttoken.Client, len(rows))

	for _, t := range rows {
		clients[t.Digest] = clienttoken.Client{Name: t.Name, Connectors: t.Connectors}
	}

	return clienttoken.NewRegistry(clients, now), nil
}

// Create implements admin.ClientTokens: it generates the value, stores its
// digest, and only then adds it to the registry.
func (t *clientTokens) Create(ctx context.Context, name string, connectors []string) (store.ClientToken, string, error) {
	value, err := clienttoken.Generate()

	if err != nil {
		return store.ClientToken{}, "", err
	}

	row := store.ClientToken{
		UserID:     t.userID,
		Name:       name,
		Digest:     clienttoken.Digest(value),
		Connectors: slices.Clone(connectors),
		// The store keeps milliseconds: truncating here makes the create's
		// answer the time a list reads back.
		CreatedAt: t.clock.Now().UTC().Truncate(time.Millisecond),
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.store.CreateClientToken(ctx, row); err != nil {
		return store.ClientToken{}, "", err
	}

	t.registry.Add(row.Digest, clienttoken.Client{Name: row.Name, Connectors: row.Connectors})

	return row, value, nil
}

// List implements admin.ClientTokens.
func (t *clientTokens) List(ctx context.Context) ([]store.ClientToken, error) {
	return t.store.ListClientTokens(ctx, t.userID)
}

// Revoke implements admin.ClientTokens: it deletes the token from the
// store, then from the registry, which refuses it from the next request on.
func (t *clientTokens) Revoke(ctx context.Context, name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.store.DeleteClientToken(ctx, t.userID, name); err != nil {
		return err
	}

	t.registry.Remove(name)

	return nil
}

// run records the registry's touches every touchEvery until ctx ends, then
// once more, so the uses of the last minute are not lost at shutdown.
func (t *clientTokens) run(ctx context.Context) {
	timer := t.clock.NewTimer(touchEvery)

	for {
		select {
		case <-timer.C():
			t.flush(ctx)
			timer = t.clock.NewTimer(touchEvery)
		case <-ctx.Done():
			timer.Stop()
			t.flush(context.WithoutCancel(ctx))

			return
		}
	}
}

// flush writes the touches since the last flush. A failed write is logged
// and dropped: it costs only some tokens' last_used_at.
func (t *clientTokens) flush(ctx context.Context) {
	used := t.registry.Touches()

	if len(used) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, touchTimeout)
	defer cancel()

	if err := t.store.TouchClientTokens(ctx, t.userID, used); err != nil {
		t.log.Warn().Err(err).Int("client_tokens", len(used)).Msg("record client token use")
	}
}

// keepTouches runs the touch flush until ctx ends or stop is called; stop
// waits for the last flush.
func (d *Daemon) keepTouches(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		d.tokens.run(ctx)
	}()

	return func() {
		cancel()
		<-done
	}
}
