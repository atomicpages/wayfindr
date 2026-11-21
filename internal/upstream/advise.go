package upstream

import (
	"cmp"
	"strings"
	"time"

	"github.com/atomicpages/wayfindr/internal/config"
)

// Advice is upstream's user-facing reading of one of its errors (GLOSSARY:
// Advice): what went wrong, the action that fixes it, and the link that
// starts the auth flow when there is one. Presenters render it as is.
type Advice struct {
	Summary string
	Action  string
	Link    string
}

// Advise says what e means to the user and how to fix it. The advice for
// e's Kind applies unless e's Case has its own. Both switches are
// exhaustive (the exhaustive linter enforces it), so a new Kind or Case
// cannot ship without advice.
func Advise(e *Error) Advice {
	if e == nil {
		return Advice{}
	}

	a := Advice{Link: e.Link}
	a.Summary, a.Action = kindAdvice(e)

	if s, act := caseAdvice(e); s != "" {
		a.Summary, a.Action = s, act
	}

	return a
}

// Advice says what s's state means to the user and how to fix it. It is
// the only reading of a connector's state: the status a user reads and the
// search results an agent reads both render it as is.
//
// A ready connector has none. A connector put in its state by an error
// gets that error's advice, Link included. A state entered without one
// reads as the Kind that state stands for (needs_auth, auth_config,
// unreachable), and starting has its own wording. It never shows Reason,
// which is for logs.
func (s ConnectorStatus) Advice() Advice {
	kind := s.Kind

	switch s.State {
	case StateReady:
		return Advice{}
	case StateStarting:
		return Advice{Summary: "The connector is still connecting.", Action: "Try again in a moment."}
	case StateNeedsAuth:
		kind = cmp.Or(kind, KindNeedsAuth)
	case StateAuthError:
		kind = cmp.Or(kind, KindAuthConfig)
	case StateUnreachable:
		kind = cmp.Or(kind, KindUnreachable)
	}

	return Advise(&Error{
		Connector: s.Connector, Kind: kind, Case: s.Case, Ref: s.Ref,
		ProviderCode: s.ProviderCode, Link: s.Link,
	})
}

// stuckAfter is how long a connector may stay out of ready before it is
// warned of as stuck.
const stuckAfter = 24 * time.Hour

// WarningStuck names the warning [ConnectorStatus.Stuck] gives (GLOSSARY:
// Warning).
const WarningStuck = "stuck"

// Stuck reports whether s has been out of ready for more than a day as of
// now (NotReadySince, whatever states it went through), with advice that
// says since when: s's own advice, with that in the summary. It adds
// nothing else to Advice; it puts the connector among the warnings, beside
// the credentials about to fail.
func (s ConnectorStatus) Stuck(now time.Time) (Advice, bool) {
	t := s.NotReadySince

	if s.State == StateReady || t.IsZero() || now.Sub(t) <= stuckAfter {
		return Advice{}, false
	}

	a := s.Advice()
	a.Summary = "The connector has not been ready since " + t.UTC().Format(time.RFC3339) + "."

	return a, true
}

const (
	adviseReauth = "Open the link to authorize the connector again."
	adviseRetry  = "Wayfindr reconnects on the next call."
)

// kindAdvice is the advice every error of e's Kind gets.
func kindAdvice(e *Error) (summary, action string) {
	switch e.Kind {
	case KindUnknownConnector:
		return "No connector is called " + e.Connector + ", or it was just removed.",
			"Check the connector name with `wayfindr status`."
	case KindNeedsAuth:
		return "The connector needs to be authorized.", "Open the link to authorize it."
	case KindAuthConfig:
		return "The connector's auth cannot work as configured.", "Fix the connector's config; the daemon log has the details."
	case KindMissingValue:
		return "The connector's config refers to " + e.Ref + ", which is not set.",
			"Set it, or fix the reference in the connector's config."
	case KindInteractiveInput:
		return "This tool asked for input that Wayfindr cannot pass on to you.",
			"Use another tool, or run this one in a client connected to the upstream directly."
	case KindInputExpired:
		return "The answer arrived too late or does not belong to this call.", "Call the tool again."
	case KindTimeout:
		return "The call outlasted the connector's timeout.",
			"Try again, or raise timeout in the connector's config if the tool is slow."
	case KindCanceled:
		return "The call was cancelled before the upstream answered.", "Try again."
	case KindRejected:
		return "The upstream refused the request.", "Check the tool name and its arguments against the tool's schema."
	case KindUnreachable:
		return "The upstream cannot be reached.", "Check that it is running and the network. " + adviseRetry
	}

	return "", "" // not a Kind constant
}

