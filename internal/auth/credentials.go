package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/store"
)

// credentials is the one owner of the user's credentials rows (design
// §7.3): nothing else in auth reads or writes a row, seals or opens its
// token or DCR client, or knows their AAD. A [Broker] owns one and hands it
// to its [Registrar].
//
// Reads return a [grantView], opened. Writes are named, and each carries
// its rule:
//
//   - only [credentials.grant] (a new grant) and [credentials.register]
//     (the DCR client an auth flow will be granted to) may start a row;
//   - every other write amends the row that exists, and fails
//     [CaseNoCredential] when there is none, so a write that raced a revoke
//     cannot bring the credential back (wayfindr-zgp.16);
//   - [credentials.renew] and [credentials.forgetClient] compare and swap
//     on the token they read, so neither undoes a newer grant;
//   - every write is a read-modify-write under one lock, so writers of
//     different fields never overwrite each other (wayfindr-zgp.10).
//
// The lock is a plain mutex: there is one daemon (ADR 0003). Server mode
// would replace it, here only, with a DB lock (ADR 0004).
//
// Errors are [*Error]s made without the broker, so a needs_auth one has no
// link yet; the broker adds it ([Broker.withLink]).
type credentials struct {
	store  store.Store
	sealer *secrets.Sealer
	// userID owns the rows (the single local user).
	userID string
	// mu is held across each read-modify-write, each delete, and whatever
	// [credentials.locked] runs.
	mu sync.Mutex
}

// grantView is a credentials row as its readers see it: opened, and
// without the columns only the module needs.
type grantView struct {
	// Token is the opened token. It is zero in a view from
	// [credentials.stored], which does not open it.
	Token token
	// Client is the DCR client stored in the row, opened. It is nil when
	// there is none, and when it cannot be opened (see ClientErr).
	Client *Registration
	// ClientErr is why a stored DCR client cannot be opened, for example
	// after a key change; nil otherwise.
	ClientErr error
	// ClientGrantTypes are the grant_types the stored DCR client was
	// registered with; empty when it has none, or there is no client.
	ClientGrantTypes []string
	// Issuer and RedirectURI are what the DCR client was registered with.
	Issuer, RedirectURI string
	// RequestedScopes are the scopes a step-up added, for the next flow.
	RequestedScopes string
	// ClientID is the client the stored grant was obtained with, manual,
	// CIMD or DCR; empty when unknown (a row from before it was recorded).
	ClientID string

	// version is the sealed token as read (nil for none): a write that must
	// not undo a newer grant compares it with the row's.
	version []byte
}

// errTokenReplaced is a write refused because the stored token is no
// longer the one its writer read: a newer grant, or a refresh, came first.
var errTokenReplaced = errors.New("a newer token was stored meanwhile")

// tokenAAD binds a sealed token to its credentials row and column (design
// §7.1).
func tokenAAD(userID, connector string) []byte {
	return secrets.AAD("credentials", userID, connector, "token")
}

// dcrClientAAD binds a sealed DCR client to its credentials row and column
// (design §7.1).
func dcrClientAAD(userID, connector string) []byte {
	return secrets.AAD("credentials", userID, connector, "dcr_client")
}

// load returns the connector's grant for use at resource, the connector's
// configured URL, with its token and DCR client opened. A missing row or
// token, a token bound to another resource, and a token that cannot be
// opened are needs_auth errors ([CaseNoCredential], [CaseURLChanged],
// [CaseCredentialUnreadable]), so a stale token is never sent to a new URL.
// A token granted to another client than the one auth names is
// [CaseClientChanged], so it is never sent after a client_id change either:
// the client is auth's client_id, else its client_metadata_url if that is
// the one the grant has, else the row's DCR client. A grant with no client
// recorded is accepted. Store failures are [CaseStore] errors.
func (c *credentials) load(ctx context.Context, connector, resource string, auth config.Auth) (grantView, error) {
	cred, err := c.store.GetCredential(ctx, c.userID, connector)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return grantView{}, fail(CaseNoCredential, connector, "no credential stored", nil)
	case err != nil:
		return grantView{}, fail(CaseStore, connector, "read credential", err)
	case len(cred.Token) == 0:
		return grantView{}, fail(CaseNoCredential, connector, "no token stored", nil)
	case cred.Resource != resourceKey(resource):
		return grantView{}, fail(CaseURLChanged, connector, "the token is bound to another connector url", nil)
	case clientChanged(cred, auth):
		return grantView{}, fail(CaseClientChanged, connector, "the token was granted to another client", nil)
	}

	plain, err := c.sealer.Open(cred.Token, tokenAAD(c.userID, connector))

	if err != nil {
		return grantView{}, fail(CaseCredentialUnreadable, connector, "open stored token", err)
	}

	v := c.view(cred)

	if err := json.Unmarshal(plain, &v.Token); err != nil || v.Token.Access == "" {
		return grantView{}, fail(CaseCredentialUnreadable, connector, "the stored token is malformed", nil)
	}

	return v, nil
}

