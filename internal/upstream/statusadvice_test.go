package upstream_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/upstream"
)

// TestConnectorStatusAdvice pins the one reading of a connector's state
// (GLOSSARY: Advice): the advice of the error that put the connector in its
// state, or the state's own wording when no error did. Reason never shows.
func TestConnectorStatusAdvice(t *testing.T) {
	const (
		link   = "http://127.0.0.1:7777/auth/linear"
		reason = "dial tcp 127.0.0.1:9: connection refused"
	)
	tests := []struct {
		name string
		s    upstream.ConnectorStatus
		want upstream.Advice
	}{
		{"ready", upstream.ConnectorStatus{State: upstream.StateReady}, upstream.Advice{}},
		{"starting", upstream.ConnectorStatus{State: upstream.StateStarting}, upstream.Advice{
			Summary: "The connector is still connecting.",
			Action:  "Try again in a moment.",
		}},
		{"needs_auth without an error", upstream.ConnectorStatus{State: upstream.StateNeedsAuth, Link: link}, upstream.Advice{
			Summary: "The connector needs to be authorized.",
			Action:  "Open the link to authorize it.",
			Link:    link,
		}},
		{"needs_auth (no_credential)", upstream.ConnectorStatus{State: upstream.StateNeedsAuth, Reason: "no credential",
			Kind: upstream.KindNeedsAuth, Case: upstream.CaseNoCredential, Link: link}, upstream.Advice{
			Summary: "The connector has not been authorized yet.",
			Action:  "Open the link to authorize it.",
			Link:    link,
		}},
		{"needs_auth (token_revoked) with a provider code", upstream.ConnectorStatus{State: upstream.StateNeedsAuth,
			Kind: upstream.KindNeedsAuth, Case: upstream.CaseTokenRevoked, ProviderCode: "invalid_grant", Link: link}, upstream.Advice{
			Summary: "The authorization server revoked or expired the connector's grant (provider code invalid_grant).",
			Action:  "Open the link to authorize the connector again.",
			Link:    link,
		}},
		{"auth_error without an error", upstream.ConnectorStatus{State: upstream.StateAuthError, Reason: reason}, upstream.Advice{
			Summary: "The connector's auth cannot work as configured.",
			Action:  "Fix the connector's config; the daemon log has the details.",
		}},
		{"auth_error (issuer_mismatch)", upstream.ConnectorStatus{State: upstream.StateAuthError,
			Kind: upstream.KindAuthConfig, Case: upstream.CaseIssuerMismatch}, upstream.Advice{
			Summary: "The authorization server's metadata names a different issuer than the one it was found for.",
			Action:  "Check authorization_server in the connector's [auth] table.",
		}},
		{"auth_error (missing_value)", upstream.ConnectorStatus{State: upstream.StateAuthError, Reason: "secret:notes is not set",
			Kind: upstream.KindMissingValue, Case: upstream.CaseSecret, Ref: "secret:notes"}, upstream.Advice{
			Summary: "The connector's config refers to secret:notes, but no such secret is stored.",
			Action:  "Store the secret, or fix the reference in the connector's config.",
		}},
		{"unreachable without an error", upstream.ConnectorStatus{State: upstream.StateUnreachable, Reason: reason}, upstream.Advice{
			Summary: "The upstream cannot be reached.",
			Action:  "Check that it is running and the network. Wayfindr reconnects on the next call.",
		}},
		{"unreachable (connect_refused)", upstream.ConnectorStatus{State: upstream.StateUnreachable, Reason: reason,
			Kind: upstream.KindUnreachable, Case: upstream.CaseConnectRefused}, upstream.Advice{
			Summary: "The upstream refused the connection.",
			Action:  "Check that the server is running at the connector's url. Wayfindr reconnects on the next call.",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.s.Connector = "linear"

			if diff := cmp.Diff(tt.want, tt.s.Advice()); diff != "" {
				t.Errorf("Advice (-want +got):\n%s", diff)
			}
		})
	}
}

// TestConnectorStatusAdviceCoversEveryStateAndKind reads every state with
// every Kind, and with none: a ready connector has no advice, every other
// has a summary and an action, and none shows the technical Reason.
func TestConnectorStatusAdviceCoversEveryStateAndKind(t *testing.T) {
	const reason = "REASON-TEXT-FOR-LOGS-ONLY"
	states := []upstream.State{
		upstream.StateStarting, upstream.StateReady, upstream.StateNeedsAuth,
		upstream.StateAuthError, upstream.StateUnreachable,
	}
	kinds := []upstream.Kind{
		"", upstream.KindUnknownConnector, upstream.KindNeedsAuth, upstream.KindAuthConfig,
		upstream.KindMissingValue, upstream.KindInteractiveInput, upstream.KindTimeout,
		upstream.KindCanceled, upstream.KindRejected, upstream.KindUnreachable,
	}

	for _, st := range states {
		for _, k := range kinds {
			t.Run(string(st)+"/"+string(k), func(t *testing.T) {
				a := upstream.ConnectorStatus{Connector: "c", State: st, Reason: reason, Kind: k}.Advice()

				if strings.Contains(a.Summary+a.Action+a.Link, reason) {
					t.Errorf("Advice = %+v shows the Reason", a)
				}

				if st == upstream.StateReady {
					if a != (upstream.Advice{}) {
						t.Errorf("Advice = %+v, want none for a ready connector", a)
					}

					return
				}

				if a.Summary == "" || a.Action == "" {
					t.Errorf("Advice = %+v, want a summary and an action", a)
				}
			})
		}
	}
}

// A connector out of ready for more than a day is stuck: its own advice,
// with since when in the summary.
func TestConnectorStatusStuck(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	// Unreachable for an hour, after needs_auth: out of ready for a day.
	down := upstream.ConnectorStatus{
		State: upstream.StateUnreachable, Since: now.Add(-time.Hour), NotReadySince: now.Add(-24 * time.Hour),
	}

	if a, ok := down.Stuck(now); ok {
		t.Errorf("Stuck after exactly a day = %+v, true; want false", a)
	}

	down.NotReadySince = down.NotReadySince.Add(-time.Second)
	a, ok := down.Stuck(now)
	want := down.Advice()
	want.Summary = "The connector has not been ready since 2026-11-07T09:59:59Z."

	if diff := cmp.Diff(want, a); !ok || diff != "" {
		t.Errorf("Stuck after more than a day: ok = %v, advice (-want +got):\n%s", ok, diff)
	}

	if _, ok := (upstream.ConnectorStatus{State: upstream.StateReady, Since: now.Add(-48 * time.Hour)}).Stuck(now); ok {
		t.Error("a ready connector is stuck; want never")
	}
}
