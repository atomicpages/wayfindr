package webui

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/atomicpages/wayfindr/internal/admin"
)

const (
	// ApprovePath is the console's approve page for a pending authorization
	// of Wayfindr's authorization server (ADR 0022, design §7.11). The
	// daemon sends the browser there, with ?req=<id>.
	ApprovePath = "/ui/oauth/approve"
	// DenyPath is where the approve page's Deny posts.
	DenyPath = "/ui/oauth/deny"
	// maxRequestID bounds the id the page takes from the query. The
	// server's ids are far shorter.
	maxRequestID = 128
)

// approveView is the approve page's data.
type approveView struct {
	// ID is the pending authorization's id.
	ID string
	// ClientName, ClientHost, and RedirectHost say who is asking and where
	// the browser returns to.
	ClientName, ClientHost, RedirectHost string
	// Started words when the request began.
	Started string
	// ReportedIP is the address the tunnel reported for the browser; empty
	// when it reported none.
	ReportedIP string
	// Name is the grant's name, refilled after a refusal.
	Name string
	// NoSuggestion is set when the server had no free name to suggest.
	NoSuggestion bool
	Connectors   []string
	Picked       map[string]bool
	// Error is why the last approve was refused.
	Error string
	// Arrived is how many connection requests arrived during the arm the
	// request was claimed from. Contested is set when that is more than
	// one: the page then says why and offers only Deny.
	Arrived   int
	Contested bool
}

// continueView is the page a browser without a claim-ready session gets.
type continueView struct {
	// Href is the approve page's own address, which the Continue link
	// follows.
	Href string
}

// approvePage serves GET /ui/oauth/approve?req=<id>.
//
// It claims the request only for a signed-in, top-level navigation. The
// browser arrives from another site, and the session cookie is
// SameSite=Strict, so that first arrival has no session: it gets a page with
// a Continue link, which is a same-site navigation and carries the cookie.
// The no-session page never calls the admin API, so a site that sends the
// browser here with a stranger's id claims nothing.
func (c *console) approvePage(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("req")
	s, signedIn := c.session(r)

	w.Header().Set("Cache-Control", "no-store")

	if !signedIn || !topLevelNavigation(r) {
		c.renderContinue(w, id)

		return
	}

	req, err := c.Admin.OAuthRequest(r.Context(), id)

	if err != nil {
		c.requestGone(w, r, s, err)

		return
	}

	c.renderApprove(w, r, s, approveFor(c.Now(), req), http.StatusOK)
}

// topLevelNavigation reports whether r is the browser following a link or
// typing an address, when it says how it was made. A request without
// Sec-Fetch headers (an older browser) is taken as one. An htmx fetch, an
// iframe or image, and a prefetch are not: they must not claim a request.
// Neither is a navigation from another site, even a same-site one such as a
// page on another loopback port: Sec-Fetch-Site must be absent, same-origin,
// or none (an address typed or a bookmark), so only the Continue link, which
// is same-origin, claims.
func topLevelNavigation(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}

	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" && mode != "navigate" {
		return false
	}

	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" && dest != "document" {
		return false
	}

	purpose := strings.ToLower(r.Header.Get("Sec-Purpose") + " " + r.Header.Get("Purpose"))

	return !strings.Contains(purpose, "prefetch") && !strings.Contains(purpose, "prerender")
}

// renderContinue renders the page with a Continue link to the approve page.
func (c *console) renderContinue(w http.ResponseWriter, id string) {
	href := ApprovePath

	if id != "" && len(id) <= maxRequestID {
		href += "?" + url.Values{"req": {id}}.Encode()
	}

	c.render(w, http.StatusOK, "continue", continueView{Href: href})
}

// approveFor builds the approve page's data for req, relative to now.
func approveFor(now time.Time, req admin.OAuthRequest) approveView {
	return approveView{
		ID:           req.ID,
		ClientName:   req.ClientName,
		ClientHost:   req.ClientHost,
		RedirectHost: req.RedirectHost,
		Started:      clock(now, req.StartedAt) + " (" + strings.ToLower(ago(now, req.StartedAt)) + ")",
		ReportedIP:   req.ReportedIP,
		Name:         req.Name,
		NoSuggestion: req.Name == "",
		Arrived:      req.Arrived,
		Contested:    req.Arrived > 1,
	}
}

