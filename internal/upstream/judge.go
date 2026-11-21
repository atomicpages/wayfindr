package upstream

import (
	"context"
	"errors"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/httpclient"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// phase is the part of a use an outcome comes from.
type phase int

const (
	// phaseConnect: opening the session (spawn or dial, and initialize).
	phaseConnect phase = iota + 1
	// phaseUse: a call or a sync over an open session.
	phaseUse
)

// outcome is everything judge needs to know about how a connect or a use
// ended.
type outcome struct {
	connector string
	phase     phase
	// err is the connect's or the use's error; nil if it succeeded.
	err error
	// declined says this call asked for input that Wayfindr declined (see
	// Call).
	declined bool
	// mask hides the connector's reference values in the verdict's error.
	mask redact.Values
	// ambiguous says an elicitation was declined because it couldn't be
	// tied to one call while this one was in flight.
	ambiguous bool
	// ctxErr is the use's context error when the outcome was taken.
	ctxErr error
	// timedOut says the context ended because the connector's timeout
	// passed (its cause is errUseTimeout), not because the caller gave up.
	timedOut bool
}

// verdict is what an outcome means (GLOSSARY: Verdict). The Manager
// applies it and decides nothing itself.
type verdict struct {
	// event is the connector state event; zero leaves the state alone.
	event Event
	// reason is the state reason, already redacted.
	reason string
	// closeSession says the session failed below the protocol and must be
	// closed, so the next use reconnects: the SDK doesn't close a session
	// whose request was refused.
	closeSession bool
	// err is what Call or ListTools returns: nil or an *Error.
	err error
}

// errUseTimeout is the cause a use's context carries when the connector's
// timeout ends it, which tells it apart from the caller's own deadline.
var errUseTimeout = errors.New("connector timeout")

// errConnectTimeout marks a connect that outlasted the connect timeout.
var errConnectTimeout = errors.New("connect timed out")

// errSpawn marks a stdio child that could not be started.
var errSpawn = errors.New("start stdio child")

// judge decides what an outcome means. The rules are tried in order, and
// the first that matches decides (design §7.2):
//
//  1. success: synced.
//  2. needs_auth: the auth RoundTripper wants the user to authorize.
//  3. a ${…} that won't resolve, whoever carries it (the connector's own
//     url, headers, or env, or auth's config such as a client_secret):
//     auth_config if it is malformed (invalid_template) or names a stored
//     secret that can't be decrypted (secret_unreadable); missing_value if
//     it names nothing. These come before auth's own auth_config so the
//     status names the reference, which is what lets setting the secret
//     reconnect the connector (Manager.SecretChanged).
//  4. auth_config: the connector's [tls] table can't be used (its ca_file
//     or client certificate), or auth can't work as configured.
//  5. interactive_input: this call asked for input; the upstream answered,
//     so synced.
//  6. timeout or canceled, in the use phase only: the state is untouched,
//     since a slow tool or a caller giving up says nothing about the
//     upstream.
//  7. rejected, in the use phase only: the upstream answered with a
//     JSON-RPC error, so synced.
//  8. unreachable: everything else, including a connect given up on,
//     which must not leave the connector in starting.
//
// Rules 2–4 and 8 close the session; 5–7 don't.
func judge(o outcome) verdict {
	if o.err == nil && !o.declined {
		return verdict{event: EventSynced}
	}

	e := &Error{Connector: o.connector, Err: o.err, mask: o.mask}
	var v verdict
	var na needsAuther
	var ac authConfiger
	var rerr *config.ResolveError
	var terr *config.TemplateError

	switch {
	case errors.As(o.err, &na):
		var stepUp bool
		e.Kind = KindNeedsAuth
		e.Link, stepUp = na.NeedsAuth()
		e.Case = authCase(o.err)

		if e.Case == "" && stepUp {
			e.Case = CaseStepUp
		}

		v = verdict{event: EventAuthRequired, closeSession: true}
	case errors.As(o.err, &terr):
		e.Kind, e.Case = KindAuthConfig, CaseInvalidTemplate
		v = verdict{event: EventAuthConfigError, closeSession: true}
	case errors.As(o.err, &rerr) && errors.Is(rerr, config.ErrUnreadable):
		e.Kind, e.Case, e.Ref = KindAuthConfig, CaseSecretUnreadable, refName(rerr.Ref)
		v = verdict{event: EventAuthConfigError, closeSession: true}
	case errors.As(o.err, &rerr) && errors.Is(rerr, config.ErrNotFound):
		e.Kind, e.Ref = KindMissingValue, refName(rerr.Ref)

		switch rerr.Ref.Kind {
		case config.RefSecret:
			e.Case = CaseSecret
		case config.RefEnv:
			e.Case = CaseEnv
		}

		v = verdict{event: EventAuthConfigError, closeSession: true}
	case errors.Is(o.err, errTLSSetup):
		e.Kind, e.Case = KindAuthConfig, CaseTLSHandshake

		if errors.Is(o.err, httpclient.ErrCAFile) {
			e.Case = CaseTLSUntrusted
		}

		v = verdict{event: EventAuthConfigError, closeSession: true}
	case errors.As(o.err, &ac) && ac.AuthConfig():
		e.Kind, e.Case = KindAuthConfig, authCase(o.err)
		v = verdict{event: EventAuthConfigError, closeSession: true}
	case o.declined:
		e.Kind, e.Case = KindInteractiveInput, CaseSampling

		if o.ambiguous {
			e.Case = CaseConcurrentCalls
		}

		v = verdict{event: EventSynced}
	case o.phase == phaseUse && o.ctxErr != nil && o.timedOut:
		e.Kind = KindTimeout
		v = verdict{}
	case o.phase == phaseUse && o.ctxErr != nil:
		// The caller's context error, so errors.Is(err, context.Canceled)
		// holds.
		e.Kind, e.Err = KindCanceled, o.ctxErr
		v = verdict{}
	case o.phase == phaseUse && isProtocolError(o.err):
		e.Kind = KindRejected
		v = verdict{event: EventSynced}
	default:
		e.Kind, e.Case = KindUnreachable, unreachableCase(o)
		v = verdict{event: EventTransportError, closeSession: true}
	}

	if p, ok := asProviderCoder(o.err); ok {
		e.ProviderCode = p.AuthProviderCode()
	}

	v.err, v.reason = e, e.detail()

	return v
}

// refName is a reference as Error.Ref holds it: "<kind>:<name>".
func refName(r config.Ref) string { return string(r.Kind) + ":" + r.Name }

// unreachableCase is the Case of an unreachable outcome.
func unreachableCase(o outcome) Case {
	if c := authCase(o.err); c != "" {
		return c
	}

	switch {
	case errors.Is(o.err, errConnectTimeout):
		return CaseConnectTimeout
	case errors.Is(o.err, errSpawn):
		return CaseSpawnFailed
	case errors.Is(o.err, errConnRefused):
		return CaseConnectRefused
	case o.phase == phaseConnect && o.ctxErr != nil && o.timedOut:
		return CaseConnectTimeout
	case o.phase == phaseConnect && o.ctxErr != nil:
		return CaseConnectCanceled
	}

	return ""
}

// authCaser is implemented by every error auth returns at its seam; the
// value is auth's Case. upstream can't import auth (design §2), so it
// matches the behaviour, which errors.As finds through net/http's
// *url.Error.
type authCaser interface {
	AuthCase() string
}

// needsAuther is implemented by auth's needs_auth errors only.
type needsAuther interface {
	NeedsAuth() (link string, stepUp bool)
}

// authConfiger is implemented by every auth error; it is true for auth's
// config errors.
type authConfiger interface {
	AuthConfig() bool
}

// providerCoder is implemented by auth errors that pass on the
// authorization server's raw error code.
type providerCoder interface {
	AuthProviderCode() string
}

func asProviderCoder(err error) (providerCoder, bool) {
	var p providerCoder
	ok := errors.As(err, &p)

	return p, ok
}

// authCase translates the AuthCase() of an auth error in err's chain into
// upstream's vocabulary. Every value auth's Transport can return is listed;
// its request cases (unknown_connector, not_oauth, not_interactive) come
// only from its auth flow entry, never from a connection. Auth's
// transport and malformed cases become CaseAuthServer, and its internal
// cases CaseAuthInternal. A value this table doesn't know, or an error
// without AuthCase(), gives the empty Case.
func authCase(err error) Case {
	var ac authCaser

	if !errors.As(err, &ac) {
		return ""
	}

	switch ac.AuthCase() {
	// needs_auth
	case "no_credential":
		return CaseNoCredential
	case "url_changed":
		return CaseURLChanged
	case "client_changed":
		return CaseClientChanged
	case "authorization_server_changed":
		return CaseAuthorizationServerChanged
	case "credential_unreadable":
		return CaseCredentialUnreadable
	case "refresh_failed":
		return CaseRefreshFailed
	case "token_revoked":
		return CaseTokenRevoked
	case "provider_error":
		return CaseProviderError
	case "token_rejected":
		return CaseTokenRejected
	case "step_up":
		return CaseStepUp
	case "registration_expired":
		return CaseRegistrationExpired
	// config
	case "client_rejected":
		return CaseClientRejected
	case "issuer_mismatch":
		return CaseIssuerMismatch
	case "no_registration":
		return CaseNoRegistration
	case "registration_rejected":
		return CaseRegistrationRejected
	case "unresolved_value":
		return CaseUnresolvedValue
	case "invalid_url":
		return CaseInvalidURL
	case "no_authorization_server":
		return CaseNoAuthorizationServer
	case "no_metadata":
		return CaseNoMetadata
	case "pkce_unsupported":
		return CasePKCEUnsupported
	case "device_unsupported":
		return CaseDeviceUnsupported
	case "auth_method_unsupported":
		return CaseAuthMethodUnsupported
	case "signing_key_invalid":
		return CaseSigningKeyInvalid
	case "tls_untrusted":
		return CaseTLSUntrusted
	case "tls_handshake":
		return CaseTLSHandshake
	case "grant_rejected":
		return CaseGrantRejected
	case "scope_denied":
		return CaseScopeDenied
	case "dpop_alg_unsupported":
		return CaseDPoPAlgUnsupported
	case "dpop_rejected":
		return CaseDPoPRejected
	// unreachable, malformed
	case "transport", "malformed_response", "unexpected_status":
		return CaseAuthServer
	case "provider_unavailable":
		return CaseProviderUnavailable
	// internal
	case "store", "not_replayable":
		return CaseAuthInternal
	}

	return ""
}

// sdkLocalCodes are the JSON-RPC codes the SDK puts on errors it makes up
// itself for a request that never got an answer: the transport rejected
// it, or the connection is closing.
func sdkLocalCodes() []int64 { return []int64{-32005, -32004, -32003} }

// isProtocolError reports whether err is a JSON-RPC error the upstream
// sent, as opposed to a failure to reach it.
func isProtocolError(err error) bool {
	var werr *jsonrpc.Error

	if !errors.As(err, &werr) || errors.Is(err, mcp.ErrConnectionClosed) {
		return false
	}

	return !slices.Contains(sdkLocalCodes(), werr.Code)
}

// contextOutcome fills in o's context fields from ctx, the context the
// connect or use ran under.
func contextOutcome(ctx context.Context, o outcome) outcome {
	// The cause, not Err: a call's context is cancelled on its caller's
	// behalf with the caller's own error as cause (see Manager.call), which
	// keeps errors.Is(err, context.DeadlineExceeded) true for the caller.
	if ctx.Err() != nil {
		o.ctxErr = context.Cause(ctx)
	}

	o.timedOut = errors.Is(o.ctxErr, errUseTimeout)

	return o
}
