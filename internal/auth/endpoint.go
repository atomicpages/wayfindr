package auth

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxTokenResponse bounds a token endpoint response.
const maxTokenResponse = 1 << 20

// bodyTimeout bounds reading a token response once its headers have
// arrived (see [oauthClient.exchange]): every oauthClient's bodyTimeout.
const bodyTimeout = 10 * time.Second

// endpointRequest is a request to the authorization server's token
// endpoint before it is sent: client authentication and the sender add to
// its form and headers.
type endpointRequest struct {
	url    string
	form   url.Values
	header http.Header
}

// exchange sends one token request (RFC 6749 §3.2) carrying form, with the
// client authenticated as oc.auth says and the request proved as oc.sender
// says, and reads the response through the [Inspectors] before anything
// else looks at it. It is the one call to the token endpoint: every grant
// and every refresh goes through it. A response that asks for a DPoP nonce
// is answered first, by sending once more with a fresh proof.
//
// It returns the token, with Expiry set from the broker's clock, and the
// granted scope, if the server reported one. Errors are [*Error]s with the
// [Case] the inspectors read and the provider code: a rejected grant, or an
// error code Wayfindr does not know, needs auth; a rejected client is a
// config error (a refresh turns a rejected DCR client into
// [CaseRegistrationExpired]; see [Broker.forgetRejectedDCR]); a non-2xx
// without an error code and a malformed response are [KindMalformed]; a
// transport failure, or a known transient code such as a rate limit, is
// [KindUnreachable]. Neither the response body nor any token is put in an
// error.
//
// ctx bounds the wait for the answer, not the reading of it: once the
// answer has arrived the server may have issued a token, perhaps rotating
// a refresh token, so the body is read within bodyTimeout even if ctx ends
// meanwhile (wayfindr-1j97).
func (oc oauthClient) exchange(ctx context.Context, form url.Values) (token, string, error) {
	// The request runs on a context of its own, which ctx's end cancels,
	// with ctx's cause, until the answer arrives.
	rctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancel(nil)
	stop := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	defer stop()

	endpoint, grant := oc.endpoints.Token, form.Get("grant_type")

	// attempt sends form unless ctx has ended. AfterFunc cancels rctx on a
	// goroutine of its own, which may not have run yet, so attempt checks
	// ctx itself: a request sent once ctx has ended could spend a refresh
	// token for an answer no one reads.
	attempt := func() (*http.Response, error) {
		if ctx.Err() != nil {
			cause := context.Cause(ctx)

			return nil, oc.fail(transportCase(cause), grant+" grant at "+endpoint, "", cause)
		}

		return oc.postToken(rctx, form)
	}

	resp, err := attempt()

	if err == nil && oc.sender.retry(resp, roleAuthorizationServer) {
		drain(resp)
		resp, err = attempt()
	}

	if err != nil {
		return token{}, "", err
	}

	// The answer has arrived: from here only bodyTimeout ends reading it.
	// If ctx ended first, the request is already cancelled, and so is the
	// read.
	stop()
	readTimer := time.AfterFunc(oc.bodyTimeout, func() { cancel(context.DeadlineExceeded) })
	defer readTimer.Stop()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse+1))

	if err != nil {
		err = oc.fail(CaseTransport, grant+" grant at "+endpoint+": read response", "", err)

		if e, ok := AsError(err); ok {
			e.status = resp.StatusCode // see answerUnread
		}

		return token{}, "", err
	}

	out := TokenOutcome{Case: CaseMalformedResponse}

	if len(body) <= maxTokenResponse {
		// A server with no known issuer (config endpoints) is matched by
		// its token endpoint instead.
		issuer := cmp.Or(oc.issuer, endpoint)
		out = oc.inspectors.Inspect(issuer, resp.StatusCode, resp.Header, body)
	}

	if out.Token == nil {
		return token{}, "", oc.tokenError(grant, resp.StatusCode, out)
	}

	tok := *out.Token
	// The grant's DPoP key, when the request was proved with one: the
	// token is bound to it if its token_type says DPoP, and a refresh
	// proves with it again either way.
	tok.DPoPKey = oc.dpopKey

	if out.ExpiresIn > 0 {
		tok.Expiry = oc.now().Add(out.ExpiresIn)
	}

	if out.RefreshExpiresIn > 0 && tok.Refresh != "" {
		tok.RefreshExpiry = oc.now().Add(out.RefreshExpiresIn)
	}

	return tok, out.Scope, nil
}

