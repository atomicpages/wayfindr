package auth

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/httpclient"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// Kind is the coarse class of an auth failure (GLOSSARY: Kind). It is a
// closed set: callers branch on these constants, never on error text.
type Kind string

// Kinds.
const (
	// KindNeedsAuth: the connector has no usable credential and belongs in
	// needs_auth until the user runs the auth flow at the error's Link.
	KindNeedsAuth Kind = "needs_auth"
	// KindConfig: auth cannot proceed as configured; the connector belongs
	// in auth_error until the user changes its config.
	KindConfig Kind = "config"
	// KindUnreachable: the authorization server could not be reached. It
	// reads as unreachable and is retried.
	KindUnreachable Kind = "unreachable"
	// KindMalformed: the authorization server answered, but with nothing
	// usable: a malformed response, or an unexpected status.
	KindMalformed Kind = "malformed"
	// KindInternal: Wayfindr's own side failed (the store, or a request
	// that cannot be replayed).
	KindInternal Kind = "internal"
	// KindRequest: the auth flow was asked for a connector it cannot
	// authorize: one the running config does not name, one that does not
	// use OAuth, or one whose unattended grant has no auth flow. Only
	// [Flows] returns it; nothing about the connector's state follows from
	// it.
	KindRequest Kind = "request"
)

// Case is the finer reason for an auth failure (GLOSSARY: Case). It is a
// closed set, and each Case belongs to exactly one [Kind].
type Case string

// Cases of [KindNeedsAuth].
const (
	// CaseNoCredential: the connector was never authorized.
	CaseNoCredential Case = "no_credential" //nolint:gosec // G101: a Case name, not a credential
	// CaseURLChanged: the token is bound to another connector URL; it is
	// never sent to the new one.
	CaseURLChanged Case = "url_changed"
	// CaseClientChanged: the token was granted to another OAuth client than
	// the one the connector will use now (its client_id changed, or it
	// switched between a configured client and a discovered one); it is
	// never sent. A changed client_secret alone is not this.
	CaseClientChanged Case = "client_changed"
	// CaseAuthorizationServerChanged: discovery now names another token
	// endpoint than the one the grant was obtained at, so the refresh token
	// is not spent there (wayfindr-afb1).
	CaseAuthorizationServerChanged Case = "authorization_server_changed"
	// CaseCredentialUnreadable: the stored token or OAuth client cannot be
	// opened or decoded (for example after a key change).
	CaseCredentialUnreadable Case = "credential_unreadable" //nolint:gosec // G101: a Case name, not a credential
	// CaseRefreshFailed: the token expired and cannot be refreshed: there
	// is no refresh token or no client to spend it with.
	CaseRefreshFailed Case = "refresh_failed"
	// CaseTokenRevoked: the authorization server rejected the grant
	// (invalid_grant and its provider aliases).
	CaseTokenRevoked Case = "token_revoked"
	// CaseProviderError: the authorization server returned an error code
	// Wayfindr does not know. The raw code is kept as the provider code.
	CaseProviderError Case = "provider_error"
	// CaseTokenRejected: the upstream answered 401 to a freshly refreshed
	// token.
	CaseTokenRejected Case = "token_rejected"
	// CaseStepUp: the upstream answered 403 insufficient_scope; the larger
	// scope set is already merged into requested_scopes.
	CaseStepUp Case = "step_up"
	// CaseRegistrationExpired: the authorization server rejected a client
	// Wayfindr registered through DCR (invalid_client and its aliases). The
	// stored client has been cleared, so the auth flow registers a new one.
	// A rejected config client_id is [CaseClientRejected] instead.
	CaseRegistrationExpired Case = "registration_expired"
	// CaseAuthorizationDenied: the authorization server answered the auth
	// flow's authorization request with an error, such as access_denied
	// when the user declined, or a device code flow's token request with
	// access_denied. The code is kept as the provider code.
	CaseAuthorizationDenied Case = "authorization_denied"
	// CaseFlowExpired: the auth flow was not completed within its
	// 10-minute lifetime, or its device code expired first (RFC 8628
	// expired_token).
	CaseFlowExpired Case = "flow_expired"
	// CaseFlowCancelled: the auth flow was cancelled before its callback
	// stored a token, because the connector's credential was revoked, its
	// url or [auth] table changed, or it was removed from the config.
	CaseFlowCancelled Case = "flow_cancelled"
	// CaseCallbackIssuerMismatch: the auth flow's callback failed the RFC
	// 9207 check: its iss names another issuer than the flow's, or is
	// missing although the authorization server advertises it. It may be a
	// mix-up attack, so the code is not exchanged. Unlike
	// [CaseIssuerMismatch], no config change is the fix.
	CaseCallbackIssuerMismatch Case = "callback_issuer_mismatch"
	// CaseAuthorizationPending: a device code flow's user has not entered
	// the user code yet (RFC 8628 §3.5 authorization_pending). The flow's
	// poller asks again; it never ends a flow.
	CaseAuthorizationPending Case = "authorization_pending"
)

