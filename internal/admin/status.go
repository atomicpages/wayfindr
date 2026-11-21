package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// Status is the body of GET /admin/status.
type Status struct {
	// Version is the daemon's build version.
	Version string `json:"version"`
	// Connectors are the configured connectors, sorted by name.
	Connectors []Connector `json:"connectors"`
	// OrphanedCredentials names, sorted, the connectors that have a stored
	// credential but are no longer in the config. Removing a connector
	// keeps its credential (design §7.8), so these linger until revoked.
	OrphanedCredentials []string `json:"orphaned_credentials"`
	// ConfigError is why the daemon's last config reload failed, while the
	// last good config keeps running; empty when the running config is the
	// file's.
	ConfigError string `json:"config_error,omitempty"`
	// Profiles are the running config's profiles by name (ADR 0011), as
	// the gateway applies them to a request that names one; absent when
	// the config defines none.
	Profiles map[string]config.Scope `json:"profiles,omitempty"`
}

// Connector is one connector's row: in Status, and in every response that
// reports how a connector came out of something the user did (SecretSet's
// Retried). Upstream's vocabulary (Kind, Case, Ref) and the technical
// Reason stay out of it: a user reads Advice, and the daemon log has the
// rest.
type Connector struct {
	Name  string         `json:"name"`
	State upstream.State `json:"state"`
	// Since is when the connector entered State, in UTC.
	Since time.Time `json:"since"`
	// Advice says what the connector's state means and how to fix it
	// (upstream.ConnectorStatus.Advice); absent once it is ready.
	Advice *Advice `json:"advice,omitempty"`
	// Tools is the number of the connector's catalog entries.
	Tools int `json:"tools"`
	// HiddenTools is how many of the connector's tools its tools list hid
	// at the last successful sync; absent when none.
	HiddenTools int `json:"hidden_tools,omitempty"`
	// LastSync is the connector's last recorded sync; absent before the
	// first.
	LastSync *Sync `json:"last_sync,omitempty"`
	// TokenExpiresAt is when the connector's OAuth access token expires,
	// in UTC; absent when it has none or the expiry is unknown.
	TokenExpiresAt *time.Time `json:"token_expires_at,omitempty"`
	// Warnings are the failures ahead that the user can head off
	// (Health); absent when there are none.
	Warnings []Warning `json:"warnings,omitempty"`
}

// Warning is one failure ahead (GLOSSARY: Warning): its code, whether it
// is only a note (nothing urgent yet), and the advice that heads it off.
type Warning struct {
	Code string `json:"code"`
	Note bool   `json:"note,omitempty"`
	Advice
}

// Advice is upstream.Advice as JSON: a summary, the action that fixes it,
// and the link that starts the auth flow when there is one.
type Advice struct {
	Summary string `json:"summary"`
	Action  string `json:"action,omitempty"`
	Link    string `json:"link,omitempty"`
}

// Sync is a connector's last recorded sync.
type Sync struct {
	At time.Time `json:"at"`
	// Error is why the sync failed; empty when it succeeded.
	Error string `json:"error,omitempty"`
}

// status serves GET /admin/status.
func (a api) status(w http.ResponseWriter, r *http.Request) {
	s, err := a.readStatus(r.Context())

	if err != nil {
		a.Logger.Error().Err(err).Msg("admin status")
		http.Error(w, "status unavailable; see the daemon log", http.StatusInternalServerError)

		return
	}

	writeJSON(w, s)
}

func (a api) readStatus(ctx context.Context) (Status, error) {
	cfg := a.Config()
	counts, err := a.Store.ToolCounts(ctx, a.UserID)

	if err != nil {
		return Status{}, err
	}

	s := Status{Version: a.Version, Connectors: []Connector{}, OrphanedCredentials: []string{}, ConfigError: cfg.ConfigError}

	for name, p := range cfg.Profiles {
		if s.Profiles == nil {
			s.Profiles = make(map[string]config.Scope, len(cfg.Profiles))
		}

		// A nil profile narrows nothing, as an empty one does.
		if p == nil {
			p = &config.Scope{}
		}

		s.Profiles[name] = *p
	}

	for _, cs := range a.Upstreams.Status() {
		c, err := a.connector(ctx, cs, counts)

		if err != nil {
			return Status{}, err
		}

		s.Connectors = append(s.Connectors, c)
	}

	creds, err := a.Store.ListCredentialConnectors(ctx, a.UserID)

	if err != nil {
		return Status{}, fmt.Errorf("orphaned credentials: %w", err)
	}

	for _, c := range creds {
		if !slices.Contains(cfg.Connectors, c) {
			s.OrphanedCredentials = append(s.OrphanedCredentials, c)
		}
	}

	return s, nil
}

// connector is cs's row, with its tool count from counts, its last sync
// from the store, and its token's expiry and warnings from Health. When the
// last sync or the health can't be read, it returns the row without it,
// and the error.
func (a api) connector(ctx context.Context, cs upstream.ConnectorStatus, counts map[string]int) (Connector, error) {
	c := Connector{
		Name:  cs.Connector,
		State: cs.State,
		Since: cs.Since.UTC(),
		Tools: counts[cs.Connector],
	}

	if adv := cs.Advice(); adv.Summary != "" {
		c.Advice = &Advice{Summary: adv.Summary, Action: adv.Action, Link: adv.Link}
	}

	rec, err := a.Store.LastSync(ctx, a.UserID, cs.Connector)

	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return c, err
	default:
		c.LastSync = &Sync{At: rec.At.UTC(), Error: rec.Error}
		c.HiddenTools = len(rec.Hidden)
	}

	if a.Health == nil {
		return c, nil
	}

	h, err := a.Health.Health(ctx, cs)

	if err != nil {
		return c, fmt.Errorf("health of %s: %w", cs.Connector, err)
	}

	if exp := h.TokenExpiresAt; !exp.IsZero() {
		exp = exp.UTC()
		c.TokenExpiresAt = &exp
	}

	c.Warnings = h.Warnings

	return c, nil
}
