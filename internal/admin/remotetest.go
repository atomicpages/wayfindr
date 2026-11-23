package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
)

// MaxRemoteTestBody bounds the body of POST /admin/remote/test: one token.
const MaxRemoteTestBody = 4 << 10

// RemoteOutcome is how a connection test of a cloud agent's path ended
// (GLOSSARY: Case). The set is closed; a console matches on it, never on
// the message.
type RemoteOutcome string

const (
	// RemoteOK means through the public URL, the token reached the gateway, and a
	// request without a token was refused as a remote one is. For the OAuth
	// test (RemoteDetailOAuth) it means the discovery documents are there,
	// name the public URL, and a request without a token got Wayfindr's own
	// challenge.
	RemoteOK RemoteOutcome = "ok"
	// RemoteNotConfigured means server.public_url is not set.
	RemoteNotConfigured RemoteOutcome = "not_configured"
	// RemoteUnreachable means nothing answered: the name does not resolve, the
	// connection was refused, or the network is down. It is also the outcome
	// for a 404 that is the tunnel provider's, not Wayfindr's (an offline
	// tunnel), with RemoteDetailTunnelOffline.
	RemoteUnreachable RemoteOutcome = "unreachable"
	// RemoteTLSError means the secure connection failed: a certificate the system
	// does not trust or one for another name, or the address does not speak
	// TLS.
	RemoteTLSError RemoteOutcome = "tls_error"
	// RemoteTimeout means no complete answer within the test's time limit.
	RemoteTimeout RemoteOutcome = "timeout"
	// RemoteInterstitial means a web page (HTML) answered instead of Wayfindr: a
	// tunnel's login, warning, or offline page.
	RemoteInterstitial RemoteOutcome = "interstitial"
	// RemoteRedirect means the address answered with a redirect, which the test
	// does not follow.
	RemoteRedirect RemoteOutcome = "redirect"
	// RemoteNotFound means Wayfindr's own 404 for the token. Through a tunnel
	// that is a token that is not a remote one, is wrong, or was revoked
	// only while OAuth is off; while it is on, a bad token gets
	// RemoteTokenInvalid, and a 404 means the path did not reach Wayfindr's
	// /mcp (a wrong address, a tunnel pointed elsewhere, or a daemon that
	// predates remote tokens).
	RemoteNotFound RemoteOutcome = "not_found"
	// RemoteTokenRefused means the address answered the token with a 401 or
	// 403 that is not Wayfindr's own challenge: a login or access check in
	// front of Wayfindr.
	RemoteTokenRefused RemoteOutcome = "token_refused"
	// RemoteTokenInvalid means the token got Wayfindr's own 401 invalid_token
	// challenge: it is not a remote token, is wrong, or was revoked. With
	// OAuth on this replaces the 404 RemoteNotFound.
	RemoteTokenInvalid RemoteOutcome = "token_invalid"
	// RemoteOAuthOff means the OAuth test was asked while server.oauth is off.
	// Nothing was sent.
	RemoteOAuthOff RemoteOutcome = "oauth_off"
	// RemoteMetadataMissing means a metadata document, or the 401 challenge
	// that points at one, is not there: Wayfindr's own 404 or no document.
	RemoteMetadataMissing RemoteOutcome = "metadata_missing"
	// RemoteMetadataMismatch means the discovery documents or the challenge
	// name another address than the public URL.
	RemoteMetadataMismatch RemoteOutcome = "metadata_mismatch"
	// RemoteProxyTreatsRemoteAsLocal means a request with no token got 401 with
	// Wayfindr's own challenge (exactly `Bearer`, no parameters), which only a
	// request the daemon takes for local gets.
	// The tunnel or proxy hides that the request came from outside, so the
	// default client token would work from the internet (wayfindr-gz1y). The
	// test fails with this even when the token itself worked.
	RemoteProxyTreatsRemoteAsLocal RemoteOutcome = "proxy_treats_remote_as_local"
	// RemoteUnexpected means an answer that is none of the above, such as a 5xx or
	// something that is not an MCP server.
	RemoteUnexpected RemoteOutcome = "unexpected_response"
)

// RemoteTestRequest is the body of POST /admin/remote/test: exactly one of
// Token and OAuth. The token is the value of a remote client token, held by
// the caller for this one request. The daemon sends it to the public URL and
// never stores or logs it. OAuth asks for the test of sign-in with OAuth
// instead, which needs no token.
type RemoteTestRequest struct {
	Token string `json:"token,omitempty"`
	OAuth bool   `json:"oauth,omitempty"`
}