// clientChanged reports whether cred's grant was obtained with another
// client than the client_id a manual connector is configured with. Only a
// manual connector is checked: its client comes from config, so a change
// is the user's. A discovered connector's client is Wayfindr's own choice
// and changes on re-registration (a new redirect URI, say), while its
// grant is still good until a refresh, which fails on its own if the
// grant belongs to another client. A client_secret is not part of the
// client, so changing it alone never counts.
func clientChanged(cred store.Credential, auth config.Auth) bool {
	if cred.ClientID == "" || auth.ClientID == "" {
		return false // recorded before the binding, or a discovered connector
	}

	return cred.ClientID != auth.ClientID
}

// stored returns the connector's row as it stands, whatever URL its token
// is bound to, with its DCR client opened but not its token. With no row it
// returns the zero view. Store failures are [CaseStore] errors.
func (c *credentials) stored(ctx context.Context, connector string) (grantView, error) {
	cred, err := c.store.GetCredential(ctx, c.userID, connector)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return grantView{}, nil
	case err != nil:
		return grantView{}, fail(CaseStore, connector, "read credential", err)
	}

	return c.view(cred), nil
}

// view is cred as a [grantView], with its DCR client opened.
func (c *credentials) view(cred store.Credential) grantView {
	v := grantView{
		Issuer: cred.Issuer, RedirectURI: cred.RedirectURI, RequestedScopes: cred.RequestedScopes,
		ClientID: cred.ClientID, version: cred.Token,
	}

	if cred.DCRClientID == "" {
		return v
	}

	reg, err := c.openClient(cred)

	if err != nil {
		v.ClientErr = err

		return v
	}

	v.Client = &reg
	v.ClientGrantTypes = c.clientGrantTypes(cred)

	return v
}

// clientGrantTypes reads the grant_types of the DCR client sealed in cred,
// which openClient has opened already; nil if it has none.
func (c *credentials) clientGrantTypes(cred store.Credential) []string {
	plain, err := c.sealer.Open(cred.DCRClient, dcrClientAAD(c.userID, cred.Connector))

	if err != nil {
		return nil
	}

	var resp struct {
		GrantTypes []string `json:"grant_types"`
	}

	if json.Unmarshal(plain, &resp) != nil {
		return nil
	}

	return resp.GrantTypes
}

// openClient opens the DCR client sealed in cred, with the client
// authentication method it was registered with: a client registered for
// client_secret_post must not send its secret as basic.
func (c *credentials) openClient(cred store.Credential) (Registration, error) {
	plain, err := c.sealer.Open(cred.DCRClient, dcrClientAAD(c.userID, cred.Connector))

	if err != nil {
		return Registration{}, fmt.Errorf("open stored oauth client: %w", err)
	}

	var resp oauthex.ClientRegistrationResponse

	if err := json.Unmarshal(plain, &resp); err != nil || resp.ClientID != cred.DCRClientID {
		return Registration{}, errors.New("stored oauth client is malformed")
	}

	return registered(&resp), nil
}

// grant stores tok, a token from a new grant, bound to resource and to
// clientID, the client it was obtained with, with the granted scopes,
// starting the row if there is none. A non-nil check runs
// first under the lock: if it returns an error, nothing is written and that
// error is returned (the auth flow checks that it was not cancelled).
func (c *credentials) grant(ctx context.Context, connector string, tok token, resource, clientID, granted string, check func() error) error {
	sealed, err := c.sealToken(connector, tok)

	if err != nil {
		return err
	}

	return c.write(ctx, connector, true, func(cred *store.Credential) error {
		if check != nil {
			if err := check(); err != nil {
				return err
			}
		}

		cred.Token = sealed
		cred.Resource = resourceKey(resource)
		cred.ClientID = clientID
		cred.GrantedScopes = granted

		return nil
	})
}

// resourceKey is what a credential row stores for the connector url its
// token is bound to: the url's SHA-256, never the url, which may hold a
// secret in its path or host (wayfindr-wt2v). The binding only compares
// urls, so the digest serves it as the url would.
func resourceKey(resource string) string {
	sum := sha256.Sum256([]byte(resource))

	return "sha256:" + hex.EncodeToString(sum[:])
}

// register stores client, registered through DCR with issuer for
// redirectURI, in place of any older one. The rest of the row is kept, its
// token and resource binding included; a new row is bound to resource.
//
// Besides a grant, this is the one write that may start a row: an auth flow
// registers its client before it has a token, and the refresh of the token
// it is granted needs the client from the row. A row revoked during the
// registration is started again, with the new client and no token: the
// revoke cancels the registering flow, but the registration is shared by
// every flow of the connector and its client is not the revoked one, so it
// is kept for the next flow. No revoked token comes back with it.
func (c *credentials) register(ctx context.Context, connector string, client *oauthex.ClientRegistrationResponse, issuer, redirectURI, resource string) error {
	doc, err := json.Marshal(client)

	if err != nil {
		return fail(CaseStore, connector, "encode registered client", err)
	}

	sealed := c.sealer.Seal(doc, dcrClientAAD(c.userID, connector))

	return c.write(ctx, connector, true, func(cred *store.Credential) error {
		if cred.Resource == "" {
			cred.Resource = resourceKey(resource)
		}

		cred.Issuer = issuer
		cred.RedirectURI = redirectURI
		cred.DCRClientID = client.ClientID
		cred.DCRClient = sealed

		return nil
	})
}

