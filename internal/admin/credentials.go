package admin

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/atomicpages/wayfindr/internal/upstream"
)

// Credentials revokes connectors' stored credentials. The daemon adapts
// the auth broker to it; admin cannot import auth (design §2).
type Credentials interface {
	// Revoke deletes the connector's credential and cancels its pending
	// auth flows. The connector need not be configured: an orphaned
	// credential is revoked the same way.
	Revoke(ctx context.Context, connector string) (RevokeReport, error)
}

// Health reads what lies ahead of a connector. The daemon builds it from
// the auth broker and the connector's status, the one place every reader
// of warnings (status, the log, search_tools) gets them from.
type Health interface {
	// Health returns s's connector's token expiry and its warnings, notes
	// included. It never returns the token.
	Health(ctx context.Context, s upstream.ConnectorStatus) (ConnectorHealth, error)
}

// ConnectorHealth is what Health reports of a connector.
type ConnectorHealth struct {
	// TokenExpiresAt is when the access token expires; zero when there is
	// none or the expiry is unknown.
	TokenExpiresAt time.Time
	// Warnings are the failures ahead (GLOSSARY: Warning), notes included.
	Warnings []Warning
}

// RevokeReport is how a revoke went, as Credentials reports it.
type RevokeReport struct {
	// Found is false when the connector had no credential to revoke.
	Found bool
	// Status is the connector's status once its reconnect after the
	// revoke has settled; nil for a connector the config no longer names.
	Status *upstream.ConnectorStatus
}

// Revoked is the response to DELETE /admin/credentials/{connector}.
type Revoked struct {
	Connector string `json:"connector"`
	// Result is the connector's row after the revoke, in needs_auth; absent
	// for an orphaned credential, whose connector is no longer configured.
	Result *Connector `json:"result,omitempty"`
}

// revoke serves DELETE /admin/credentials/{connector}. A connector with no
// credential is 404 with a message that says so.
func (a api) revoke(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("connector")

	if a.Credentials == nil {
		http.Error(w, "revoking credentials is not available", http.StatusNotImplemented)

		return
	}

	rep, err := a.Credentials.Revoke(r.Context(), name)

	if err != nil {
		a.Logger.Error().Err(err).Str("connector", name).Msg("revoke credential")
		http.Error(w, "revoking the credential failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	if !rep.Found {
		// %.64q bounds the echoed name.
		http.Error(w, fmt.Sprintf("%.64q has no stored credential; there is nothing to revoke", name), http.StatusNotFound)

		return
	}

	a.Logger.Info().Str("connector", name).Msg("credential revoked")
	out := Revoked{Connector: name}

	if rep.Status != nil {
		rows := a.rows(r.Context(), []upstream.ConnectorStatus{*rep.Status})
		out.Result = &rows[0]
	}

	writeJSON(w, out)
}