// Cases of [KindConfig].
const (
	// CaseClientRejected: the authorization server rejected the OAuth
	// client (invalid_client and its aliases). A rejected DCR client is
	// [CaseRegistrationExpired] instead: no config change can fix it.
	CaseClientRejected Case = "client_rejected"
	// CaseIssuerMismatch: authorization server metadata names another
	// issuer than the one it was discovered for (RFC 8414 §3.3).
	CaseIssuerMismatch Case = "issuer_mismatch"
	// CaseNoRegistration: no client_id, and the server supports neither
	// CIMD nor dynamic client registration.
	CaseNoRegistration Case = "no_registration"
	// CaseRegistrationRejected: dynamic client registration was rejected
	// (RFC 7591 §3.2.2).
	CaseRegistrationRejected Case = "registration_rejected"
	// CaseUnresolvedValue: a ${…} reference in the connector's config (url,
	// client_secret, signing_key.key, assertion.value, tls.key) or the
	// assertion file cannot be resolved, or the jwt_bearer grant has no
	// assertion.
	CaseUnresolvedValue Case = "unresolved_value"
	// CaseInvalidURL: a configured or discovered URL is unusable (not
	// absolute).
	CaseInvalidURL Case = "invalid_url"
	// CaseNoAuthorizationServer: the protected resource metadata lists no
	// authorization server.
	CaseNoAuthorizationServer Case = "no_authorization_server"
	// CaseNoMetadata: the authorization server publishes no metadata and
	// config names no endpoints.
	CaseNoMetadata Case = "no_metadata"
	// CasePKCEUnsupported: a connector with no client_id whose
	// authorization server does not advertise PKCE S256 (ADR 0007).
	CasePKCEUnsupported Case = "pkce_unsupported"
	// CaseDeviceUnsupported: a device code flow for a connector whose
	// authorization server has no device authorization endpoint (RFC 8628),
	// in its metadata or as device_authorization_endpoint in config.
	CaseDeviceUnsupported Case = "device_unsupported"
	// CaseAuthMethodUnsupported: the OAuth client cannot authenticate at
	// the token endpoint: the authorization server lists no client
	// authentication method its credential can do, or the method
	// configured (or registered) needs a credential it doesn't hold. It is
	// found before any request is sent.
	CaseAuthMethodUnsupported Case = "auth_method_unsupported"
	// CaseSigningKeyInvalid: the connector's signing_key is not a PEM
	// private key Wayfindr can sign with (PKCS#8 EC P-256/P-384, RSA of
	// 2048 bits or more, or Ed25519), or cannot sign its configured alg.
	CaseSigningKeyInvalid Case = "signing_key_invalid"
	// CaseTLSUntrusted: Wayfindr does not trust the certificate of the
	// authorization server (or of the resource, while discovering): it is
	// issued by an unknown authority, or fails verification, such as for
	// another host name; or the connector's ca_file cannot be read.
	CaseTLSUntrusted Case = "tls_untrusted"
	// CaseTLSHandshake: the TLS handshake with the authorization server
	// failed for another reason, such as the server refusing the client
	// certificate (or the lack of one) with an alert; or the connector's
	// client certificate cannot be loaded.
	CaseTLSHandshake Case = "tls_handshake"
	// CaseGrantRejected: the authorization server refused an unattended
	// grant (invalid_grant, invalid_scope, unauthorized_client,
	// unsupported_grant_type, or a code Wayfindr does not know), or the
	// upstream answered 401 to a token fresh from one. No auth flow can fix
	// it: the client, its access, or the assertion is fixed at the provider
	// or in config. The code is kept as the provider code.
	CaseGrantRejected Case = "grant_rejected"
	// CaseScopeDenied: the upstream answered 403 insufficient_scope to a
	// token from an unattended grant that already asked for every scope the
	// challenge names, so another grant cannot help.
	CaseScopeDenied Case = "scope_denied"
	// CaseDPoPAlgUnsupported: the connector's dpop is "always", and its
	// authorization server lists dpop_signing_alg_values_supported without
	// ES256, the one algorithm Wayfindr signs DPoP proofs with. It is found
	// before any token request is sent.
	CaseDPoPAlgUnsupported Case = "dpop_alg_unsupported"
	// CaseDPoPRejected: the authorization server rejected a token request's
	// DPoP proof (invalid_dpop_proof, RFC 9449 §5): most often the clock of
	// the daemon's host is off, so the proof's iat is outside the server's
	// window, or the server does not accept its key. Neither a new auth
	// flow, whose code exchange carries the same kind of proof, nor a retry
	// fixes it: the host's clock, or dpop = "never", does.
	CaseDPoPRejected Case = "dpop_rejected"
)

