package admin

import (
	"context"
	"errors"
	"net/http"
)

// MaxPublicURLBody bounds the body of PUT /admin/server/public-url: one
// URL.
const MaxPublicURLBody = 4 << 10

// PublicURL is server.public_url (ADR 0021): the https address a tunnel
// serves the daemon at, or "" when none is set. It is the response to GET
// and PUT /admin/server/public-url and the body of the PUT.
//
// It is informational: the console uses it for setup text and to test the
// connection, and no request is ever checked against it.
type PublicURL struct {
	// URL is an absolute https URL with no path but "/", no query,
	// fragment, or userinfo; "" clears it. Always present in a response.
	URL string `json:"public_url"`
}

// publicURL serves GET /admin/server/public-url: the running config's
// server.public_url.
func (a api) publicURL(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, PublicURL{URL: a.Config().PublicURL})
}

// setPublicURL serves PUT /admin/server/public-url: it writes
// server.public_url to the daemon's config file and applies it, so the
// answer is the value the daemon is running.
func (a api) setPublicURL(w http.ResponseWriter, r *http.Request) {
	if a.SetPublicURL == nil {
		http.Error(w, "setting the public URL is not available", http.StatusNotImplemented)

		return
	}

	// A pointer, so a body without the key is refused rather than read as
	// clearing the URL.
	var body struct {
		PublicURL *string `json:"public_url"`
	}

	if err := readJSON(w, r, MaxPublicURLBody, &body); err != nil || body.PublicURL == nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			http.Error(w, "the URL is too large", http.StatusRequestEntityTooLarge)

			return
		}

		http.Error(w, `body must be {"public_url": "<https URL>"}; "" clears it`, http.StatusBadRequest)

		return
	}

	err := a.SetPublicURL(r.Context(), *body.PublicURL)

	if writeConfigError(w, err) {
		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Msg("set public url")
		http.Error(w, "setting the public URL failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	writeJSON(w, PublicURL{URL: *body.PublicURL})
}

// PublicURL calls GET /admin/server/public-url: the running config's
// server.public_url, "" when none is set.
func (c *Client) PublicURL(ctx context.Context) (PublicURL, error) {
	var out PublicURL
	err := c.do(ctx, http.MethodGet, pathPublicURL, nil, &out)

	return out, err
}

// SetPublicURL calls PUT /admin/server/public-url: it writes
// server.public_url to the daemon's config file, where "" removes it, and
// returns the value once the daemon runs it.
func (c *Client) SetPublicURL(ctx context.Context, publicURL string) (PublicURL, error) {
	var out PublicURL
	err := c.do(ctx, http.MethodPut, pathPublicURL, PublicURL{URL: publicURL}, &out)

	return out, err
}
