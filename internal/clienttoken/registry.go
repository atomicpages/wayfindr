package clienttoken

import (
	"crypto/sha256"
	"slices"
	"sync"
	"time"
)

// Registry holds the scoped client tokens the daemon accepts, keyed by the
// SHA-256 of their values, so httpd can resolve a bearer to its Client
// without the token value ever being kept. It also records when each token
// was last used, in memory, for the daemon to flush to the store.
//
// The default token is not in the Registry: httpd compares it on its own.
// Every Client a Registry holds is Scoped, so a Lookup hit is never the
// default token. A Registry is safe for concurrent use.
type Registry struct {
	now func() time.Time

	mu      sync.Mutex
	clients map[[sha256.Size]byte]Client
	// digests maps a token name to its digest, so Remove can find it.
	digests map[string][sha256.Size]byte
	// touches maps a token digest to when Lookup last found it, since the
	// last call to Touches. Keyed by digest, not name, so a use of a revoked
	// token is never credited to a new token of the same name.
	touches map[[sha256.Size]byte]time.Time
}

// NewRegistry returns a Registry holding clients, keyed by token digest
// (Digest), less any Add would ignore. now stamps touches; nil means
// time.Now. clients is copied, so the caller may reuse it.
func NewRegistry(clients map[[sha256.Size]byte]Client, now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}

	r := &Registry{
		now:     now,
		clients: make(map[[sha256.Size]byte]Client, len(clients)),
		digests: make(map[string][sha256.Size]byte, len(clients)),
		touches: make(map[[sha256.Size]byte]time.Time),
	}

	for d, c := range clients {
		r.add(d, c)
	}

	return r
}

// Lookup returns the Client whose token has digest d, and records that it
// was used now. The Client is a copy: changing it never changes r.
func (r *Registry) Lookup(d [sha256.Size]byte) (Client, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	c, ok := r.clients[d]

	if !ok {
		return Client{}, false
	}

	r.touches[d] = r.now()

	return clone(c), true
}

// Add registers c under digest d, replacing any token of the same name or
// digest. c is copied. It ignores a c that is not Scoped or is named
// DefaultName (see add).
func (r *Registry) Add(d [sha256.Size]byte, c Client) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.add(d, c)
}

// Remove drops the token called name, so the next Lookup of its digest
// fails. It reports whether there was one. A touch recorded before the
// removal is kept: the token was used, and Touches still reports it.
func (r *Registry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	d, ok := r.digests[name]

	if !ok {
		return false
	}

	delete(r.digests, name)
	delete(r.clients, d)

	return true
}

// Touches returns when each token was last found by Lookup since the
// previous call, keyed by token digest, and forgets them.
func (r *Registry) Touches() map[[sha256.Size]byte]time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()

	t := r.touches
	r.touches = make(map[[sha256.Size]byte]time.Time)

	return t
}

// add is Add without the lock. Dropping the old entries of both the name and
// the digest keeps clients and digests one-to-one.
//
// It is the one defensive check that a Lookup hit is scoped: a Client with
// no connector limit, or named for the default token, would pass for the
// default token and get every connector and the admin API. Callers never
// pass one (the store refuses a token with no connectors, and admin the
// name default), so it is a bug upstream, and ignoring it fails closed.
func (r *Registry) add(d [sha256.Size]byte, c Client) {
	if !c.Scoped() || c.Name == DefaultName {
		return
	}

	if old, ok := r.digests[c.Name]; ok {
		delete(r.clients, old)
	}

	if old, ok := r.clients[d]; ok {
		delete(r.digests, old.Name)
	}

	r.clients[d] = clone(c)
	r.digests[c.Name] = d
}

func clone(c Client) Client {
	return Client{Name: c.Name, Connectors: slices.Clone(c.Connectors)}
}