// RemoteTestResult is the answer to POST /admin/remote/test, whatever the
// outcome: a test that fails is still 200, with Outcome and OK false.
type RemoteTestResult struct {
	// Outcome is the closed code to branch on.
	Outcome RemoteOutcome `json:"outcome"`
	// OK is true only for RemoteOK.
	OK bool `json:"ok"`
	// Message says in plain words what happened.
	Message string `json:"message"`
	// Action says what to do about it; empty for RemoteOK.
	Action string `json:"action,omitempty"`
	// URL is the address tested: server.public_url and /mcp. Empty for
	// RemoteNotConfigured.
	URL string `json:"url,omitempty"`
	// Status is the HTTP status of the answer that decided the outcome; 0
	// when there was none (a network failure) or for RemoteOK.
	Status int `json:"status,omitempty"`
	// Connectors is how many connectors the token may use; RemoteOK only.
	Connectors int `json:"connectors,omitempty"`
	// Tools is how many tools tools/list returned through the public URL,
	// the gateway's two meta-tools (search_tools and call_tool); RemoteOK
	// only.
	Tools int `json:"tools,omitempty"`
}

// RemoteDetail is the finer reason behind an outcome that has more than one
// (GLOSSARY: Case). The set is closed; RemoteDetailNone is the usual one.
type RemoteDetail string

const (
	// RemoteDetailNone is an outcome with one reason, or the usual one.
	RemoteDetailNone RemoteDetail = ""
	// RemoteDetailInvalidURL is a RemoteNotConfigured for an address that is
	// set but is not a usable https address.
	RemoteDetailInvalidURL RemoteDetail = "invalid_url"
	// RemoteDetailNotMCP is a RemoteUnexpected for an answer that was not an
	// MCP reply to the token's request.
	RemoteDetailNotMCP RemoteDetail = "not_mcp"
	// RemoteDetailBareStatus is a RemoteUnexpected for a token that worked
	// while the same request with no token got a status other than 404.
	RemoteDetailBareStatus RemoteDetail = "bare_status"
	// RemoteDetailTunnelOffline is a RemoteUnreachable for a 404 that is not
	// Wayfindr's own: the tunnel provider answering for a tunnel that is not
	// running or not connected to Wayfindr.
	RemoteDetailTunnelOffline RemoteDetail = "tunnel_offline"
	// RemoteDetailOAuth marks a RemoteOK as the answer of the OAuth test,
	// which has no connectors or tools to count.
	RemoteDetailOAuth RemoteDetail = "oauth"
)

// RemoteFacts is what a connection test found: the inputs to the words
// DescribeRemote gives its outcome. A field an outcome does not use is
// ignored.
type RemoteFacts struct {
	// Host is the public URL's host, with its port when it has one.
	Host string
	// URL is the address tested (server.public_url and /mcp).
	URL string
	// Status is the HTTP status of the answer that decided the outcome.
	Status int
	// Redirect is the host a RemoteRedirect pointed at; empty when the
	// answer did not name one.
	Redirect string
	// Connectors and Tools are what a RemoteOK saw.
	Connectors, Tools int
	Detail            RemoteDetail
}

