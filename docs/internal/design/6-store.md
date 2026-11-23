# §6 Store interface

Part of the [Wayfindr technical design](../design.md). Go code cites this file
as "design §6".

The interface and its row types are in `internal/store/store.go`; each method's
doc comment is its contract, and the shared suite (`internal/store/storetest`)
checks every driver against it. The methods fall into the catalog and its sync
bookkeeping (`ApplySync`, `DeleteCatalog`, `ToolCounts`, `GetTool`,
`PendingEmbeddings`, `SetEmbeddings`, `Search`, `LastSync`), opaque-byte
persistence that callers seal (`GetCredential`, `PutCredential`,
`DeleteCredential`, `ListCredentialConnectors`, `GetSecret`, `PutSecret`,
`DeleteSecret`, `HasSealedData`, `GetMetadata`, `PutMetadata`), the call log
(`RecordCall`, `ListCalls`, `PruneCalls`), scoped client tokens, digests
only (`CreateClientToken`, `ListClientTokens`, `DeleteClientToken`,
`TouchClientTokens`), and OAuth grants and their tokens, digests only
(`CreateOAuthGrant`, `GetOAuthGrant`, `ListOAuthGrants`, `DeleteOAuthGrant`,
`GetOAuthToken`, `ListOAuthAccessTokens`, `IssueOAuthTokens`,
`RotateOAuthTokens`, `TouchOAuthGrants`, `PruneOAuth`; their contracts are in
[§7.11](7.11-oauthserver.md)). Why they are shaped as they are:

- **`Tool`** holds `Connector`, `Name`, `Definition json.RawMessage`,
  `ContentHash`, `Embedding`, `EmbeddingModel`, and the parsed fields the index
  needs (`Title`, `Description`, `Params`). The parsed fields are not stored:
  `store.NewTool` derives them from the stored definition when a row is read, so
  they always agree with it. The title falls back to `annotations.title`.
- **`ApplySync`** stores what one sync found, catalog and sync record in one
  transaction. A successful sync makes `o.Tools` the connector's complete
  catalog: it upserts every tool, deletes rows not in `o.Tools`, and returns the
  sorted names whose content hash changed. New tools count as changed. A changed
  tool's embedding is cleared, so it becomes pending. A failed sync (`o.Error`
  set) records only `At` and `Error`; the catalog stays as it was, and so the
  record keeps the last success's hidden tools. Both drivers execute the same
  plan, `store.PlanCatalog(connector, existing, tools)`, a pure function of the
  stored content hashes and the new list: it validates the list (every
  definition parses, each name matches its definition, no name twice; a list it
  rejects fails the call and writes nothing) and sorts the tools into insert,
  update (hash changed), touch (same hash, new definition), and delete.
- **`DeleteCatalog`** drops the connector's tools and its sync record in one
  transaction; its credential stays.
- **`SetEmbeddings`** skips any vector whose `ContentHash` no longer matches the
  row (the tool changed or was removed since `PendingEmbeddings` read it), so a
  stale embedding is never stored.
- **`HasSealedData`** reports whether any credential or secret row exists, that
  is, ciphertext a new master key could not open. `init` checks it before it
  generates a key ([§7.1](7.1-secrets.md)).
- **`DeleteSecret`** removes one user secret, or returns `ErrNotFound`
  (`wayfindr secret remove`, through `DELETE /admin/secrets/{name}`). It
  reconnects nothing: a connector that resolved the value keeps it until its
  next connect, which then fails `missing_value`.
- **`ListSecrets`** returns the sorted names of one user's secrets, never their
  values (`wayfindr secret list`, through `GET /admin/secrets`).
- **`Search`** takes a `SearchQuery` (text, an optional vector with the model
  identity that made it, a connector filter, page and size, and `Explain`) and
  returns a page with `HasMore` and, when asked, each hit's `Score`
  ([§7.5](7.5-search.md)).
- **Migrations.** `store.Migrate(ctx, db, fsys)` is the one runner every driver
  shares; drivers supply only their `NNNN_name.sql` files. The runner owns
  `schema_version`: it creates the table, applies each newer migration in its
  own transaction together with the version update, and returns
  `ErrSchemaTooNew` when the database is past the newest migration.
