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
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
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

	fresh, scope, err := redeem(ctx, b.oauthClient(req.conn, hc, disc, client, tok.DPoPKey), a, tok, req.resource)

	if err != nil {
		err = b.forgetRejectedDCR(ctx, name, client, v, err)

		if errors.Is(err, errTokenReplaced) {
			return b.replaced(ctx, req, disc)
		}

		ev := b.log.Warn().Str("connector", name).Str("issuer", redact.URL(disc.Issuer)).
			Str("grant_type", "refresh_token").Str("outcome", outcome(err))

		if e, ok := AsError(err); ok {
			ev = ev.Str("case", string(e.Case)).Str("provider_code", e.ProviderCode)
		}

		ev.Msg("oauth token refresh failed")

		return token{}, err
	}

	if !fresh.PRMKnown {
		fresh.PRMURL, fresh.PRMKnown = disc.PRMURL, true
	}

	// The token is stored only over the one refreshed. A revoke meanwhile
	// left no row, and the call fails no_credential; a new grant meanwhile
	// is the credential now, and the call uses its token. Either way the
	// refreshed token is dropped.
	err = b.withLink(b.creds.renew(ctx, name, v, fresh, client.ClientID, scope))

	if errors.Is(err, errTokenReplaced) {
		return b.replaced(ctx, req, disc)
	}

	if err != nil {
		b.log.Info().Str("connector", name).Str("issuer", redact.URL(disc.Issuer)).
			Str("grant_type", "refresh_token").Str("outcome", outcome(err)).Msg("oauth token refresh discarded")

		return token{}, err
	}

	b.log.Info().Str("connector", name).Str("issuer", redact.URL(disc.Issuer)).
		Str("grant_type", "refresh_token").Str("outcome", "ok").Msg("oauth token refreshed")

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
