package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/atomicpages/wayfindr/internal/config"
)

// MaxOAuthSettingsBody bounds the body of PUT /admin/server/oauth: one
// boolean.
const MaxOAuthSettingsBody = 4 << 10

// OAuthSettings is server.oauth (ADR 0022, design §7.11): the response to GET
// and PUT /admin/server/oauth. The console toggles Enabled; nothing edits
// Clients through the API.
type OAuthSettings struct {
	// Enabled is the running config's server.oauth.enabled.
	Enabled bool `json:"enabled"`
	// Clients are the allowed client IDs, never null.
	Clients []string `json:"clients"`
	// Issuer is the running issuer (config.Server.Issuer); "" without a
	// public URL.
	Issuer string `json:"issuer,omitempty"`
}

// oauthSettings is the running config's settings.
func (a api) oauthSettings() OAuthSettings {
	s := a.Config()

	return OAuthSettings{
		Enabled: s.OAuth.Enabled,
		Clients: append([]string{}, s.OAuth.Clients...),
		Issuer:  config.Server{PublicURL: s.PublicURL}.Issuer(),
	}
}

// getOAuthSettings serves GET /admin/server/oauth: the running config's
// server.oauth.
func (a api) getOAuthSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, a.oauthSettings())
}

// putOAuthSettings serves PUT /admin/server/oauth: it writes
// server.oauth.enabled to the daemon's config file and applies it, so the
// answer is what the daemon is running.
func (a api) putOAuthSettings(w http.ResponseWriter, r *http.Request) {
	if a.SetOAuthEnabled == nil {
		http.Error(w, "turning OAuth on or off is not available", http.StatusNotImplemented)

		return
	}

	// A pointer, so a body without the key is refused rather than read as
	// turning it off.
	var body struct {
		Enabled *bool `json:"enabled"`
	}

	if err := readJSON(w, r, MaxOAuthSettingsBody, &body); err != nil || body.Enabled == nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			http.Error(w, "the body is too large", http.StatusRequestEntityTooLarge)

			return
		}

		http.Error(w, `body must be {"enabled": true} or {"enabled": false}`, http.StatusBadRequest)

		return
	}

	err := a.SetOAuthEnabled(r.Context(), *body.Enabled)

	if writeConfigError(w, err) {
		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Msg("set oauth enabled")
		http.Error(w, "turning OAuth on or off failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	writeJSON(w, a.oauthSettings())
}

// OAuthSettings calls GET /admin/server/oauth: the running config's
// server.oauth.
func (c *Client) OAuthSettings(ctx context.Context) (OAuthSettings, error) {
	var out OAuthSettings
	err := c.do(ctx, http.MethodGet, pathOAuthSettings, nil, &out)

	return out, err
}

// SetOAuthEnabled calls PUT /admin/server/oauth: it writes
// server.oauth.enabled to the daemon's config file and returns the settings
// once the daemon runs them.
func (c *Client) SetOAuthEnabled(ctx context.Context, enabled bool) (OAuthSettings, error) {
	var out OAuthSettings
	err := c.do(ctx, http.MethodPut, pathOAuthSettings, struct {
		Enabled bool `json:"enabled"`
	}{enabled}, &out)

	return out, err
}
