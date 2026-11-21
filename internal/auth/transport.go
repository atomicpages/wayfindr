package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// tokenSource is where a connector's Transport gets its tokens, and what
// decides what the upstream's refusals mean. It is an interface because the
// connector's grant decides both: the stored tokens of an auth flow
// ([storedTokens]) and the in-memory tokens of an unattended grant
// ([unattendedTokens]).
type tokenSource interface {
	// token returns a usable token.
	token(ctx context.Context) (token, error)
	// renew returns a token to replace one the upstream answered 401 to,
	// or fails.
	renew(ctx context.Context, why rejection) (token, error)
	// stepUp handles a 403 insufficient_scope that asks for scopes: it
	// returns a token with them to replay the request with once, or the
	// error the request fails with.
	stepUp(ctx context.Context, scopes []string) (token, error)
	// refused returns the error a request fails with when the upstream
	// refused the token renew or stepUp handed back, as why says. There is
	// no further try.
	refused(ctx context.Context, why rejection) error
}

// rejection is why the upstream refused a token: a 401, or a 403
// insufficient_scope.
type rejection struct {
	// status is the upstream's answer, 401 or 403.
	status int
	// held is the token the upstream refused.
	held token
	// challenge is the answer's WWW-Authenticate.
	challenge []string
}

// roundTripper is the Transport for one OAuth connector: it sends each
// request with a token from tokens, bound as sender says (design §7.3).
type roundTripper struct {
	connector string
	// link is the connector's auth link, for needs_auth errors.
	link   string
	tokens tokenSource
	sender sender
	next   http.RoundTripper
}

// RoundTrip sends req with the connector's token (design §7.3).
func (t *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	tok, err := t.tokens.token(ctx)

	if err != nil {
		closeBody(req)

		return nil, err
	}

	resp, err := t.send(req, tok)

	if err != nil {
		return nil, err // a transport failure, unchanged: unreachable
	}

	if resp.StatusCode == http.StatusUnauthorized {
		if resp, err = t.replay(req, resp, tok); err != nil {
			return nil, err
		}
	}

	if resp.StatusCode == http.StatusForbidden {
		if scopes, ok := insufficientScope(resp.Header.Values("WWW-Authenticate")); ok {
			drain(resp)

			return t.stepUp(req, scopes)
		}
	}

	return resp, nil
}

// insufficientScope returns the scopes of a Bearer or DPoP
// insufficient_scope challenge (RFC 6750 §3.1, RFC 9449 §7.1), and whether
// there was one.
func insufficientScope(challenge []string) ([]string, bool) {
	cs, _ := oauthex.ParseWWWAuthenticate(challenge)

	for _, c := range cs {
		if tokenScheme(c.Scheme) && c.Params["error"] == "insufficient_scope" {
			return strings.Fields(c.Params["scope"]), true
		}
	}

	return nil, false
}

// replay handles a 401 to req, sent with tok: one retry the sender asks
// for, then one renewed token and one replay of req. A 401 to the renewed
// token means the upstream will not take any token we can get; the token
// source says what that means for the connector.
func (t *roundTripper) replay(req *http.Request, resp *http.Response, tok token) (*http.Response, error) {
	var err error

	if t.sender.retry(resp, roleResource) {
		drain(resp)

		if resp, err = t.resend(req, tok); err != nil || resp.StatusCode != http.StatusUnauthorized {
			return resp, err
		}
	}

	why := rejection{status: resp.StatusCode, held: tok, challenge: resp.Header.Values("WWW-Authenticate")}
	drain(resp)

	if tok, err = t.tokens.renew(req.Context(), why); err != nil {
		return nil, err
	}

	resp, err = t.resend(req, tok)

	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		why = rejection{status: resp.StatusCode, held: tok, challenge: resp.Header.Values("WWW-Authenticate")}
		drain(resp)

		return nil, t.tokens.refused(req.Context(), why)
	}

	return resp, nil
}

// stepUp handles a 403 insufficient_scope to req that asks for scopes: the
// token source fails the request or hands back a token with the larger
// scope set, and req is replayed once with it. A second 403
// insufficient_scope goes to the token source's refused; there is no third
// try, so a step-up cannot loop.
func (t *roundTripper) stepUp(req *http.Request, scopes []string) (*http.Response, error) {
	tok, err := t.tokens.stepUp(req.Context(), scopes)

	if err != nil {
		return nil, err
	}

	resp, err := t.resend(req, tok)

	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusForbidden {
		challenge := resp.Header.Values("WWW-Authenticate")

		if _, ok := insufficientScope(challenge); ok {
			drain(resp)

			return nil, t.tokens.refused(req.Context(), rejection{status: resp.StatusCode, held: tok, challenge: challenge})
		}
	}

	return resp, nil
}

// resend sends req once more with tok, its body rewound.
func (t *roundTripper) resend(req *http.Request, tok token) (*http.Response, error) {
	again := req.Clone(req.Context())

	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return nil, linkedFail(t.link, CaseNotReplayable, t.connector, "the request body cannot be rewound", "", nil)
		}

		var err error

		if again.Body, err = req.GetBody(); err != nil {
			return nil, linkedFail(t.link, CaseNotReplayable, t.connector, "rewind request body", "", err)
		}
	}

	return t.send(again, tok)
}

// send sends a copy of req with tok, as the sender binds it, and shows the
// answer to the sender.
func (t *roundTripper) send(req *http.Request, tok token) (*http.Response, error) {
	r := req.Clone(req.Context())

	if err := t.sender.resourceRequest(r, tok); err != nil {
		closeBody(r)

		return nil, fmt.Errorf("bind token: %w", err)
	}

	resp, err := t.next.RoundTrip(r)

	if err == nil {
		t.sender.observe(resp, roleResource)
	}

	return resp, err
}

// drain discards and closes a response body that is not returned, so its
// connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// closeBody honours the RoundTripper contract of closing the request body
// even when the request is never sent.
func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}
