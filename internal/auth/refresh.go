package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// refreshWindow is how close to expiry a token is refreshed before use
// (ADR 0004).
const refreshWindow = 60 * time.Second

// refreshTimeout bounds one refresh. The refresh runs detached from the
// caller's context, because a refresh cancelled after the authorization
// server rotated the refresh token, but before the new one is stored, would
// lose the credential.
const refreshTimeout = 30 * time.Second

// storeTimeout bounds storing a refreshed token. The store runs on a
// context of its own: once the authorization server has answered it may
// have spent the old refresh token, and only the stored answer holds the
// new one, so the store does not inherit what is left of refreshTimeout
// (wayfindr-1j97).
const storeTimeout = 10 * time.Second

// expiring reports whether tok must be refreshed before use.
func (b *Broker) expiring(tok token) bool {
	return !tok.Expiry.IsZero() && b.now().Add(refreshWindow).After(tok.Expiry)
}

// refreshRequest is one call's need for a fresher token.
type refreshRequest struct {
	conn     config.Connector
	resource string
	// seen is the access token the caller holds. A flight that finds a
	// different one stored returns it without refreshing.
	seen string
	// force refreshes even a token that is not near expiry (after a 401).
	force bool
	// challenge is the 401's WWW-Authenticate, for discovery. When set,
	// cached metadata is fetched again (design §7.3).
	challenge []string
}

// refresh returns a token fresher than req.seen, refreshing at most once
// per credential at a time: concurrent callers share the flight's result,
// and the new token is stored before any of them is released (ADR 0004).
func (b *Broker) refresh(ctx context.Context, req refreshRequest) (token, error) {
	key := b.userID + "|" + req.conn.Name

	// A forced caller that joined a lazy flight which found the token still
	// fresh gets back the token it already holds; it then needs a flight
	// of its own.
	for range 2 {
		ch := b.flights.DoChan(key, func() (any, error) {
			if !b.startRefresh() {
				return token{}, b.fail(CaseStore, req.conn.Name, "the daemon is stopping; no refresh starts", "", nil)
			}

			defer b.refreshes.Done()
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.refreshTimeout)
			defer cancel()

			return b.refreshOnce(fctx, req)
		})
		var res singleflight.Result

		select {
		case res = <-ch:
		case <-ctx.Done():
			return token{}, ctx.Err()
		}

		if res.Err != nil {
			return token{}, res.Err
		}

		tok, _ := res.Val.(token)

		if !req.force || tok.Access != req.seen {
			return tok, nil
		}
	}

	return token{}, b.fail(CaseRefreshFailed, req.conn.Name, "a forced refresh returned the token already held", "", nil)
}

