package upstream

// State is a connector's state (GLOSSARY.md, "Connector states"). A
// connector is in exactly one state at a time. The zero value means the
// manager does not know the connector.
type State string

// Connector states.
const (
	// StateStarting: configured, first connect not finished yet.
	StateStarting State = "starting"
	// StateReady: connected and callable.
	StateReady State = "ready"
	// StateNeedsAuth: no valid credential; the user runs the auth flow.
	StateNeedsAuth State = "needs_auth"
	// StateAuthError: auth cannot proceed as configured; the user changes
	// config, or sets the missing secret and the connector is retried.
	StateAuthError State = "auth_error"
	// StateUnreachable: transport-level failure; retried on the next sync or
	// call.
	StateUnreachable State = "unreachable"
)

// Event is something that happened to a connector and may move it to
// another state. The events are the edge labels of the diagram in
// docs/internal/design.md §7.2.
type Event int

// Connector events.
const (
	// EventConfigured: Apply added the connector or changed its config.
	EventConfigured Event = iota + 1
	// EventSynced: a connect, sync, or call succeeded. In needs_auth and
	// auth_error it is stale (see EventAuthorized) and refused.
	EventSynced
	// EventTransportError: spawn failed, connection refused, timeout.
	EventTransportError
	// EventAuthRequired: no valid credential (never authorized, refresh
	// failed, 403 step-up).
	EventAuthRequired
	// EventURLChanged: Apply changed the connector's url, so its tokens,
	// bound to the old url, no longer apply.
	EventURLChanged
	// EventAuthorized: the user may have fixed what kept the connector
	// from working (authorized it, set a missing secret, brought its
	// upstream back), so it is connected again. Manager.Retry fires it, and
	// so does a success over a session opened after the connector entered
	// needs_auth or auth_error, just before that success's EventSynced.
	EventAuthorized
	// EventAuthConfigError: the config can't work (missing ${…} reference,
	// no PKCE, no client_id and no DCR, issuer mismatch).
	EventAuthConfigError
)

// String names the event for logs and test output.
func (e Event) String() string {
	switch e {
	case EventConfigured:
		return "configured"
	case EventSynced:
		return "synced"
	case EventTransportError:
		return "transport_error"
	case EventAuthRequired:
		return "auth_required"
	case EventURLChanged:
		return "url_changed"
	case EventAuthorized:
		return "authorized"
	case EventAuthConfigError:
		return "auth_config_error"
	default:
		return "unknown"
	}
}

// Transition returns the state a connector in from moves to on event. ok is
// false when the event does not apply in from; the state is then unchanged.
//
// Config events (EventConfigured, EventURLChanged, EventAuthConfigError)
// apply from any state, because a new config replaces whatever the old one
// produced. Runtime events are refused where they would hide a problem the
// user must fix: a success or transport error doesn't clear needs_auth or
// auth_error, and a lost credential doesn't hide auth_error. Only a config
// change or EventAuthorized (a retry) leaves those states.
func Transition(from State, event Event) (to State, ok bool) {
	switch event {
	case EventConfigured:
		return StateStarting, true
	case EventURLChanged:
		return StateNeedsAuth, true
	case EventAuthConfigError:
		return StateAuthError, true
	case EventSynced:
		switch from {
		case StateStarting, StateReady, StateUnreachable:
			return StateReady, true
		case StateNeedsAuth, StateAuthError: // refused
		}
	case EventTransportError:
		switch from {
		case StateStarting, StateReady, StateUnreachable:
			return StateUnreachable, true
		case StateNeedsAuth, StateAuthError: // refused
		}
	case EventAuthRequired:
		switch from {
		case StateStarting, StateReady, StateUnreachable, StateNeedsAuth:
			return StateNeedsAuth, true
		case StateAuthError: // refused: only a config change clears it
		}
	case EventAuthorized:
		switch from {
		case StateNeedsAuth, StateAuthError, StateUnreachable:
			return StateStarting, true
		case StateStarting, StateReady: // refused: nothing to retry
		}
	}

	return from, false
}
