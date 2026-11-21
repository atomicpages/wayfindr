package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
)

// This file tests judge directly: it is a pure decision table from the
// design (§7.2, rule order), the agreed exception in CODING_STANDARDS
// (Testing: "a pure decision table is its own seam"). The rules' effect on
// state and on what Call and ListTools return is still covered at the
// Manager's interface (callerr_test.go, call_test.go, connectfail_test.go).

// stubNeedsAuth stands in for auth's needs_auth errors, matched by
// behaviour: NeedsAuth() and AuthCase().
type stubNeedsAuth struct {
	authCase string
	link     string
}

func (e stubNeedsAuth) Error() string { return "auth needs_auth (" + e.authCase + ")" }
func (e stubNeedsAuth) NeedsAuth() (string, bool) {
	return e.link, e.authCase == "step_up"
}
func (e stubNeedsAuth) AuthCase() string { return e.authCase }

// stubAuth stands in for auth's other errors: AuthCase() and AuthConfig(),
// and Unwrap() to the cause auth wraps, if any.
type stubAuth struct {
	authCase string
	config   bool
	err      error
}

func (e stubAuth) Error() string    { return "auth error (" + e.authCase + ")" }
func (e stubAuth) AuthCase() string { return e.authCase }
func (e stubAuth) AuthConfig() bool { return e.config }
func (e stubAuth) Unwrap() error    { return e.err }

// viaHTTP wraps err the way net/http wraps a RoundTripper's error.
func viaHTTP(err error) error {
	return &url.Error{Op: "Post", URL: "https://mcp.example.com/mcp?key=hunter2", Err: err}
}

const testLink = "http://127.0.0.1:7777/auth/c"

func missing(kind config.RefKind, name string) error {
	return fmt.Errorf("connector c: headers X: %w",
		&config.ResolveError{Ref: config.Ref{Kind: kind, Name: name}, Err: config.ErrNotFound})
}

// unreadable is the error the resolver gives for a stored secret that
// exists but can't be opened.
func unreadable(name string) error {
	return fmt.Errorf("connector c: headers X: %w", &config.ResolveError{
		Ref: config.Ref{Kind: config.RefSecret, Name: name},
		Err: fmt.Errorf("secret %s: %w", name, config.ErrUnreadable),
	})
}

func protocolErr() error {
	return fmt.Errorf("calling tool: %w", &jsonrpc.Error{Code: -32602, Message: "unknown tool"})
}

type want struct {
	event Event
	close bool
	// kind is "" for success.
	kind Kind
	kase Case
	link string
	ref  string
}