// answerUnread reports whether err is [oauthClient.exchange]'s for a
// success answer whose body could not be read: the server may have issued
// a token, perhaps rotating a refresh token, that no one has.
func answerUnread(err error) bool {
	e, ok := AsError(err)

	return ok && e.Case == CaseTransport && isSuccess(e.status)
}

// postToken sends one attempt of a token request carrying form, with the
// client authentication and the sender applied afresh. The caller closes
// the response body.
func (oc oauthClient) postToken(ctx context.Context, form url.Values) (*http.Response, error) {
	return oc.send(ctx, oc.endpoints.Token, form, oc.sender.tokenRequest)
}

// post sends form to endpoint, an endpoint of the authorization server
// other than the token endpoint, such as the device authorization endpoint
// (RFC 8628 §3.1), with the client authenticated as for a token request.
// The sender adds nothing: it binds tokens, and no token is issued here.
// The caller closes the response body.
func (oc oauthClient) post(ctx context.Context, endpoint string, form url.Values) (*http.Response, error) {
	return oc.send(ctx, endpoint, form, nil)
}

// send is one POST of form to endpoint: the client authentication applies,
// then prove, if set, adds to the request. The sender sees every answer.
func (oc oauthClient) send(ctx context.Context, endpoint string, form url.Values, prove func(*endpointRequest) error) (*http.Response, error) {
	what := "request to " + endpoint

	if grant := form.Get("grant_type"); grant != "" {
		what = grant + " grant at " + endpoint
	}

	r := &endpointRequest{url: endpoint, form: maps.Clone(form), header: http.Header{}}
	r.header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.header.Set("Accept", "application/json")

	if err := oc.auth.apply(r, oc.now()); err != nil {
		if _, ok := AsError(err); ok {
			return nil, err
		}

		// Only signing a client assertion fails without a Case.
		return nil, oc.fail(CaseSigningKeyInvalid, "client authentication at "+endpoint, "", err)
	}

	if prove != nil {
		if err := prove(r); err != nil {
			if _, ok := AsError(err); ok {
				return nil, err
			}

			// Only a grant's DPoP key fails without a Case: a new grant
			// makes a new key.
			return nil, oc.fail(CaseCredentialUnreadable, "dpop proof for "+endpoint, "", err)
		}
	}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, strings.NewReader(r.form.Encode()))

	if err != nil {
		return nil, oc.fail(CaseInvalidURL, "endpoint "+endpoint, "", err)
	}

	hreq.Header = r.header
	resp, err := noRedirects(oc.http).Do(hreq)

	if err != nil {
		return nil, oc.fail(transportCase(err), what, "", err)
	}

	oc.sender.observe(resp, roleAuthorizationServer)

	return resp, nil
}

// noRedirects is a copy of c that hands back a redirect instead of
// following it. A request that carries a client secret, an assertion, a
// refresh token, or a DPoP proof is never sent on to wherever the server
// points: a 307 or 308 would send its body again. The 3xx reads as any
// other unexpected status.
func noRedirects(c *http.Client) *http.Client {
	nc := *c
	nc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return &nc
}

// tokenError is the error for a failed [TokenOutcome]: its Case and
// provider code, with the grant, endpoint, and status as detail.
func (oc oauthClient) tokenError(grant string, status int, out TokenOutcome) error {
	c := cmp.Or(out.Case, CaseMalformedResponse)
	detail := fmt.Sprintf("%s grant at %s answered status %d", grant, oc.endpoints.Token, status)
	err := oc.fail(c, detail, out.ProviderCode, nil)

	if e, ok := AsError(err); ok {
		e.status = status
	}

	return err
}
