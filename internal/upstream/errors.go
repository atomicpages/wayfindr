package upstream

import (
	"errors"

	"github.com/atomicpages/wayfindr/internal/redact"
)

// Kind is the coarse class of a failed Call or ListTools (GLOSSARY: Kind).
// It is a closed set: callers branch on these constants, never on error
// text.
type Kind string

// Kinds.
const (
	// KindUnknownConnector: no connector has the name, or Apply or Close
	// removed it, during the use included.
	KindUnknownConnector Kind = "unknown_connector"
	// KindNeedsAuth: the connector has no usable credential. Link starts
	// the auth flow. The connector is in needs_auth.
	KindNeedsAuth Kind = "needs_auth"
	// KindAuthConfig: auth cannot proceed as configured, or the connector's
	// config is malformed. The connector is in auth_error.
	KindAuthConfig Kind = "auth_config"
	// KindMissingValue: a ${secret:…} or ${env:…} reference in the
	// connector's config names nothing; Ref names it. The connector is in
	// auth_error.
	KindMissingValue Kind = "missing_value"
	// KindInteractiveInput: the upstream asked for input that Wayfindr
	// can't put to the user: sampling, or an elicitation it can't tie to
	// one call. Elicitations it can are a result, not an error (see Call).
	KindInteractiveInput Kind = "interactive_input"
	// KindInputExpired: Continue was given a state whose interaction
	// expired, was discarded, or was answered already, or one Wayfindr
	// never issued.
	KindInputExpired Kind = "input_expired"
	// KindTimeout: the use outlasted the connector's timeout. The state is
	// unchanged: a slow tool says nothing about the upstream.
	KindTimeout Kind = "timeout"
	// KindCanceled: the caller gave up (its context was cancelled, or its
	// own deadline passed). The state is unchanged.
	KindCanceled Kind = "canceled"
	// KindRejected: the upstream answered with a JSON-RPC error, such as an
	// unknown tool or bad arguments.
	KindRejected Kind = "rejected"
	// KindUnreachable: a failure below the protocol (spawn failed,
	// connection refused, connect timed out or given up on, the auth server
	// failed). The connector is unreachable; the next use reconnects.
	KindUnreachable Kind = "unreachable"
)

// Case is the finer reason within a [Kind] (GLOSSARY: Case). It is a
// closed set, and each Case belongs to one Kind. The empty Case means no
// finer reason is known.
type Case string

// Cases of [KindNeedsAuth]. They mirror auth's needs_auth cases, which
// arrive through AuthCase() (see authCase).
const (
	// CaseNoCredential: the connector was never authorized.
	CaseNoCredential Case = "no_credential" //nolint:gosec // G101: a Case name, not a credential
	// CaseURLChanged: the credential is bound to the connector's old url.
	CaseURLChanged Case = "url_changed"
	// CaseClientChanged: the credential was granted to another OAuth client
	// than the connector's current one.
	CaseClientChanged Case = "client_changed"
	// CaseAuthorizationServerChanged: discovery now names another token
	// endpoint than the one the credential was granted at.
	CaseAuthorizationServerChanged Case = "authorization_server_changed"
	// CaseCredentialUnreadable: the stored credential can't be read, for
	// example after a key change.
	CaseCredentialUnreadable Case = "credential_unreadable" //nolint:gosec // G101: a Case name, not a credential
	// CaseRefreshFailed: the token expired and can't be refreshed.
	CaseRefreshFailed Case = "refresh_failed"
	// CaseTokenRevoked: the authorization server rejected the grant.
	CaseTokenRevoked Case = "token_revoked"
	// CaseProviderError: the authorization server returned an error code
	// Wayfindr doesn't know; ProviderCode holds it when auth passes it on.
	CaseProviderError Case = "provider_error"
	// CaseTokenRejected: the upstream answered 401 to a fresh token.
	CaseTokenRejected Case = "token_rejected"
	// CaseStepUp: the upstream needs scopes the credential lacks.
	CaseStepUp Case = "step_up"
	// CaseRegistrationExpired: the authorization server rejected the
	// dynamically registered client, which was forgotten; authorizing
	// again registers a new one.
	CaseRegistrationExpired Case = "registration_expired"
)

