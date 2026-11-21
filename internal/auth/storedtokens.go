package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/atomicpages/wayfindr/internal/config"
)

// storedTokens is the [tokenSource] of a connector authorized by an auth
// flow: the token in its credentials row, refreshed lazily and
// single-flight per credential (ADR 0004).
type storedTokens struct {
	b *Broker
	// conn is the config the connector is being dialed with.
	conn config.Connector
}

// token returns a usable token for the connector, refreshing it first if it
// is near expiry.
func (s *storedTokens) token(ctx context.Context) (token, error) {
	resource, err := s.resource(ctx)

	if err != nil {
		return token{}, err
	}

	v, err := s.b.load(ctx, s.conn.Name, resource, *s.conn.Auth)

	if err != nil {
		return token{}, s.b.unauthorizable(ctx, s.conn, resource, err)
	}

	tok := v.Token

	if s.b.expiring(tok) {
		return s.b.refresh(ctx, refreshRequest{conn: s.conn, resource: resource, seen: tok.Access})
	}

	return tok, nil
}

// renew forces one refresh of the token the upstream rejected, with
// discovery fetched again.
func (s *storedTokens) renew(ctx context.Context, why rejection) (token, error) {
	resource, err := s.resource(ctx)

	if err != nil {
		return token{}, err
	}

	// A refresh with a challenge, even an empty one, fetches the cached
	// metadata again.
	challenge := why.challenge

	if challenge == nil {
		challenge = []string{}
	}

	return s.b.refresh(ctx, refreshRequest{
		conn: s.conn, resource: resource, seen: why.held.Access, force: true, challenge: challenge,
	})
}

// stepUp merges scopes into the credential's requested_scopes, together
// with the scopes already requested and granted, so the next auth flow asks
// for the larger set (ADR 0004). It never hands back a token: only the
// user can grant more scopes, so it returns the step-up error, or
// no_credential if the credential was revoked meanwhile: the scopes are
// then dropped with it.
func (s *storedTokens) stepUp(ctx context.Context, scopes []string) (token, error) {
	name := s.conn.Name
	merged, err := s.b.creds.addScopes(ctx, name, scopes)

	if err != nil {
		return token{}, s.b.withLink(err)
	}

	s.b.log.Info().Str("connector", name).Str("scopes", merged).Msg("oauth step-up required")

	return token{}, s.b.fail(CaseStepUp, name, "the upstream requires scopes "+strings.Join(scopes, " "), "", nil)
}

// refused is the error for a token the upstream refused after a refresh:
// the connector needs auth (token_rejected). stepUp never hands back a
// token, so only a 401 to a refreshed token reaches it; a 403
// insufficient_scope would be a step-up like any other.
func (s *storedTokens) refused(ctx context.Context, why rejection) error {
	if scopes, ok := insufficientScope(why.challenge); ok && why.status == http.StatusForbidden {
		_, err := s.stepUp(ctx, scopes)

		return err
	}

	// The resource may want a certificate-bound token the connector can't
	// get; the detail says so when its metadata hints at it.
	detail := "the upstream answered 401 to a freshly refreshed token" + s.b.certBoundHint(ctx, s.conn)

	return s.b.fail(CaseTokenRejected, s.conn.Name, detail, "", nil)
}

// resource resolves the connector's URL, which its token is bound to.
func (s *storedTokens) resource(ctx context.Context) (string, error) {
	resource, err := s.conn.URL.Resolve(ctx, s.b.resolver)

	if err != nil {
		return "", s.b.fail(CaseUnresolvedValue, s.conn.Name, "connector url", "", err)
	}

	return resource, nil
}

// unauthorizable checks, for err, the error of reading connector c's
// token, whether the auth flow a no_credential or url_changed error points
// to could ever succeed: the check of the flow kind its grant selects
// ([selectFlowKind]). A discovered connector whose authorization server
// does not advertise PKCE S256 cannot be authorized (ADR 0007), so it gets
// [Broker.checkPKCE]'s pkce_unsupported config error, and a device code
// one whose server has no device authorization endpoint gets
// device_unsupported; the connector is auth_error rather than needs_auth
// with a link that always fails.
//
// Both errors get the same rule, so they agree (wayfindr-zgp.24): a url
// that moved to a legacy server with no metadata, or to one with no OAuth
// at all, passes it as a connector with no credential there does, and the
// url change stays the news.
//
// Discovery runs as a starting flow's does, with the probe's challenge, but
// from cached documents and with the challenge of the last probe, so a
// connector that keeps reconnecting probes once per discovery
// (wayfindr-zgp.20). Any other error, a manual connector, and a discovery
// that fails return err unchanged.
func (b *Broker) unauthorizable(ctx context.Context, c config.Connector, resource string, err error) error {
	e, ok := AsError(err)

	if !ok || (e.Case != CaseNoCredential && e.Case != CaseURLChanged) || c.Auth.IsManual() {
		return err
	}

	hc, derr := b.httpFor(ctx, c)

	if derr != nil {
		return err
	}

	disc, derr := b.discover.Discover(ctx, DiscoverRequest{
		Connector: c.Name, ResourceURL: resource, Auth: *c.Auth, Probe: true, Client: hc, Mask: b.urlMask(ctx, c),
	})

	if derr != nil {
		return err
	}

	if kerr := selectFlowKind(StartOptions{}, *c.Auth, nil).check(b, c.Name, *c.Auth, disc); kerr != nil {
		return kerr
	}

	return err
}
