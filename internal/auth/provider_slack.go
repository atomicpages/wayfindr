package auth

import (
	"encoding/json"
	"net/http"
	"strings"
)

// slackInspector reads Slack's token responses (oauth.v2.access and
// oauth.v2.exchange), which bend RFC 6749:
//
//   - every response carries "ok"; errors are HTTP 200 with "ok": false and
//     Slack's own codes in "error" (invalid_refresh_token, token_revoked,
//     invalid_client_id, bad_client_secret, …);
//   - a user-token-only install has no top-level access_token: the token,
//     with its refresh_token and expires_in under token rotation, is under
//     "authed_user";
//   - scope is comma-separated, and token_type is "bot" or "user".
//
// See https://docs.slack.dev/reference/methods/oauth.v2.access and
// https://docs.slack.dev/reference/methods/oauth.v2.exchange.
type slackInspector struct{}

func (slackInspector) Name() string { return "slack" }

func (slackInspector) Matches(issuer string) bool { return hostIs(issuerHost(issuer), "slack.com") }

// Inspect claims bodies that carry "ok" and leaves the rest to the next
// inspector.
func (slackInspector) Inspect(status int, _ http.Header, body []byte) (TokenOutcome, bool) {
	var r struct {
		tokenFields
		AuthedUser *tokenFields `json:"authed_user"`
	}

	if json.Unmarshal(body, &r) != nil || r.OK == nil {
		return TokenOutcome{}, false
	}

	if code, failed := r.failure(); failed {
		return errorOutcome(code), true
	}

	if status < 200 || status > 299 {
		return TokenOutcome{Case: CaseUnexpectedStatus}, true
	}

	tok := r.tokenFields

	if tok.AccessToken == "" && r.AuthedUser != nil {
		tok = *r.AuthedUser
	}

	if tok.AccessToken == "" {
		return TokenOutcome{Case: CaseMalformedResponse}, true
	}

	out := tok.outcome()
	out.Scope = strings.Join(strings.FieldsFunc(out.Scope, func(c rune) bool { return c == ',' || c == ' ' }), " ")

	return out, true
}
