package auth

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestSlackInspectorMatches(t *testing.T) {
	tests := []struct {
		issuer string
		want   bool
	}{
		{"https://slack.com", true},
		{"https://slack.com/", true},
		{"https://SLACK.com:443/oauth", true},
		{"https://api.slack.com", true},
		{"https://notslack.com", false},
		{"https://slack.com.evil.example", false},
		{"https://auth.example.com", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := (slackInspector{}).Matches(tt.issuer); got != tt.want {
			t.Errorf("Matches(%q) = %v, want %v", tt.issuer, got, tt.want)
		}
	}
}

// TestInspectSlack runs Slack-shaped responses through the default chain
// for issuer slack.com. Bodies follow the examples at
// https://docs.slack.dev/reference/methods/oauth.v2.access.
func TestInspectSlack(t *testing.T) {
	const issuer = "https://slack.com"
	tests := []struct {
		name   string
		status int
		body   string
		want   TokenOutcome
	}{
		{
			name:   "bot and user tokens use the top-level token",
			status: http.StatusOK,
			body: `{"ok":true,"access_token":"xoxb-1","token_type":"bot","scope":"commands,incoming-webhook",
				"bot_user_id":"U0KRQLJ9H","app_id":"A0KRD7HC3","team":{"name":"Slack Softball Team","id":"T9TK3CUKW"},
				"authed_user":{"id":"U1234","scope":"chat:write","access_token":"xoxp-1","token_type":"user"}}`,
			want: TokenOutcome{
				Token: &token{Access: "xoxb-1", Type: "bot"},
				Scope: "commands incoming-webhook",
			},
		},
		{
			name:   "token rotation",
			status: http.StatusOK,
			body: `{"ok":true,"access_token":"xoxe.xoxb-1","token_type":"bot","scope":"chat:write",
				"expires_in":43200,"refresh_token":"xoxe-1-r",
				"authed_user":{"id":"U1234","access_token":"xoxe.xoxp-1","token_type":"user","expires_in":43200,"refresh_token":"xoxe-1-u"}}`,
			want: TokenOutcome{
				Token: &token{Access: "xoxe.xoxb-1", Refresh: "xoxe-1-r", Type: "bot"},
				Scope: "chat:write", ExpiresIn: 12 * time.Hour,
			},
		},
		{
			name:   "user token only lives under authed_user",
			status: http.StatusOK,
			body: `{"ok":true,"app_id":"A0118NQPZDF","team":{"id":"T024BE7LD"},"enterprise":null,"is_enterprise_install":false,
				"authed_user":{"id":"U1234","scope":"identity.basic,identity.email","access_token":"xoxp-1",
				"token_type":"user","expires_in":43200,"refresh_token":"xoxe-1-u"}}`,
			want: TokenOutcome{
				Token: &token{Access: "xoxp-1", Refresh: "xoxe-1-u", Type: "user"},
				Scope: "identity.basic identity.email", ExpiresIn: 12 * time.Hour,
			},
		},
		{
			name:   "invalid_refresh_token on 200",
			status: http.StatusOK,
			body:   `{"ok":false,"error":"invalid_refresh_token"}`,
			want:   TokenOutcome{Case: CaseTokenRevoked, ProviderCode: "invalid_refresh_token"},
		},
		{
			name:   "token_revoked",
			status: http.StatusOK,
			body:   `{"ok":false,"error":"token_revoked"}`,
			want:   TokenOutcome{Case: CaseTokenRevoked, ProviderCode: "token_revoked"},
		},
		{
			name:   "invalid_client_id",
			status: http.StatusOK,
			body:   `{"ok":false,"error":"invalid_client_id"}`,
			want:   TokenOutcome{Case: CaseClientRejected, ProviderCode: "invalid_client_id"},
		},
		{
			name:   "bad_client_secret",
			status: http.StatusOK,
			body:   `{"ok":false,"error":"bad_client_secret"}`,
			want:   TokenOutcome{Case: CaseClientRejected, ProviderCode: "bad_client_secret"},
		},
		{
			name:   "ratelimited is transient",
			status: http.StatusOK,
			body:   `{"ok":false,"error":"ratelimited"}`,
			want:   TokenOutcome{Case: CaseProviderUnavailable, ProviderCode: "ratelimited"},
		},
		{
			name:   "unknown code keeps the provider code",
			status: http.StatusOK,
			body:   `{"ok":false,"error":"team_added_to_org"}`,
			want:   TokenOutcome{Case: CaseProviderError, ProviderCode: "team_added_to_org"},
		},
		{
			name:   "ok true without any token",
			status: http.StatusOK,
			body:   `{"ok":true,"app_id":"A1","authed_user":{"id":"U1"}}`,
			want:   TokenOutcome{Case: CaseMalformedResponse},
		},
		{
			name:   "a body without ok is left to the RFC 6749 inspector",
			status: http.StatusOK,
			body:   `{"access_token":"at-1","token_type":"Bearer","expires_in":60,"scope":"a,b"}`,
			want:   TokenOutcome{Token: &token{Access: "at-1", Type: "Bearer"}, Scope: "a,b", ExpiresIn: time.Minute},
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