// Cases of [KindAuthConfig]. All but CaseInvalidTemplate and
// CaseSecretUnreadable mirror auth's config cases; the two tls Cases are
// also what a [tls] table that can't be used is.
const (
	// CaseClientRejected: the authorization server rejected the OAuth
	// client.
	CaseClientRejected Case = "client_rejected"
	// CaseIssuerMismatch: the authorization server's metadata names another
	// issuer.
	CaseIssuerMismatch Case = "issuer_mismatch"
	// CaseNoRegistration: no client_id, and neither CIMD nor dynamic client
	// registration is supported.
	CaseNoRegistration Case = "no_registration"
	// CaseRegistrationRejected: dynamic client registration was rejected.
	CaseRegistrationRejected Case = "registration_rejected"
	// CaseUnresolvedValue: a ${…} reference in the auth setup can't be
	// resolved.
	CaseUnresolvedValue Case = "unresolved_value"
	// CaseInvalidURL: a URL in the auth setup is not absolute.
	CaseInvalidURL Case = "invalid_url"
	// CaseNoAuthorizationServer: the protected resource metadata lists no
	// authorization server.
	CaseNoAuthorizationServer Case = "no_authorization_server"
	// CaseNoMetadata: the authorization server publishes no metadata and
	// config names no endpoints.
	CaseNoMetadata Case = "no_metadata"
	// CasePKCEUnsupported: no client_id, and the authorization server does
	// not advertise PKCE S256 (ADR 0007).
	CasePKCEUnsupported Case = "pkce_unsupported"
	// CaseDeviceUnsupported: the connector's grant is device_code, and its
	// authorization server has no device authorization endpoint.
	CaseDeviceUnsupported Case = "device_unsupported"
	// CaseAuthMethodUnsupported: the authorization server accepts no client
	// authentication method that fits the OAuth client's credential.
	CaseAuthMethodUnsupported Case = "auth_method_unsupported"
	// CaseSigningKeyInvalid: the connector's signing_key is not a private
	// key Wayfindr can sign with, or cannot sign its configured alg.
	CaseSigningKeyInvalid Case = "signing_key_invalid"
	// CaseTLSUntrusted: Wayfindr does not trust the TLS certificate of the
	// connector's authorization server, or the connector's ca_file can't be
	// read.
	CaseTLSUntrusted Case = "tls_untrusted"
	// CaseTLSHandshake: the authorization server refused the connector's
	// client certificate (or the lack of one) in the TLS handshake, or the
	// certificate can't be loaded.
	CaseTLSHandshake Case = "tls_handshake"
	// CaseGrantRejected: the authorization server refused the connector's
	// unattended grant, or the upstream refused a token fresh from it.
	CaseGrantRejected Case = "grant_rejected"
	// CaseScopeDenied: the upstream needs scopes the connector's unattended
	// grant asked for but was not given.
	CaseScopeDenied Case = "scope_denied"
	// CaseDPoPAlgUnsupported: the connector's dpop is "always", and its
	// authorization server does not accept ES256 DPoP proofs.
	CaseDPoPAlgUnsupported Case = "dpop_alg_unsupported"
	// CaseDPoPRejected: the authorization server rejected Wayfindr's DPoP
	// proof (invalid_dpop_proof).
	CaseDPoPRejected Case = "dpop_rejected"
	// CaseInvalidTemplate: a ${…} reference in the connector's config is
	// malformed.
	CaseInvalidTemplate Case = "invalid_template"
	// CaseSecretUnreadable: a ${secret:…} reference names a stored secret
	// that can't be decrypted, likely because the master key changed; Ref
	// names it.
	CaseSecretUnreadable Case = "secret_unreadable" //nolint:gosec // G101: a Case name, not a credential
)

// Cases of [KindMissingValue]: where the missing reference reads from.
const (
	// CaseSecret: a ${secret:…} reference names no stored secret.
	CaseSecret Case = "secret"
	// CaseEnv: a ${env:…} reference names an unset environment variable.
	CaseEnv Case = "env"
)

