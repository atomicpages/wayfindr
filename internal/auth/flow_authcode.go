package auth

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/url"
	"slices"

	"golang.org/x/oauth2"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// CallbackPath is where the authorization server sends the browser back,
// on the daemon's listen address unless the connector sets redirect_uri.
// The daemon gives it to httpd, which serves it on any Host (ADR 0007).
const CallbackPath = "/oauth/callback"

// authCode is the authorization-code flow (RFC 6749 §4.1): the user
// consents in a browser, which the authorization server sends back to
// [CallbackPath] with a code that is exchanged for the token.
type authCode struct{}

func (authCode) grantType() string { return grantAuthorizationCode }

// check is ADR 0007's PKCE rule ([Broker.checkPKCE]), which only this kind
// has: a device code flow sends no code challenge.
func (authCode) check(b *Broker, connector string, a config.Auth, disc Discovery) error {
	return b.checkPKCE(connector, a, disc)
}

// armed does nothing: the browser's callback completes the flow.
func (authCode) armed(context.Context, *Broker, *flow, oauthClient) error { return nil }

// begin picks the PKCE verifier and the OAuth state, and returns the
// authorization URL the user opens.
func (authCode) begin(_ context.Context, _ *Broker, f *flow, oc oauthClient) (Prompt, error) {
	// pkce = false is allowed only with a client secret (ADR 0007, enforced
	// by config validation); a secret that resolved empty keeps PKCE on.
	if f.auth.PKCE || oc.registration.ClientSecret == "" {
		f.verifier = oauth2.GenerateVerifier()
	}

	state := random(32)
	f.stateKey = sha256.Sum256([]byte(state))
	authorizeURL, err := f.authorizeURL(state)

	if err != nil {
		return Prompt{}, oc.fail(CaseInvalidURL, "authorization endpoint "+oc.endpoints.Authorization, "", err)
	}

	return Prompt{AuthorizeURL: authorizeURL}, nil
}

// checkPKCE applies ADR 0007's PKCE rule for authorizing a connector with
// auth a, whose discovery is d: a discovered connector (no client_id) needs
// its authorization server to advertise S256, or it cannot be authorized
// and fails [CasePKCEUnsupported]. A manual connector always passes. It is
// the rule's one home: a starting flow applies it before registering, and the
// Transport before answering needs_auth (see [Broker.unauthorizable]).
//
// A legacy server with no metadata ([SourceLegacyDefaults], the 2025-03-26
// fallback) passes too (wayfindr-zgp.24): it has nowhere to advertise
// PKCE, so S256 is unknown rather than unsupported, and the flow sends S256
// anyway, which a server that ignores PKCE ignores. Only metadata that
// lists methods without S256, or lists none, is pkce_unsupported.
func (b *Broker) checkPKCE(connector string, a config.Auth, d Discovery) error {
	if a.IsManual() || d.Source == SourceLegacyDefaults || advertisesS256(d) {
		return nil
	}

	return b.fail(CasePKCEUnsupported, connector,
		"the authorization server does not advertise code_challenge_methods_supported S256", "", nil)
}

// advertisesS256 reports whether the authorization server metadata lists
// PKCE S256.
func advertisesS256(d Discovery) bool {
	return d.Metadata != nil && slices.Contains(d.Metadata.CodeChallengeMethodsSupported, "S256")
}

// authorizeURL is the authorization request (RFC 6749 §4.1.1) with PKCE
// (RFC 7636), the resource indicator (RFC 8707), and the DPoP key's
// thumbprint (RFC 9449 §10) when the grant uses DPoP, keeping any query the
// endpoint already has and adding the connector's authorize_params.
// authorize_params never set a protocol parameter: validation refuses
// those keys, and they are skipped here too, including the ones (scope,
// PKCE's, resource) this request may leave out.
func (f *flow) authorizeURL(state string) (string, error) {
	u, err := url.Parse(f.disc.Endpoints.Authorization)

	if err != nil {
		return "", err
	}

	q := u.Query()

	for k, v := range f.auth.AuthorizeParams {
		if _, reserved := config.ReservedAuthorizeParam(k); !reserved {
			q.Set(k, v)
		}
	}

	q.Set("response_type", "code")
	q.Set("client_id", f.client.ClientID)
	q.Set("redirect_uri", f.redirectURI)
	q.Set("state", state)

	if f.scope != "" {
		q.Set("scope", f.scope)
	}

	if f.verifier != "" {
		q.Set("code_challenge", oauth2.S256ChallengeFromVerifier(f.verifier))
		q.Set("code_challenge_method", "S256")
	}

	if f.auth.ResourceIndicator {
		q.Set("resource", f.resource)
	}

	// RFC 9449 §10: the code is bound to the grant's DPoP key, so only a
	// token request proved with it can redeem it.
	if f.dpopKey != nil {
		q.Set("dpop_jkt", f.dpopKey.thumbprint())
	}

	u.RawQuery = q.Encode()

	return u.String(), nil
}