// Cases of [KindUnreachable].
const (
	// CaseTransport: a request to the authorization server failed in
	// transport (connection refused, DNS, TLS, timeout).
	CaseTransport Case = "transport"
	// CaseProviderUnavailable: the authorization server answered with a
	// known transient code (rate limited, temporarily_unavailable,
	// server_error). The grant may still be good, so this is retried, not
	// sent back through the auth flow. The code is kept as the provider
	// code.
	CaseProviderUnavailable Case = "provider_unavailable"
	// CaseSlowDown: a device code flow polls too often (RFC 8628 §3.5
	// slow_down). The flow's poller waits 5 seconds longer from then on; it
	// never ends a flow.
	CaseSlowDown Case = "slow_down"
)

// Cases of [KindMalformed].
const (
	// CaseMalformedResponse: a response that is neither a result nor an
	// error the protocol defines.
	CaseMalformedResponse Case = "malformed_response"
	// CaseUnexpectedStatus: a non-2xx response with no error code, such as
	// a proxy's 502 page.
	CaseUnexpectedStatus Case = "unexpected_status"
)

// Cases of [KindInternal].
const (
	// CaseStore: reading or writing the store failed.
	CaseStore Case = "store"
	// CaseNotReplayable: the upstream answered 401 and the request body
	// cannot be sent again; the token was refreshed for the next call.
	CaseNotReplayable Case = "not_replayable"
)

// Cases of [KindRequest].
const (
	// CaseUnknownConnector: no connector of that name is configured.
	CaseUnknownConnector Case = "unknown_connector"
	// CaseNotOAuth: the connector has no [auth] table of type "oauth".
	CaseNotOAuth Case = "not_oauth"
	// CaseNotInteractive: the connector uses an unattended grant, which has
	// no auth flow: Wayfindr gets its token itself.
	CaseNotInteractive Case = "not_interactive"
)

// kindOf is the one table from a [Case] to its [Kind].
func kindOf(c Case) Kind {
	switch c {
	case CaseNoCredential, CaseURLChanged, CaseClientChanged, CaseAuthorizationServerChanged,
		CaseCredentialUnreadable, CaseRefreshFailed,
		CaseTokenRevoked, CaseProviderError, CaseTokenRejected, CaseStepUp, CaseRegistrationExpired,
		CaseAuthorizationDenied, CaseFlowExpired, CaseFlowCancelled, CaseCallbackIssuerMismatch,
		CaseAuthorizationPending:
		return KindNeedsAuth
	case CaseClientRejected, CaseIssuerMismatch, CaseNoRegistration, CaseRegistrationRejected,
		CaseUnresolvedValue, CaseInvalidURL, CaseNoAuthorizationServer, CaseNoMetadata,
		CasePKCEUnsupported, CaseDeviceUnsupported, CaseAuthMethodUnsupported, CaseSigningKeyInvalid,
		CaseTLSUntrusted, CaseTLSHandshake, CaseGrantRejected, CaseScopeDenied, CaseDPoPAlgUnsupported,
		CaseDPoPRejected:
		return KindConfig
	case CaseTransport, CaseProviderUnavailable, CaseSlowDown:
		return KindUnreachable
	case CaseMalformedResponse, CaseUnexpectedStatus:
		return KindMalformed
	case CaseStore, CaseNotReplayable:
		return KindInternal
	case CaseUnknownConnector, CaseNotOAuth, CaseNotInteractive:
		return KindRequest
	}

	return KindInternal // not a Case constant
}

