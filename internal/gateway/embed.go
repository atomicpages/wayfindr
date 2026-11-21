package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// queryCacheSize is how many query embeddings the gateway keeps (design
// §7.5).
const queryCacheSize = 256

// Embedder embeds texts with one model. *embed.Client satisfies it.
type Embedder interface {
	// Embed returns one vector per input, in input order.
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	// ModelID is the model identity of the vectors Embed returns; the
	// catalog compares a query vector only with entries of the same one.
	ModelID() string
	// Model is the configured model name, for people to read.
	Model() string
}

// EmbedMode is how a search query was embedded.
type EmbedMode string

const (
	// EmbedOff means no embedder is configured, so the search is lexical
	// only.
	EmbedOff EmbedMode = "off"
	// EmbedOn means the query was embedded and the search is hybrid.
	EmbedOn EmbedMode = "on"
	// EmbedFailed means the provider failed or missed the deadline, so the
	// search is lexical only.
	EmbedFailed EmbedMode = "failed"
)

// QueryEmbedding says how a search query was embedded.
type QueryEmbedding struct {
	Mode EmbedMode
	// Model is the configured model name; empty when Mode is EmbedOff.
	Model string
	// Reason is why the embedding failed, with URL queries redacted; set
	// only when Mode is EmbedFailed.
	Reason string
}

// queryKey identifies a cached query embedding. The model identity is part
// of it, so a vector is never reused under another model.
type queryKey struct{ modelID, query string }

// queryEmbedder embeds search queries through an LRU cache, bounded by the
// embedding.timeout deadline.
type queryEmbedder struct {
	log   zerolog.Logger
	cache *lru.Cache[queryKey, []float32]

	mu       sync.RWMutex
	embedder Embedder // nil: lexical search only
	timeout  time.Duration
}

func newQueryEmbedder(e Embedder, timeout time.Duration, log zerolog.Logger) *queryEmbedder {
	cache, err := lru.New[queryKey, []float32](queryCacheSize)

	if err != nil {
		panic(err) // only for a non-positive size, which queryCacheSize is not
	}

	q := &queryEmbedder{log: log, cache: cache}
	q.set(e, timeout)

	return q
}

func (q *queryEmbedder) set(e Embedder, timeout time.Duration) {
	if timeout <= 0 {
		timeout = config.DefaultEmbeddingTimeout
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	q.embedder, q.timeout = e, timeout
}

// embed returns text's vector, the model identity that produced it, and how
// the query was embedded. It returns a nil vector, so the search runs
// lexical-only, when no embedder is configured or the provider fails or
// misses the deadline; the reason is logged at debug level.
func (q *queryEmbedder) embed(ctx context.Context, text string) ([]float32, string, QueryEmbedding) {
	q.mu.RLock()
	e, timeout := q.embedder, q.timeout
	q.mu.RUnlock()

	if e == nil {
		return nil, "", QueryEmbedding{Mode: EmbedOff}
	}

	key := queryKey{e.ModelID(), text}
	how := QueryEmbedding{Mode: EmbedOn, Model: e.Model()}

	if v, ok := q.cache.Get(key); ok {
		return v, key.modelID, how
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	vecs, err := e.Embed(ctx, []string{text})

	if err == nil && len(vecs) != 1 {
		err = errors.New("provider returned no vector for the query")
	}

	if err != nil {
		q.log.Debug().Err(err).Int64("timeout_ms", timeout.Milliseconds()).Msg("query embedding failed, searching lexically")
		how.Mode, how.Reason = EmbedFailed, redact.URLQueries(err.Error())

		return nil, "", how
	}

	q.cache.Add(key, vecs[0])

	return vecs[0], key.modelID, how
}