// DescribeRemote is the one place an outcome gets its Message and Action
// (wayfindr-4ppx): the daemon decides the outcome and the facts, and the
// words are written here, next to the closed set of outcomes. The result
// holds no token, query, or response body.
func DescribeRemote(o RemoteOutcome, f RemoteFacts) RemoteTestResult {
	res := RemoteTestResult{Outcome: o, OK: o == RemoteOK, URL: f.URL, Status: f.Status}

	switch o {
	case RemoteOK:
		if f.Detail == RemoteDetailOAuth {
			res.Message = "It works. Through the tunnel, a cloud agent finds Wayfindr's sign-in at " + f.URL +
				", and a request with no token is refused as one from the internet is."

			break
		}

		res.Message = "It works. A cloud agent with this token reaches Wayfindr at " + f.URL + " and sees " +
			plural(f.Connectors, "connector") + "."
		res.Connectors, res.Tools = f.Connectors, f.Tools
	case RemoteNotConfigured:
		// No address was tested, so there is no URL or status to report.
		res.URL, res.Status = "", 0

		if f.Detail == RemoteDetailInvalidURL {
			res.Message = "The public URL is not a valid https address."
			res.Action = "Set server.public_url to the https address your tunnel serves Wayfindr at, then test again."
		} else {
			res.Message = "No public URL is set, so there is no address to test."
			res.Action = "Set the https address your tunnel serves Wayfindr at (server.public_url), then test again."
		}
	case RemoteUnreachable:
		if f.Detail == RemoteDetailTunnelOffline {
			res.Message = "The tunnel at " + f.Host + " answered 404 itself, not Wayfindr. The tunnel is not running or not connected to Wayfindr."
			res.Action = "Start your tunnel and point it at Wayfindr's port, then test again."

			break
		}

		res.Message = "Nothing answered at " + f.Host + ". The name may not exist, or the tunnel is not running."
		res.Action = "Check the public URL, and that your tunnel is running and points at Wayfindr."
	case RemoteTLSError:
		res.Message = "The secure connection to " + f.Host + " failed: its certificate is not one that can be trusted or is for another name, or the address does not speak https."
		res.Action = "Check the public URL, and the certificate your tunnel or proxy serves for it."
	case RemoteTimeout:
		res.Message = f.Host + " did not answer in time."
		res.Action = "Check that the tunnel and Wayfindr are running, then test again."
	case RemoteInterstitial:
		res.Message = f.Host + " answered with a web page instead of Wayfindr, such as a tunnel's login, warning, or offline page. " +
			"A cloud agent can't click through a page like that."
		res.Action = "Turn off the tunnel's login or warning page for this address, or check that the tunnel is running and points at Wayfindr."
	case RemoteRedirect:
		where := f.Redirect

		if where == "" {
			where = "another address"
		}

		res.Message = f.Host + " redirected the request to " + where + ". The test does not follow redirects, and a cloud agent may not either."
		res.Action = "Set the public URL to the address the tunnel serves directly: the exact host name, with https and no redirect."
	case RemoteNotFound:
		res.Message = f.Host + " answered 404. With sign-in (OAuth) off, Wayfindr answers that through a tunnel when the token is not a remote token, " +
			"is wrong, or was revoked, and when the URL is wrong. With sign-in on, a bad token gets a 401 instead, so a 404 means the request " +
			"did not reach Wayfindr's /mcp: the URL is wrong, or the tunnel points somewhere else."
		res.Action = "Check that the token was created as remote and is still listed, that the public URL is the address your tunnel serves, " +
			"that the tunnel points at Wayfindr's port, and that Wayfindr is up to date."
	case RemoteTokenRefused:
		res.Message = f.Host + " refused the request (HTTP " + strconv.Itoa(f.Status) + "), and not with Wayfindr's own answer. A login or access check in front of Wayfindr, " +
			"in the tunnel or proxy, is stopping it before Wayfindr sees the token."
		res.Action = "Turn off the login or access check the tunnel or proxy adds in front of Wayfindr for this address, then test again."
	case RemoteTokenInvalid:
		res.Message = f.Host + " answered 401: Wayfindr does not accept this token. It is not a remote token, is wrong, or was revoked."
		res.Action = "Check that the token was created as remote and is still listed (wayfindr token list), then test again."
	case RemoteOAuthOff:
		// Nothing was sent, so there is no URL or status to report.
		res.URL, res.Status = "", 0
		res.Message = "Sign-in with OAuth is off, so Wayfindr does not answer sign-in requests through the tunnel, and there is nothing to test."
		res.Action = "Turn on sign-in for cloud agents (server.oauth.enabled), then test again."
	case RemoteMetadataMissing:
		res.Message = f.Host + " does not give the sign-in information a cloud agent looks for" + statusWords(f.Status) +
			". Wayfindr gives it only while sign-in is on, at its public address."
		res.Action = "Check that sign-in is on, that the public URL is the address your tunnel serves, " +
			"and that the tunnel passes every path, including /.well-known/ and /oauth/, to Wayfindr."
	case RemoteMetadataMismatch:
		res.Message = "The sign-in information at " + f.Host + " names another address than the public URL, so a cloud agent would be sent there to sign in."
		res.Action = "Set the public URL to the exact address your tunnel serves (https, same host name), or point the tunnel at this Wayfindr, then test again."
	case RemoteProxyTreatsRemoteAsLocal:
		res.Message = "Not safe: the tunnel or proxy at " + f.Host + " hides that requests come from the internet. " +
			"Wayfindr takes them for requests from your own computer, so your default client token, which reaches every connector, " +
			"would work from the internet, whatever token you test with. Do not give a cloud agent this address until this is fixed."
		res.Action = "Make the tunnel or proxy add a forwarding header such as X-Forwarded-For or X-Forwarded-Host, and keep the public Host name. " +
			"ngrok, Cloudflare Tunnel, and Tailscale Funnel do this by default. Then test again."
	case RemoteUnexpected:
		switch f.Detail {
		case RemoteDetailNotMCP:
			res.Message = f.Host + " answered, but not like Wayfindr's MCP endpoint."
			res.Action = "Check that the public URL is the address your tunnel serves Wayfindr at, and that the tunnel points at Wayfindr's port."
		case RemoteDetailBareStatus:
			res.Message = "The token works, but the same request without a token got HTTP " + strconv.Itoa(f.Status) +
				" from " + f.Host + ", not the 404 that Wayfindr gives a request from the internet. The test can't confirm the path is safe."
			res.Action = "Check the proxy or tunnel in front of Wayfindr, and that it adds a forwarding header such as X-Forwarded-For."
		case RemoteDetailNone, RemoteDetailInvalidURL, RemoteDetailTunnelOffline, RemoteDetailOAuth:
			res.Message = f.Host + " answered HTTP " + strconv.Itoa(f.Status) + ", which is not what Wayfindr's MCP endpoint answers."
			res.Action = "Check that the tunnel points at Wayfindr's port and that Wayfindr is running."
		}
	}

	return res
}

