package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/atomicpages/wayfindr/internal/store"
)

// MaxOAuthApprovalBody bounds the body of POST
// /admin/oauth/requests/{id}/approve: a name and a list of connector names.
const MaxOAuthApprovalBody = 4 << 10

// maxOAuthEmptyBody bounds the optional body of the arm and the deny: none
// or {}.
const maxOAuthEmptyBody = 1 << 10

// ErrInvalidApproval is what OAuthRequests.Approve wraps for a name the
// authorization server refuses although admin's own check let it through.
// The daemon turns oauthserver.ErrInvalidName into it, since admin cannot
// import oauthserver (design §2). The route answers it with 400.
var ErrInvalidApproval = errors.New("invalid approval")

// ErrContested is what OAuthRequests.Approve wraps when more than one
// authorization request arrived during the arm the request was claimed from
// (OAuthRequest.Arrived): one may be a stranger's, so none is approved. The
// daemon turns oauthserver.ErrContested into it. The route answers it with
// 409, and the request stays claimed for Deny.
var ErrContested = errors.New("contested authorization")

// OAuthRequests arms the authorization server and reads and answers its
// pending authorizations (ADR 0022, design §7.11). The daemon adapts
// oauthserver.Server to it; admin cannot import oauthserver (design §2).
type OAuthRequests interface {
	// Arm lets the next authorization through for 10 minutes and returns
	// when the arm ends (oauthserver.Server.Arm).
	Arm(ctx context.Context) (time.Time, error)
	// Get claims the pending authorization id (oauthserver.Server.Claim: it
	// consumes the arm and drops every other pending authorization; the
	// claimed id again has no other effect) and returns it with a free
	// grant name to suggest. An unknown, expired, answered, or unclaimable
	// id is an error wrapping store.ErrNotFound.
	Get(ctx context.Context, id string) (OAuthRequest, error)
	// Approve creates the grant and returns the URL the browser goes to
	// next. store.ErrNotFound as for Get, and for an id Get has not
	// claimed; store.ErrExists for a taken name, in which case the request
	// stays claimed; ErrInvalidApproval for a name that cannot be one;
	// ErrContested, changing nothing, when the request's Arrived is above
	// one.
	Approve(ctx context.Context, id, name string, connectors []string) (redirect string, err error)
	// Deny ends the request and returns the URL the browser goes to next.
	// store.ErrNotFound as for Approve.
	Deny(ctx context.Context, id string) (redirect string, err error)
}

// OAuthRequest is the response to GET /admin/oauth/requests/{id}: a pending
// authorization (GLOSSARY) as the approve page shows it.
type OAuthRequest struct {
	ID       string `json:"id"`
	ClientID string `json:"client_id"`
	// ClientName is the name the client's document gives itself, printable
	// and short; the client's host when it gives none.
	ClientName string `json:"client_name"`
	// ClientHost is the host[:port] of the client's ID.
	ClientHost string `json:"client_host"`
	// RedirectHost is the host[:port] the browser returns to.
	RedirectHost string    `json:"redirect_host"`
	StartedAt    time.Time `json:"started_at"`
	// ReportedIP is the browser's address as the tunnel reported it. Shown,
	// never trusted.
	ReportedIP string `json:"reported_ip,omitempty"`
	// Name is a free grant name to suggest; empty when every suggestion is
	// taken.
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
	// Arrived is how many authorization requests became pending during
	// the arm this one was claimed from, this one included. Only one was
	// expected: above one, approve answers 409 and only deny is left.
	Arrived int `json:"arrived"`
}

// OAuthApproval is the body of POST /admin/oauth/requests/{id}/approve.
type OAuthApproval struct {
	// Name follows the connector-name rule and is never "default".
	Name string `json:"name"`
	// Connectors are the configured connectors the grant may use; at least
	// one.
	Connectors []string `json:"connectors"`
}

// OAuthDecision is the response to approve and deny.
type OAuthDecision struct {
	// Redirect is where the browser goes next: the client's redirect_uri
	// with code (or error), state, and iss. It holds a one-use code, so
	// nothing logs it.
	Redirect string `json:"redirect"`
	// Name is the new grant's name; approve only.
	Name string `json:"name,omitempty"`
}

// OAuthArm is the response to POST /admin/oauth/arm.
type OAuthArm struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// The routes' paths of the OAuth requests. Client builds its requests from
// the same constants.
const (
	pathOAuthArm      = "/admin/oauth/arm"
	pathOAuthRequests = "/admin/oauth/requests/"
)

// withOAuth serves h only when the daemon passed Options.OAuth; otherwise
// it answers 501.
func (a api) withOAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.OAuth == nil {
			http.Error(w, "OAuth sign-in is not available", http.StatusNotImplemented)

			return
		}

		h(w, r)
	}
}

// readOptionalEmpty reads a body that may be absent or {}, and refuses
// anything else.
func readOptionalEmpty(w http.ResponseWriter, r *http.Request) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOAuthEmptyBody))

	if err != nil {
		return err
	}

	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	var v struct{}

	if err := dec.Decode(&v); err != nil {
		return errBody
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errBody
	}

	return nil
}