// handleCallback is [Flows.HandleCallback]; stale is the config rule
// ([Flows.stale]). It consumes the state, so a state is accepted once;
// checks that the flow's connector config still holds; checks iss (RFC
// 9207) unless validate_issuer = false; exchanges the code with the PKCE
// verifier and the resource; stores the token through storeGrant, which
// reports the changed credential; ends the flow; and renders a page saying
// how it went. The code, state, and query are never logged.
func (b *Broker) handleCallback(w http.ResponseWriter, r *http.Request, stale func(*flow) error) {
	q := r.URL.Query()
	f, ok := b.flows.consume(q.Get("state"))

	if !ok {
		b.log.Warn().Msg("oauth callback with an unknown or used state")
		renderPage(w, http.StatusBadRequest, page{
			Title:   "Authorization link not recognised",
			Summary: "This authorization link is unknown or was already used.",
			Action:  "Start the authorization again from Wayfindr.",
		})

		return
	}

	if res := b.flowResult(f); res.State != FlowPending {
		// Expired or cancelled (a used state was consumed already).
		b.log.Warn().Str("connector", f.Connector).Str("outcome", string(res.State)).Msg("oauth callback for an ended flow")
		renderAdvice(w, http.StatusBadRequest, "Authorization of "+f.Connector+" failed", res.Err)

		return
	}

	if err := b.complete(r.Context(), f, q, stale); err != nil {
		res, _ := b.flows.finish(f, FlowFailed, err)
		ev := b.log.Warn().Str("connector", f.Connector).Str("issuer", redact.URL(f.disc.Issuer)).
			Str("grant_type", "authorization_code").Str("outcome", outcome(err))

		if e, ok := AsError(err); ok {
			ev = ev.Str("case", string(e.Case)).Str("provider_code", e.ProviderCode)
		}

		ev.Msg("oauth flow failed")
		renderAdvice(w, http.StatusBadRequest, "Authorization of "+f.Connector+" failed", res.Err)

		return
	}

	if res, ok := b.flows.finish(f, FlowDone, nil); !ok {
		// Revoked between storing the token and here: the revoke deleted it.
		renderAdvice(w, http.StatusBadRequest, "Authorization of "+f.Connector+" failed", res.Err)

		return
	}

	b.log.Info().Str("connector", f.Connector).Str("issuer", redact.URL(f.disc.Issuer)).
		Str("grant_type", "authorization_code").Str("outcome", "ok").Msg("oauth flow completed")
	renderPage(w, http.StatusOK, page{
		Title:   "Authorized " + f.Connector,
		Summary: "Wayfindr is now authorized for " + f.Connector + ".",
		Action:  "You can close this tab.",
	})
}

// complete checks the callback's flow and parameters and exchanges the
// code. A flow whose connector config changed since it started is
// cancelled before any code is exchanged, and again, authoritatively,
// under the row lock as its token is stored.
func (b *Broker) complete(ctx context.Context, f *flow, q url.Values, stale func(*flow) error) error {
	name := f.Connector

	if err := stale(f); err != nil {
		return err
	}

	if err := b.checkIss(f, q); err != nil {
		return err
	}

	if code := q.Get("error"); code != "" {
		return b.fail(CaseAuthorizationDenied, name, "the authorization server answered the authorization request with an error",
			sanitizeProviderCode(code), nil)
	}

	code := q.Get("code")

	if code == "" {
		return b.fail(CaseMalformedResponse, name, "the callback has neither a code nor an error", "", nil)
	}

	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {f.redirectURI},
	}

	if f.verifier != "" {
		form.Set("code_verifier", f.verifier)
	}

	if f.auth.ResourceIndicator {
		form.Set("resource", f.resource)
	}

	tok, scope, err := b.oauthClient(f.conn, f.http, f.disc, f.client, f.dpopKey).exchange(ctx, form)

	if err != nil {
		return b.forgetRejectedDCR(ctx, name, f.client, f.seen, err)
	}

	return b.completeFlow(ctx, f, tok, scope, stale)
}

// checkIss applies RFC 9207: an iss parameter must name the issuer the
// flow discovered, and one the server advertises it sends must be present.
// With validate_issuer = false the check is skipped and logged (ADR 0007).
func (b *Broker) checkIss(f *flow, q url.Values) error {
	want := f.disc.Issuer
	required := false

	if m := f.disc.Metadata; m != nil {
		want = m.Issuer
		required = m.AuthorizationResponseIssParameterSupported
	}

	iss, present := q.Get("iss"), q.Has("iss")

	if !f.auth.ValidateIssuer {
		if present || required {
			b.log.Warn().Str("connector", f.Connector).Str("issuer", redact.URL(want)).
				Str("callback_issuer", redact.URL(iss)).Msg("issuer validation skipped")
		}

		return nil
	}

	switch {
	case !present && required:
		return b.fail(CaseCallbackIssuerMismatch, f.Connector,
			"the callback has no iss, which the authorization server advertises", "", nil)
	case present && want != "" && !sameIssuer(iss, want):
		return b.fail(CaseCallbackIssuerMismatch, f.Connector,
			"the callback's iss "+redact.URL(iss)+" is not the issuer "+redact.URL(want), "", nil)
	}

	return nil
}