// refreshOnce is the body of a refresh flight.
func (b *Broker) refreshOnce(ctx context.Context, req refreshRequest) (token, error) {
	name := req.conn.Name
	// Wall-clock, so a refresh that a laptop slept through shows it: Go's
	// monotonic clock stops during sleep on macOS.
	start := b.now().Round(0)
	elapsed := func() int64 { return b.now().Round(0).Sub(start).Milliseconds() }
	v, err := b.load(ctx, name, req.resource, *req.conn.Auth)

	if err != nil {
		return token{}, err
	}

	tok := v.Token

	if tok.Access != req.seen || (!req.force && !b.expiring(tok)) {
		return tok, nil // another flight already refreshed it
	}

	if tok.Refresh == "" {
		return token{}, b.fail(CaseRefreshFailed, name, "the token expired and there is no refresh token", "", nil)
	}

	a := *req.conn.Auth
	hc, err := b.httpFor(ctx, req.conn)

	if err != nil {
		return token{}, err
	}

	// A grant proved with DPoP reads the resource's challenge as its flow
	// did, from the last probe, so the refresh chooses its sender the same
	// way (see chooseSender). The PRM is looked for first where the flow
	// found it: with no challenge, a PRM only the challenge named is not
	// otherwise found (wayfindr-q8y9). A grant sealed before that was kept
	// probes for the challenge instead, once: its refresh records what this
	// discovery found.
	disc, err := b.discover.Discover(ctx, DiscoverRequest{
		Connector: name, ResourceURL: req.resource, Auth: a, PRMURL: tok.PRMURL,
		Challenge: req.challenge, Refetch: req.challenge != nil, Client: hc,
		Probe: tok.DPoPKey != nil || !tok.PRMKnown,
		Mask:  b.urlMask(ctx, req.conn),
	})

	if err != nil {
		return token{}, fmt.Errorf("discover token endpoint: %w", err)
	}

	// Discovery follows what the upstream names now, so the refresh token
	// and client authentication go only where the grant was obtained. A
	// token stored before the binding has none and is authorized again.
	if disc.Endpoints.Token != tok.TokenEndpoint {
		return token{}, b.fail(CaseAuthorizationServerChanged, name,
			"the grant was obtained at "+redact.URL(tok.TokenEndpoint)+"; the token endpoint is now "+redact.URL(disc.Endpoints.Token), "", nil)
	}

	client, err := b.registrar.existing(ctx, RegisterRequest{Connector: name, Auth: a, Discovery: disc}, v)

	if err != nil {
		return token{}, b.withLink(err)
	}

	// Once the daemon is stopping, the refresh token is not spent: a kill
	// could land between the server rotating it and the answer being
	// stored, and unspent it serves the next start.
	if b.stoppingRefreshes() {
		return token{}, b.fail(CaseStore, name, "the daemon is stopping; the refresh token is kept for the next start", "", nil)
	}

	// The token request gets a budget of its own: discovery and the client
	// lookup may have spent most of ctx's, and a request sent with little
	// time left could spend the refresh token for an answer that comes too
	// late to read (wayfindr-1j97).
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), b.refreshTimeout)
	defer rcancel()
	fresh, scope, err := redeem(rctx, b.oauthClient(req.conn, hc, disc, client, tok.DPoPKey), a, tok, req.resource)

	// What follows the answer runs under storeTimeout of its own: the
	// server may have rotated the refresh token, and only the stored answer
	// holds the new one.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()

	if err != nil {
		err = b.forgetRejectedDCR(sctx, name, client, v, err)

		if errors.Is(err, errTokenReplaced) {
			return b.replaced(sctx, req, disc)
		}

		ev, msg := b.log.Warn(), "oauth token refresh failed"

		// A success answer that could not be read is lost as a store
		// failure is: the server may have rotated the refresh token.
		if answerUnread(err) {
			ev, msg = b.log.Error(), "oauth token refresh lost"
		}

		ev = ev.Str("connector", name).Str("issuer", redact.URL(disc.Issuer)).
			Str("grant_type", "refresh_token").Str("outcome", outcome(err)).Int64("duration_ms", elapsed())

		if e, ok := AsError(err); ok {
			ev = ev.Str("case", string(e.Case)).Str("provider_code", e.ProviderCode)
		}

		ev.Msg(msg)

		return token{}, err
	}

	if !fresh.PRMKnown {
		fresh.PRMURL, fresh.PRMKnown = disc.PRMURL, true
	}

	// The token is stored only over the one refreshed. A revoke meanwhile
	// left no row, and the call fails no_credential; a new grant meanwhile
	// is the credential now, and the call uses its token. Either way the
	// refreshed token is dropped.
	err = b.withLink(b.creds.renew(sctx, name, v, fresh, client.ClientID, scope))

	if errors.Is(err, errTokenReplaced) {
		return b.replaced(sctx, req, disc)
	}

	if err != nil {
		// The answer is lost: if the server rotated, the grant is gone. The
		// event is logged here; the error itself is returned, not logged. A
		// revoke meanwhile (no_credential) loses nothing the user kept.
		ev, msg := b.log.Error(), "oauth token refresh lost"

		if e, ok := AsError(err); ok && e.Case == CaseNoCredential {
			ev, msg = b.log.Info(), "oauth token refresh discarded"
		}

		ev.Str("connector", name).Str("issuer", redact.URL(disc.Issuer)).
			Str("grant_type", "refresh_token").Str("outcome", outcome(err)).Int64("duration_ms", elapsed()).
			Msg(msg)

		return token{}, err
	}

	b.log.Info().Str("connector", name).Str("issuer", redact.URL(disc.Issuer)).
		Str("grant_type", "refresh_token").Str("outcome", "ok").Int64("duration_ms", elapsed()).
		Msg("oauth token refreshed")

	return fresh, nil
}

// replaced is a refresh's outcome when a newer token was stored while it
// ran: its own result is dropped, and the stored token returned.
func (b *Broker) replaced(ctx context.Context, req refreshRequest, disc Discovery) (token, error) {
	b.log.Info().Str("connector", req.conn.Name).Str("issuer", redact.URL(disc.Issuer)).
		Str("grant_type", "refresh_token").Str("outcome", "replaced").Msg("oauth token refresh discarded")
	v, err := b.load(ctx, req.conn.Name, req.resource, *req.conn.Auth)

	return v.Token, err
}

// outcome names a failed refresh for logs, by its [Kind], without
// including any response body.
func outcome(err error) string {
	if e, ok := AsError(err); ok {
		return string(e.Kind)
	}

	return "error"
}

// redeem spends old's refresh token at oc's token endpoint (RFC 6749 §6).
// It is done here rather than through oauth2.TokenSource so the resource
// indicator (RFC 8707) is sent on refresh too (ADR 0007). It returns the
// new token and the granted scope, if the server reported one. A new token
// without a refresh token keeps old's, with its expiry, and one got without
// a DPoP proof keeps old's DPoP key, the grant's, which a later refresh may
// need. oc is built with old's key, so a DPoP refresh proves with it.
// Errors are those of [oauthClient.exchange].
func redeem(ctx context.Context, oc oauthClient, a config.Auth, old token, resource string) (token, string, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {old.Refresh}}

	if a.ResourceIndicator {
		form.Set("resource", resource)
	}

	fresh, scope, err := oc.exchange(ctx, form)

	if err != nil {
		return token{}, "", err
	}

	if fresh.Refresh == "" {
		fresh.Refresh = old.Refresh // RFC 6749 §6: the old one stays valid
	}

	if fresh.DPoPKey == nil {
		fresh.DPoPKey = old.DPoPKey
	}

	fresh.TokenEndpoint = old.TokenEndpoint
	fresh.PRMURL, fresh.PRMKnown = old.PRMURL, old.PRMKnown

	// The same refresh token keeps its end unless the server sent a new one.
	if fresh.Refresh == old.Refresh && fresh.RefreshExpiry.IsZero() {
		fresh.RefreshExpiry = old.RefreshExpiry
	}

	return fresh, scope, nil
}
