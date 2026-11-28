// Package store defines the persistence seam: the Store interface, its row
// types, and the migration runner shared by every driver. The implementations
// live in internal/store/sqlite and internal/store/postgres; each one also
// implements hybrid search itself (ADR 0005).
package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned by the Get methods when no row matches.
var ErrNotFound = errors.New("not found")

// ErrExists is returned by the Create methods when a row with the same key
// already exists.
var ErrExists = errors.New("already exists")

// Store is the persistence backend. Every row is scoped by user ID; in local
// mode there is exactly one user, returned by EnsureUser.
//
// User IDs are UUIDs, as EnsureUser makes them. Given anything else, a
// driver may fail with its own error instead of ErrNotFound or an empty
// result: Postgres stores user IDs as uuid and rejects the value as invalid
// input (SQLSTATE 22P02), where SQLite finds no row. Either way no row is
// returned.
type Store interface {
	// EnsureUser returns the single local user, creating it on first use.
	EnsureUser(ctx context.Context) (userID string, err error)

	// ApplySync stores what one sync of o.Connector found, catalog and sync
	// record together in one transaction, so neither is ever written
	// without the other.
	//
	// A successful sync (o.Error empty) makes o.Tools the connector's
	// complete catalog, as PlanCatalog plans it: it upserts every tool,
	// deletes rows not in o.Tools, and returns the sorted names whose
	// content hash changed (new tools included). A changed tool's embedding
	// is cleared, so it becomes pending. The record gets o.At and o.Hidden.
	// A list PlanCatalog rejects fails the call and writes nothing.
	//
	// A failed sync records o.At and o.Error, which the caller has already
	// redacted, and ignores o.Tools and o.Hidden: the catalog stays as it
	// was, and so the record keeps the hidden tools of the last successful
	// sync.
	ApplySync(ctx context.Context, userID string, o SyncOutcome) (changed []string, err error)
	// DeleteCatalog drops the connector's catalog and its last-sync record
	// in one transaction, as when the connector is removed; its credential
	// stays. Deleting a missing one is not an error.
	DeleteCatalog(ctx context.Context, userID, connector string) error
	// ToolCounts returns how many catalog entries each of the user's
	// connectors has. A connector with none is absent from the map.
	ToolCounts(ctx context.Context, userID string) (map[string]int, error)
	// GetTool returns one catalog entry, or ErrNotFound.
	GetTool(ctx context.Context, userID, connector, name string) (Tool, error)
	// PendingEmbeddings returns up to limit entries whose embedding was not
	// produced by modelID, ordered by connector and name.
	PendingEmbeddings(ctx context.Context, userID, modelID string, limit int) ([]Tool, error)
	// SetEmbeddings stores vectors produced by modelID. A vector whose
	// content hash no longer matches the row (the tool changed or was removed
	// since it was read) is skipped, so a stale embedding is never stored.
	SetEmbeddings(ctx context.Context, userID, modelID string, vecs []ToolVector) error
	// Search runs hybrid search over the catalog (design §7.5): the lexical
	// candidates and, when q.Vector is set, the 50 nearest entries by cosine
	// (above 0, embedded under q.ModelID), each ranked over the user's whole
	// catalog, fused by reciprocal-rank fusion (k = 5, ties by qualified
	// name), then filtered by q.Connectors and paginated. A tool that
	// q.Text names exactly (its words, unfolded, equal those of the tool's
	// name or qualified name) ranks ahead of the rest, several such tools in
	// fused order. With q.Explain, the page also holds each result's Score,
	// parallel to its tools.
	Search(ctx context.Context, q SearchQuery) (SearchPage, error)

	// LastSync returns the last recorded sync of a connector, or ErrNotFound.
	LastSync(ctx context.Context, userID, connector string) (SyncRecord, error)

	// Opaque-byte persistence. Sealing happens in callers via secrets.Sealer.

	// GetCredential returns a connector's credential, or ErrNotFound.
	GetCredential(ctx context.Context, userID, connector string) (Credential, error)
	// PutCredential inserts or replaces a credential.
	PutCredential(ctx context.Context, c Credential) error
	// DeleteCredential removes a credential; deleting a missing one is not an error.
	DeleteCredential(ctx context.Context, userID, connector string) error
	// ListCredentialConnectors returns the sorted connector names that have a credential.
	ListCredentialConnectors(ctx context.Context, userID string) ([]string, error)
	// GetSecret returns a sealed secret value, or ErrNotFound.
	GetSecret(ctx context.Context, userID, name string) ([]byte, error)
	// PutSecret inserts or replaces a sealed secret value.
	PutSecret(ctx context.Context, userID, name string, sealed []byte) error
	// DeleteSecret removes a secret, or returns ErrNotFound when there is none.
	DeleteSecret(ctx context.Context, userID, name string) error
	// ListSecrets returns the sorted names of the user's secrets, never
	// their values.
	ListSecrets(ctx context.Context, userID string) ([]string, error)
	// HasSealedData reports whether any user has a credential or secret row,
	// that is, data a new master key could not open. `wayfindr init` asks
	// before it generates a key (design §7.1).
	HasSealedData(ctx context.Context) (bool, error)
	// GetMetadata returns a cached OAuth metadata document, or ErrNotFound.
	GetMetadata(ctx context.Context, url string) (Metadata, error)
	// PutMetadata inserts or replaces a cached OAuth metadata document.
	PutMetadata(ctx context.Context, m Metadata) error

	// The call log (GLOSSARY: Call log).

	// RecordCall stores one call_tool call under a new UUIDv7 ID; c.ID is
	// ignored. StartedAt is kept to the millisecond.
	RecordCall(ctx context.Context, c Call) error
	// ListCalls returns the user's calls that match f, newest first (ties by
	// ID, newest first), at most f.Limit of them. f.Limit must be positive.
	ListCalls(ctx context.Context, f CallFilter) ([]Call, error)
	// PruneCalls deletes the user's calls that started before before, and
	// returns how many it deleted.
	PruneCalls(ctx context.Context, userID string, before time.Time) (int64, error)

	// Scoped client tokens (docs/internal/plans/scoped-client-tokens.md).
	// Only a token's digest is stored, never its value.

	// CreateClientToken stores a scoped client token. It fails with an
	// error wrapping ErrExists when the user already has a token of that
	// name (or an OAuth grant has it) or any user has one with that digest,
	// and refuses an empty t.Connectors or a zero t.CreatedAt. CreatedAt is
	// kept to the millisecond; t.LastUsedAt is ignored. t.Remote is stored
	// as given and is not changed afterwards.
	CreateClientToken(ctx context.Context, t ClientToken) error
	// ListClientTokens returns the user's scoped client tokens, digests and
	// Remote included, ordered by name.
	ListClientTokens(ctx context.Context, userID string) ([]ClientToken, error)
	// DeleteClientToken removes a scoped client token, or returns
	// ErrNotFound.
	DeleteClientToken(ctx context.Context, userID, name string) error
	// TouchClientTokens records when each token, keyed by its digest, was
	// last used, in one transaction, kept to the millisecond. A token's
	// LastUsedAt never moves backwards, and a digest with no token (one
	// revoked since it was used) is ignored, even if a new token has taken
	// the old one's name.
	TouchClientTokens(ctx context.Context, userID string, used map[[sha256.Size]byte]time.Time) error

	// OAuth grants and their tokens (ADR 0022, design §7.11). A grant's
	// name is in the name space scoped client token names are in. Only a
	// token's digest is stored, never its value. Every time is kept to the
	// millisecond.

	// CreateOAuthGrant stores g. It fails with an error wrapping ErrExists
	// when the user has an OAuth grant or a scoped client token of that
	// name, and refuses an empty Connectors, ClientID, or Resource, or a
	// zero CreatedAt. LastUsedAt is ignored.
	CreateOAuthGrant(ctx context.Context, g OAuthGrant) error
	// GetOAuthGrant returns the grant called name, or ErrNotFound.
	GetOAuthGrant(ctx context.Context, userID, name string) (OAuthGrant, error)
	// ListOAuthGrants returns the user's grants, ordered by name.
	ListOAuthGrants(ctx context.Context, userID string) ([]OAuthGrant, error)
	// DeleteOAuthGrant removes the grant and, by cascade, its tokens, or
	// returns ErrNotFound.
	DeleteOAuthGrant(ctx context.Context, userID, name string) error
	// GetOAuthToken returns the user's token with digest d, or ErrNotFound.
	GetOAuthToken(ctx context.Context, userID string, d [sha256.Size]byte) (OAuthToken, error)
	// ListOAuthAccessTokens returns the user's access tokens that expire
	// after now, ordered by grant name.
	ListOAuthAccessTokens(ctx context.Context, userID string, now time.Time) ([]OAuthToken, error)
	// IssueOAuthTokens stores a grant's first access and refresh tokens in
	// one transaction (a code exchange). Both must name the same grant,
	// which must exist (else ErrNotFound) and have no token yet (else
	// ErrExists); access must be OAuthAccess with ExpiresAt, refresh
	// OAuthRefresh without, and neither UsedAt.
	IssueOAuthTokens(ctx context.Context, userID string, access, refresh OAuthToken) error
	// RotateOAuthTokens spends the refresh token spent and stores its
	// successors, in one transaction: it sets spent's UsedAt to at only if
	// it is unset (one conditional UPDATE, so two racing rotations can't
	// both win), deletes the grant's access tokens, and inserts access and
	// refresh, which must name spent's grant; a zero at is refused. Spent
	// refresh tokens stay
	// (PruneOAuth drops them after OAuthSpentRetention). A refresh token
	// already spent is ErrSpent and changes nothing; no refresh token with
	// that digest is ErrNotFound. A failed rotation leaves spent unspent.
	RotateOAuthTokens(ctx context.Context, userID string, spent [sha256.Size]byte, at time.Time, access, refresh OAuthToken) error
	// TouchOAuthGrants records, for each digest that is one of the user's
	// access tokens, that its grant was last used at that time, in one
	// transaction. LastUsedAt never moves backwards; other digests (client
	// tokens, revoked grants) are ignored.
	TouchOAuthGrants(ctx context.Context, userID string, used map[[sha256.Size]byte]time.Time) error
	// PruneOAuth deletes the user's access tokens that expired at or
	// before now, the refresh tokens spent at or before now minus
	// OAuthSpentRetention, and the grants that have no token and were
	// created at or before now minus OAuthUnexchangedTTL (approved, never
	// exchanged), and returns how many rows it deleted.
	PruneOAuth(ctx context.Context, userID string, now time.Time) (int64, error)

	// Close releases the database.
	Close() error
}

