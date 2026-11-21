package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// rfc6749Inspector reads RFC 6749 §5 token responses. It is last in every
// chain and claims every response.
//
// An error is detected by body, whatever the HTTP status: a non-empty
// "error" field, or "ok": false (servers such as Slack and GitHub answer
// errors with 200). A 2xx with neither a token nor an error is malformed.
type rfc6749Inspector struct{}

func (rfc6749Inspector) Name() string { return "rfc6749" }

func (rfc6749Inspector) Matches(string) bool { return true }

func (rfc6749Inspector) Inspect(status int, _ http.Header, body []byte) (TokenOutcome, bool) {
	var r tokenFields
	parsed := json.Unmarshal(body, &r) == nil

	if parsed {
		if code, failed := r.failure(); failed {
			return errorOutcome(code), true
		}
	}

	switch {
	case status < 200 || status > 299:
		return TokenOutcome{Case: CaseUnexpectedStatus}, true
	case !parsed || r.AccessToken == "":
		return TokenOutcome{Case: CaseMalformedResponse}, true
	}

	return r.outcome(), true
}

// tokenFields is a token response body (RFC 6749 §5.1, §5.2), with the
// "ok" flag some providers add.
type tokenFields struct {
	AccessToken  string  `json:"access_token"`
	TokenType    string  `json:"token_type"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresIn    seconds `json:"expires_in"`
	// RefreshTokenExpiresIn is the refresh token's lifetime, an extension
	// some providers send (GitHub Apps, for one).
	RefreshTokenExpiresIn seconds         `json:"refresh_token_expires_in"`
	Scope                 string          `json:"scope"`
	Error                 json.RawMessage `json:"error"`
	OK                    *bool           `json:"ok"`
}

// failure reports whether the body is an error, and its code. An "error"
// that is not a string (some servers send an object) is an error without
// a code.
func (r tokenFields) failure() (code string, failed bool) {
	raw := bytes.TrimSpace(r.Error)

	if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		if json.Unmarshal(raw, &code) != nil {
			return "", true
		}

		if code != "" {
			return code, true
		}
	}

	return "", r.OK != nil && !*r.OK
}

// outcome is the success outcome for a body with an access token.
func (r tokenFields) outcome() TokenOutcome {
	return TokenOutcome{
		Token:            &token{Access: r.AccessToken, Refresh: r.RefreshToken, Type: r.TokenType},
		Scope:            r.Scope,
		ExpiresIn:        time.Duration(r.ExpiresIn) * time.Second,
		RefreshExpiresIn: time.Duration(r.RefreshTokenExpiresIn) * time.Second,
	}
}

// seconds is expires_in (or refresh_token_expires_in), which some servers
// send as a string. A value that
// is neither a number nor a numeric string reads as zero (no expiry).
type seconds int64

func (s *seconds) UnmarshalJSON(b []byte) error {
	var n json.Number // accepts 3600 and "3600"
	v, err := int64(0), json.Unmarshal(b, &n)

	if err == nil {
		v, err = strconv.ParseInt(string(n), 10, 64)
	}

	if err != nil || v < 0 {
		v = 0
	}

	*s = seconds(v)

	return nil
}
