package upstream_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/atomicpages/wayfindr/internal/upstream"
)

func TestAdviseCoversEveryKind(t *testing.T) {
	kinds := []upstream.Kind{
		upstream.KindUnknownConnector, upstream.KindNeedsAuth, upstream.KindAuthConfig,
		upstream.KindMissingValue, upstream.KindInteractiveInput, upstream.KindTimeout,
		upstream.KindCanceled, upstream.KindRejected, upstream.KindUnreachable, upstream.KindResponseTooLarge,
	}

	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			a := upstream.Advise(&upstream.Error{Connector: "c", Kind: k})

			if a.Summary == "" || a.Action == "" {
				t.Errorf("Advise = %+v, want a summary and an action", a)
			}
		})
	}
}

func TestAdviseCoversEveryCase(t *testing.T) {
	cases := map[upstream.Kind][]upstream.Case{
		upstream.KindNeedsAuth: {
			upstream.CaseNoCredential, upstream.CaseURLChanged, upstream.CaseClientChanged, upstream.CaseCredentialUnreadable,
			upstream.CaseRefreshFailed, upstream.CaseTokenRevoked, upstream.CaseProviderError,
			upstream.CaseTokenRejected, upstream.CaseStepUp, upstream.CaseRegistrationExpired,
		},
		upstream.KindAuthConfig: {
			upstream.CaseClientRejected, upstream.CaseIssuerMismatch, upstream.CaseNoRegistration,
			upstream.CaseRegistrationRejected, upstream.CaseUnresolvedValue, upstream.CaseInvalidURL,
			upstream.CaseNoAuthorizationServer, upstream.CaseNoMetadata, upstream.CasePKCEUnsupported,
			upstream.CaseDeviceUnsupported, upstream.CaseAuthMethodUnsupported, upstream.CaseSigningKeyInvalid,
			upstream.CaseGrantRejected, upstream.CaseScopeDenied, upstream.CaseDPoPAlgUnsupported,
			upstream.CaseDPoPRejected, upstream.CaseInvalidTemplate, upstream.CaseSecretUnreadable,
		},
		upstream.KindMissingValue: {upstream.CaseSecret, upstream.CaseEnv},
		upstream.KindUnreachable: {
			upstream.CaseConnectRefused, upstream.CaseConnectTimeout, upstream.CaseConnectCanceled,
			upstream.CaseSpawnFailed, upstream.CaseAuthServer, upstream.CaseProviderUnavailable,
			upstream.CaseAuthInternal,
		},
		upstream.KindResponseTooLarge: {
			upstream.CaseBody, upstream.CaseMessage, upstream.CaseSyncPages, upstream.CaseSyncTools, upstream.CaseSyncBytes,
		},
	}

	for k, cs := range cases {
		kindOnly := upstream.Advise(&upstream.Error{Connector: "c", Kind: k})

		for _, c := range cs {
			t.Run(string(c), func(t *testing.T) {
				a := upstream.Advise(&upstream.Error{Connector: "c", Kind: k, Case: c})

				if a.Summary == "" || a.Action == "" {
					t.Errorf("Advise = %+v, want a summary and an action", a)
				}

				if a.Summary == kindOnly.Summary {
					t.Errorf("Advise summary = %q, the same as the bare %s's; want the case's own", a.Summary, k)
				}
			})
		}
	}
}

func TestAdviseCarriesLinkRefAndProviderCode(t *testing.T) {
	const link = "http://127.0.0.1:7777/auth/c"
	a := upstream.Advise(&upstream.Error{Connector: "c", Kind: upstream.KindNeedsAuth, Case: upstream.CaseStepUp, Link: link})

	if a.Link != link {
		t.Errorf("Link = %q, want %q", a.Link, link)
	}

	if !strings.Contains(a.Action, "scopes") {
		t.Errorf("step-up Action = %q, want it to mention scopes", a.Action)
	}

	a = upstream.Advise(&upstream.Error{Connector: "c", Kind: upstream.KindMissingValue, Case: upstream.CaseSecret, Ref: "secret:linear"})

	if !strings.Contains(a.Summary, "secret:linear") {
		t.Errorf("Summary = %q, want it to name secret:linear", a.Summary)
	}

	a = upstream.Advise(&upstream.Error{Connector: "c", Kind: upstream.KindAuthConfig, Case: upstream.CaseSecretUnreadable, Ref: "secret:linear"})

	if !strings.Contains(a.Summary, "secret:linear") || !strings.Contains(a.Summary, "master key") {
		t.Errorf("Summary = %q, want it to name secret:linear and the master key", a.Summary)
	}

	if !strings.Contains(a.Action, "`wayfindr secret set linear`") {
		t.Errorf("Action = %q, want the command that stores the secret again", a.Action)
	}

	a = upstream.Advise(&upstream.Error{Connector: "c", Kind: upstream.KindNeedsAuth, Case: upstream.CaseProviderError, ProviderCode: "invalid_refresh_token"})

	if !strings.Contains(a.Summary, "invalid_refresh_token") {
		t.Errorf("Summary = %q, want it to quote the provider code", a.Summary)
	}

	if a := upstream.Advise(nil); a != (upstream.Advice{}) {
		t.Errorf("Advise(nil) = %+v, want zero", a)
	}
}

func TestAsError(t *testing.T) {
	e := &upstream.Error{Connector: "c", Kind: upstream.KindTimeout}
	tests := []struct {
		name string
		err  error
		want *upstream.Error
	}{
		{"the error itself", e, e},
		{"wrapped", fmt.Errorf("gateway: %w", e), e},
		{"another error", errors.New("boom"), nil},
		{"nil", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := upstream.AsError(tt.err)

			if got != tt.want || ok != (tt.want != nil) {
				t.Errorf("AsError = (%v, %v), want (%v, %v)", got, ok, tt.want, tt.want != nil)
			}
		})
	}
}

func TestErrorText(t *testing.T) {
	tests := []struct {
		name string
		err  *upstream.Error
		want string
	}{
		{"kind only", &upstream.Error{Connector: "c", Kind: upstream.KindUnknownConnector}, "connector c: unknown_connector"},
		{"kind and case", &upstream.Error{Connector: "c", Kind: upstream.KindNeedsAuth, Case: upstream.CaseStepUp},
			"connector c: needs_auth (step_up)"},
		{"with a cause, url redacted", &upstream.Error{Connector: "c", Kind: upstream.KindUnreachable, Case: upstream.CaseConnectRefused,
			Err: errors.New(`Post "https://x.example/mcp?key=hunter2": connection refused`)},
			`connector c: unreachable (connect_refused): Post "https://x.example/mcp?REDACTED": connection refused`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}