// Tool is one catalog entry. Definition is the upstream mcp.Tool JSON exactly
// as received; Title, Description, and Params are parsed from it (see
// NewTool) and are not stored separately.
type Tool struct {
	Connector   string
	Name        string // upstream (unqualified) tool name
	Definition  json.RawMessage
	ContentHash []byte // sha256 of the embed text

	Title       string
	Description string
	Params      []Param  // top-level inputSchema properties, sorted by name
	Required    []string // top-level inputSchema required names, as listed

	Embedding      []float32 // nil until embedded
	EmbeddingModel string    // model identity that produced Embedding
}

// Param is one top-level input parameter of a tool.
type Param struct {
	Name        string
	Description string
}

// ToolVector is an embedding for one catalog entry. ContentHash is the hash
// of the text that was embedded.
type ToolVector struct {
	Connector   string
	Name        string
	ContentHash []byte
	Vector      []float32
}

// SearchQuery is one search_tools request.
type SearchQuery struct {
	UserID     string
	Text       string
	Vector     []float32 // query embedding; nil → lexical only
	ModelID    string    // identity that produced Vector; only entry vectors with it are compared
	Connectors []string  // empty = all
	Page, Size int
	Explain    bool // fill SearchPage.Scores
}

// SearchPage is one page of ranked results.
type SearchPage struct {
	Tools []Tool
	// Scores[i] says why Tools[i] ranked where it did. Set only when the
	// query's Explain is true.
	Scores  []Score
	HasMore bool
}

