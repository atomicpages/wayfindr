package auth

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestInspectRFC6749 runs token-endpoint responses from an issuer no
// provider inspector claims through the default chain, so the RFC 6749
// inspector reads them.
func TestInspectRFC6749(t *testing.T) {
	const issuer = "https://auth.example.com"
	tests := []struct {
		name   string
		status int
		body   string
		want   TokenOutcome
	}{
		{
			name:   "success",
			status: http.StatusOK,
			body:   `{"access_token":"at-1","token_type":"Bearer","refresh_token":"rt-1","expires_in":3600,"scope":"read write"}`,
			want: TokenOutcome{
				Token: &token{Access: "at-1", Refresh: "rt-1", Type: "Bearer"},
				Scope: "read write", ExpiresIn: time.Hour,
			},
		},
		{
			name:   "success with refresh_token_expires_in",
			status: http.StatusOK,
			body:   `{"access_token":"at-1","refresh_token":"rt-1","expires_in":28800,"refresh_token_expires_in":"15897600"}`,
			want: TokenOutcome{
				Token:     &token{Access: "at-1", Refresh: "rt-1"},
				ExpiresIn: 8 * time.Hour, RefreshExpiresIn: 184 * 24 * time.Hour,
			},
		},
		{
			name:   "success with expires_in as a string",
			status: http.StatusOK,
			body:   `{"access_token":"at-1","token_type":"bearer","expires_in":"120"}`,
			want:   TokenOutcome{Token: &token{Access: "at-1", Type: "bearer"}, ExpiresIn: 2 * time.Minute},
		},
		{
			name:   "success on another 2xx with an empty error field",
			status: http.StatusCreated,
			body:   `{"access_token":"at-1","error":""}`,
			want:   TokenOutcome{Token: &token{Access: "at-1"}},
		},
		{
			name:   "invalid_grant on 400",
			status: http.StatusBadRequest,
			body:   `{"error":"invalid_grant","error_description":"refresh token revoked"}`,
			want:   TokenOutcome{Case: CaseTokenRevoked, ProviderCode: "invalid_grant"},
		},
		{
			name:   "invalid_client on 401",
			status: http.StatusUnauthorized,
			body:   `{"error":"invalid_client"}`,
			want:   TokenOutcome{Case: CaseClientRejected, ProviderCode: "invalid_client"},
		},
		{
			name:   "unauthorized_client",
			status: http.StatusBadRequest,
			body:   `{"error":"unauthorized_client"}`,
			want:   TokenOutcome{Case: CaseClientRejected, ProviderCode: "unauthorized_client"},
		},
		{
			name:   "ok false on 200 is an error whatever the status",
			status: http.StatusOK,
			body:   `{"ok":false,"error":"invalid_refresh_token"}`,
			want:   TokenOutcome{Case: CaseTokenRevoked, ProviderCode: "invalid_refresh_token"},
		},
		{
			name:   "temporarily_unavailable is transient",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"temporarily_unavailable"}`,
			want:   TokenOutcome{Case: CaseProviderUnavailable, ProviderCode: "temporarily_unavailable"},
		},
		{
			name:   "server_error is transient",
			status: http.StatusInternalServerError,
			body:   `{"error":"server_error"}`,
			want:   TokenOutcome{Case: CaseProviderUnavailable, ProviderCode: "server_error"},
		},
		{
			name:   "rate_limited is transient",
			status: http.StatusTooManyRequests,
			body:   `{"error":"rate_limited"}`,
			want:   TokenOutcome{Case: CaseProviderUnavailable, ProviderCode: "rate_limited"},
		},
		{
			name:   "service_unavailable is transient",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"service_unavailable"}`,
			want:   TokenOutcome{Case: CaseProviderUnavailable, ProviderCode: "service_unavailable"},
		},
		{
			name:   "transient codes match case-insensitively",
			status: http.StatusTooManyRequests,
			body:   `{"error":"RateLimited"}`,
			want:   TokenOutcome{Case: CaseProviderUnavailable, ProviderCode: "RateLimited"},
		},
		{
			name:   "authorization_pending (RFC 8628)",
			status: http.StatusBadRequest,
			body:   `{"error":"authorization_pending"}`,
			want:   TokenOutcome{Case: CaseAuthorizationPending, ProviderCode: "authorization_pending"},
		},
		{
			name:   "slow_down (RFC 8628)",
			status: http.StatusBadRequest,
			body:   `{"error":"slow_down"}`,
			want:   TokenOutcome{Case: CaseSlowDown, ProviderCode: "slow_down"},
		},
		{
			name:   "access_denied (RFC 8628)",
			status: http.StatusBadRequest,
			body:   `{"error":"access_denied"}`,
			want:   TokenOutcome{Case: CaseAuthorizationDenied, ProviderCode: "access_denied"},
		},
		{
			name:   "expired_token (RFC 8628)",
			status: http.StatusBadRequest,
			body:   `{"error":"expired_token"}`,
			want:   TokenOutcome{Case: CaseFlowExpired, ProviderCode: "expired_token"},
		},
		{
			name:   "invalid_dpop_proof (RFC 9449)",
			status: http.StatusBadRequest,
			body:   `{"error":"invalid_dpop_proof"}`,
			want:   TokenOutcome{Case: CaseDPoPRejected, ProviderCode: "invalid_dpop_proof"},
		},
		{
			name:   "use_dpop_nonce left after the retry (RFC 9449)",
			status: http.StatusBadRequest,
			body:   `{"error":"use_dpop_nonce"}`,
			want:   TokenOutcome{Case: CaseProviderUnavailable, ProviderCode: "use_dpop_nonce"},
		},
		{
			name:   "error field on 200 is an error",
			status: http.StatusOK,
			body:   `{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`,
			want:   TokenOutcome{Case: CaseProviderError, ProviderCode: "bad_verification_code"},
		},
		{
			name:   "error wins over an access token",
			status: http.StatusOK,
			body:   `{"access_token":"at-1","error":"token_expired"}`,
			want:   TokenOutcome{Case: CaseTokenRevoked, ProviderCode: "token_expired"},
		},
		{
			name:   "alias matching ignores case",
			status: http.StatusBadRequest,
			body:   `{"error":"Invalid_Grant"}`,
			want:   TokenOutcome{Case: CaseTokenRevoked, ProviderCode: "Invalid_Grant"},
		},
		{
			name:   "ok false without a code",
			status: http.StatusOK,
			body:   `{"ok":false}`,
			want:   TokenOutcome{Case: CaseProviderError},
		},
		{
			name:   "unknown code on 5xx is still the provider's error",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"maintenance_window"}`,
			want:   TokenOutcome{Case: CaseProviderError, ProviderCode: "maintenance_window"},
		},
		{
			name:   "error that is not a string",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"x","message":"nope"}}`,
			want:   TokenOutcome{Case: CaseProviderError},
		},
		{
			name:   "provider code with unprintable and odd characters",
			status: http.StatusBadRequest,
			body:   `{"error":"bad code\u0000<b>"}`,
			want:   TokenOutcome{Case: CaseProviderError, ProviderCode: "bad_code__b_"},
		},
		{
			name:   "overlong provider code is dropped",
			status: http.StatusBadRequest,
			body:   `{"error":"` + strings.Repeat("x", 65) + `"}`,
			want:   TokenOutcome{Case: CaseProviderError},
		},
		{
			name:   "2xx without a token or an error",
			status: http.StatusOK,
			body:   `{"token_type":"Bearer"}`,
			want:   TokenOutcome{Case: CaseMalformedResponse},
		},
		{
			name:   "2xx that is not JSON",
			status: http.StatusOK,
			body:   `<html>hello</html>`,
			want:   TokenOutcome{Case: CaseMalformedResponse},
		},
		{
			name:   "non-2xx without an error code",
			status: http.StatusBadGateway,
			body:   `<html>bad gateway</html>`,
			want:   TokenOutcome{Case: CaseUnexpectedStatus},
		},
		{
			name:   "non-2xx with a token but no error",
			status: http.StatusNotFound,
			body:   `{"access_token":"at-1"}`,
			want:   TokenOutcome{Case: CaseUnexpectedStatus},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultInspectors().Inspect(issuer, tt.status, http.Header{}, []byte(tt.body))

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Inspect (-want +got):\n%s", diff)
			}
		})
	}
}
