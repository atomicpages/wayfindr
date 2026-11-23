// Package oauthserver is Wayfindr's authorization server for remote clients
// (ADR 0022, design §7.11): it lets a client such as ChatGPT, which cannot
// send a static header, get a grant to /mcp through the user's own tunnel.
// Consent is given only in the loopback console, and only while the user
// has armed a connection there.
//
// This file holds the names the daemon's token service and the server share;
// the handlers live beside it.
package oauthserver

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/atomicpages/wayfindr/internal/store"
)

// Lifetimes and caps (design §7.11, Limits).
const (
	AccessTTL        = time.Hour        // access token lifetime
	CodeTTL          = 60 * time.Second // authorization code lifetime; single use
	PendingTTL       = 10 * time.Minute // how long an arm, and a pending authorization, lasts
	RefreshGrace     = 30 * time.Second // a refresh token spent this recently is refused, not revoked
	MaxDocument      = 64 << 10         // CIMD body cap, bytes
	FetchTimeout     = 5 * time.Second  // one CIMD fetch
	DocumentTTL      = time.Hour        // a fetched, valid CIMD document is reused this long
	FailureTTL       = time.Minute      // a failed fetch is remembered this long
	MaxTokenBody     = 16 << 10         // token request body cap, bytes
	TokenReadTimeout = 10 * time.Second // reading one token request's body
	MaxPending       = 8                // pending authorizations kept while armed; past it the oldest goes
)

// Paths the server answers on. httpd keeps its own copies, since it cannot
// import this package; a daemon test checks they agree.
const (
	ResourceMetadataPath = "/.well-known/oauth-protected-resource"
	ServerMetadataPath   = "/.well-known/oauth-authorization-server"
	AuthorizePath        = "/oauth/authorize"
	TokenPath            = "/oauth/token" //nolint:gosec // G101: a path, not a credential
)

// Resource is the resource every grant is bound to: issuer + "/mcp".
func Resource(issuer string) string { return issuer + "/mcp" }

// ResourceMetadataURL is the URL a 401 challenge names:
// issuer + ResourceMetadataPath + "/mcp" (RFC 9728 §3.1).
func ResourceMetadataURL(issuer string) string {
	return issuer + ResourceMetadataPath + "/mcp"
}

// ErrSuspended is what Grants' issue and rotate return for a suspended
// grant: the authorization server is off, public_url changed since the
// grant was approved (its Resource is not the current one), or its client
// is no longer in server.oauth.clients. Undoing the change lifts it.
var ErrSuspended = errors.New("the grant is suspended")

// ErrNoRequest means a pending authorization id that is unknown, expired,
// not claimed, already answered, or no longer current (its resource
// changed, its client is no longer allowed, or the daemon disarmed the
// server).
var ErrNoRequest = errors.New("no such pending authorization")

// ErrContested is what Approve returns when more than one authorization
// request became pending during the arm the claimed request came from
// (Request.Arrived): one may be a stranger's, so none is approved. Deny
// still answers the request; the user arms again and retries.
var ErrContested = errors.New("more than one authorization request arrived during the arm")

// ErrInvalidName is what Grants.CreateGrant wraps for a name a client token
// could not have: not a connector name, or the default token's. The admin
// API checks the name before Approve, so this is its 400 should a name get
// past that check.
var ErrInvalidName = errors.New("invalid grant name")

// Grants is what the authorization server needs of the daemon's token
// service. The daemon's clientTokens implements it under the one lock it
// holds for client tokens, so grants and client tokens share one name
// space. UserID on every row passed in is ignored: the daemon sets its
// user.
type Grants interface {
	// CreateGrant stores g. A taken name, by a grant or a client token, is
	// an error wrapping store.ErrExists; a name a client token could not
	// have, one wrapping ErrInvalidName.
	CreateGrant(ctx context.Context, g store.OAuthGrant) error
	// Grant returns the grant called name, or store.ErrNotFound.
	Grant(ctx context.Context, name string) (store.OAuthGrant, error)
	// GrantToken returns the stored token with digest d, or
	// store.ErrNotFound.
	GrantToken(ctx context.Context, d [sha256.Size]byte) (store.OAuthToken, error)
	// IssueGrantTokens stores a grant's first tokens and starts accepting
	// access until its ExpiresAt: store.IssueOAuthTokens, then
	// Registry.AddUntil. created is the CreatedAt of the grant the code
	// was made for, compared to the millisecond (as the store keeps it):
	// a grant re-created under the same name since is another grant, and
	// gets no tokens. store.ErrNotFound when the grant is gone or was
	// re-created; ErrSuspended when it is suspended.
	IssueGrantTokens(ctx context.Context, created time.Time, access, refresh store.OAuthToken) error
	// RotateGrantTokens spends refresh token spent and stores and accepts
	// its successors: store.RotateOAuthTokens, then Registry.AddUntil,
	// which replaces the grant's previous access token. store.ErrSpent,
	// store.ErrNotFound, and ErrSuspended as there.
	RotateGrantTokens(ctx context.Context, spent [sha256.Size]byte, access, refresh store.OAuthToken) error
	// RevokeGrant deletes the grant called name with its tokens and stops
	// accepting its access token. It never deletes a client token.
	// store.ErrNotFound when there is no such grant.
	RevokeGrant(ctx context.Context, name string) error
}