func TestJudge(t *testing.T) {
	canceled := context.Canceled
	deadline := context.DeadlineExceeded
	tests := []struct {
		name string
		o    outcome
		want want
	}{
		// success
		{"use succeeds", outcome{phase: phaseUse}, want{event: EventSynced}},
		{"connect succeeds", outcome{phase: phaseConnect}, want{event: EventSynced}},

		// needs_auth
		{"needs auth at connect",
			outcome{phase: phaseConnect, err: viaHTTP(stubNeedsAuth{"no_credential", testLink})},
			want{EventAuthRequired, true, KindNeedsAuth, CaseNoCredential, testLink, ""}},
		{"needs auth mid-use",
			outcome{phase: phaseUse, err: viaHTTP(stubNeedsAuth{"token_revoked", testLink})},
			want{EventAuthRequired, true, KindNeedsAuth, CaseTokenRevoked, testLink, ""}},
		{"needs auth beats a cancelled use",
			outcome{phase: phaseUse, err: viaHTTP(stubNeedsAuth{"no_credential", testLink}), ctxErr: canceled},
			want{EventAuthRequired, true, KindNeedsAuth, CaseNoCredential, testLink, ""}},
		{"needs auth beats a timed-out use",
			outcome{phase: phaseUse, err: viaHTTP(stubNeedsAuth{"no_credential", testLink}), ctxErr: deadline, timedOut: true},
			want{EventAuthRequired, true, KindNeedsAuth, CaseNoCredential, testLink, ""}},
		{"needs auth beats a declined input",
			outcome{phase: phaseUse, err: viaHTTP(stubNeedsAuth{"no_credential", testLink}), declined: true},
			want{EventAuthRequired, true, KindNeedsAuth, CaseNoCredential, testLink, ""}},
		{"needs auth without AuthCase: step-up",
			outcome{phase: phaseUse, err: viaHTTP(legacyNeedsAuth{testLink, true})},
			want{EventAuthRequired, true, KindNeedsAuth, CaseStepUp, testLink, ""}},
		{"needs auth without AuthCase: no case",
			outcome{phase: phaseUse, err: viaHTTP(legacyNeedsAuth{testLink, false})},
			want{EventAuthRequired, true, KindNeedsAuth, "", testLink, ""}},

		// auth_config
		{"auth config",
			outcome{phase: phaseConnect, err: viaHTTP(stubAuth{"issuer_mismatch", true, nil})},
			want{EventAuthConfigError, true, KindAuthConfig, CaseIssuerMismatch, "", ""}},
		{"auth config beats a cancelled use",
			outcome{phase: phaseUse, err: viaHTTP(stubAuth{"client_rejected", true, nil}), ctxErr: canceled},
			want{EventAuthConfigError, true, KindAuthConfig, CaseClientRejected, "", ""}},
		{"malformed template",
			outcome{phase: phaseConnect, err: fmt.Errorf("url: %w", &config.TemplateError{Offset: 3, Reason: "unclosed"})},
			want{EventAuthConfigError, true, KindAuthConfig, CaseInvalidTemplate, "", ""}},
		{"unreadable secret",
			outcome{phase: phaseConnect, err: unreadable("linear")},
			want{EventAuthConfigError, true, KindAuthConfig, CaseSecretUnreadable, "", "secret:linear"}},
		{"unreadable secret beats a timed-out use",
			outcome{phase: phaseUse, err: unreadable("linear"), ctxErr: deadline, timedOut: true},
			want{EventAuthConfigError, true, KindAuthConfig, CaseSecretUnreadable, "", "secret:linear"}},

		// missing_value
		{"missing secret",
			outcome{phase: phaseConnect, err: missing(config.RefSecret, "linear")},
			want{EventAuthConfigError, true, KindMissingValue, CaseSecret, "", "secret:linear"}},
		{"missing env",
			outcome{phase: phaseConnect, err: missing(config.RefEnv, "GITHUB_TOKEN")},
			want{EventAuthConfigError, true, KindMissingValue, CaseEnv, "", "env:GITHUB_TOKEN"}},
		{"missing value beats a timed-out use",
			outcome{phase: phaseUse, err: missing(config.RefEnv, "GITHUB_TOKEN"), ctxErr: deadline, timedOut: true},
			want{EventAuthConfigError, true, KindMissingValue, CaseEnv, "", "env:GITHUB_TOKEN"}},
		{"auth config wrapping a missing secret is missing_value",
			outcome{phase: phaseConnect, err: viaHTTP(stubAuth{"unresolved_value", true, missing(config.RefSecret, "cs")})},
			want{EventAuthConfigError, true, KindMissingValue, CaseSecret, "", "secret:cs"}},
		{"auth config wrapping an unreadable secret is secret_unreadable",
			outcome{phase: phaseConnect, err: viaHTTP(stubAuth{"unresolved_value", true, unreadable("cs")})},
			want{EventAuthConfigError, true, KindAuthConfig, CaseSecretUnreadable, "", "secret:cs"}},
		{"auth config wrapping a malformed template is invalid_template",
			outcome{phase: phaseConnect, err: viaHTTP(stubAuth{"unresolved_value", true, &config.TemplateError{Offset: 3, Reason: "unclosed"}})},
			want{EventAuthConfigError, true, KindAuthConfig, CaseInvalidTemplate, "", ""}},
		{"a resolver failure other than not-found is unreachable",
			outcome{phase: phaseConnect, err: &config.ResolveError{Ref: config.Ref{Kind: config.RefSecret, Name: "x"}, Err: errors.New("store locked")}},
			want{EventTransportError, true, KindUnreachable, "", "", ""}},

		// interactive_input
		{"declined with a result",
			outcome{phase: phaseUse, declined: true},
			want{EventSynced, false, KindInteractiveInput, CaseSampling, "", ""}},
		{"declined with a protocol error",
			outcome{phase: phaseUse, err: protocolErr(), declined: true},
			want{EventSynced, false, KindInteractiveInput, CaseSampling, "", ""}},
		{"declined beats a timed-out use",
			outcome{phase: phaseUse, err: protocolErr(), declined: true, ctxErr: deadline, timedOut: true},
			want{EventSynced, false, KindInteractiveInput, CaseSampling, "", ""}},
		{"declined as ambiguous",
			outcome{phase: phaseUse, err: protocolErr(), declined: true, ambiguous: true},
			want{EventSynced, false, KindInteractiveInput, CaseConcurrentCalls, "", ""}},

		// timeout | canceled (use phase only)
		{"use timed out",
			outcome{phase: phaseUse, err: deadline, ctxErr: deadline, timedOut: true},
			want{0, false, KindTimeout, "", "", ""}},
		{"use cancelled by the caller",
			outcome{phase: phaseUse, err: canceled, ctxErr: canceled},
			want{0, false, KindCanceled, "", "", ""}},
		{"caller's own deadline is a cancel, not the connector timeout",
			outcome{phase: phaseUse, err: deadline, ctxErr: deadline},
			want{0, false, KindCanceled, "", "", ""}},
		{"timeout beats a protocol error",
			outcome{phase: phaseUse, err: protocolErr(), ctxErr: deadline, timedOut: true},
			want{0, false, KindTimeout, "", "", ""}},
		{"cancel beats a transport error",
			outcome{phase: phaseUse, err: viaHTTP(errConnRefused), ctxErr: canceled},
			want{0, false, KindCanceled, "", "", ""}},
		{"connector timeout during connect is unreachable",
			outcome{phase: phaseConnect, err: errors.New("connect given up"), ctxErr: deadline, timedOut: true},
			want{EventTransportError, true, KindUnreachable, CaseConnectTimeout, "", ""}},
		{"cancel during connect is unreachable",
			outcome{phase: phaseConnect, err: errors.New("connect given up"), ctxErr: canceled},
			want{EventTransportError, true, KindUnreachable, CaseConnectCanceled, "", ""}},

		// rejected
		{"protocol refusal",
			outcome{phase: phaseUse, err: protocolErr()},
			want{EventSynced, false, KindRejected, "", "", ""}},
		{"protocol error at connect is unreachable",
			outcome{phase: phaseConnect, err: protocolErr()},
			want{EventTransportError, true, KindUnreachable, "", "", ""}},
		{"SDK-local code is not a refusal",
			outcome{phase: phaseUse, err: &jsonrpc.Error{Code: -32005, Message: "rejected by transport"}},
			want{EventTransportError, true, KindUnreachable, "", "", ""}},
		{"closed connection is not a refusal",
			outcome{phase: phaseUse, err: fmt.Errorf("%w: %w", mcp.ErrConnectionClosed, &jsonrpc.Error{Code: -32602})},
			want{EventTransportError, true, KindUnreachable, "", "", ""}},

		// unreachable
		{"connection refused",
			outcome{phase: phaseUse, err: viaHTTP(errConnRefused)},
			want{EventTransportError, true, KindUnreachable, CaseConnectRefused, "", ""}},
		{"connect timeout",
			outcome{phase: phaseConnect, err: fmt.Errorf("%w after 60s", errConnectTimeout)},
			want{EventTransportError, true, KindUnreachable, CaseConnectTimeout, "", ""}},
		{"spawn failed",
			outcome{phase: phaseConnect, err: fmt.Errorf("%w: exec: not found", errSpawn)},
			want{EventTransportError, true, KindUnreachable, CaseSpawnFailed, "", ""}},
		{"anything else",
			outcome{phase: phaseUse, err: errors.New("EOF")},
			want{EventTransportError, true, KindUnreachable, "", "", ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.o.connector = "c"
			v := judge(tt.o)
			checkVerdict(t, v, tt.want)
		})
	}
}

