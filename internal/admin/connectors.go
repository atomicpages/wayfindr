package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/atomicpages/wayfindr/internal/upstream"
)

// MaxConnectorBody bounds the body of POST /admin/connectors: one
// connector's keys and the values of its secrets.
const MaxConnectorBody = 64 << 10

// How a ConnectorSpec chooses the connector's auth (ConnectorSpec.Auth).
const (
	// AuthDetect asks the server whether it uses OAuth, as `wayfindr
	// connector add` does. It is the default for a URL connector.
	AuthDetect = "detect"
	// AuthOAuth writes an OAuth auth table without asking.
	AuthOAuth = "oauth"
	// AuthNone writes no auth table.
	AuthNone = "none"
)

// How OAuth detection went (ConnectorAdded.OAuth).
const (
	// OAuthDetected: the server uses OAuth; an auth table was written.
	OAuthDetected = "detected"
	// OAuthOpen: the server needs no auth.
	OAuthOpen = "open"
	// OAuthUnclear: the server answered something else; no auth table.
	OAuthUnclear = "unclear"
	// OAuthUnreachable: the server could not be reached; no auth table.
	OAuthUnreachable = "unreachable"
	// OAuthNotChecked: nothing was asked: a command connector, auth chosen
	// in the spec, headers given, or a URL with a reference.
	OAuthNotChecked = "not_checked"
)

// ConnectorSpec is the body of POST /admin/connectors: one new connector,
// as `wayfindr connector add` takes it from its flags. A URL connector
// sets URL; a command connector sets Command. Secret values are stored as
// secrets named <connector>_<key> and referenced from the table; they are
// never written to the config file or logged.
type ConnectorSpec struct {
	Name string `json:"name"`

	URL string `json:"url,omitempty"`
	// Transport is "" (Streamable HTTP) or "sse".
	Transport     string            `json:"transport,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	SecretHeaders map[string]string `json:"secret_headers,omitempty"`
	// Auth is AuthDetect, AuthOAuth, or AuthNone; "" means AuthDetect. A
	// client ID or client secret means AuthOAuth.
	Auth         string   `json:"auth,omitempty"`
	ClientID     string   `json:"client_id,omitempty"`
	ClientSecret string   `json:"client_secret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`

	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	SecretEnv map[string]string `json:"secret_env,omitempty"`

	// Timeout is a Go duration, such as "2m"; "" is the default.
	Timeout string `json:"timeout,omitempty"`
}

// ConnectorReport is how an add went, as Options.AddConnector reports it.
type ConnectorReport struct {
	// Status is the connector's status once its first connect, and, when it
	// is ready, its first sync, are done.
	Status upstream.ConnectorStatus
	// OAuth is how OAuth detection went: one of the OAuth* constants.
	OAuth string
	// Secrets are the names of the secrets stored for the connector.
	Secrets []string
}

// ConnectorAdded is the response to POST /admin/connectors.
type ConnectorAdded struct {
	// Result is the new connector's row.
	Result Connector `json:"result"`
	// OAuth is how OAuth detection went: one of the OAuth* constants.
	OAuth string `json:"oauth"`
	// Secrets are the names of the secrets stored. Always an array.
	Secrets []string `json:"secrets"`
}

// AddError is an add the daemon refused, such as a taken or invalid name:
// the status to answer, the config package's Kind and Case, and the advice,
// whose text is the response body.
type AddError struct {
	Status     int
	Kind, Case string
	Advice     Advice
}

// Error is technical text for logs: the Kind and Case. What the error
// means to the user is its Advice.
func (e *AddError) Error() string { return "add connector: " + e.Kind + "/" + e.Case }

// addConnector serves POST /admin/connectors (ADR 0017). It never logs the
// body, which holds secret values (design §9).
func (a api) addConnector(w http.ResponseWriter, r *http.Request) {
	if a.AddConnector == nil {
		http.Error(w, "adding connectors is not available", http.StatusNotImplemented)

		return
	}

	var spec ConnectorSpec

	if err := readJSON(w, r, MaxConnectorBody, &spec); err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			http.Error(w, "the connector is too large", http.StatusRequestEntityTooLarge)

			return
		}

		http.Error(w, "body must be one connector spec", http.StatusBadRequest)

		return
	}

	rep, err := a.AddConnector(r.Context(), spec)

	if ae := (*AddError)(nil); errors.As(err, &ae) {
		http.Error(w, ae.Advice.text(), ae.Status)

		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Str("connector", spec.Name).Msg("add connector")
		http.Error(w, "adding the connector failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	out := ConnectorAdded{
		Result:  a.rows(r.Context(), []upstream.ConnectorStatus{rep.Status})[0],
		OAuth:   rep.OAuth,
		Secrets: rep.Secrets,
	}

	if out.Secrets == nil {
		out.Secrets = []string{}
	}

	writeJSON(w, out)
}

// AddConnector calls POST /admin/connectors: it adds spec's connector to the
// daemon's config file and returns its row once it has connected and, when
// ready, synced.
func (c *Client) AddConnector(ctx context.Context, spec ConnectorSpec) (ConnectorAdded, error) {
	var out ConnectorAdded
	err := c.do(ctx, http.MethodPost, pathConnectors, spec, &out)

	return out, err
}