// armOAuth serves POST /admin/oauth/arm.
func (a api) armOAuth(w http.ResponseWriter, r *http.Request) {
	if err := readOptionalEmpty(w, r); err != nil {
		http.Error(w, "the body must be empty or {}", http.StatusBadRequest)

		return
	}

	if !a.Config().OAuth.Enabled {
		http.Error(w, "Turn on sign-in for cloud agents first.", http.StatusConflict)

		return
	}

	expires, err := a.OAuth.Arm(r.Context())

	if err != nil {
		a.Logger.Error().Err(err).Msg("arm oauth")
		http.Error(w, "arming sign-in failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	writeJSON(w, OAuthArm{ExpiresAt: expires.UTC()})
}

// getOAuthRequest serves GET /admin/oauth/requests/{id}. It is not a safe
// GET: the daemon claims the request.
func (a api) getOAuthRequest(w http.ResponseWriter, r *http.Request) {
	req, err := a.OAuth.Get(r.Context(), r.PathValue("id"))

	if errors.Is(err, store.ErrNotFound) {
		noPendingOAuth(w)

		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Msg("get oauth request")
		http.Error(w, "reading the request failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	req.StartedAt, req.ExpiresAt = req.StartedAt.UTC(), req.ExpiresAt.UTC()
	writeJSON(w, req)
}

// approveOAuthRequest serves POST /admin/oauth/requests/{id}/approve.
func (a api) approveOAuthRequest(w http.ResponseWriter, r *http.Request) {
	var in OAuthApproval

	if err := readJSON(w, r, MaxOAuthApprovalBody, &in); err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			http.Error(w, "request body is too large", http.StatusRequestEntityTooLarge)

			return
		}

		http.Error(w, `body must be {"name": "…", "connectors": ["…"]}`, http.StatusBadRequest)

		return
	}

	// The rules of a client token's name and connectors: a grant's name is a
	// client token's. The words are the approve page's own.
	connectors, msg := a.checkTokenSpec(TokenSpec{Name: in.Name, Connectors: in.Connectors}, "connection")

	if msg != "" {
		http.Error(w, msg, http.StatusBadRequest)

		return
	}

	redirect, err := a.OAuth.Approve(r.Context(), r.PathValue("id"), in.Name, connectors)

	switch {
	case errors.Is(err, store.ErrNotFound):
		noPendingOAuth(w)

		return
	case errors.Is(err, ErrContested):
		http.Error(w, contestedMessage, http.StatusConflict)

		return
	case errors.Is(err, store.ErrExists):
		http.Error(w, fmt.Sprintf("a client token or OAuth grant called %q already exists", in.Name), http.StatusConflict)

		return
	case errors.Is(err, ErrInvalidApproval):
		// %.64q bounds the echoed name.
		http.Error(w, fmt.Sprintf("%.64q can't be used as a name; choose another", in.Name), http.StatusBadRequest)

		return
	case err != nil:
		a.Logger.Error().Err(err).Str("grant", in.Name).Msg("approve oauth request")
		http.Error(w, "approving the request failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	// The redirect holds the authorization code: it is answered, never logged.
	a.Logger.Info().Str("grant", in.Name).Strs("connectors", connectors).Msg("oauth grant approved")
	writeJSON(w, OAuthDecision{Redirect: redirect, Name: in.Name})
}

// denyOAuthRequest serves POST /admin/oauth/requests/{id}/deny.
func (a api) denyOAuthRequest(w http.ResponseWriter, r *http.Request) {
	if err := readOptionalEmpty(w, r); err != nil {
		http.Error(w, "the body must be empty or {}", http.StatusBadRequest)

		return
	}

	redirect, err := a.OAuth.Deny(r.Context(), r.PathValue("id"))

	if errors.Is(err, store.ErrNotFound) {
		noPendingOAuth(w)

		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Msg("deny oauth request")
		http.Error(w, "denying the request failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	a.Logger.Info().Msg("oauth request denied")
	writeJSON(w, OAuthDecision{Redirect: redirect})
}

// contestedMessage is the 409 body of an approve that ErrContested refused.
const contestedMessage = "More than one connection request arrived after you selected Connect a client, " +
	"so this one can't be approved. Choose Deny, then select Connect a client and try again."

// noPendingOAuth answers a request id that is unknown, expired, answered, or
// not claimable. The id is not echoed.
func noPendingOAuth(w http.ResponseWriter) {
	http.Error(w, "no such pending authorization: it has expired or was already answered", http.StatusNotFound)
}

// ArmOAuth calls POST /admin/oauth/arm: Wayfindr's authorization server
// keeps authorization requests pending until the returned time. While
// server.oauth is off it is an error with status 409.
func (c *Client) ArmOAuth(ctx context.Context) (OAuthArm, error) {
	var out OAuthArm
	err := c.do(ctx, http.MethodPost, pathOAuthArm, nil, &out)

	return out, err
}

// OAuthRequest calls GET /admin/oauth/requests/{id}, which claims the
// pending authorization (it ends the arm and drops every other pending
// authorization). An unknown or unclaimable id is an error with status 404.
func (c *Client) OAuthRequest(ctx context.Context, id string) (OAuthRequest, error) {
	var out OAuthRequest
	err := c.do(ctx, http.MethodGet, pathOAuthRequests+url.PathEscape(id), nil, &out)

	return out, err
}

// ApproveOAuth calls POST /admin/oauth/requests/{id}/approve: it creates the
// grant a and returns where the browser goes next. A taken name, or a
// request that is not the only one that arrived during its arm
// (OAuthRequest.Arrived above one), is an error with status 409, and the
// request stays claimed.
func (c *Client) ApproveOAuth(ctx context.Context, id string, a OAuthApproval) (OAuthDecision, error) {
	var out OAuthDecision
	err := c.do(ctx, http.MethodPost, pathOAuthRequests+url.PathEscape(id)+"/approve", a, &out)

	return out, err
}

// DenyOAuth calls POST /admin/oauth/requests/{id}/deny: it ends the request
// and returns where the browser goes next.
func (c *Client) DenyOAuth(ctx context.Context, id string) (OAuthDecision, error) {
	var out OAuthDecision
	err := c.do(ctx, http.MethodPost, pathOAuthRequests+url.PathEscape(id)+"/deny", nil, &out)

	return out, err
}
