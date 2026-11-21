// Package embed is the client for an embedding provider: any
// OpenAI-compatible POST {base_url}/embeddings endpoint (design §2, §4). It
// is a leaf below catalog; it knows nothing about tools or the store.
//
// # Contract
//
//   - The caller resolves embedding.api_key (a ${env:…}/${secret:…}
//     template) and passes the plain value in Config.APIKey. This package
//     never resolves templates. An empty key sends no Authorization header;
//     otherwise it is sent as "Authorization: Bearer <key>". The key is
//     never logged and is scrubbed from error text.
//   - Each request carries "model", "input" (an array of strings), and
//     "dimensions". Dimensions is required by the config, so it is always
//     sent. OpenAI text-embedding-3-* use it to shorten vectors; servers
//     that ignore it are caught by the length check below, and servers that
//     reject it fail with a *StatusError (usually HTTP 400).
//   - Every returned vector must have exactly Config.Dimensions entries;
//     otherwise Embed returns a *DimensionError. Vectors are returned in
//     input order, using each item's "index".
//   - Inputs go in batches of BatchSize (64), sequentially. A failed batch
//     fails the whole call with no partial result.
//   - There is no client-side timeout: the deadline comes from ctx (the
//     daemon uses embedding.timeout for query embeddings). Errors from a
//     cancelled or expired ctx satisfy errors.Is with context.Canceled or
//     context.DeadlineExceeded.
//   - The model identity (ModelID) is documented on ModelID.
//
// # Manual check
//
// The unit tests use httptest only. To check a real provider, set these and
// run the live test, which is skipped when WAYFINDR_EMBED_LIVE_URL is unset:
//
//	WAYFINDR_EMBED_LIVE_URL=https://api.openai.com/v1 \
//	WAYFINDR_EMBED_LIVE_MODEL=text-embedding-3-small \
//	WAYFINDR_EMBED_LIVE_DIMENSIONS=512 \
//	WAYFINDR_EMBED_LIVE_KEY=sk-… \
//	go test ./internal/embed -run TestLive -v
//
// For Ollama (after `ollama pull qwen3-embedding:0.6b`), use
// WAYFINDR_EMBED_LIVE_URL=http://localhost:11434/v1,
// WAYFINDR_EMBED_LIVE_MODEL=qwen3-embedding:0.6b, and the model's native
// size, 1024 (or a smaller one: Ollama honours "dimensions"), for
// WAYFINDR_EMBED_LIVE_DIMENSIONS; leave the key unset. The test embeds 70
// inputs (two batches) and checks the count, the lengths, and that distinct
// inputs give distinct vectors.
package embed