// Cases of [KindInteractiveInput].
const (
	// CaseSampling: the upstream asked the client to sample a message.
	CaseSampling Case = "sampling"
	// CaseConcurrentCalls: an upstream at 2025-x sent elicitation/create
	// while another call to it was in flight, so Wayfindr couldn't tell
	// which call asked.
	CaseConcurrentCalls Case = "concurrent_calls"
)

// Cases of [KindUnreachable].
const (
	// CaseConnectRefused: the upstream refused the connection.
	CaseConnectRefused Case = "connect_refused"
	// CaseConnectTimeout: the connect (spawn or dial, and initialize)
	// outlasted the connect timeout or the connector's timeout.
	CaseConnectTimeout Case = "connect_timeout"
	// CaseConnectCanceled: the caller gave up while the connect was in
	// progress, before the upstream answered.
	CaseConnectCanceled Case = "connect_canceled"
	// CaseSpawnFailed: a stdio connector's command could not be started.
	CaseSpawnFailed Case = "spawn_failed"
	// CaseAuthServer: the authorization server could not be reached, or
	// answered with nothing usable.
	CaseAuthServer Case = "auth_server"
	// CaseProviderUnavailable: the authorization server answered with a
	// transient error code (rate limited, temporarily unavailable).
	CaseProviderUnavailable Case = "provider_unavailable"
	// CaseAuthInternal: Wayfindr's own auth side failed (its credential
	// store, or a request that could not be sent again).
	CaseAuthInternal Case = "auth_internal"
)

// Error is every error Call and ListTools return (CODING_STANDARDS: Seam
// errors). It is always returned as error; narrow it with [AsError].
type Error struct {
	Connector string
	Kind      Kind
	Case      Case
	// Link starts the auth flow (the daemon's /auth/<connector>). Set on
	// KindNeedsAuth errors whose cause carried one.
	Link string
	// Ref names the missing reference on KindMissingValue errors, and the
	// unreadable secret on CaseSecretUnreadable ones, as "<kind>:<name>"
	// (e.g. "secret:linear", "env:GITHUB_TOKEN").
	Ref string
	// ProviderCode is the authorization server's raw error code, if the
	// auth error passed one on (GLOSSARY: Provider code).
	ProviderCode string
	// Err is the cause, if any.
	Err error
	// mask hides the connector's reference values, which a resolved url
	// carries into Err's text, wherever they sit (wayfindr-wt2v).
	mask redact.Values
}

// Error is technical text for logs and status:
// "connector <c>: <kind>[ (<case>)][: <cause>]", with URLs redacted
// (design §9). What it means to the user comes from [Advise].
func (e *Error) Error() string {
	return "connector " + e.Connector + ": " + e.detail()
}

// detail is Error without the connector, which a connector's state reason
// already belongs to.
func (e *Error) detail() string {
	s := string(e.Kind)

	if e.Case != "" {
		s += " (" + string(e.Case) + ")"
	}

	if e.Err != nil {
		s += ": " + e.Err.Error()
	}

	return e.mask.Apply(redact.URLQueries(s))
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// UpstreamKind returns the Kind, for packages that match upstream errors by
// behaviour because they may not import upstream (CODING_STANDARDS: Seam
// errors), as catalog does to tell an unknown_connector failure apart.
func (e *Error) UpstreamKind() string { return string(e.Kind) }

// AsError narrows err to the upstream [*Error] it carries, if any.
func AsError(err error) (*Error, bool) {
	var e *Error

	if errors.As(err, &e) {
		return e, true
	}

	return nil, false
}

// redactErr returns err with URL queries redacted from its text, for an
// error that is logged and may quote the resolved url: net/http puts it in
// a *url.Error, and an upstream may put one in its error message (design
// §9). errors.Is and errors.As still reach err.
func redactErr(err error) error {
	if err == nil {
		return nil
	}

	return redactedError{err}
}

// redactedError is an error whose text has URL queries redacted.
type redactedError struct{ err error }

func (e redactedError) Error() string { return redact.URLQueries(e.err.Error()) }

func (e redactedError) Unwrap() error { return e.err }
