package auth

import "strconv"

// Advice is auth's user-facing reading of one of its errors (GLOSSARY:
// Advice): what went wrong, the action that fixes it, and the link that
// starts the auth flow when there is one. Presenters render it as is.
type Advice struct {
	Summary string
	Action  string
	Link    string
}

// Advise says what e means to the user and how to fix it. Its switch on
// [Case] is exhaustive (the exhaustive linter enforces it), so a new Case
// cannot ship without advice.
func Advise(e *Error) Advice {
	if e == nil {
		return Advice{}
	}

	const (
		reauth   = "Open the link to authorize the connector again."
		register = "Register an app with the provider and set client_id (and client_secret) in the connector's [auth] table."
		retry    = "Wayfindr tries again on the next call."
		// reconnects ends the advice of a config Case the connector's [tls]
		// table fixes: a saved config file reconnects it.
		reconnects = "Wayfindr reconnects the connector when the config file changes."
	)
	a := Advice{Link: e.Link}

	switch e.Case {
	case CaseNoCredential:
		a.Summary, a.Action = "The connector has not been authorized yet.", "Open the link to authorize it."
	case CaseURLChanged:
		a.Summary = "The connector's URL changed since it was authorized, so its token is not sent to the new URL."
		a.Action = "Open the link to authorize it for the new URL."
	case CaseClientChanged:
		a.Summary = "The connector's client_id changed since it was authorized. Authorize it again."
		a.Action = reauth
	case CaseAuthorizationServerChanged:
		a.Summary = "The server now sends Wayfindr to a different sign-in server than the one that issued the connector's token, " +
			"so the token is not refreshed there."
		a.Action = "If you trust the new sign-in server, open the link to authorize the connector again. " +
			"If not, set authorization_server in the connector's [auth] table to the one you trust."
	case CaseCredentialUnreadable:
		a.Summary, a.Action = "The stored credential cannot be read, for example because the master key changed.", reauth
	case CaseRefreshFailed:
		a.Summary, a.Action = "The connector's token expired and cannot be refreshed.", reauth
	case CaseTokenRevoked:
		a.Summary, a.Action = "The authorization server revoked or expired the connector's grant"+e.providerCode()+".", reauth
	case CaseProviderError:
		a.Summary = "The authorization server refused the token with an error Wayfindr does not recognise" + e.providerCode() + "."
		a.Action = reauth + " If that fails too, look the provider code up in the provider's documentation."
	case CaseTokenRejected:
		a.Summary, a.Action = "The upstream rejected a freshly refreshed token.", reauth
	case CaseStepUp:
		a.Summary = "The upstream needs scopes the connector was not granted."
		a.Action = "Open the link to authorize the connector with the larger set of scopes."
	case CaseRegistrationExpired:
		a.Summary = "The authorization server no longer accepts the OAuth client Wayfindr registered for this connector" +
			e.providerCode() + "."
		a.Action = reauth + " Wayfindr registers a new client."
	case CaseAuthorizationDenied:
		a.Summary = "The authorization was not granted" + e.providerCode() + "."
		a.Action = "Open the link to try again, and allow access on the provider's page."
	case CaseFlowExpired:
		a.Summary = "The authorization expired: it was not completed within 10 minutes."
		a.Action = "Open the link to start it again."
	case CaseFlowCancelled:
		a.Summary = "The authorization was cancelled: the connector's credential was revoked, " +
			"or its url or [auth] table changed or it was removed from the config, while it was in progress."
		a.Action = "Open the link to start it again."
	case CaseCallbackIssuerMismatch:
		a.Summary = "The authorization server's answer did not name the issuer this authorization was started with, " +
			"so Wayfindr did not use it. This may be a mix-up attack."
		a.Action = "Open the link to start it again. If it happens again, report it with the daemon log."
	case CaseAuthorizationPending:
		a.Summary = "The authorization is waiting for you to enter the code on the provider's page."
		a.Action = "Enter the code at the address Wayfindr showed, or open the link to start again."
	case CaseClientRejected:
		a.Summary = "The authorization server rejected the configured OAuth client" + e.providerCode() + "."
		a.Action = "Check client_id and client_secret in the connector's [auth] table."
	case CaseIssuerMismatch:
		a.Summary = "The authorization server's metadata names a different issuer than the one it was found for."
		a.Action = "Check authorization_server in the connector's [auth] table. " +
			"Set validate_issuer = false only if you trust this server."
	case CaseNoRegistration:
		a.Summary = "There is no OAuth client to authorize with: no client_id is configured, and the authorization server " +
			"supports neither client ID metadata documents nor dynamic client registration."
		a.Action = register
	case CaseRegistrationRejected:
		a.Summary = "The authorization server rejected dynamic client registration" + e.providerCode() + "."
		a.Action = register
	case CaseUnresolvedValue:
		a.Summary = "A value in the connector's config cannot be resolved: a ${…} reference " +
			"(url, client_secret, signing_key.key, assertion.value, or tls.key) or the assertion file."
		a.Action = "Set the missing secret or environment variable, create the file, or fix the reference."
	case CaseInvalidURL:
		a.Summary = "A URL in the connector's auth setup is not an absolute URL."
		a.Action = "Fix url, authorization_server, or token_endpoint in the connector's config."
	case CaseNoAuthorizationServer:
		a.Summary = "The connector's protected resource metadata names no authorization server."
		a.Action = "Set authorization_server, or authorization_endpoint and token_endpoint, in the connector's [auth] table."
	case CaseNoMetadata:
		a.Summary = "The authorization server publishes no metadata."
		a.Action = "Set authorization_endpoint and token_endpoint in the connector's [auth] table."
	case CasePKCEUnsupported:
		a.Summary = "The authorization server does not advertise PKCE (S256), which Wayfindr requires " +
			"for a connector without a client_id."
		a.Action = register
	case CaseDeviceUnsupported:
		a.Summary = "The authorization server does not support signing in with a code on another device (device code)."
		a.Action = "Set device_authorization_endpoint in the connector's [auth] table if the provider has one, " +
			"or sign in in a browser: drop --device, or set grant = \"authorization_code\"."
	case CaseAuthMethodUnsupported:
		a.Summary = "Wayfindr cannot authenticate as the connector's OAuth client: the authorization server " +
			"accepts no client authentication method that fits the client's credential."
		a.Action = "Set token_endpoint_auth_method in the connector's [auth] table to a method the provider " +
			"accepts, and configure its credential: client_secret, or signing_key for private_key_jwt."
	case CaseSigningKeyInvalid:
		a.Summary = "The connector's signing_key is not a private key Wayfindr can sign with, " +
			"or cannot sign the configured alg."
		a.Action = "Store a PEM private key (EC P-256 or P-384, RSA of 2048 bits or more, or Ed25519) with " +
			"`wayfindr connector edit " + e.Connector + " --signing-key < key.pem`, or fix signing_key.alg."
	case CaseTLSUntrusted:
		a.Summary = "Wayfindr does not trust the authorization server's TLS certificate: it is issued by an unknown " +
			"authority or fails verification, or the connector's ca_file cannot be read."
		a.Action = "If the server uses a private CA, set ca_file in the connector's [tls] table to that CA's PEM " +
			"certificate. " + reconnects
	case CaseTLSHandshake:
		a.Summary = "The TLS handshake with the authorization server failed: it refused the connector's client " +
			"certificate, or the lack of one, or the certificate cannot be loaded."
		a.Action = "Check cert_file and key_file (or key) in the connector's [tls] table: the certificate must be " +
			"one the server accepts, and match its key. " + reconnects
	case CaseGrantRejected:
		a.Summary = "The authorization server refused the connector's unattended grant" + e.providerCode() + "."
		a.Action = "Fix the OAuth client's access or the assertion at the provider, or client_id, scopes, or " +
			"[auth.assertion] in the connector's config. " + regrant(e.Connector)
	case CaseScopeDenied:
		a.Summary = "The upstream needs scopes the connector's unattended grant asked for but was not given."
		a.Action = "Allow the OAuth client those scopes at the provider, or fix scopes in the connector's [auth] table. " +
			regrant(e.Connector)
	case CaseDPoPAlgUnsupported:
		a.Summary = "The connector's dpop is \"always\", but the authorization server does not accept " +
			"ES256 DPoP proofs, the only kind Wayfindr signs."
		a.Action = "Set dpop = \"auto\" or \"never\" in the connector's [auth] table to send bearer tokens."
	case CaseDPoPRejected:
		a.Summary = "The authorization server rejected Wayfindr's DPoP proof" + e.providerCode() +
			". The clock of the host the daemon runs on may be wrong."
		a.Action = "Check the host's clock. If the server still rejects the proof, set dpop = \"never\" in the " +
			"connector's [auth] table to send bearer tokens; a config change retries at once."
	case CaseTransport:
		a.Summary, a.Action = "The authorization server cannot be reached.", "Check the network. "+retry
	case CaseProviderUnavailable:
		a.Summary = "The authorization server is temporarily unavailable or rate limiting Wayfindr" + e.providerCode() + "."
		a.Action = "Try again later. " + retry
	case CaseSlowDown:
		a.Summary = "The authorization server asked Wayfindr to poll less often" + e.providerCode() + "."
		a.Action = retry
	case CaseMalformedResponse:
		a.Summary = "The authorization server sent a response Wayfindr cannot read."
		a.Action = retry + " If it persists, report it with the daemon log."
	case CaseUnexpectedStatus:
		a.Summary = "The authorization server answered with an unexpected HTTP status."
		a.Action = retry + " If it persists, check the provider's status."
	case CaseStore:
		a.Summary = "Wayfindr could not read or write its credential store."
		a.Action = "Check the daemon log and the disk the store is on."
	case CaseNotReplayable:
		a.Summary = "The upstream rejected the token and the request could not be sent again. The token has been refreshed."
		a.Action = "Retry the call."
	case CaseUnknownConnector:
		a.Summary = "No connector named " + strconv.Quote(e.Connector) + " is configured."
		a.Action = "Check the connector's name with `wayfindr status`."
	case CaseNotOAuth:
		a.Summary = e.Connector + ` does not use OAuth: it has no [auth] table of type "oauth".`
		a.Action = "Add one with `wayfindr connector edit " + e.Connector + " --oauth` to authorize it through Wayfindr, or put its API key in headers."
	case CaseNotInteractive:
		a.Summary = e.Connector + " uses an unattended grant, so there is nothing to sign in to: Wayfindr gets its token itself."
		a.Action = "Check it with `wayfindr status`. To have Wayfindr get a new token, run `wayfindr auth logout " +
			e.Connector + "`."
	}

	return a
}

// regrant is the last step of fixing an unattended grant. auth_error is
// sticky: a config change reconnects the connector at once, but a fix at
// the provider does not, so the user revokes the connector's in-memory
// token, which reconnects it and runs the grant again.
func regrant(connector string) string {
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
