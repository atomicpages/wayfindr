package auth

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenOutcome is what an [Inspector] reads from a token-endpoint response:
// a token, or a failure [Case] with the provider's code.
type TokenOutcome struct {
	// Token is set on success. Its Expiry and RefreshExpiry are zero: the
	// caller sets them from ExpiresIn, RefreshExpiresIn, and its clock.
	Token *token
	// Scope is the granted scope, space-separated as in RFC 6749 §3.3, or
	// empty if the server did not report one.
	Scope string
	// ExpiresIn is the token's lifetime, or zero if the server sent none.
	ExpiresIn time.Duration
	// RefreshExpiresIn is the refresh token's lifetime
	// (refresh_token_expires_in), or zero if the server sent none.
	RefreshExpiresIn time.Duration

	// Case is set on failure.
	Case Case
	// ProviderCode is the sanitised error code the server returned, if any
	// (see [sanitizeProviderCode]).
	ProviderCode string
}

// Inspector reads token-endpoint responses for the authorization servers
// it matches (GLOSSARY: Inspector). Provider inspectors handle servers that
// bend RFC 6749, such as Slack's HTTP 200 with "ok": false.
//
// To support a provider, add provider_<name>.go with its inspector and a
// table test, and list it in [defaultInspectors].
type Inspector interface {
	// Name identifies the inspector in tests and logs.
	Name() string
	// Matches reports whether the inspector applies to the issuer.
	Matches(issuer string) bool
	// Inspect reads one response. ok=false passes it to the next
	// inspector. body is never logged: it may hold tokens.
	Inspect(status int, header http.Header, body []byte) (outcome TokenOutcome, ok bool)
}

// Inspectors is an ordered chain of [Inspector]s.
type Inspectors []Inspector

// defaultInspectors is the broker's chain: provider inspectors first, then
// the RFC 6749 inspector, which claims every response.
func defaultInspectors() Inspectors {
	return Inspectors{
		slackInspector{},
		rfc6749Inspector{},
	}
}

// Inspect runs a token-endpoint response from issuer through the chain and
// returns the first claimed outcome. Every token request (refresh,
// authorization_code, device_code) goes through it before any other
// parsing.
func (c Inspectors) Inspect(issuer string, status int, header http.Header, body []byte) TokenOutcome {
	for _, in := range c {
		if !in.Matches(issuer) {
			continue
		}

		if out, ok := in.Inspect(status, header, body); ok {
			return out
		}
	}

	// Unreachable with defaultInspectors, whose last inspector claims all.
	return TokenOutcome{Case: CaseMalformedResponse}
}

// issuerHost returns the lowercased host of an issuer URL, without the
// port, or "" if it has none.
func issuerHost(issuer string) string {
	u, err := url.Parse(issuer)

	if err != nil {
		return ""
	}

	return strings.ToLower(u.Hostname())
}

// hostIs reports whether host is domain or a subdomain of it.
func hostIs(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// errorOutcome is the failure outcome for a provider's error code.
func errorOutcome(code string) TokenOutcome {
	return TokenOutcome{Case: caseOf(code), ProviderCode: sanitizeProviderCode(code)}
}

// caseOf maps a provider's error code to a [Case]. This is the one alias
// table for every provider: add a provider's spelling of a known condition
// here.
func caseOf(code string) Case {
	switch strings.ToLower(code) {
	case "invalid_grant", // RFC 6749 §5.2
		"invalid_refresh_token", // Slack
		"token_revoked",         // Slack
		"token_expired":         // Slack
		return CaseTokenRevoked
	case "invalid_client", // RFC 6749 §5.2
		"unauthorized_client", // RFC 6749 §5.2
		"invalid_client_id",   // Slack
		"bad_client_secret":   // Slack
		return CaseClientRejected
	case "temporarily_unavailable", // RFC 6749 §4.1.2.1
		"server_error",        // RFC 6749 §4.1.2.1
		"service_unavailable", // common spelling of the same outage
		"rate_limited",        // common spelling of 429
		"ratelimited",         // Slack
		// RFC 9449 §8: a nonce demand left after the one retry with the
		// server's nonce, which a nonce rotated meanwhile explains; the
		// next call proves with the newer one.
		"use_dpop_nonce":
		return CaseProviderUnavailable
	case "invalid_dpop_proof": // RFC 9449 §5
		return CaseDPoPRejected
	case "authorization_pending": // RFC 8628 §3.5
		return CaseAuthorizationPending
	case "slow_down": // RFC 8628 §3.5
		return CaseSlowDown
	case "access_denied": // RFC 8628 §3.5
		return CaseAuthorizationDenied
	case "expired_token": // RFC 8628 §3.5
		return CaseFlowExpired
	}

	// An unknown code needs auth: re-authorizing is the one action that
	// can fix a condition Wayfindr does not recognise.
	return CaseProviderError
}

// maxProviderCode bounds a provider code. Real codes are short identifiers;
// anything longer is more likely free text, which could hold a token, so it
// is dropped rather than truncated.
const maxProviderCode = 64

// sanitizeProviderCode makes a provider's error code safe to log and show:
// at most [maxProviderCode] bytes, with every byte outside [A-Za-z0-9_.:-]
// replaced by '_'. An overlong code becomes "".
func sanitizeProviderCode(code string) string {
	if len(code) > maxProviderCode {
		return ""
	}

	b := []byte(code)

	for i, c := range b {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '_', c == '.', c == ':', c == '-':
		default:
			b[i] = '_'
		}
	}

	return string(b)
}