// Score is one result's place in each ranking hybrid search fuses (design
// §7.5). A rank counts from 1 over the user's whole catalog, before the
// connector filter; 0 means the result is not in that ranking, and its
// score is then 0.
type Score struct {
	LexicalRank int
	Lexical     float64 // BM25F
	VectorRank  int
	Cosine      float64
	Fused       float64 // Σ 1/(5 + rank) over both rankings
	// ExactName is set when the query names the tool exactly (design
	// §7.5); such a result ranks ahead of every other, whatever its Fused.
	ExactName bool
}

// SyncOutcome is what one sync of a connector found (GLOSSARY: Sync).
type SyncOutcome struct {
	Connector string
	At        time.Time
	// Error is the sync's failure, already redacted (design §9); empty when
	// the sync succeeded.
	Error string
	// Tools is the connector's complete catalog; ignored when Error is set.
	Tools []Tool
	// Hidden is the tools the connector's tool policy kept out of the
	// catalog; ignored when Error is set.
	Hidden []HiddenTool
}

// SyncRecord is the last recorded sync of a connector.
type SyncRecord struct {
	At    time.Time
	Error string // empty when the sync succeeded
	// Hidden is the tools the last successful sync kept out of the catalog,
	// in the order recorded; nil when there are none.
	Hidden []HiddenTool
}