func checkVerdict(t *testing.T, v verdict, w want) {
	t.Helper()

	if v.event != w.event || v.closeSession != w.close {
		t.Errorf("verdict = {event %v, close %v}, want {event %v, close %v}", v.event, v.closeSession, w.event, w.close)
	}

	if w.kind == "" {
		if v.err != nil || v.reason != "" {
			t.Errorf("verdict = {err %v, reason %q}, want no error", v.err, v.reason)
		}

		return
	}

	e, ok := AsError(v.err)

	if !ok {
		t.Fatalf("err = %v (%T), want an *Error", v.err, v.err)
	}

	got := want{event: w.event, close: w.close, kind: e.Kind, kase: e.Case, link: e.Link, ref: e.Ref}

	if got != w {
		t.Errorf("*Error = {Kind %q, Case %q, Link %q, Ref %q}, want {%q, %q, %q, %q}",
			e.Kind, e.Case, e.Link, e.Ref, w.kind, w.kase, w.link, w.ref)
	}

	if e.Connector != "c" {
		t.Errorf("Connector = %q, want c", e.Connector)
	}

	if v.reason == "" {
		t.Errorf("reason is empty, want why the use failed")
	}
}

// legacyNeedsAuth has NeedsAuth() but no AuthCase().
type legacyNeedsAuth struct {
	link   string
	stepUp bool
}

