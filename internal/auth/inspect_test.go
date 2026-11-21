package auth

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// fixedInspector is a provider inspector for chain tests.
type fixedInspector struct {
	name    string
	host    string
	claim   bool
	outcome TokenOutcome
}

func (f fixedInspector) Name() string { return f.name }

func (f fixedInspector) Matches(issuer string) bool { return issuerHost(issuer) == f.host }

func (f fixedInspector) Inspect(int, http.Header, []byte) (TokenOutcome, bool) {
	return f.outcome, f.claim
}

// TestInspectorsOrder covers selection: inspectors whose issuer does not
// match are skipped, a matching one that declines passes to the next, the
// first to claim wins, and the RFC 6749 inspector is the fallback.
func TestInspectorsOrder(t *testing.T) {
	revoked := TokenOutcome{Case: CaseTokenRevoked, ProviderCode: "first"}
	second := TokenOutcome{Case: CaseProviderError, ProviderCode: "second"}
	chain := Inspectors{
		fixedInspector{name: "other host", host: "other.example", claim: true, outcome: TokenOutcome{ProviderCode: "other"}},
		fixedInspector{name: "declines", host: "as.example", claim: false, outcome: TokenOutcome{ProviderCode: "declined"}},
		fixedInspector{name: "claims", host: "as.example", claim: true, outcome: revoked},
		fixedInspector{name: "too late", host: "as.example", claim: true, outcome: second},
		rfc6749Inspector{},
	}
	body := []byte(`{"error":"invalid_client"}`)
	tests := []struct {
		name   string
		issuer string
		want   TokenOutcome
	}{
		{"first claiming match wins", "https://as.example/tenant", revoked},
		{"no provider match falls back to RFC 6749", "https://elsewhere.example", TokenOutcome{Case: CaseClientRejected, ProviderCode: "invalid_client"}},
		{"unparsable issuer falls back to RFC 6749", "::not a url", TokenOutcome{Case: CaseClientRejected, ProviderCode: "invalid_client"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chain.Inspect(tt.issuer, http.StatusBadRequest, http.Header{}, body)

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Inspect (-want +got):\n%s", diff)
			}
		})
	}
}

// TestInspectorsWithoutClaim covers a chain with no inspector that claims,
// which is a programming error reported as a malformed response rather
// than a panic.
func TestInspectorsWithoutClaim(t *testing.T) {
	got := Inspectors{}.Inspect("https://as.example", http.StatusOK, http.Header{}, []byte(`{"access_token":"x"}`))

	if diff := cmp.Diff(TokenOutcome{Case: CaseMalformedResponse}, got); diff != "" {
		t.Errorf("Inspect (-want +got):\n%s", diff)
	}
}

// TestDefaultInspectorsEndWithRFC6749 pins the fallback's position: a
// provider inspector added after it would never run.
func TestDefaultInspectorsEndWithRFC6749(t *testing.T) {
	chain := defaultInspectors()

	if got := chain[len(chain)-1].Name(); got != "rfc6749" {
		t.Errorf("last inspector = %q, want rfc6749", got)
	}
}