// statusWords words an HTTP status for the end of a sentence: " (HTTP 404)",
// or nothing when there was none.
func statusWords(status int) string {
	if status == 0 {
		return ""
	}

	return " (HTTP " + strconv.Itoa(status) + ")"
}

// plural words n of noun: "1 connector", "3 connectors".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}

	return strconv.Itoa(n) + " " + noun + "s"
}

// remoteTest serves POST /admin/remote/test: it tests the cloud agent's path
// to the daemon through server.public_url with the token in the body.
func (a api) remoteTest(w http.ResponseWriter, r *http.Request) {
	if a.TestRemote == nil {
		http.Error(w, "testing the connection is not available", http.StatusNotImplemented)

		return
	}

	var body RemoteTestRequest

	// Exactly one of the two: a token to test with, or the OAuth test.
	if err := readJSON(w, r, MaxRemoteTestBody, &body); err != nil || (body.Token != "") == body.OAuth {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			http.Error(w, "the token is too large", http.StatusRequestEntityTooLarge)

			return
		}

		http.Error(w, `body must be {"token": "<remote client token>"} or {"oauth": true}`, http.StatusBadRequest)

		return
	}

	// The OAuth test of an authorization server that is off sends nothing.
	if body.OAuth && !a.Config().OAuth.Enabled {
		writeJSON(w, DescribeRemote(RemoteOAuthOff, RemoteFacts{}))

		return
	}

	publicURL := a.Config().PublicURL

	if publicURL == "" {
		writeJSON(w, DescribeRemote(RemoteNotConfigured, RemoteFacts{}))

		return
	}

	// Whether the address is a usable https one is the tester's call, not
	// checked here: it is what sends the secret there, so its guard is the one.
	// An empty token is the OAuth test.
	res := a.TestRemote(r.Context(), publicURL, body.Token)
	res.OK = res.Outcome == RemoteOK

	writeJSON(w, res)
}

// TestRemote calls POST /admin/remote/test: it sends MCP requests to the
// running config's server.public_url with token, and reports how far they
// got. Every outcome, failures included, is a RemoteTestResult; an error is
// the call itself failing (501, a malformed token body, the daemon down).
//
// The token is sent to the daemon for this one call and goes no further than
// the public URL.
func (c *Client) TestRemote(ctx context.Context, token string) (RemoteTestResult, error) {
	var out RemoteTestResult
	err := c.do(ctx, http.MethodPost, pathRemoteTest, RemoteTestRequest{Token: token}, &out)

	return out, err
}

// TestRemoteOAuth calls POST /admin/remote/test with {"oauth": true}: it
// fetches, through the running config's server.public_url, what a cloud agent
// that signs in with OAuth (ChatGPT) fetches, and reports how far it got. No
// token is involved. As for TestRemote, every outcome is a RemoteTestResult;
// while server.oauth is off the answer is RemoteOAuthOff and nothing is sent.
func (c *Client) TestRemoteOAuth(ctx context.Context) (RemoteTestResult, error) {
	var out RemoteTestResult
	err := c.do(ctx, http.MethodPost, pathRemoteTest, RemoteTestRequest{OAuth: true}, &out)

	return out, err
}