func (e legacyNeedsAuth) Error() string             { return "no token" }
func (e legacyNeedsAuth) NeedsAuth() (string, bool) { return e.link, e.stepUp }

func TestJudgeRedactsReasonAndError(t *testing.T) {
	v := judge(outcome{connector: "c", phase: phaseUse, err: viaHTTP(errors.New("EOF"))})

	for what, s := range map[string]string{"reason": v.reason, "Error()": v.err.Error()} {
		if strings.Contains(s, "hunter2") {
			t.Errorf("%s = %q, contains the url's query", what, s)
		}
	}

	if want := "connector c: unreachable: "; !strings.HasPrefix(v.err.Error(), want) {
		t.Errorf("Error() = %q, want it to start %q", v.err.Error(), want)
	}

	if want := "unreachable: "; !strings.HasPrefix(v.reason, want) {
		t.Errorf("reason = %q, want it to start %q", v.reason, want)
	}
}

// TestJudgeMapsEveryAuthCase sends each of auth's AuthCase() values through
// a *url.Error, as net/http delivers the auth RoundTripper's errors, and
// checks the upstream Kind and Case it becomes.
func TestJudgeMapsEveryAuthCase(t *testing.T) {
	tests := []struct {
		err      error
		wantKind Kind
		wantCase Case
	}{
		{stubNeedsAuth{"no_credential", testLink}, KindNeedsAuth, CaseNoCredential},
		{stubNeedsAuth{"url_changed", testLink}, KindNeedsAuth, CaseURLChanged},
		{stubNeedsAuth{"client_changed", testLink}, KindNeedsAuth, CaseClientChanged},
		{stubNeedsAuth{"credential_unreadable", testLink}, KindNeedsAuth, CaseCredentialUnreadable},
		{stubNeedsAuth{"refresh_failed", testLink}, KindNeedsAuth, CaseRefreshFailed},
		{stubNeedsAuth{"token_revoked", testLink}, KindNeedsAuth, CaseTokenRevoked},
		{stubNeedsAuth{"provider_error", testLink}, KindNeedsAuth, CaseProviderError},
		{stubNeedsAuth{"token_rejected", testLink}, KindNeedsAuth, CaseTokenRejected},
		{stubNeedsAuth{"step_up", testLink}, KindNeedsAuth, CaseStepUp},
		{stubNeedsAuth{"registration_expired", testLink}, KindNeedsAuth, CaseRegistrationExpired},
		{stubNeedsAuth{"something_new", testLink}, KindNeedsAuth, ""},
		{stubAuth{"client_rejected", true, nil}, KindAuthConfig, CaseClientRejected},
		{stubAuth{"issuer_mismatch", true, nil}, KindAuthConfig, CaseIssuerMismatch},
		{stubAuth{"no_registration", true, nil}, KindAuthConfig, CaseNoRegistration},
		{stubAuth{"registration_rejected", true, nil}, KindAuthConfig, CaseRegistrationRejected},
		{stubAuth{"unresolved_value", true, nil}, KindAuthConfig, CaseUnresolvedValue},
		{stubAuth{"invalid_url", true, nil}, KindAuthConfig, CaseInvalidURL},
		{stubAuth{"no_authorization_server", true, nil}, KindAuthConfig, CaseNoAuthorizationServer},
		{stubAuth{"no_metadata", true, nil}, KindAuthConfig, CaseNoMetadata},
		{stubAuth{"pkce_unsupported", true, nil}, KindAuthConfig, CasePKCEUnsupported},
		{stubAuth{"device_unsupported", true, nil}, KindAuthConfig, CaseDeviceUnsupported},
		{stubAuth{"auth_method_unsupported", true, nil}, KindAuthConfig, CaseAuthMethodUnsupported},
		{stubAuth{"signing_key_invalid", true, nil}, KindAuthConfig, CaseSigningKeyInvalid},
		{stubAuth{"grant_rejected", true, nil}, KindAuthConfig, CaseGrantRejected},
		{stubAuth{"scope_denied", true, nil}, KindAuthConfig, CaseScopeDenied},
		{stubAuth{"dpop_alg_unsupported", true, nil}, KindAuthConfig, CaseDPoPAlgUnsupported},
		{stubAuth{"dpop_rejected", true, nil}, KindAuthConfig, CaseDPoPRejected},
		{stubAuth{"something_new", true, nil}, KindAuthConfig, ""},
		{stubAuth{"transport", false, nil}, KindUnreachable, CaseAuthServer},
		{stubAuth{"malformed_response", false, nil}, KindUnreachable, CaseAuthServer},
		{stubAuth{"unexpected_status", false, nil}, KindUnreachable, CaseAuthServer},
		{stubAuth{"provider_unavailable", false, nil}, KindUnreachable, CaseProviderUnavailable},
		{stubAuth{"store", false, nil}, KindUnreachable, CaseAuthInternal},
		{stubAuth{"not_replayable", false, nil}, KindUnreachable, CaseAuthInternal},
		{stubAuth{"something_new", false, nil}, KindUnreachable, ""},
	}

	for _, tt := range tests {
		t.Run(tt.err.Error(), func(t *testing.T) {
			v := judge(outcome{connector: "c", phase: phaseUse, err: viaHTTP(tt.err)})
			e, ok := AsError(v.err)

			if !ok {
				t.Fatalf("err = %v, want an *Error", v.err)
			}

			if e.Kind != tt.wantKind || e.Case != tt.wantCase {
				t.Errorf("{Kind, Case} = {%q, %q}, want {%q, %q}", e.Kind, e.Case, tt.wantKind, tt.wantCase)
			}
		})
	}
}