// renew stores fresh, the refresh of seen's token, with the granted scope
// if the server reported one. It stores it only over the token it
// refreshed: if another token was stored since (a new grant, or another
// refresh), nothing is written and the error is errTokenReplaced; if the
// row was deleted (a revoke), it is [CaseNoCredential]. clientID is the
// client that refreshed: the grant keeps the client it has, and a grant
// with none recorded takes clientID, which the refresh proved holds it.
func (c *credentials) renew(ctx context.Context, connector string, seen grantView, fresh token, clientID, granted string) error {
	sealed, err := c.sealToken(connector, fresh)

	if err != nil {
		return err
	}

	return c.write(ctx, connector, false, func(cred *store.Credential) error {
		if !bytes.Equal(cred.Token, seen.version) {
			return errTokenReplaced
		}

		cred.Token = sealed

		if cred.ClientID == "" {
			cred.ClientID = clientID
		}

		if granted != "" {
			cred.GrantedScopes = granted
		}

		return nil
	})
}

// addScopes merges scopes into the row's requested_scopes, together with
// the scopes already requested and granted, so the next auth flow asks for
// the larger set (ADR 0004 step-up). It returns the merged set. With no row
// (revoked meanwhile) the scopes are dropped and the error is
// [CaseNoCredential].
func (c *credentials) addScopes(ctx context.Context, connector string, scopes []string) (string, error) {
	var merged string
	err := c.write(ctx, connector, false, func(cred *store.Credential) error {
		var all []string

		for _, s := range slices.Concat(strings.Fields(cred.RequestedScopes), strings.Fields(cred.GrantedScopes), scopes) {
			if !slices.Contains(all, s) {
				all = append(all, s)
			}
		}

		merged = strings.Join(all, " ")
		cred.RequestedScopes = merged

		return nil
	})

	return merged, err
}

// forgetClient clears the DCR client clientID, which the token endpoint
// rejected, so the next auth flow registers a new one. It forgets it only
// if the row still holds seen's token: a token stored since was granted
// to, or refreshed with, a client the server accepted, so the error is
// errTokenReplaced and the client is kept. A client registered since is
// not clientID, and is kept too. With no row the error is
// [CaseNoCredential].
func (c *credentials) forgetClient(ctx context.Context, connector, clientID string, seen grantView) error {
	return c.write(ctx, connector, false, func(cred *store.Credential) error {
		if !bytes.Equal(cred.Token, seen.version) {
			return errTokenReplaced
		}

		if cred.DCRClientID == clientID {
			cred.DCRClientID, cred.DCRClient = "", nil

			if cred.ClientID == clientID {
				cred.ClientID = "" // the client is gone, so is what bound the grant to it
			}
		}

		return nil
	})
}

// delete runs before, then removes the connector's row, both under the
// lock, and reports whether there was a row. before runs whether or not
// there is one. Store failures are [CaseStore] errors.
func (c *credentials) delete(ctx context.Context, connector string, before func()) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	before()
	_, err := c.store.GetCredential(ctx, c.userID, connector)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fail(CaseStore, connector, "read credential", err)
	}

	if err := c.store.DeleteCredential(ctx, c.userID, connector); err != nil {
		return false, fail(CaseStore, connector, "delete credential", err)
	}

	return true, nil
}

// locked runs f under the lock, so no write runs meanwhile.
func (c *credentials) locked(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f()
}

// write reads the connector's row, applies fn, and writes the row back, all
// under the lock. If fn returns an error, nothing is written and that error
// is returned as it is. With no row, a create write applies fn to a new
// one; any other write runs nothing, writes nothing, and fails
// [CaseNoCredential].
func (c *credentials) write(ctx context.Context, connector string, create bool, fn func(*store.Credential) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cred, err := c.store.GetCredential(ctx, c.userID, connector)

	switch {
	case errors.Is(err, store.ErrNotFound) && create:
		cred = store.Credential{UserID: c.userID, Connector: connector}
	case errors.Is(err, store.ErrNotFound):
		return fail(CaseNoCredential, connector, "the credential was deleted", nil)
	case err != nil:
		return fail(CaseStore, connector, "read credential", err)
	}

	if err := fn(&cred); err != nil {
		return err
	}

	if err := c.store.PutCredential(ctx, cred); err != nil {
		return fail(CaseStore, connector, "store credential", err)
	}

	return nil
}

// sealToken seals tok for the connector's row.
func (c *credentials) sealToken(connector string, tok token) ([]byte, error) {
	doc, err := json.Marshal(tok)

	if err != nil {
		return nil, fail(CaseStore, connector, "encode token", err)
	}

	return c.sealer.Seal(doc, tokenAAD(c.userID, connector)), nil
}