// caseAdvice is the advice for e's Case where it says more than its
// Kind's; empty when it doesn't.
func caseAdvice(e *Error) (summary, action string) {
	switch e.Case {
	case CaseNoCredential:
		return "The connector has not been authorized yet.", "Open the link to authorize it."
	case CaseURLChanged:
		return "The connector's URL changed since it was authorized, so its token is not sent to the new URL.",
			"Open the link to authorize it for the new URL."
	case CaseClientChanged:
		return "The connector's client_id changed since it was authorized. Authorize it again.", adviseReauth
	case CaseAuthorizationServerChanged:
		return "The server now sends Wayfindr to a different sign-in server than the one that issued the connector's token, " +
				"so the token is not refreshed there.",
			"If you trust the new sign-in server, open the link to authorize the connector again. " +
				"If not, set authorization_server in the connector's [auth] table to the one you trust."
	case CaseCredentialUnreadable:
		return "The stored credential cannot be read, for example because the master key changed.", adviseReauth
	case CaseRefreshFailed:
		return "The connector's token expired and cannot be refreshed.", adviseReauth
	case CaseTokenRevoked:
		return "The authorization server revoked or expired the connector's grant" + e.providerCode() + ".", adviseReauth
	case CaseProviderError:
		return "The authorization server refused the token with an error Wayfindr does not recognise" + e.providerCode() + ".",
			adviseReauth + " If that fails too, look the provider code up in the provider's documentation."
	case CaseTokenRejected:
		return "The upstream rejected a freshly refreshed token.", adviseReauth
	case CaseStepUp:
		return "The upstream needs scopes the connector was not granted.",
			"Open the link to authorize the connector with the larger set of scopes."
	case CaseRegistrationExpired:
		return "The authorization server no longer accepts Wayfindr's registered OAuth client.",
			"Open the link to authorize the connector again; Wayfindr registers a new client."
	case CaseClientRejected:
		return "The authorization server rejected the configured OAuth client" + e.providerCode() + ".",
			"Check client_id and client_secret in the connector's [auth] table."
	case CaseIssuerMismatch:
		return "The authorization server's metadata names a different issuer than the one it was found for.",
			"Check authorization_server in the connector's [auth] table."
	case CaseNoRegistration, CaseRegistrationRejected:
		return "Wayfindr has no OAuth client to authorize with" + e.providerCode() + ".",
			"Register an app with the provider and set client_id (and client_secret) in the connector's [auth] table."
	case CaseUnresolvedValue:
		return "A ${…} reference or file in the connector's auth setup cannot be resolved.",
			"Set the missing secret or environment variable, create the file, or fix the reference."
	case CaseInvalidURL:
		return "A URL in the connector's auth setup is not an absolute URL.",
			"Fix url, authorization_server, or token_endpoint in the connector's config."
	case CaseNoAuthorizationServer, CaseNoMetadata:
		return "Wayfindr cannot find the connector's authorization server endpoints.",
			"Set authorization_endpoint and token_endpoint in the connector's [auth] table."
	case CaseDeviceUnsupported:
		return "The authorization server does not support signing in with a code on another device (device code).",
			"Set device_authorization_endpoint in the connector's [auth] table if the provider has one, " +
				"or set grant = \"authorization_code\" to sign in in a browser."
	case CasePKCEUnsupported:
		return "The authorization server does not advertise PKCE (S256), which Wayfindr requires " +
				"for a connector without a client_id.",
			"Register an app with the provider and set client_id (and client_secret) in the connector's [auth] table."
	case CaseAuthMethodUnsupported:
		return "Wayfindr cannot authenticate as the connector's OAuth client: the authorization server accepts " +
				"no client authentication method that fits the client's credential.",
			"Set token_endpoint_auth_method in the connector's [auth] table to a method the provider accepts, " +
				"and configure its credential: client_secret, or signing_key for private_key_jwt."
	case CaseSigningKeyInvalid:
		return "The connector's signing_key is not a private key Wayfindr can sign with, or cannot sign the configured alg.",
			"Store a PEM private key (EC P-256 or P-384, RSA of 2048 bits or more, or Ed25519) with " +
				"`wayfindr connector edit " + e.Connector + " --signing-key < key.pem`, or fix signing_key.alg."
	case CaseTLSUntrusted:
		return "Wayfindr does not trust the TLS certificate of the connector's authorization server, " +
				"or the connector's ca_file cannot be read.",
			"If the server uses a private CA, set ca_file in the connector's [tls] table to that CA's PEM certificate. " +
				"Wayfindr reconnects the connector when the config file changes."
	case CaseTLSHandshake:
		return "The TLS handshake with the connector's authorization server failed: it refused the client " +
				"certificate, or the lack of one, or the certificate cannot be loaded.",
			"Check cert_file and key_file (or key) in the connector's [tls] table: the certificate must be one the " +
				"server accepts, and match its key. Wayfindr reconnects the connector when the config file changes."
	case CaseGrantRejected:
		return "The authorization server refused the connector's unattended grant" + e.providerCode() + ".",
			"Fix the OAuth client's access or the assertion at the provider, or client_id, scopes, or " +
				"[auth.assertion] in the connector's config. " + adviseRegrant(e.Connector)
	case CaseScopeDenied:
		return "The upstream needs scopes the connector's unattended grant asked for but was not given.",
			"Allow the OAuth client those scopes at the provider, or fix scopes in the connector's [auth] table. " +
				adviseRegrant(e.Connector)
	case CaseDPoPAlgUnsupported:
		return "The connector's dpop is \"always\", but the authorization server does not accept " +
				"ES256 DPoP proofs, the only kind Wayfindr signs.",
			"Set dpop = \"auto\" or \"never\" in the connector's [auth] table to send bearer tokens."
	case CaseDPoPRejected:
		return "The authorization server rejected Wayfindr's DPoP proof" + e.providerCode() +
				". The clock of the host the daemon runs on may be wrong.",
			"Check the host's clock. If the server still rejects the proof, set dpop = \"never\" in the " +
				"connector's [auth] table to send bearer tokens; a config change retries at once."
	case CaseInvalidTemplate:
		return "A ${…} reference in the connector's config is malformed.",
			"Write references as ${secret:name} or ${env:NAME}."
	case CaseSecretUnreadable:
		return "The connector's config refers to " + e.Ref + ", but the stored secret cannot be decrypted, likely because the master key changed.",
			"Store it again with `wayfindr secret set " + strings.TrimPrefix(e.Ref, string(config.RefSecret)+":") + "`."
	case CaseSecret:
		return "The connector's config refers to " + e.Ref + ", but no such secret is stored.",
			"Store the secret, or fix the reference in the connector's config."
	case CaseEnv:
		return "The connector's config refers to " + e.Ref + ", but the daemon's environment does not set it.",
			"Set the variable in the daemon's environment and restart it, or fix the reference."
	case CaseSampling:
		return "This tool asks the client to sample a message with its model, which Wayfindr does not support.",
			"Use another tool, or run this one in a client connected to the upstream directly."
	case CaseConcurrentCalls:
		return "The upstream asked for input while another call to it was running, so Wayfindr could not tell which call the question was for.",
			"Call the tool again once the connector's other calls have finished."
	case CaseConnectRefused:
		return "The upstream refused the connection.", "Check that the server is running at the connector's url. " + adviseRetry
	case CaseConnectTimeout:
		return "The upstream did not answer the connect in time.", "Check that it is up and not overloaded. " + adviseRetry
	case CaseConnectCanceled:
		return "The call was cancelled while Wayfindr was still connecting to the upstream.", "Try again."
	case CaseSpawnFailed:
		return "The connector's command could not be started.",
			"Check command in the connector's config: it must exist and be executable on the daemon's PATH."
	case CaseAuthServer:
		return "The connector's authorization server cannot be reached or sent an unusable answer.",
			"Check the network and the provider's status. " + adviseRetry
	case CaseProviderUnavailable:
		return "The authorization server is temporarily unavailable or rate limiting" + e.providerCode() + ".",
			"Wait a moment and try again."
	case CaseAuthInternal:
		return "Wayfindr's credential handling failed for this call.", "Try again; if it persists, check the daemon log."
	}

	return "", "" // no Case, or not a Case constant
}

// adviseRegrant is the last step of fixing an unattended grant, as auth
// words it. auth_error is sticky: a config change reconnects the connector
// at once, but a fix at the provider does not, so the user revokes the
// connector's in-memory token, which reconnects it and runs the grant again.
func adviseRegrant(connector string) string {
	return "A config change retries at once; after a fix at the provider, run `wayfindr auth logout " + connector +
		"` to have Wayfindr run the grant again."
}

// providerCode is " (provider code X)", or "" when there is none.
func (e *Error) providerCode() string {
	if e.ProviderCode == "" {
		return ""
	}

	return " (provider code " + e.ProviderCode + ")"
}
