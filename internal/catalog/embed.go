package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/atomicpages/wayfindr/internal/embed"
	"github.com/atomicpages/wayfindr/internal/store"
)

// EmbedPending runs the embedding pass and waits for its outcome. The pass
// embeds, in batches of embed.BatchSize, every catalog entry whose embedding
// is missing or was made under another model identity. It stops at the
// first failed batch: the entries left stay pending, and lexically
// searchable, until a later pass. With no embedder it does nothing.
//
// If a pass is already running, EmbedPending waits for the rerun queued
// behind it. ctx bounds only the wait. Once Close has begun, EmbedPending
// returns ErrClosed, and Close stops a running pass.
func (s *Syncer) EmbedPending(ctx context.Context) error {
	return s.wait(ctx, s.enqueue(ctx, job{embed: true}, true))
}

// RequestEmbed starts the embedding pass in the background and returns at
// once; a failure is logged. The daemon calls it at startup, and every
// successful sync calls it. ctx supplies values only. With no embedder, or
// once Close has begun, RequestEmbed does nothing.
func (s *Syncer) RequestEmbed(ctx context.Context) {
	if s.currentEmbedder() == nil {
		return
	}

	if s.enqueue(ctx, job{embed: true}, false) == nil {
		s.log.Debug().Msg("catalog embedding request after close ignored")
	}
}

// SetEmbedder replaces the embedder, as on an EmbeddingChanged config
// reload, and starts an embedding pass in the background, which re-embeds
// the whole catalog when the model identity changed. A nil e (an untyped
// nil interface, not a nil *embed.Client) removes the embedder: entries
// keep their vectors but none are embedded. A running pass switches to e
// from its next batch.
func (s *Syncer) SetEmbedder(ctx context.Context, e Embedder) {
	s.mu.Lock()
	s.embedder = e
	s.mu.Unlock()
	s.RequestEmbed(ctx)
}

func (s *Syncer) currentEmbedder() Embedder {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.embedder
}

// embedOnce runs one embedding pass. It reads the embedder before every
// batch, so a pass running when the embedder is replaced embeds the rest
// under the new model. Its ctx is cancelled when Close begins.
func (s *Syncer) embedOnce(ctx context.Context) error {
	start := s.now()
	embedded := 0
	var lastFirst string // the first entry of the previous batch, to detect a stall

	for {
		e := s.currentEmbedder()

		if e == nil {
			return nil
		}

		model := e.ModelID()
		batch, err := s.store.PendingEmbeddings(ctx, s.userID, model, embed.BatchSize)

		if err != nil {
			return s.passErr(fmt.Errorf("list pending embeddings: %w", err))
		}

		if len(batch) == 0 {
			if embedded > 0 {
				s.log.Debug().Int("entries", embedded).
					Int64("duration_ms", s.now().Sub(start).Milliseconds()).Msg("catalog embedded")
			}

			return nil
		}

		// A batch that was stored leaves the pending set, so seeing the same
		// first entry again, under the same model and content hash, means
		// the store kept none of it; stop rather than call the provider
		// forever.
		first := fmt.Sprintf("%s\x00%s\x00%s\x00%x", model, batch[0].Connector, batch[0].Name, batch[0].ContentHash)

		if first == lastFirst {
			return fmt.Errorf("embedding pass made no progress at %s.%s", batch[0].Connector, batch[0].Name)
		}

		lastFirst = first

		texts := make([]string, len(batch))

		for i, t := range batch {
			texts[i] = EmbedText(t)
		}

		vecs, err := s.embedBatch(ctx, e, texts)

		if err != nil {
			return s.passErr(fmt.Errorf("embed %d entries: %w", len(batch), err))
		}

		if len(vecs) != len(batch) {
			return fmt.Errorf("embed %d entries: embedder returned %d vectors", len(batch), len(vecs))
		}

		if err := s.storeEmbeddings(ctx, model, toolVectors(batch, vecs)); err != nil {
			return s.passErr(fmt.Errorf("store embeddings: %w", err))
		}

		embedded += len(batch)
	}
}

// embedBatch embeds texts with e within the batch timeout. A provider that
// takes the request and never answers fails the batch, as any provider
// error does, rather than stall the pass until Close (wayfindr-gxnw).
func (s *Syncer) embedBatch(ctx context.Context, e Embedder, texts []string) ([][]float32, error) {
	bctx, cancel := context.WithTimeout(ctx, s.embedTimeout)
	defer cancel()
	vecs, err := e.Embed(bctx, texts)

	if err != nil && ctx.Err() == nil && errors.Is(bctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("no answer within %s: %w", s.embedTimeout, err)
	}

	return vecs, err
}

// storeEmbeddings stores vecs in the write slot, unless Close has begun:
// an embedder that ignored Close's cancellation may return after the store
// has closed (wayfindr-t08.7).
func (s *Syncer) storeEmbeddings(ctx context.Context, model string, vecs []store.ToolVector) error {
	s.writing <- struct{}{}
	defer func() { <-s.writing }()

	if s.isClosed() {
		return ErrClosed
	}

	return s.store.SetEmbeddings(ctx, s.userID, model, vecs)
}

// passErr is err, or ErrClosed with it if Close stopped the run, a sync or
// the embedding pass.
func (s *Syncer) passErr(err error) error {
	select {
	case <-s.closing:
		return errors.Join(ErrClosed, err)
	default:
		return err
	}
}

func toolVectors(batch []store.Tool, vecs [][]float32) []store.ToolVector {
	tvs := make([]store.ToolVector, len(batch))

	for i, t := range batch {
		tvs[i] = store.ToolVector{
			Connector:   t.Connector,
			Name:        t.Name,
			ContentHash: t.ContentHash,
			Vector:      vecs[i],
		}
	}

	return tvs
}