// HiddenTool is an upstream tool the connector's tool policy hides
// (GLOSSARY: Hidden tool). Class is its tool class (config.ToolClass).
type HiddenTool struct {
	Name  string `json:"name"`
	Class string `json:"class"`
}

// Credential is what the daemon holds for one connector. DCRClient and Token
// are sealed by the caller; the store treats them as opaque bytes.
type Credential struct {
	UserID          string
	Connector       string
	Resource        string // canonical MCP URL the tokens are bound to (RFC 8707)
	Issuer          string
	RedirectURI     string // URI the DCR client was registered with
	DCRClientID     string // set only when obtained via DCR
	ClientID        string // the client the grant was obtained with, manual or DCR; empty means unknown
	DCRClient       []byte // sealed
	Token           []byte // sealed
	GrantedScopes   string
	RequestedScopes string // includes step-up additions
}

// MetadataKind says which OAuth metadata document a Metadata row holds.
type MetadataKind string

// Metadata kinds.
const (
	MetadataPRM MetadataKind = "prm" // protected-resource metadata
	MetadataAS  MetadataKind = "as"  // authorization-server metadata
)

// Metadata is a cached OAuth metadata document.
type Metadata struct {
	URL       string // document URL actually fetched
	Kind      MetadataKind
	Document  []byte
	FetchedAt time.Time
}

// CallResult is how a call_tool call ended.
type CallResult string

// Call results.
const (
	// CallOK: the upstream returned a result without isError.
	CallOK CallResult = "ok"
	// CallToolError: the upstream returned a result with isError.
	CallToolError CallResult = "tool_error"
	// CallRejected: the gateway refused the call before calling the
	// upstream (bad name, unknown tool, arguments not matching the schema).
	CallRejected CallResult = "rejected"
	// CallFailed: Wayfindr or the upstream call failed (catalog unreadable,
	// auth, unreachable, timeout, input the client can't be asked for).
	CallFailed CallResult = "failed"
	// CallInputRequired: the upstream asked the user something, and the
	// call returned the question to the client, which answers with another
	// call (GLOSSARY: Elicitation).
	CallInputRequired CallResult = "input_required"
)