// transportCase is the Case of err, a failure to reach the authorization
// server: [CaseTLSUntrusted] when its certificate failed verification,
// [CaseTLSHandshake] when the handshake failed otherwise (a TLS alert, such
// as one refusing the client certificate, or a peer that does not speak
// TLS), and [CaseTransport] for the rest. Every request auth sends to the
// authorization server, discovery's included, classifies its failure here.
func transportCase(err error) Case {
	var (
		verify   *tls.CertificateVerificationError
		unknown  x509.UnknownAuthorityError
		invalid  x509.CertificateInvalidError
		hostname x509.HostnameError
		alert    tls.AlertError
		record   tls.RecordHeaderError
		op       *net.OpError
	)

	switch {
	case errors.As(err, &verify), errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &hostname):
		return CaseTLSUntrusted
	case errors.As(err, &alert), errors.As(err, &record):
		return CaseTLSHandshake
	case errors.As(err, &op) && op.Op == tlsRemoteAlert:
		// crypto/tls reports an alert the peer sent, such as
		// certificate_required, as a *net.OpError with this Op around its
		// unexported alert type.
		return CaseTLSHandshake
	}

	return CaseTransport
}

// tlsRemoteAlert is the Op of the *net.OpError crypto/tls returns for an
// alert the peer sent.
const tlsRemoteAlert = "remote error"

// clientSetupCase is the Case of err, the error of building a connector's
// HTTP client from its tls table ([httpclient.Clients.For]): a key that
// cannot be resolved is [CaseUnresolvedValue] (the error keeps the
// *config.ResolveError, so upstream names the reference); a ca_file that
// cannot be used is [CaseTLSUntrusted]; a client certificate that cannot be
// loaded, [CaseTLSHandshake].
func clientSetupCase(err error) Case {
	var rerr *config.ResolveError

	switch {
	case errors.As(err, &rerr):
		return CaseUnresolvedValue
	case errors.Is(err, httpclient.ErrCAFile):
		return CaseTLSUntrusted
	}

	return CaseTLSHandshake
}

// Error is every error auth returns at its seam (CODING_STANDARDS: Errors).
// It is always returned as error; narrow it with [AsError].
//
// upstream cannot import auth (design §2), so it matches by behaviour
// instead, through errors.As, which also sees through the *url.Error that
// net/http wraps around a RoundTripper's error:
//
//   - every Error has AuthCase() string, the Case;
//   - a [KindNeedsAuth] error has NeedsAuth() (link string, stepUp bool);
//   - every Error has AuthConfig() bool, true for [KindConfig].
//   - every Error has AuthProviderCode() string, the sanitised provider
//     code, empty if the authorization server returned none.
//
// A transport failure of the upstream itself, and the caller's own
// cancellation, are not auth errors: the Transport returns them unchanged.
type Error struct {
	Kind Kind
	Case Case
	// Connector is the connector the error is about, if known.
	Connector string
	// Link is http://<listen>/auth/<connector>, the daemon page that starts
	// the auth flow. Set on [KindNeedsAuth] errors only.
	Link string
	// ProviderCode is the sanitised error code the authorization server
	// returned, if any (GLOSSARY: Provider code).
	ProviderCode string
	// Detail is technical context for logs and status. It never holds a
	// token; URLs in it are redacted by Error.
	Detail string
	// Err is the underlying cause, if any.
	Err error

	// status is the HTTP status of the token endpoint's answer the error
	// reads, or 0: a device code poll tells a server failure from an
	// answer about the grant by it.
	status int
	// mask hides the values of the references in the connector's url,
	// which Detail and Err may quote, in Error (see [withMask]).
	mask redact.Values
}

