package upstream_test

import (
	"testing"

	"github.com/atomicpages/wayfindr/internal/upstream"
)

// TestTransition covers every edge of the state diagram in docs/internal/design.md
// §7.2, plus the self-loops and refusals that keep a connector's state
// honest. Transition is the design's decision table, so it is tested
// directly (CODING_STANDARDS: Tests); the recovery tests cover its effect
// through the Manager.
func TestTransition(t *testing.T) {
	const (
		none        = upstream.State("")
		starting    = upstream.StateStarting
		ready       = upstream.StateReady
		needsAuth   = upstream.StateNeedsAuth
		authError   = upstream.StateAuthError
		unreachable = upstream.StateUnreachable
	)
	tests := []struct {
		name   string
		from   upstream.State
		event  upstream.Event
		want   upstream.State
		wantOK bool
	}{
		// Diagram edges.
		{"Apply(add) enters starting", none, upstream.EventConfigured, starting, true},
		{"Apply(changed) restarts a ready connector", ready, upstream.EventConfigured, starting, true},
		{"starting sync ok", starting, upstream.EventSynced, ready, true},
		{"starting transport err", starting, upstream.EventTransportError, unreachable, true},
		{"starting auth needed", starting, upstream.EventAuthRequired, needsAuth, true},
		{"ready transport err", ready, upstream.EventTransportError, unreachable, true},
		{"ready refresh failed or step-up", ready, upstream.EventAuthRequired, needsAuth, true},
		{"ready url changed", ready, upstream.EventURLChanged, needsAuth, true},
		{"unreachable transport err again", unreachable, upstream.EventTransportError, unreachable, true},
		{"unreachable sync or call ok", unreachable, upstream.EventSynced, ready, true},
		{"needs_auth auth flow completed", needsAuth, upstream.EventAuthorized, starting, true},
		{"starting config can't work", starting, upstream.EventAuthConfigError, authError, true},
		{"ready config can't work", ready, upstream.EventAuthConfigError, authError, true},
		{"unreachable config can't work", unreachable, upstream.EventAuthConfigError, authError, true},
		{"needs_auth config can't work", needsAuth, upstream.EventAuthConfigError, authError, true},
		{"auth_error config changed", authError, upstream.EventConfigured, starting, true},
		{"auth_error retried after a fix", authError, upstream.EventAuthorized, starting, true},
		{"unreachable retried", unreachable, upstream.EventAuthorized, starting, true},

		// Self-loops and config events that apply from anywhere.
		{"ready sync ok stays ready", ready, upstream.EventSynced, ready, true},
		{"auth_error still can't work", authError, upstream.EventAuthConfigError, authError, true},
		{"unreachable reconfigured", unreachable, upstream.EventConfigured, starting, true},
		{"needs_auth reconfigured", needsAuth, upstream.EventConfigured, starting, true},
		{"starting reconfigured", starting, upstream.EventConfigured, starting, true},
		{"unreachable url changed", unreachable, upstream.EventURLChanged, needsAuth, true},
		{"auth_error url changed", authError, upstream.EventURLChanged, needsAuth, true},
		{"unreachable retry finds no credential", unreachable, upstream.EventAuthRequired, needsAuth, true},

		// Refused: the event can't move the connector out of this state.
		{"needs_auth ignores sync ok", needsAuth, upstream.EventSynced, needsAuth, false},
		{"needs_auth ignores transport err", needsAuth, upstream.EventTransportError, needsAuth, false},
		{"auth_error ignores sync ok", authError, upstream.EventSynced, authError, false},
		{"auth_error ignores transport err", authError, upstream.EventTransportError, authError, false},
		{"auth_error ignores auth needed", authError, upstream.EventAuthRequired, authError, false},
		{"ready ignores authorized", ready, upstream.EventAuthorized, ready, false},
		{"starting ignores authorized", starting, upstream.EventAuthorized, starting, false},
		{"unconfigured ignores sync ok", none, upstream.EventSynced, none, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := upstream.Transition(tt.from, tt.event)

			if got != tt.want || ok != tt.wantOK {
				t.Errorf("Transition(%q, %v) = (%q, %v), want (%q, %v)", tt.from, tt.event, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