// Call is one row of the call log: one call_tool call. It never holds the
// call's arguments or result, only the error text of one that did not end
// ok (ADR 0014).
type Call struct {
	ID     string // UUIDv7, set by RecordCall
	UserID string
	// Client is the clientInfo name the calling client asserts; empty when
	// unknown.
	Client string
	// TokenName names the client token the call authenticated with:
	// "default" or a scoped token's name, never a token value. Unlike
	// Client, the server checked it. Empty when unknown, as for calls
	// logged before tokens were recorded.
	TokenName string
	// Connector and Tool are as the agent wrote them, even when no such
	// connector or tool exists; empty when the name had no such part.
	Connector string
	Tool      string
	StartedAt time.Time
	// Duration is how long Wayfindr spent on the upstream call, connecting
	// included; nil when the call was never handed to the upstream. A
	// connector that fails at once, as one in needs_auth does, takes ~0.
	Duration *time.Duration
	Result   CallResult
	// ErrorKind and ErrorCase are the gateway's or upstream's Kind and Case
	// of a call that did not end ok; empty otherwise or when unknown.
	ErrorKind string
	ErrorCase string
	// ErrorText is the error text the agent got back from a call that did
	// not end ok: the upstream's text for a tool_error, the gateway's
	// message otherwise. The gateway cleans and clips it (design §7.6);
	// empty for ok and input_required calls, or when unknown.
	ErrorText string
	// ResultBytes is the size of the result the agent got back: the length
	// of its JSON encoding, content and structuredContent included. It is
	// metadata, never the result. Nil when no upstream result came back (a
	// refusal, a failure), for an input_required call, and for calls logged
	// before the column.
	ResultBytes *int64
}

// ClientToken is a scoped client token (GLOSSARY: Client token): a name
// and the connectors it may use. The token value is never stored, only its
// SHA-256 (clienttoken.Digest).
type ClientToken struct {
	UserID string
	Name   string
	Digest [sha256.Size]byte
	// Connectors are the connectors the token may use; at least one.
	Connectors []string
	// Remote is whether the token may reach /mcp through a tunnel (ADR
	// 0021). It is set at creation and never changes.
	Remote    bool
	CreatedAt time.Time
	// LastUsedAt is when the token was last used, as TouchClientTokens
	// recorded it; nil until first use.
	LastUsedAt *time.Time
}

// ErrSpent is returned by RotateOAuthTokens when the refresh token was
// already spent: a reuse, which revokes the grant (ADR 0022).
var ErrSpent = errors.New("already spent")

const (
	// OAuthSpentRetention is how long a spent refresh token is kept, so
	// that presenting it again is seen as a reuse.
	OAuthSpentRetention = 30 * 24 * time.Hour
	// OAuthUnexchangedTTL is how long a grant with no token (approved, its
	// code never exchanged) is kept.
	OAuthUnexchangedTTL = 10 * time.Minute
)

// OAuthGrant is an OAuth grant (GLOSSARY): an allowed client's access,
// approved in the console, to its connectors. Its name is in the name
// space ClientToken names are in.
type OAuthGrant struct {
	UserID string
	Name   string
	// ClientID is the allowed client's CIMD URL.
	ClientID string
	// Resource is <public_url>/mcp when the grant was approved (RFC 8707).
	Resource string
	// Connectors are the connectors the grant may use; at least one.
	Connectors []string
	CreatedAt  time.Time
	// LastUsedAt is when its access token was last used, as
	// TouchOAuthGrants recorded it; nil until first use.
	LastUsedAt *time.Time
}

// OAuthTokenKind is what an OAuthToken is.
type OAuthTokenKind string

const (
	// OAuthAccess is a short-lived token that authenticates /mcp requests.
	OAuthAccess OAuthTokenKind = "access"
	// OAuthRefresh is a token that is spent by being exchanged for a new
	// pair.
	OAuthRefresh OAuthTokenKind = "refresh"
)

// OAuthToken is one access or refresh token of a grant. Only its SHA-256
// is stored (clienttoken.Digest).
type OAuthToken struct {
	UserID string
	Digest [sha256.Size]byte
	// Grant is the grant's name.
	Grant     string
	Kind      OAuthTokenKind
	CreatedAt time.Time
	// ExpiresAt is set for an access token, nil for a refresh token.
	ExpiresAt *time.Time
	// UsedAt is when a refresh token was spent; nil until then, and always
	// nil for an access token.
	UsedAt *time.Time
}

// CallFilter selects calls for ListCalls.
type CallFilter struct {
	UserID     string
	Connectors []string  // empty = all
	Since      time.Time // zero = no lower bound; otherwise StartedAt >= Since
	ErrorsOnly bool      // only calls whose Result is not CallOK
	Limit      int
}