// withMask returns err with its text masked by m: an [*Error] in it gets
// m, so its Error masks the values; any other error is wrapped. Errors that
// quote the connector's resolved url (discovery's) leave auth through it
// (wayfindr-wt2v).
func withMask(err error, m redact.Values) error {
	if e, ok := AsError(err); ok {
		e.mask = m

		return err
	}

	return m.Err(err)
}

// newError returns an Error of c's Kind.
func newError(c Case, connector, detail string, cause error) *Error {
	return &Error{Kind: kindOf(c), Case: c, Connector: connector, Detail: detail, Err: cause}
}

// fail returns a new Error as the error a seam returns.
func fail(c Case, connector, detail string, cause error) error {
	return newError(c, connector, detail, cause).seam()
}

// seam returns e as the error auth's seam returns: a needs_auth error
// inside [needsAuthError], so that it, and only it, has NeedsAuth().
func (e *Error) seam() error {
	if e.Kind == KindNeedsAuth {
		return needsAuthError{e}
	}

	return e
}

// Error is technical text for logs and status, with URLs redacted. What it
// means to the user comes from [Advise].
func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "auth %s (%s)", e.Kind, e.Case)

	if e.Connector != "" {
		fmt.Fprintf(&b, " for connector %q", e.Connector)
	}

	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}

	if e.ProviderCode != "" {
		b.WriteString(" [provider code " + e.ProviderCode + "]")
	}

	if e.Err != nil {
		b.WriteString(": " + e.Err.Error())
	}

	if e.Link != "" {
		b.WriteString("; authorize at " + e.Link)
	}

	return e.mask.Apply(redact.URLQueries(b.String()))
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// AuthCase returns the Case, for packages that match auth errors by
// behaviour.
func (e *Error) AuthCase() string { return string(e.Case) }

// AuthConfig reports whether the error is a [KindConfig] (auth_error)
// condition.
func (e *Error) AuthConfig() bool { return e.Kind == KindConfig }

// AuthProviderCode returns the sanitised provider code, if any, for
// packages that match auth errors by behaviour.
func (e *Error) AuthProviderCode() string { return e.ProviderCode }

// needsAuthError is how a [KindNeedsAuth] [*Error] leaves the seam. It adds
// NeedsAuth(), which upstream matches by method set: on *Error itself it
// would make every auth error read as needs_auth.
type needsAuthError struct{ e *Error }

func (n needsAuthError) Error() string { return n.e.Error() }

// Unwrap returns the *Error, so [AsError] and AuthCase() reach it.
func (n needsAuthError) Unwrap() error { return n.e }

// AuthProviderCode returns the wrapped *Error's provider code.
func (n needsAuthError) AuthProviderCode() string { return n.e.AuthProviderCode() }

// NeedsAuth returns the link that starts the auth flow and whether this is
// a step-up.
func (n needsAuthError) NeedsAuth() (link string, stepUp bool) {
	return n.e.Link, n.e.Case == CaseStepUp
}

// AsError narrows err to the auth [*Error] it carries, if any.
func AsError(err error) (*Error, bool) {
	var e *Error

	if errors.As(err, &e) {
		return e, true
	}

	return nil, false
}

// HTTPStatus is the HTTP status of an auth flow that cannot start, the
// same for the browser's pages and the admin API: whose problem it is. A
// request for a connector the flow cannot authorize is 404; one the user
// must fix (the connector's config, or its credential) is 409; the
// authorization server's fault is 502; Wayfindr's own, and an error that
// is not an auth [*Error], is 500.
func HTTPStatus(err error) int {
	e, ok := AsError(err)

	if !ok {
		return http.StatusInternalServerError
	}

	switch e.Kind {
	case KindRequest:
		return http.StatusNotFound
	case KindNeedsAuth, KindConfig:
		return http.StatusConflict
	case KindUnreachable, KindMalformed:
		return http.StatusBadGateway
	case KindInternal:
		return http.StatusInternalServerError
	}

	return http.StatusInternalServerError
}