// requestGone answers an admin refusal for a request id: a 404 is the page
// that says to start again, anything else is the usual error page.
func (c *console) requestGone(w http.ResponseWriter, r *http.Request, s sess, err error) {
	if ae := (*admin.APIError)(nil); errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		c.fail(w, r, s, http.StatusNotFound, "This request expired or was already answered. Start connecting again in the app.")

		return
	}

	c.failErr(w, r, s, err)
}

// renderApprove renders the approve page with its connectors.
func (c *console) renderApprove(w http.ResponseWriter, r *http.Request, s sess, body approveView, status int) {
	v, st, err := c.base(r, s, "cloud-agents", "Approve a connection")

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	v.Parent, v.ParentHref = "Cloud agents", cloudPath

	for _, cn := range st.Connectors {
		body.Connectors = append(body.Connectors, cn.Name)
	}

	v.Body = body
	c.render(w, status, "approve", v)
}

// approve serves POST /ui/oauth/approve: it creates the grant and sends the
// browser back to the client with the authorization code. A refused name or
// connector list, or a taken name, shows the page again, with the request
// still waiting.
func (c *console) approve(w http.ResponseWriter, r *http.Request, s sess) {
	id := r.PostForm.Get("req")
	name := strings.TrimSpace(r.PostForm.Get("name"))
	picked := r.PostForm["connector"]
	dec, err := c.Admin.ApproveOAuth(r.Context(), id, admin.OAuthApproval{Name: name, Connectors: picked})

	if err != nil {
		c.approveRefused(w, r, s, id, name, picked, err)

		return
	}

	c.sendBack(w, r, s, dec.Redirect)
}

// approveRefused shows the approve page again for a 400 or 409, which leave
// the request claimed, with what the user entered; any other refusal is the
// error page, a 404 the one that says to start again. A request more than
// one arrived with (a 409 too) is shown with the page's own words for it,
// not the refusal's.
func (c *console) approveRefused(w http.ResponseWriter, r *http.Request, s sess, id, name string, picked []string, err error) {
	ae := (*admin.APIError)(nil)

	if !errors.As(err, &ae) || (ae.Status != http.StatusBadRequest && ae.Status != http.StatusConflict) {
		c.requestGone(w, r, s, err)

		return
	}

	// The request is still claimed, so asking again returns it with no
	// other effect.
	req, getErr := c.Admin.OAuthRequest(r.Context(), id)

	if getErr != nil {
		c.requestGone(w, r, s, getErr)

		return
	}

	body := approveFor(c.Now(), req)
	body.Name, body.Picked = name, pickedSet(picked)

	if !body.Contested {
		body.Error = ae.Error()
	}

	c.renderApprove(w, r, s, body, ae.Status)
}

// deny serves POST /ui/oauth/deny: it ends the request and sends the browser
// back to the client with access_denied.
func (c *console) deny(w http.ResponseWriter, r *http.Request, s sess) {
	dec, err := c.Admin.DenyOAuth(r.Context(), r.PostForm.Get("req"))

	if err != nil {
		c.requestGone(w, r, s, err)

		return
	}

	c.sendBack(w, r, s, dec.Redirect)
}

// sendBack answers a form with a 303 to the client's redirect_uri. Only http
// and https are followed. The address holds the code, so it is neither shown
// nor logged, and the answer says to send no Referer: the page's own policy
// (same-origin) already sends none to another site, and this makes it so for
// the hop to the client whatever the policy.
func (c *console) sendBack(w http.ResponseWriter, r *http.Request, s sess, redirect string) {
	if u, err := url.Parse(redirect); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		c.fail(w, r, s, http.StatusBadGateway, "The client gave a return address that is not a web page.")

		return
	}

	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", redirect)
	w.WriteHeader(http.StatusSeeOther)
}

// backToConnectors serves GET /ui/oauth/deny, which a browser can reach
// through its history after the Deny post: it goes to the console's first
// page, with nothing about the request.
func (c *console) backToConnectors(w http.ResponseWriter, r *http.Request, _ sess) {
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}
